// Package tasks implements the PRD F6 unified operations plane (D13): a
// task queue + board backing fully-managed dispatch. The Hub spawns the
// agent headless (claude -p / codex exec), owns its lifecycle (并行、失败
// 重试、暂停续跑、取消) and keeps the approval inbox in the loop — managed
// Claude Code runs carry a settings file whose PreToolUse hook routes every
// tool call through the same gate as attached sessions.
package tasks

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/agenttarmac/agenttarmac/internal/acpclient"
	"github.com/agenttarmac/agenttarmac/internal/runner"
)

// EventSink receives task lifecycle events (EVENT_MODEL v0.2 type "task").
type EventSink interface {
	TaskEvent(taskID, action, sessionID, agentKind, title, errText string)
}

// FrameDispatch routes agent output through the standard frame pipeline.
type FrameDispatch interface {
	Dispatch(agentKind, method string, params json.RawMessage) error
}

// Store is the task persistence surface (subset of *acpclient.Store).
type Store interface {
	InsertTask(ctx context.Context, t *acpclient.Task) error
	GetTask(ctx context.Context, id string) (*acpclient.Task, error)
	ListTasks(ctx context.Context, state string) ([]*acpclient.Task, error)
	QueuedTasks(ctx context.Context) ([]*acpclient.Task, error)
	UpdateTask(ctx context.Context, t *acpclient.Task) error
	CreateSession(ctx context.Context, s *acpclient.Session) error
	UpdateSession(ctx context.Context, s *acpclient.Session) error
	GetSession(ctx context.Context, id string) (*acpclient.Session, error)
}

// Queue is the task dispatcher. All methods are safe for concurrent use.
type Queue struct {
	store             Store
	sink              EventSink
	frames            FrameDispatch
	hookCommand       string // hub-hook command for managed CC approvals
	permissionDecider func(ctx context.Context, kind string, in acpclient.HookIntake) (acpclient.Decision, string, error)
	concurrency       int
	taskTimeout       time.Duration // hard cap per managed run (0 = no cap)

	mu      sync.Mutex
	paused  bool
	running map[string]*exec.Cmd // taskID → process
	cancels map[string]context.CancelFunc
	wakeCh  chan struct{}
	wg      sync.WaitGroup

	statePath string // optional queue-state file (pause persistence, B11)

	spawner func(ctx context.Context, t *acpclient.Task, sessionID string) (*exec.Cmd, string, error)
}

// New creates the queue and starts the dispatch loop.
func New(store Store, sink EventSink, frames FrameDispatch, hookCommand string, concurrency int) *Queue {
	if concurrency <= 0 {
		concurrency = 2
	}
	q := &Queue{
		store:       store,
		sink:        sink,
		frames:      frames,
		hookCommand: hookCommand,
		concurrency: concurrency,
		running:     map[string]*exec.Cmd{},
		cancels:     map[string]context.CancelFunc{},
		wakeCh:      make(chan struct{}, 1),
	}
	q.loadPaused()
	go q.loop()
	// Recover: tasks left "running" by a daemon restart are marked failed
	// (their processes died with the daemon).
	q.recoverOrphans()
	q.wake()
	return q
}

// loadPaused restores the persisted pause flag (vibe-trial B11: a daemon
// restart used to silently un-pause the queue and fire held-back tasks).
func (q *Queue) loadPaused() {
	if q.statePath == "" {
		return
	}
	raw, err := os.ReadFile(q.statePath)
	if err != nil {
		return
	}
	var st struct {
		Paused bool `json:"paused"`
	}
	if json.Unmarshal(raw, &st) == nil && st.Paused {
		q.mu.Lock()
		q.paused = true
		q.mu.Unlock()
	}
}

// savePaused persists the pause flag; best-effort (a failed write only
// means the flag is not restored across restarts).
func (q *Queue) savePaused() {
	if q.statePath == "" {
		return
	}
	st := struct {
		Paused bool `json:"paused"`
	}{Paused: q.Paused()}
	if raw, err := json.Marshal(st); err == nil {
		_ = os.WriteFile(q.statePath, raw, 0o600)
	}
}

// SetStatePath enables pause persistence across daemon restarts (B11).
// Call right after New; empty (default) keeps the flag in-memory only.
func (q *Queue) SetStatePath(path string) {
	q.mu.Lock()
	q.statePath = path
	q.mu.Unlock()
	q.loadPaused()
}

