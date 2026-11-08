// Package runner implements PRD F7 native session resume: "继续此会话" for
// attached Claude Code / Codex CLI sessions by spawning the vendor's own
// resume command headless and mapping its JSON output back into Hub frames.
//
// Per PRD F7: CC `--continue` skips -p/SDK sessions, so resume MUST use the
// explicit id (`claude --resume <id>`); the cwd is mandatory because CC's
// session-id lookup is scoped to it. Codex resumes with
// `codex exec resume <id> --json`. Runs are fire-and-forget with a single
// in-flight guard per session (PRD D13: 无人值守挂起等人).
package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
)

// FrameSink routes runner output back through the standard frame pipeline
// (persistence + events + approval gate) exactly like a connector would.
// The daemon adapts it to the bridge; defined here to avoid an import cycle.
type FrameSink interface {
	Dispatch(agentKind, method string, params json.RawMessage) error
}

// Runner drives native CLI resumes.
type Runner struct {
	sink FrameSink
	log  Logger

	mu       sync.Mutex
	inFlight map[string]bool // sessionID → a resume is running

	// commandBuilder is injectable for tests; nil = production defaults.
	commandBuilder func(kind, sessionID, prompt string) (name string, args []string)
}

// Logger is the minimal logging surface (avoids a log import in tests).
type Logger interface {
	Printf(format string, args ...any)
}

// New creates a runner.
func New(sink FrameSink, log Logger) *Runner {
	return &Runner{sink: sink, log: log, inFlight: map[string]bool{}}
}

// SetCommandBuilder overrides the resume command construction (tests).
func (r *Runner) SetCommandBuilder(fn func(kind, sessionID, prompt string) (string, []string)) {
	r.commandBuilder = fn
}

// SupportsResume reports whether the agent kind has a native headless resume
// command (PRD F7/EVENT_MODEL §6 table; ACP-only kinds go through the Hub
// command bus instead).
func SupportsResume(kind string) bool {
	return kind == "claude_code" || kind == "codex"
}

// Continue resumes sessionID with a new task. It returns (sessionID, started,
// error); started=false means another resume is already in flight for this
// session. The run itself is asynchronous: status/message/tool frames stream
// back through the FrameSink and the session ends completed or failed.
func (r *Runner) Continue(ctx context.Context, kind, sessionID, cwd, prompt string) (string, bool, error) {
	if !SupportsResume(kind) {
		return sessionID, false, fmt.Errorf("runner: agent kind %q has no native resume", kind)
	}
	r.mu.Lock()
	if r.inFlight[sessionID] {
		r.mu.Unlock()
		return sessionID, false, fmt.Errorf("runner: session %s already has a resume in flight", sessionID)
	}
	r.inFlight[sessionID] = true
	r.mu.Unlock()

	runCtx, cancel := context.WithCancel(ctx)
	go func() {
		defer func() {
			r.mu.Lock()
			delete(r.inFlight, sessionID)
			r.mu.Unlock()
			cancel()
		}()
		r.run(runCtx, kind, sessionID, cwd, prompt)
	}()
	return sessionID, true, nil
}

// run executes the resume command and maps its JSON output to frames.
func (r *Runner) run(ctx context.Context, kind, sessionID, cwd, prompt string) {
	name, args := r.buildCommand(kind, sessionID, prompt)
	_ = r.dispatch("", "session/update", map[string]any{
		"sessionId": sessionID, "status": "running", "turnIndex": 1,
	})
	if err := r.stream(ctx, kind, sessionID, cwd, name, args); err != nil {
		_ = r.dispatch("", "session/update", map[string]any{
			"sessionId": sessionID, "status": "failed", "error": err.Error(),
		})
		if r.log != nil {
			r.log.Printf("runner: resume %s failed: %v", sessionID, err)
		}
		return
	}
	_ = r.dispatch("", "session/update", map[string]any{
		"sessionId": sessionID, "status": "completed", "isComplete": true,
	})
	if r.log != nil {
		r.log.Printf("runner: resume %s finished", sessionID)
	}
}

func (r *Runner) buildCommand(kind, sessionID, prompt string) (string, []string) {
	if r.commandBuilder != nil {
		return r.commandBuilder(kind, sessionID, prompt)
	}
	// PRD F7: explicit-id resume (never --continue); prompt as positional arg.
	if kind == "claude_code" {
		return "claude", []string{"--resume", sessionID, "--print", "--output-format", "stream-json", "--verbose", prompt}
	}
	// codex: exec resume with JSONL event output.
	return "codex", []string{"exec", "resume", sessionID, "--json", prompt}
}

