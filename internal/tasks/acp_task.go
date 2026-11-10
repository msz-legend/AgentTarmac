// Package tasks implements the PRD F6 unified operations plane (D13): a
// task queue + board backing fully-managed dispatch. This file adds ACP
// stdio server driving for opencode (`opencode acp`) and hermes
// (`hermes-acp`) as managed tasks, preserving the unified Hub approval
// inbox — every `session/request_permission` is routed through
// IntakeToolCall so a human verdict lands in the same placement queue as
// Claude Code / Codex hooks.
//
// Dataflow contract (vibe-trial 2026-11-10 findings A1/A2/A3):
//   - exactly ONE reader pumps the agent's stdout for the whole task life
//     (a json.Decoder pre-read buffer next to a Scanner silently drops
//     frames that arrive while the decoder holds them);
//   - the handshake is initialize -> session/new -> session/prompt with
//     id-correlated responses, so notifications that interleave (real
//     opencode/hermes emit session/update at any time) are handled on the
//     way instead of being mistaken for handshake responses;
//   - ACP `session/update` params `{sessionId, update}` are translated to
//     Hub frames keyed by the Hub task session id before Dispatch.
package tasks

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/agenttarmac/agenttarmac/internal/acpclient"
)

// permissionDecider resolves an ACP permission request through the unified
// Hub inbox (same path as Claude Code / Codex hooks). Injected by main.go;
// nil means fail-closed (every request is cancelled).
type permissionDecider func(ctx context.Context, kind string, in acpclient.HookIntake) (acpclient.Decision, string, error)

// acpHandshakeTimeout bounds each initialize / session/new round trip. A
// compliant agent answers in milliseconds; when it does not, the task must
// fail fast instead of pinning a concurrency slot forever (vibe-trial A1).
const acpHandshakeTimeout = 60 * time.Second

type jsonrpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int            `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	Result  json.RawMessage `json:"result"`
	Error   *jsonrpcError   `json:"error"`
}

type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// acpPump is the single stdout reader for one ACP task. All reads go
// through next(); all writes go through the shared encoder on the same
// goroutine, so no locking is needed beyond what run() serializes.
type acpPump struct {
	q        *Queue
	t        *acpclient.Task
	hubSess  string // Hub task session id (task-*)
	enc      *json.Encoder
	sc       *bufio.Scanner
	agentSes string // agent-side session id from session/new
	seq      int    // last JSON-RPC request id we minted
}

// streamACP drives a stdio ACP server (opencode `acp` / hermes `hermes-acp`)
// as a managed task. Handshake: initialize -> session/new -> session/prompt.
// session/update notifications are translated and dispatched to the Hub UI;
// session/request_permission blocks on permissionDecider for a human verdict.
// The task ends when session/prompt returns or the process exits.
func (q *Queue) streamACP(ctx context.Context, t *acpclient.Task, sessionID string, cmd *exec.Cmd) error {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn %s: %w", t.AgentKind, err)
	}
	defer stdin.Close()
	// Any error exit must reap the agent (vibe-trial: error returns used to
	// leak the spawned process, holding cwd locks and orphaned sockets).
	reaped := false
	defer func() {
		if !reaped {
			_ = cmd.Process.Kill()
		}
	}()

	p := &acpPump{
		q:       q,
		t:       t,
		hubSess: sessionID,
		enc:     json.NewEncoder(stdin),
		sc:      newScanner(stdout),
	}

	// 1. initialize
	initID := p.nextID()
	if err := p.send(map[string]any{
		"jsonrpc": "2.0", "id": initID, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": 1,
			"clientCapabilities": map[string]any{
				"fs":       map[string]any{"readTextFile": true, "writeTextFile": true},
				"terminal": false,
			},
		},
	}); err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	if _, err := p.roundTrip(ctx, initID, "initialize"); err != nil {
		return err
	}

	// 2. session/new — the server generates its own sessionId; keep the
	// Hub's task id as a fallback for agents that never send one.
	newID := p.nextID()
	if err := p.send(map[string]any{
		"jsonrpc": "2.0", "id": newID, "method": "session/new",
		"params": map[string]any{
			"cwd":        t.CWD,
			"mcpServers": []any{},
		},
	}); err != nil {
		return fmt.Errorf("session/new: %w", err)
	}
	resp, err := p.roundTrip(ctx, newID, "session/new")
	if err != nil {
		return err
	}
	p.agentSes = sessionID
	if resp.Result != nil {
		var newRes struct {
			SessionID string `json:"sessionId"`
		}
		if err := json.Unmarshal(resp.Result, &newRes); err == nil && newRes.SessionID != "" {
			p.agentSes = newRes.SessionID
		}
	}

	// 3. session/prompt (content-block array, per the ACP spec).
	promptID := p.nextID()
	blocks := []map[string]any{{"type": "text", "text": t.Prompt}}
	if err := p.send(map[string]any{
		"jsonrpc": "2.0", "id": promptID, "method": "session/prompt",
		"params": map[string]any{
			"sessionId": p.agentSes,
			"prompt":    blocks,
		},
	}); err != nil {
		return fmt.Errorf("session/prompt: %w", err)
	}

	// Pump until the prompt response (task done), an error, or EOF. Every
	// notification/request seen on the way is handled inline.
	for {
		msg, err := p.next(ctx)
		if err != nil {
			return err
		}
		done, err := p.handle(ctx, msg, promptID)
		if err != nil {
			return err
		}
		if done {
			// Prompt answered: close stdin so a well-behaved agent exits,
			// then reap it within a grace period.
			reaped = true
			_ = stdin.Close()
			exited := make(chan struct{})
			go func() { _ = cmd.Wait(); close(exited) }()
			select {
			case <-exited:
			case <-time.After(5 * time.Second):
				_ = cmd.Process.Kill()
				<-exited
			}
			return nil
		}
	}
}

// nextID mints monotonically increasing request ids.
func (p *acpPump) nextID() int {
	p.seq++
	return p.seq
}

func (p *acpPump) send(v any) error {
	return p.enc.Encode(v)
}

// next reads one JSON-RPC message. ctx cancellation kills nothing here — the
// caller's run context cancel already killed the process, so the scanner
// unblocks with EOF and the ctx error is surfaced.
func (p *acpPump) next(ctx context.Context) (jsonrpcMessage, error) {
	for p.sc.Scan() {
		if ctx.Err() != nil {
			return jsonrpcMessage{}, ctx.Err()
		}
		line := strings.TrimSpace(p.sc.Text())
		if line == "" {
			continue
		}
		var msg jsonrpcMessage
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue // not JSON-RPC; ignore and keep pumping
		}
		return msg, nil
	}
	if err := p.sc.Err(); err != nil {
		return jsonrpcMessage{}, err
	}
	if ctx.Err() != nil {
		return jsonrpcMessage{}, ctx.Err()
	}
	return jsonrpcMessage{}, fmt.Errorf("agent %s exited before session/prompt returned", p.t.AgentKind)
}

// roundTrip waits for the response to one of our requests, handling any
// interleaved traffic on the way (vibe-trial A1: unsolicited session/update
// must never be mistaken for a handshake response).
func (p *acpPump) roundTrip(ctx context.Context, id int, what string) (jsonrpcMessage, error) {
	deadline := time.After(acpHandshakeTimeout)
	type result struct {
		msg jsonrpcMessage
		err error
	}
	ch := make(chan result, 1)
	go func() {
		for {
			msg, err := p.next(ctx)
			if err != nil {
				ch <- result{err: err}
				return
			}
			if msg.Method == "" && msg.ID != nil && *msg.ID == id {
				ch <- result{msg: msg}
				return
			}
			done, err := p.handle(ctx, msg, -1)
			if err != nil || done {
				ch <- result{err: fmt.Errorf("%s: interrupted by %s handling (%v)", what, msg.Method, err)}
				return
			}
		}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			return jsonrpcMessage{}, fmt.Errorf("%s response: %w", what, r.err)
		}
		if r.msg.Error != nil {
			return jsonrpcMessage{}, fmt.Errorf("%s error %d: %s", what, r.msg.Error.Code, r.msg.Error.Message)
		}
		return r.msg, nil
	case <-deadline:
		return jsonrpcMessage{}, fmt.Errorf("%s timed out after %s", what, acpHandshakeTimeout)
	}
}

// handle processes one inbound message. Returns done=true when the response
// to the session/prompt request (promptID) arrived and the task is finished.
func (p *acpPump) handle(ctx context.Context, msg jsonrpcMessage, promptID int) (bool, error) {
	if msg.Method != "" {
		switch msg.Method {
		case "session/update":
			p.handleUpdate(msg.Params)
			return false, nil
		case "session/request_permission":
			return false, p.handlePermission(ctx, msg.ID, msg.Params)
		default:
			// Unknown server request: if it carries an id, say not implemented.
			if msg.ID != nil {
				_ = p.send(map[string]any{
					"jsonrpc": "2.0", "id": *msg.ID,
					"error": map[string]any{"code": -32601, "message": "method not found"},
				})
			}
			return false, nil
		}
	}
	// Response to a request we sent. Only session/prompt ends the task.
	if msg.ID != nil && *msg.ID == promptID {
		if msg.Error != nil {
			return true, fmt.Errorf("session/prompt: %s", msg.Error.Message)
		}
		return true, nil
	}
	return false, nil
}

// handleUpdate translates an ACP `session/update` notification
// (`{sessionId: ses_*, update: {...}}`) into Hub frames keyed by the Hub
// task session id (vibe-trial A3: raw forwarding produced all-empty
// payloads plus `unknown session` errors).
func (p *acpPump) handleUpdate(raw json.RawMessage) {
	var payload struct {
		SessionID string `json:"sessionId"`
		Update    struct {
			SessionUpdate string `json:"sessionUpdate"`
			Content       struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			ToolCall struct {
				ToolCallID string `json:"toolCallId"`
				Title      string `json:"title"`
				Kind       string `json:"kind"`
				RawInput   any    `json:"rawInput"`
				Status     string `json:"status"`
				Output     struct {
					Content string `json:"content"`
				} `json:"output"`
			} `json:"toolCall"`
			Plan json.RawMessage `json:"plan"`
		} `json:"update"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return
	}
	up := payload.Update
	text := up.Content.Text

	switch up.SessionUpdate {
	case "agent_message_chunk", "agent_thought_chunk":
		if text == "" {
			return
		}
		p.dispatchUpdate("running", text, "")
	case "plan":
		if len(up.Plan) == 0 {
			return
		}
		p.dispatchUpdate("", "", string(up.Plan))
	case "tool_call":
		// Display-only progress (the row itself is recorded by the approval
		// path via IntakeToolCall when the agent requests permission).
		label := up.ToolCall.Title
		if label == "" {
			label = up.ToolCall.ToolCallID
		}
		if cmd, ok := up.ToolCall.RawInput.(map[string]any); ok {
			if c, ok := cmd["command"].(string); ok && c != "" {
				label = fmt.Sprintf("%s: %s", label, c)
			}
		}
		p.dispatchUpdate("running", "[tool] "+label, "")
	case "tool_call_update":
		if up.ToolCall.Status == "completed" || up.ToolCall.Status == "failed" {
			p.dispatchUpdate("running", "[tool "+up.ToolCall.Status+"] "+up.ToolCall.ToolCallID, "")
		}
	default:
		if text != "" {
			p.dispatchUpdate("running", text, "")
		}
	}
}