// SetTaskTimeout sets the hard per-run cap (vibe-trial A1 companion: a hung
// agent must not pin a concurrency slot forever). 0 disables the cap.
func (q *Queue) SetTaskTimeout(d time.Duration) {
	q.taskTimeout = d
}

// recoverOrphans fails tasks that were running when the daemon died, and
// settles their session rows to a terminal state (vibe-trial A6: sessions
// used to stay "active" forever after a restart). It also sweeps temp
// settings files leaked by crashed runs (S7).
func (q *Queue) recoverOrphans() {
	ctx := context.Background()
	running, err := q.store.ListTasks(ctx, acpclient.TaskRunning)
	if err != nil {
		return
	}
	for _, t := range running {
		t.State = acpclient.TaskFailed
		t.Error = "daemon restart during run"
		t.FinishedAt = time.Now().Unix()
		_ = q.store.UpdateTask(ctx, t)
		if t.SessionID != "" {
			q.settleSession(ctx, t.SessionID, acpclient.StatusFailed, t.Error, 0)
		}
		q.emit(t, "failed")
	}
	sweepStaleSettings()
}

// sweepStaleSettings deletes agenthub-task-*-settings.json files older than
// a day from the temp dir (crashed runs never cleaned them up, S7).
func sweepStaleSettings() {
	matches, err := filepath.Glob(filepath.Join(os.TempDir(), "agenthub-task-*-settings.json"))
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-24 * time.Hour)
	for _, m := range matches {
		if st, err := os.Stat(m); err == nil && st.ModTime().Before(cutoff) {
			_ = os.Remove(m)
		}
	}
}

// settleSession writes a terminal state onto the task's session row
// (vibe-trial A4: task endings used to never reach the session row, leaving
// every managed session "active" with zero turns). status is one of the
// acpclient Status* constants; turnsDelta increments the turn count.
// A terminal session/update frame is dispatched as well, so the existing
// notification plumbing (feishu/wecom/web-push on failed, vibe-trial B7)
// and the console event stream see the ending.
func (q *Queue) settleSession(ctx context.Context, sessionID, status, lastErr string, turnsDelta int) {
	if sessionID == "" {
		return
	}
	sess, err := q.store.GetSession(ctx, sessionID)
	if err != nil {
		return
	}
	// Terminal frame for the event stream + notification plumbing (B7).
	// Skipped when the row already reached this exact terminal state —
	// claude_code runs set completed via the result-line mapping before the
	// queue unwinds, and a repeat frame would bounce off the adapter's
	// transition state machine as an error event.
	already := sess.IsTerminal && sess.Status == status
	if !already {
		if raw, err := json.Marshal(map[string]any{
			"sessionId": sessionID, "status": status, "error": lastErr,
		}); err == nil {
			kind := sess.AgentKind
			if kind == "" {
				kind = acpclient.AgentKindClaudeCode
			}
			_ = q.Dispatch(kind, "session/update", raw)
		}
	}
	if status != "" {
		sess.Status = status
	}
	if lastErr != "" {
		sess.LastError = truncate(lastErr, 500)
	}
	if turnsDelta > 0 {
		sess.Turns += turnsDelta
	}
	sess.IsTerminal = true
	if status == acpclient.StatusClosed {
		sess.IsClosed = true
	}
	_ = q.store.UpdateSession(ctx, sess)
}

func (q *Queue) wake() {
	select {
	case q.wakeCh <- struct{}{}:
	default:
	}
}

// loop dispatches queued tasks while slots are free and the queue is not
// paused.
func (q *Queue) loop() {
	for range q.wakeCh {
		for {
			q.mu.Lock()
			paused := q.paused
			slots := q.concurrency - len(q.running)
			q.mu.Unlock()
			if paused || slots <= 0 {
				break
			}
			ctx := context.Background()
			queued, err := q.store.QueuedTasks(ctx)
			if err != nil || len(queued) == 0 {
				break
			}
			t := queued[0]
			if !q.tryStart(ctx, t) {
				break // start failed; avoid a hot loop
			}
		}
	}
}

// maxPromptLen caps the dispatch prompt (vibe-trial B9: unbounded prompts
// could be queued and only fail at spawn time).
const maxPromptLen = 64 * 1024