// stream runs the command and dispatches frames from its JSON lines.
func (r *Runner) stream(ctx context.Context, kind, sessionID, cwd, name string, args []string) error {
	cmd := command(ctx, name, args...)
	cmd.Dir = cwd // PRD F7: resume must carry the cwd (CC session-id scoping)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn %s: %w", name, err)
	}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		if ctx.Err() != nil {
			_ = cmd.Process.Kill()
			return ctx.Err()
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		r.mapLine(kind, sessionID, line)
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return cmd.Wait()
}

// mapLine converts one stream-json / codex-JSONL line into Hub frames.
func (r *Runner) mapLine(kind, sessionID, line string) {
	MapStreamLine(r.sink, kind, sessionID, line)
}

// MapStreamLine converts one `claude --output-format stream-json` /
// `codex exec --json` line into Hub frames (shared with the F6 task queue).
func MapStreamLine(sink FrameSink, kind, sessionID, line string) {
	var m map[string]any
	if json.Unmarshal([]byte(line), &m) != nil {
		return
	}
	if kind == "claude_code" {
		mapClaude(sink, kind, sessionID, m)
		return
	}
	mapCodex(sink, kind, sessionID, m)
}

// mapClaude maps `claude --output-format stream-json` lines.
func mapClaude(sink FrameSink, kind, sessionID string, m map[string]any) {
	typ, _ := m["type"].(string)
	switch typ {
	case "assistant":
		msg, _ := m["message"].(map[string]any)
		content, _ := msg["content"].([]any)
		for _, blk := range content {
			b, _ := blk.(map[string]any)
			if b["type"] == "tool_use" {
				id, _ := b["id"].(string)
				name, _ := b["name"].(string)
				input, _ := json.Marshal(b["input"])
				_ = dispatchTo(sink, kind, "tool_call", map[string]any{
					"sessionId": sessionID, "toolCallId": id, "name": name,
					"kind": "execute", "rawInput": json.RawMessage(input),
				})
			}
		}
	case "user":
		// tool results come back as user messages
		msg, _ := m["message"].(map[string]any)
		content, _ := msg["content"].([]any)
		for _, blk := range content {
			b, _ := blk.(map[string]any)
			if b["type"] == "tool_result" {
				id, _ := b["tool_use_id"].(string)
				ok := true
				if isErr, exists := b["is_error"]; exists {
					if boolErr, _ := isErr.(bool); boolErr {
						ok = false
					}
				}
				_ = dispatchTo(sink, kind, "tool_result", map[string]any{
					"sessionId": sessionID, "toolCallId": id,
					"status":  map[bool]string{true: "success", false: "failed"}[ok],
					"content": fmt.Sprintf("%v", b["content"]),
				})
			}
		}
	}
}

// mapCodex maps `codex exec --json` JSONL events (v1: command executions and
// turn completion).
func mapCodex(sink FrameSink, kind, sessionID string, m map[string]any) {
	typ, _ := m["type"].(string)
	switch {
	case strings.HasSuffix(typ, "item.started"):
		if item, _ := m["item"].(map[string]any); item != nil {
			if it, _ := item["type"].(string); it == "command_execution" {
				id, _ := item["id"].(string)
				cmdStr, _ := item["command"].(string)
				_ = dispatchTo(sink, kind, "tool_call", map[string]any{
					"sessionId": sessionID, "toolCallId": id, "name": "Bash",
					"kind": "execute", "rawInput": map[string]string{"command": cmdStr},
				})
			}
		}
	case strings.HasSuffix(typ, "item.completed"):
		if item, _ := m["item"].(map[string]any); item != nil {
			if it, _ := item["type"].(string); it == "command_execution" {
				id, _ := item["id"].(string)
				exit, _ := item["exit_code"].(float64)
				status := "success"
				if exit != 0 {
					status = "failed"
				}
				out, _ := item["aggregated_output"].(string)
				_ = dispatchTo(sink, kind, "tool_result", map[string]any{
					"sessionId": sessionID, "toolCallId": id,
					"status": status, "content": out,
				})
			}
		}
	}
}

func dispatchTo(sink FrameSink, agentKind, method string, params map[string]any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return sink.Dispatch(agentKind, method, raw)
}

func (r *Runner) dispatch(agentKind, method string, params map[string]any) error {
	return dispatchTo(r.sink, agentKind, method, params)
}

// command abstracts exec.Command for tests (injected fake binary).
var command = exec.CommandContext
