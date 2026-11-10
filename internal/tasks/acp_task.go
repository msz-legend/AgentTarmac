// Package tasks implements the PRD F6 unified operations plane (D13): a
// task queue + board backing fully-managed dispatch. This file adds ACP
// stdio server driving for opencode (`opencode acp`) and hermes
// (`hermes-acp`) as managed tasks, preserving the unified Hub approval
// inbox — every `session/request_permission` is routed through
// IntakeToolCall so a human verdict lands in the same placement queue as
// Claude Code / Codex hooks.
package tasks

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"

	"github.com/agenttarmac/agenttarmac/internal/acpclient"
)

// permissionDecider resolves an ACP permission request through the unified
// Hub inbox (same path as Claude Code / Codex hooks). Injected by main.go;
// nil means fail-closed (every request is cancelled).
type permissionDecider func(ctx context.Context, kind string, in acpclient.HookIntake) (acpclient.Decision, string, error)

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

// errTaskDone signals that the session/prompt result arrived and the task
// is finished.
var errTaskDone = fmt.Errorf("task done")

// streamACP drives a stdio ACP server (opencode `acp` / hermes `hermes-acp`)
// as a managed task. Handshake: initialize -> session/new -> session/prompt.
// session/update notifications are dispatched to the Hub UI;
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

	enc := json.NewEncoder(stdin)
	dec := json.NewDecoder(stdout)

	var idMu sync.Mutex
	nextID := 0
	next := func() int {
		idMu.Lock()
		defer idMu.Unlock()
		nextID++
		return nextID
	}

	consume := func() error {
		var discarded json.RawMessage
		if err := dec.Decode(&discarded); err != nil {
			return err
		}
		return nil
	}

	// 1. initialize
	if err := enc.Encode(map[string]any{
		"jsonrpc": "2.0", "id": next(), "method": "initialize",
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
	if err := consume(); err != nil {
		return fmt.Errorf("initialize response: %w", err)
	}

	// 2. session/new (server generates its own sessionId; keep Hub's as fallback).
	agentSessionID := sessionID
	var resp jsonrpcMessage
	if err := dec.Decode(&resp); err != nil {
		return fmt.Errorf("session/new response: %w", err)
	}
	if resp.Result != nil {
		var newRes struct {
			SessionID string `json:"sessionId"`
		}
		if err := json.Unmarshal(resp.Result, &newRes); err == nil && newRes.SessionID != "" {
			agentSessionID = newRes.SessionID
		}
	}

	// 3. session/prompt (content-block array, like the ACP probe).
	promptID := next()
	blocks := []map[string]any{{"type": "text", "text": t.Prompt}}
	if err := enc.Encode(map[string]any{
		"jsonrpc": "2.0", "id": promptID, "method": "session/prompt",
		"params": map[string]any{
			"sessionId": agentSessionID,
			"prompt":    blocks,
		},
	}); err != nil {
		return fmt.Errorf("session/prompt: %w", err)
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
		if err := q.handleACPLine(ctx, t, enc, promptID, line); err != nil {
			if err == errTaskDone {
				return nil
			}
			return err
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return cmd.Wait()
}

// handleACPLine processes one JSON-RPC line from the ACP server's stdout.
// Notifications (method set) are dispatched or handled; responses (id set)
// to our session/prompt end the task.
func (q *Queue) handleACPLine(ctx context.Context, t *acpclient.Task, enc *json.Encoder, promptID int, line string) error {
	var msg jsonrpcMessage
	if err := json.Unmarshal([]byte(line), &msg); err != nil {
		// Ignore undecodable lines; keep reading.
		return nil
	}

	if msg.Method != "" {
		// Server request or notification.
		switch msg.Method {
		case "session/update":
			// Forward progress to the Hub UI (board / event stream).
			_ = q.Dispatch(t.AgentKind, "session/update", msg.Params)
			return nil
		case "session/request_permission":
			return q.handleACPPermission(ctx, t, enc, msg.ID, msg.Params)
		default:
			// Unknown server request: if it carries an id, say not implemented.
			if msg.ID != nil {
				_ = enc.Encode(map[string]any{
					"jsonrpc": "2.0", "id": *msg.ID,
					"error": map[string]any{"code": -32601, "message": "method not found"},
				})
			}
			return nil
		}
	}

	// Response to a request we sent. Only session/prompt is outstanding here.
	if msg.ID != nil && *msg.ID == promptID {
		if msg.Error != nil {
			return fmt.Errorf("session/prompt: %s", msg.Error.Message)
		}
		return errTaskDone
	}
	return nil
}

// handleACPPermission resolves a session/request_permission through the
// unified Hub inbox and answers the agent's pending request id.
func (q *Queue) handleACPPermission(ctx context.Context, t *acpclient.Task, enc *json.Encoder, agentReqID *int, params json.RawMessage) error {
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
		SessionID: t.SessionID,
		ToolUseID: pr.ToolCall.ToolCallID,
		Tool:      toolName,
		Command:   command,
		Cwd:       t.CWD,
		AgentKind: t.AgentKind,
		Reason:    pr.Reason,
	}

	decision := acpclient.Decision{Outcome: acpclient.OutcomeCancelled}
	if q.permissionDecider != nil {
		d, _, err := q.permissionDecider(ctx, t.AgentKind, in)
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
		_ = enc.Encode(map[string]any{
			"jsonrpc": "2.0", "id": *agentReqID, "result": result,
		})
	}
	return nil
}