// Enqueue adds a task (PRD D13: 派发) and wakes the dispatcher.
// Fail-fast validation (vibe-trial B9): cwd must exist, the agent binary
// must be resolvable, and an identical queued/running task is returned
// as-is instead of being duplicated.
func (q *Queue) Enqueue(ctx context.Context, agentKind, cwd, prompt string) (*acpclient.Task, error) {
	if agentKind != acpclient.AgentKindClaudeCode && agentKind != acpclient.AgentKindCodex &&
		agentKind != acpclient.AgentKindOpenCode && agentKind != acpclient.AgentKindHermes {
		return nil, fmt.Errorf("tasks: agent kind %q is not drivable (v1: claude_code | codex | opencode | hermes)", agentKind)
	}
	if strings.TrimSpace(cwd) == "" || strings.TrimSpace(prompt) == "" {
		return nil, fmt.Errorf("tasks: cwd and prompt are required")
	}
	if len(prompt) > maxPromptLen {
		return nil, fmt.Errorf("tasks: prompt exceeds %d bytes", maxPromptLen)
	}
	if st, err := os.Stat(cwd); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("tasks: cwd %q is not an accessible directory", cwd)
	}
	if _, err := resolveAgentBinary(agentKind); err != nil {
		return nil, err
	}
	if dup := q.findDuplicate(ctx, agentKind, cwd, prompt); dup != nil {
		return dup, nil
	}
	t := &acpclient.Task{
		ID:        "task_" + newHex8(),
		AgentKind: agentKind,
		CWD:       cwd,
		Prompt:    prompt,
		State:     acpclient.TaskQueued,
	}
	if err := q.store.InsertTask(ctx, t); err != nil {
		return nil, err
	}
	q.emit(t, "queued")
	q.wake()
	return t, nil
}

// EnqueueInSession is Enqueue with a caller-chosen managed session id (the
// continue/redispatch flow pre-creates the session row so lineage survives
// scheduling races, vibe-trial A10).
func (q *Queue) EnqueueInSession(ctx context.Context, agentKind, cwd, prompt, sessionID string) (*acpclient.Task, error) {
	t, err := q.Enqueue(ctx, agentKind, cwd, prompt)
	if err != nil {
		return nil, err
	}
	t.SessionID = sessionID
	if err := q.store.UpdateTask(ctx, t); err != nil {
		return nil, err
	}
	return t, nil
}

// findDuplicate returns an existing queued/running task with the same
// agent+cwd+prompt (lightweight idempotency for double-clicks and retries
// of the same request, vibe-trial B9).
func (q *Queue) findDuplicate(ctx context.Context, agentKind, cwd, prompt string) *acpclient.Task {
	for _, state := range []string{acpclient.TaskQueued, acpclient.TaskRunning} {
		list, err := q.store.ListTasks(ctx, state)
		if err != nil {
			return nil
		}
		for _, t := range list {
			if t.AgentKind == agentKind && t.CWD == cwd && t.Prompt == prompt {
				return t
			}
		}
	}
	return nil
}

// Retry requeues a failed or cancelled task (PRD D13: 失败重试；自动重试
// 发生在 run 尾部。vibe-trial B10: cancelled tasks used to be unrecoverable).
func (q *Queue) Retry(ctx context.Context, id string) (*acpclient.Task, error) {
	t, err := q.store.GetTask(ctx, id)
	if err != nil {
		return nil, err
	}
	if t.State != acpclient.TaskFailed && t.State != acpclient.TaskCancelled {
		return nil, fmt.Errorf("tasks: only failed or cancelled tasks can be retried (state %s)", t.State)
	}
	t.State = acpclient.TaskQueued
	t.Error = ""
	if err := q.store.UpdateTask(ctx, t); err != nil {
		return nil, err
	}
	q.emit(t, "queued")
	q.wake()
	return t, nil
}