// dispatchUpdate forwards one translated session/update to the Hub pipeline.
func (p *acpPump) dispatchUpdate(status, content, plan string) {
	params := map[string]any{"sessionId": p.hubSess}
	if status != "" {
		params["status"] = status
	}
	if content != "" {
		params["content"] = content
	}
	if plan != "" {
		params["plan"] = plan
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return
	}
	_ = p.q.Dispatch(p.t.AgentKind, "session/update", raw)
}

// handlePermission resolves a session/request_permission through the
// unified Hub inbox and answers the agent's pending request id.
func (p *acpPump) handlePermission(ctx context.Context, agentReqID *int, params json.RawMessage) error {
	var pr struct {
		ToolCall struct {
			ToolCallID string `json:"toolCallId"`
			Title      string `json:"title"`
			Kind       string `json:"kind"`
			RawInput   any    `json:"rawInput"`
		} `json:"toolCall"`
		Options []struct {
			OptionID string `json:"optionId"`
			Name     string `json:"name"`
			Kind     string `json:"kind"`
		} `json:"options"`
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(params, &pr)

	command := ""
	if m, ok := pr.ToolCall.RawInput.(map[string]any); ok {
		if c, ok := m["command"].(string); ok {
			command = c
		}
	}
	toolName := pr.ToolCall.Title
	if toolName == "" {
		toolName = pr.ToolCall.ToolCallID
	}
	if toolName == "" {
		toolName = "unknown"
	}

	in := acpclient.HookIntake{
		SessionID: p.hubSess,
		ToolUseID: pr.ToolCall.ToolCallID,
		Tool:      toolName,
		Command:   command,
		Cwd:       p.t.CWD,
		AgentKind: p.t.AgentKind,
		Reason:    pr.Reason,
	}

	decision := acpclient.Decision{Outcome: acpclient.OutcomeCancelled}
	if p.q.permissionDecider != nil {
		d, _, err := p.q.permissionDecider(ctx, p.t.AgentKind, in)
		if err != nil {
			decision = acpclient.TimedOutDecision()
		} else {
			decision = d
		}
	}

	opt := ""
	if decision.Outcome == acpclient.OutcomeAllowedOnce {
		// Pick the option the agent actually offered, matching the verdict
		// scope when possible (allow_once / allow_session / allow_always).
		want := "allow_once"
		if decision.RuleScope == acpclient.ScopeSession {
			want = "allow_session"
		} else if decision.RuleScope == acpclient.ScopeAlways {
			want = "allow_always"
		}
		for _, o := range pr.Options {
			if o.Kind == want {
				opt = o.OptionID
				break
			}
		}
		if opt == "" {
			for _, o := range pr.Options {
				if strings.HasPrefix(o.Kind, "allow") {
					opt = o.OptionID
					break
				}
			}
		}
	}
	result := map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": opt}}
	if decision.Outcome == acpclient.OutcomeRejected ||
		decision.Outcome == acpclient.OutcomeCancelled ||
		decision.Outcome == acpclient.OutcomeUnavailable {
		result = map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}
	}
	if agentReqID != nil {
		_ = p.send(map[string]any{
			"jsonrpc": "2.0", "id": *agentReqID, "result": result,
		})
	}
	return nil
}
