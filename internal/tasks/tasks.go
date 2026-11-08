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
	store       Store
	sink        EventSink
	frames      FrameDispatch
	hookCommand string // hub-hook command for managed CC approvals
	concurrency int

	mu      sync.Mutex
	paused  bool
	running map[string]*exec.Cmd // taskID → process
	cancels map[string]context.CancelFunc
	wakeCh  chan struct{}
	wg      sync.WaitGroup

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
	go q.loop()
	// Recover: tasks left "running" by a daemon restart are marked failed
	// (their processes died with the daemon).
	q.recoverOrphans()
	q.wake()
	return q
}

// recoverOrphans fails tasks that were running when the daemon died.
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
		q.emit(t, "failed")
	}
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

// Enqueue adds a task (PRD D13: 派发) and wakes the dispatcher.
func (q *Queue) Enqueue(ctx context.Context, agentKind, cwd, prompt string) (*acpclient.Task, error) {
	if agentKind != acpclient.AgentKindClaudeCode && agentKind != acpclient.AgentKindCodex {
		return nil, fmt.Errorf("tasks: agent kind %q is not drivable (v1: claude_code | codex)", agentKind)
	}
	if strings.TrimSpace(cwd) == "" || strings.TrimSpace(prompt) == "" {
		return nil, fmt.Errorf("tasks: cwd and prompt are required")
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

// Retry requeues a failed task (PRD D13: 失败重试；自动重试发生在 run 尾部).
func (q *Queue) Retry(ctx context.Context, id string) (*acpclient.Task, error) {
	t, err := q.store.GetTask(ctx, id)
	if err != nil {
		return nil, err
	}
	if t.State != acpclient.TaskFailed {
		return nil, fmt.Errorf("tasks: only failed tasks can be retried (state %s)", t.State)
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
		q.mu.Lock()
		cancel := q.cancels[id]
		q.mu.Unlock()
		if cancel != nil {
			cancel() // the run goroutine marks the task cancelled
		}
	default:
		return nil, fmt.Errorf("tasks: task %s is %s, cannot cancel", id, t.State)
	}
	return t, nil
}

// Pause stops dispatching queued tasks (running ones keep going; 续跑 on
// resume).
func (q *Queue) Pause() {
	q.mu.Lock()
	q.paused = true
	q.mu.Unlock()
}

// Resume re-enables dispatching.
func (q *Queue) Resume() {
	q.mu.Lock()
	q.paused = false
	q.mu.Unlock()
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
	// Managed session row (PRD D13: Hub 启动的会话全托管).
	sessionID := "task-" + strings.TrimPrefix(t.ID, "task_")
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
		err := q.streamTask(runCtx, t, sessionID, cmd)
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
		q.emit(t, "finished")
		q.wake()
	}()
	return true
}

// fail marks a task failed with the auto-retry policy (PRD D13: 失败重试).
func (q *Queue) fail(ctx context.Context, t *acpclient.Task, msg string) {
	fresh, _ := q.store.GetTask(ctx, t.ID)
	if fresh != nil {
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
	name := "claude"
	if kind == acpclient.AgentKindCodex {
		name = "codex"
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
	// codex
	cmd := exec.CommandContext(ctx, bin, "exec", "--json", t.Prompt)
	cmd.Dir = t.CWD
	return cmd, "", nil
}

// streamTask parses the agent's JSONL stdout and routes tool frames through
// the standard pipeline (same mapping as native resume).
func (q *Queue) streamTask(ctx context.Context, t *acpclient.Task, sessionID string, cmd *exec.Cmd) error {
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

// GetTask proxies the store lookup (server.TaskQueue interface).
func (q *Queue) GetTask(ctx context.Context, id string) (*acpclient.Task, error) {
	return q.store.GetTask(ctx, id)
}

// ListTasks proxies the store query (server.TaskQueue interface).
func (q *Queue) ListTasks(ctx context.Context, state string) ([]*acpclient.Task, error) {
	return q.store.ListTasks(ctx, state)
}