// Cancel stops a queued or running task (PRD D13: 可暂停续跑中的取消面).
// The cancelled state is persisted BEFORE the run goroutine is woken
// (vibe-trial A5: the old order let the goroutine unwind into fail(),
// auto-requeueing a task the user had just cancelled, and erasing the
// cancellation from the record).
func (q *Queue) Cancel(ctx context.Context, id string) (*acpclient.Task, error) {
	t, err := q.store.GetTask(ctx, id)
	if err != nil {
		return nil, err
	}
	switch t.State {
	case acpclient.TaskQueued:
		t.State = acpclient.TaskCancelled
		t.FinishedAt = time.Now().Unix()
		if err := q.store.UpdateTask(ctx, t); err != nil {
			return nil, err
		}
		q.emit(t, "cancelled")
		q.wake()
	case acpclient.TaskRunning:
		t.State = acpclient.TaskCancelled
		t.Error = "cancelled by user"
		t.FinishedAt = time.Now().Unix()
		if err := q.store.UpdateTask(ctx, t); err != nil {
			return nil, err
		}
		q.mu.Lock()
		cancel := q.cancels[id]
		q.mu.Unlock()
		if cancel != nil {
			cancel() // the run goroutine observes the persisted state on unwind
		}
		q.emit(t, "cancelled")
		q.settleSession(ctx, t.SessionID, acpclient.StatusClosed, "cancelled by user", 0)
	default:
		return nil, fmt.Errorf("tasks: task %s is %s, cannot cancel", id, t.State)
	}
	return t, nil
}

// Pause stops dispatching queued tasks (running ones keep going; 续跑 on
// resume). The flag persists across daemon restarts when a state path is
// configured (vibe-trial B11).
func (q *Queue) Pause() {
	q.mu.Lock()
	q.paused = true
	q.mu.Unlock()
	q.savePaused()
}

// Resume re-enables dispatching.
func (q *Queue) Resume() {
	q.mu.Lock()
	q.paused = false
	q.mu.Unlock()
	q.savePaused()
	q.wake()
}

// Paused reports the queue pause state.
func (q *Queue) Paused() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.paused
}

// tryStart moves a queued task to running and spawns the agent headless.
func (q *Queue) tryStart(ctx context.Context, t *acpclient.Task) bool {
	// Managed session row (PRD D13: Hub 启动的会话全托管). A caller-provided
	// session id (EnqueueInSession) wins over the derived default.
	sessionID := t.SessionID
	if sessionID == "" {
		sessionID = "task-" + strings.TrimPrefix(t.ID, "task_")
	}
	sess := &acpclient.Session{
		ID:        sessionID,
		AgentKind: t.AgentKind,
		CWD:       t.CWD,
		Status:    acpclient.StatusActive,
		Origin:    "managed",
		Title:     truncate(t.Prompt, 80),
	}
	if existing, gerr := q.store.GetSession(ctx, sessionID); gerr == nil {
		// Session already exists (retry of a previously failed task); reset it
		// instead of re-inserting (avoids UNIQUE constraint failure).
		existing.Status = acpclient.StatusActive
		existing.CWD = t.CWD
		existing.Title = truncate(t.Prompt, 80)
		existing.LastError = ""
		existing.IsTerminal = false
		existing.IsClosed = false
		if err := q.store.UpdateSession(ctx, existing); err != nil {
			q.fail(ctx, t, "reset session: "+err.Error())
			return true
		}
	} else if err := q.store.CreateSession(ctx, sess); err != nil {
		q.fail(ctx, t, "create session: "+err.Error())
		return true
	}

	runCtx, cancel := context.WithCancel(ctx)
	cmd, settingsFile, err := q.spawnCommand(runCtx, t, sessionID)
	if err != nil {
		cancel()
		// Roll back the session row so the board stays truthful.
		sess.Status = acpclient.StatusFailed
		sess.LastError = err.Error()
		q.fail(ctx, t, err.Error())
		return true
	}

	t.State = acpclient.TaskRunning
	t.SessionID = sessionID
	t.StartedAt = time.Now().Unix()
	if err := q.store.UpdateTask(ctx, t); err != nil {
		cancel()
		q.fail(ctx, t, err.Error())
		return true
	}

	q.mu.Lock()
	q.running[t.ID] = cmd
	q.cancels[t.ID] = cancel
	q.mu.Unlock()
	q.emit(t, "started")
	q.wake() // fill remaining slots

	q.wg.Add(1)
	go func() {
		defer q.wg.Done()
		// Hard per-run cap (vibe-trial A1 companion): a hung agent must not
		// pin a concurrency slot forever. 0 = no cap.
		var timedOut atomic.Bool
		var timer *time.Timer
		if q.taskTimeout > 0 {
			timer = time.AfterFunc(q.taskTimeout, func() {
				timedOut.Store(true)
				_ = cmd.Process.Kill()
			})
			defer timer.Stop()
		}
		err := q.streamTask(runCtx, t, sessionID, cmd)
		if err != nil && timedOut.Load() {
			err = fmt.Errorf("task timeout after %s", q.taskTimeout)
		}
		if settingsFile != "" {
			_ = os.Remove(settingsFile)
		}
		q.mu.Lock()
		delete(q.running, t.ID)
		delete(q.cancels, t.ID)
		paused := q.paused
		q.mu.Unlock()

		fresh, _ := q.store.GetTask(ctx, t.ID)
		if fresh != nil && fresh.State == acpclient.TaskCancelled {
			// Cancel() already persisted the terminal state; do not let the
			// unwind downgrade it to failed or requeue it (vibe-trial A5).
			q.emit(fresh, "cancelled")
			return
		}
		if runCtx.Err() != nil && paused {
			// Queue paused while running: the run was cancelled by shutdown;
			// record and stop.
			return
		}
		if err != nil {
			q.fail(ctx, t, err.Error())
			return
		}
		t.State = acpclient.TaskFinished
		t.FinishedAt = time.Now().Unix()
		_ = q.store.UpdateTask(ctx, t)
		q.settleSession(ctx, t.SessionID, acpclient.StatusCompleted, "", 1)
		q.emit(t, "finished")
		q.wake()
	}()
	return true
}

// fail marks a task failed with the auto-retry policy (PRD D13: 失败重试).
// A task the user already cancelled is never resurrected by an auto-retry
// (vibe-trial A5), and any terminal failure settles the session row (A4).
func (q *Queue) fail(ctx context.Context, t *acpclient.Task, msg string) {
	fresh, _ := q.store.GetTask(ctx, t.ID)
	if fresh != nil {
		if fresh.State == acpclient.TaskCancelled {
			q.emit(fresh, "cancelled")
			return
		}
		t = fresh
	}
	t.Error = truncate(msg, 500)
	t.FinishedAt = time.Now().Unix()
	if t.Retries < t.MaxRetries {
		t.Retries++
		t.State = acpclient.TaskQueued
		_ = q.store.UpdateTask(ctx, t)
		q.emit(t, "queued")
		q.wake()
		return
	}
	t.State = acpclient.TaskFailed
	_ = q.store.UpdateTask(ctx, t)
	q.settleSession(ctx, t.SessionID, acpclient.StatusFailed, t.Error, 0)
	q.emit(t, "failed")
	q.wake()
}

// emit publishes one task event (EVENT_MODEL v0.2 type "task").
func (q *Queue) emit(t *acpclient.Task, action string) {
	if q.sink != nil {
		q.sink.TaskEvent(t.ID, action, t.SessionID, t.AgentKind, truncate(t.Prompt, 60), t.Error)
	}
}

// resolveAgentBinary resolves the agent executable for a task, returning a
// clear error when the CLI is not installed (PRD F6: fail fast with an
// actionable message instead of an opaque exec error at spawn time).
func resolveAgentBinary(kind string) (string, error) {
	var name string
	switch kind {
	case acpclient.AgentKindClaudeCode:
		name = "claude"
	case acpclient.AgentKindCodex:
		name = "codex"
	case acpclient.AgentKindOpenCode:
		name = "opencode"
	case acpclient.AgentKindHermes:
		name = "hermes-acp"
	default:
		return "", fmt.Errorf("tasks: unknown agent kind %q", kind)
	}
	p, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("%s CLI not found in PATH; install it or add it to PATH", name)
	}
	return p, nil
}

// spawnCommand builds the headless agent command for a managed task.
//
//	claude_code: `claude -p <prompt> --output-format stream-json --verbose
//	             --settings <tmpfile>` — the settings file routes PreToolUse
//	             through the hub-hook binary so managed approvals land in the
//	             same inbox (PRD D13: Hub 审批).
//	codex:       `codex exec --json <prompt>` (approvals ride the globally
//	             installed codex-hook).
func (q *Queue) spawnCommand(ctx context.Context, t *acpclient.Task, sessionID string) (*exec.Cmd, string, error) {
	if q.spawner != nil {
		return q.spawner(ctx, t, sessionID)
	}
	bin, err := resolveAgentBinary(t.AgentKind)
	if err != nil {
		return nil, "", err
	}
	if t.AgentKind == acpclient.AgentKindClaudeCode {
		if q.hookCommand == "" {
			return nil, "", fmt.Errorf("managed claude_code 需要 hub-hook（-hook-path 未配置或文件缺失）")
		}
		settings := map[string]any{
			"hooks": map[string]any{
				"PreToolUse": []any{map[string]any{
					"matcher": "",
					"hooks":   []any{map[string]any{"type": "command", "command": q.hookCommand}},
				}},
			},
		}
		raw, err := json.Marshal(settings)
		if err != nil {
			return nil, "", err
		}
		// Include the task id so concurrent tasks never collide on the same
		// temp settings file (session id alone repeats across retries).
		file := filepath.Join(os.TempDir(), "agenthub-task-"+sessionID+"-"+t.ID+"-settings.json")
		if err := os.WriteFile(file, raw, 0o600); err != nil {
			return nil, "", err
		}
		cmd := exec.CommandContext(ctx, bin, "-p", t.Prompt,
			"--output-format", "stream-json", "--verbose",
			"--settings", file)
		cmd.Dir = t.CWD
		return cmd, file, nil
	}
	// opencode (ACP stdio server: `opencode acp`).
	if t.AgentKind == acpclient.AgentKindOpenCode {
		cmd := exec.CommandContext(ctx, bin, "acp")
		cmd.Dir = t.CWD
		return cmd, "", nil
	}
	// hermes (ACP stdio server: `hermes-acp`).
	if t.AgentKind == acpclient.AgentKindHermes {
		cmd := exec.CommandContext(ctx, bin)
		cmd.Dir = t.CWD
		return cmd, "", nil
	}
	// codex
	cmd := exec.CommandContext(ctx, bin, "exec", "--json", t.Prompt)
	cmd.Dir = t.CWD
	return cmd, "", nil
}

// streamTask parses the agent's JSONL stdout and routes tool frames through
// the standard pipeline (same mapping as native resume).
func (q *Queue) streamTask(ctx context.Context, t *acpclient.Task, sessionID string, cmd *exec.Cmd) error {
	// opencode/hermes are driven as stdio ACP servers (bidirectional JSON-RPC
	// with unified Hub approval), not line-mapped like Claude Code / Codex.
	if t.AgentKind == acpclient.AgentKindOpenCode || t.AgentKind == acpclient.AgentKindHermes {
		return q.streamACP(ctx, t, sessionID, cmd)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn %s: %w", t.AgentKind, err)
	}
	sc := newScanner(stdout)
	for sc.Scan() {
		if ctx.Err() != nil {
			_ = cmd.Process.Kill()
			return ctx.Err()
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		q.mapLine(t.AgentKind, sessionID, line)
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return cmd.Wait()
}

// mapLine converts one stream-json / codex-JSONL line into Hub frames
// (shared mapping with the F7 native resume, internal/runner).
func (q *Queue) mapLine(kind, sessionID, line string) {
	runner.MapStreamLine(q, kind, sessionID, line)
}

// Dispatch implements runner.FrameSink.
func (q *Queue) Dispatch(agentKind, method string, params json.RawMessage) error {
	return q.frames.Dispatch(agentKind, method, params)
}

func newHex8() string {
	buf := make([]byte, 4)
	_, _ = cryptoRand.Read(buf)
	return fmt.Sprintf("%x", buf)
}

// newScanner returns a scanner with a generous line budget (agent JSONL can
// carry large outputs).
func newScanner(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	return sc
}

var cryptoRand = rand.Reader

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// SetSpawner overrides the headless command construction (tests inject a
// portable fake agent).
func (q *Queue) SetSpawner(fn func(ctx context.Context, t *acpclient.Task, sessionID string) (*exec.Cmd, string, error)) {
	q.spawner = fn
}

// SetPermissionDecider injects the approval inbox for ACP-managed tasks
// (opencode/hermes). main.go wires it to Bridge.AdapterFor(kind).IntakeToolCall
// so every tool call lands in the same unified Hub approval queue.
func (q *Queue) SetPermissionDecider(fn func(ctx context.Context, kind string, in acpclient.HookIntake) (acpclient.Decision, string, error)) {
	q.permissionDecider = fn
}

// GetTask proxies the store lookup (server.TaskQueue interface).
func (q *Queue) GetTask(ctx context.Context, id string) (*acpclient.Task, error) {
	return q.store.GetTask(ctx, id)
}

// ListTasks proxies the store query (server.TaskQueue interface).
func (q *Queue) ListTasks(ctx context.Context, state string) ([]*acpclient.Task, error) {
	return q.store.ListTasks(ctx, state)
}
