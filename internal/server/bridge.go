// Package server implements the AgentHub daemon: the ACP bridge that hosts
// runtime adapters (agent_kind=claude_code), the HTTP/WS API on
// 127.0.0.1:8799 and the Bearer-token auth boundary.
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/agenttarmac/agenttarmac/internal/acpclient"
)

// ACPAdapter is the contract the bridge requires from a runtime adapter.
// *acpclient.BaseAdapter (via the ClaudeCode/Codex flavors) is the implementation.
type ACPAdapter interface {
	AgentKind() string
	HandleFrame(method string, params json.RawMessage) (json.RawMessage, error)
	ResolvePermission(requestUID string, dec acpclient.PermissionDecision) (acpclient.PermissionResolution, error)
	IntakeToolCall(ctx context.Context, in acpclient.HookIntake, deadline time.Duration) (acpclient.Decision, string, error)
	Initialize(ctx context.Context) error
}

// Bridge connects adapters to the daemon: it routes inbound ACP frames to the
// adapter registered for their agent kind and fans outbound frames out to
// WebSocket subscribers.
type Bridge struct {
	log *log.Logger

	store    *acpclient.Store // optional; stamps agent.kind on ring events (ENV-1)
	mu       sync.RWMutex
	adapters map[string]ACPAdapter

	subMu sync.Mutex
	subs  map[chan []byte]struct{}

	evMu     sync.Mutex
	uiEvents []UIEvent // in-memory ring backing GET /api/events (UI 稿 §11)
}

// Version is the daemon version surfaced in the console (settings + statusline).
const Version = "v0.2.0-m2"

// UIEvent is the web-console event shape — the PRD §6 / EVENT_MODEL v0.2
// frozen envelope: v=2, event_id, RFC3339(ms) UTC timestamp, agent ref,
// session_id, type, risk, typed payload. Served by GET /api/events and
// mirrored live over the WS feed.
type UIEvent struct {
	V         int            `json:"v"` // 2 per EVENT_MODEL v0.2 (C1)
	EventID   string         `json:"event_id"`
	TS        string         `json:"ts"` // RFC3339, UTC, ms precision
	Agent     *UIEventAgent  `json:"agent,omitempty"`
	SessionID string         `json:"session_id,omitempty"`
	Type      string         `json:"type"`
	Risk      string         `json:"risk,omitempty"` // tool_call / permission_request 必填
	Payload   map[string]any `json:"payload,omitempty"`
}

// UIEventAgent attributes an event to an agent kind.
type UIEventAgent struct {
	Kind       string `json:"kind,omitempty"`
	Version    string `json:"version,omitempty"`
	InstanceID string `json:"instance_id,omitempty"`
}

// agentRef stamps the agent attribution block; nil when the kind is unknown
// (the envelope then falls back to the session lookup in recordUIEvent).
func agentRef(kind string) *UIEventAgent {
	if kind == "" {
		return nil
	}
	return &UIEventAgent{Kind: kind, Version: Version, InstanceID: "hub"}
}

// uiEventRingCap bounds the in-memory event ring backing GET /api/events
// (spec §11: the console replays the feed on load). Events are daemon-lifetime
// only; the durable audit lives in SQLite (approval_history / tool_calls).
const uiEventRingCap = 500

// newEventID generates a uuid-shaped event id (v7 ordering is not required
// here; the ring preserves order).
func newEventID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "evt_" + fmt.Sprintf("%d", time.Now().UnixNano())
	}
	// RFC 4122-ish formatting for readability.
	buf[6] = (buf[6] & 0x0f) | 0x70
	buf[8] = (buf[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16])
}

// SetStore attaches the persistence layer so ring events can be attributed
// to their agent kind (EVENT_MODEL v0.2: every envelope carries agent).
func (b *Bridge) SetStore(store *acpclient.Store) { b.store = store }

// agentKindFor looks up the owning agent kind for a session (best effort,
// never blocks the ring).

// kindForRequest resolves the adapter kind that owns a pending request via
// the persisted permission_request row → its session → agent_kind. Falls
// back to claude_code when the row or session is unknown (legacy behaviour).
func (b *Bridge) kindForRequest(requestUID string) string {
	if b.store == nil {
		return acpclient.AgentKindClaudeCode
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	pr, err := b.store.GetPermissionRequestByUID(ctx, requestUID)
	if err != nil || pr.SessionID == "" {
		return acpclient.AgentKindClaudeCode
	}
	sess, err := b.store.GetSession(ctx, pr.SessionID)
	if err != nil || sess.AgentKind == "" {
		return acpclient.AgentKindClaudeCode
	}
	return sess.AgentKind
}

// agentKindFor looks up the owning agent kind for a session (best effort,
// never blocks the ring).
func (b *Bridge) agentKindFor(sessionID string) string {
	if b.store == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	sess, err := b.store.GetSession(ctx, sessionID)
	if err != nil {
		return ""
	}
	return sess.AgentKind
}

// recordUIEvent stamps the frozen envelope fields and appends to the ring
// (never blocks, never fails).
func (b *Bridge) recordUIEvent(e UIEvent) {
	e.V = 2
	if e.EventID == "" {
		e.EventID = newEventID()
	}
	if e.TS == "" {
		e.TS = time.Now().UTC().Format("2006-01-02T15:04:05.000Z07:00")
	}
	if e.Agent == nil && e.SessionID != "" {
		if kind := b.agentKindFor(e.SessionID); kind != "" {
			e.Agent = &UIEventAgent{Kind: kind}
		}
	}
	b.evMu.Lock()
	b.uiEvents = append(b.uiEvents, e)
	if len(b.uiEvents) > uiEventRingCap {
		b.uiEvents = b.uiEvents[len(b.uiEvents)-uiEventRingCap:]
	}
	b.evMu.Unlock()
}

// UIEvents returns a copy of the event ring, oldest first.
func (b *Bridge) UIEvents() []UIEvent {
	b.evMu.Lock()
	defer b.evMu.Unlock()
	out := make([]UIEvent, len(b.uiEvents))
	copy(out, b.uiEvents)
	return out
}

// recordFrameIn derives spec events from a successfully processed inbound
// frame. Only tool_result is recorded here: tool calls surface through their
// frame.out outcome (approved / rejected / permission_request), and status
// transitions surface through the adapter's own session/update emit — both of
// which land in RecordFrameOut. Recording them here as well would duplicate.
func (b *Bridge) recordFrameIn(f InboundFrame) {
	if f.Method != "tool_result" {
		return
	}
	var p struct {
		SessionID  string `json:"sessionId"`
		ToolCallID string `json:"toolCallId"`
		Name       string `json:"name"`
	}
	if len(f.Params) > 0 {
		if err := json.Unmarshal(f.Params, &p); err != nil {
			return
		}
	}
	b.recordUIEvent(UIEvent{Type: "tool_result",
		Agent:     &UIEventAgent{Kind: f.AgentKind, Version: Version, InstanceID: "hub"},
		SessionID: p.SessionID,
		Payload:   map[string]any{"tool": p.Name, "call_id": p.ToolCallID}})
}

// mapUIState maps adapter session statuses onto the spec's 7-state machine
// (UI 稿 §1.4).
func mapUIState(status string, isComplete bool) string {
	if isComplete {
		return "finished"
	}
	switch status {
	case "running":
		return "running"
	case "awaiting_approval":
		return "waiting_approval"
	case "failed":
		return "failed"
	case "closed":
		return "finished"
	default: // active, awaiting_input, ""
		return "idle"
	}
}

// RecordFrameOut derives spec events from adapter-emitted outbound frames
// (permission requests, gate outcomes, status updates). agentKind may be
// empty; the console attributes via the session row.
func (b *Bridge) RecordFrameOut(method string, params json.RawMessage) {
	var p struct {
		SessionID      string `json:"sessionId"`
		RequestID      string `json:"requestId"`
		ToolCallID     string `json:"toolCallId"`
		Name           string `json:"name"`
		Risk           string `json:"risk"`
		CommandPattern string `json:"commandPattern"`
		MainArg        string `json:"mainArg"`
		Reason         string `json:"reason"`
		Status         string `json:"status"`
		IsComplete     bool   `json:"isComplete"`
	}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return
		}
	}
	switch method {
	case "session/request_permission":
		cmd := p.MainArg
		if cmd == "" {
			cmd = p.CommandPattern
		}
		b.recordUIEvent(UIEvent{Type: "permission_request", Risk: p.Risk, SessionID: p.SessionID,
			Payload: map[string]any{
				"request_id":     p.RequestID,
				"tool":           p.Name,
				"command":        cmd, // 命令原文（主参数），PRD F3 审批卡展示
				"args_pattern":   p.CommandPattern,
				"args_digest":    cmd,
				"risk":           p.Risk,
				"reason":         p.Reason,
				"capabilities":   []string{"answerable"},
				"rule_supported": true,
			}})
	case "session/update":
		b.recordUIEvent(UIEvent{Type: "status", SessionID: p.SessionID,
			Payload: map[string]any{"state": mapUIState(p.Status, p.IsComplete)}})
	case "tool_call/approved", "tool_call/rejected":
		action := "approved"
		if method == "tool_call/rejected" {
			action = "rejected"
		}
		b.recordUIEvent(UIEvent{Type: "tool_call", Risk: p.Risk, SessionID: p.SessionID,
			Payload: map[string]any{"tool": p.Name, "call_id": p.ToolCallID, "action": action}})
	}
}

// recordPermissionResolved records the human decision as a permission_resolved
// event (spec vocabulary: outcome + rule_scope + decided_by).
func (b *Bridge) recordPermissionResolved(agentKind string, res acpclient.PermissionResolution) {
	scope := "none"
	switch res.Decision {
	case acpclient.DecisionAllowSession:
		scope = "session"
	case acpclient.DecisionAllowAlways, acpclient.DecisionRejectAlways:
		scope = "always"
	}
	outcome := "rejected"
	if res.Outcome == acpclient.OutcomeApproved {
		outcome = "allowed_once"
	}
	b.recordUIEvent(UIEvent{Type: "permission_resolved",
		Agent: agentRef(agentKind),
		Payload: map[string]any{
			"request_id": res.RequestID,
			"outcome":    outcome,
			"rule_scope": scope,
			"decided_by": "hub",
			"decision":   res.Decision,
			"note":       res.Note,
		}})
}

// NewBridge creates an empty bridge.
func NewBridge(logger *log.Logger) *Bridge {
	if logger == nil {
		logger = log.Default()
	}
	return &Bridge{
		log:      logger,
		adapters: map[string]ACPAdapter{},
		subs:     map[chan []byte]struct{}{},
	}
}

// Register installs an adapter under its agent kind.
func (b *Bridge) Register(a ACPAdapter) error {
	kind := a.AgentKind()
	if kind == "" {
		return fmt.Errorf("bridge: adapter kind must not be empty")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, dup := b.adapters[kind]; dup {
		return fmt.Errorf("bridge: adapter %q already registered", kind)
	}
	b.adapters[kind] = a
	b.log.Printf("bridge: registered adapter %q", kind)
	return nil
}

// AgentKinds lists the registered agent kinds.
func (b *Bridge) AgentKinds() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	kinds := make([]string, 0, len(b.adapters))
	for k := range b.adapters {
		kinds = append(kinds, k)
	}
	return kinds
}

// adapter returns the adapter for a kind.
func (b *Bridge) adapter(kind string) (ACPAdapter, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	a, ok := b.adapters[kind]
	if !ok {
		return nil, fmt.Errorf("no adapter registered for agent kind %q", kind)
	}
	return a, nil
}

// AdapterFor is the exported adapter lookup (hook intake resolves the
// agent-kind adapter before parking the call).
func (b *Bridge) AdapterFor(kind string) (ACPAdapter, error) { return b.adapter(kind) }

// InboundFrame is the wire format of POST /acp/frame.
type InboundFrame struct {
	AgentKind string          `json:"agentKind,omitempty"`
	Method    string          `json:"method"`
	Params    json.RawMessage `json:"params,omitempty"`
}

// DispatchFrame routes an inbound frame to the matching adapter and broadcasts
// the exchange to WS subscribers.
func (b *Bridge) DispatchFrame(f InboundFrame) (json.RawMessage, error) {
	if f.Method == "" {
		return nil, fmt.Errorf("frame: method is required")
	}
	if f.AgentKind == "" {
		f.AgentKind = acpclient.AgentKindClaudeCode
	}
	a, err := b.adapter(f.AgentKind)
	if err != nil {
		return nil, err
	}

	b.broadcast(map[string]any{
		"type":      "frame.in",
		"ts":        time.Now().Unix(),
		"agentKind": f.AgentKind,
		"method":    f.Method,
		"params":    f.Params,
	})
	result, err := a.HandleFrame(f.Method, f.Params)
	if err != nil {
		b.broadcast(map[string]any{
			"type":      "frame.error",
			"ts":        time.Now().Unix(),
			"agentKind": f.AgentKind,
			"method":    f.Method,
			"error":     err.Error(),
		})
		b.recordUIEvent(UIEvent{Type: "error",
			Payload: map[string]any{"message": f.Method + ": " + err.Error()}})
		return nil, err
	}
	b.recordFrameIn(f)
	return result, nil
}

// ResolvePermission forwards a human decision to the adapter that raised the
// request and broadcasts the resolution.
func (b *Bridge) ResolvePermission(agentKind, requestUID string, dec acpclient.PermissionDecision) (acpclient.PermissionResolution, error) {
	if agentKind == "" {
		// The decision API carries no kind — find the owning adapter via the
		// persisted request (request → session → agent_kind). Without this,
		// pendings from non-claude kinds (opencode/hermes/codex) would be
		// unresolvable from the console (defect found by fs-gate test).
		agentKind = b.kindForRequest(requestUID)
	}
	a, err := b.adapter(agentKind)
	if err != nil {
		return acpclient.PermissionResolution{}, err
	}
	res, err := a.ResolvePermission(requestUID, dec)
	if err != nil {
		return res, err
	}
	b.broadcast(map[string]any{
		"type":      "permission.resolved",
		"ts":        time.Now().Unix(),
		"agentKind": agentKind,
		"requestId": res.RequestID,
		"outcome":   res.Outcome,
		"applied":   res.Applied,
		"decision":  res.Decision,
	})
	if res.Applied {
		b.recordPermissionResolved(agentKind, res)
	}
	return res, nil
}

// AgentRequest is a Hub-initiated command destined for a connected agent
// (session/new, session/prompt, session/cancel). The stdio connector forwards
// it over the agent's stdin and brings the JSON-RPC response back via
// ResolveAgentResponse.
type AgentRequest struct {
	RequestID string          `json:"requestId"`
	AgentKind string          `json:"agentKind,omitempty"`
	Method    string          `json:"method"`
	Params    json.RawMessage `json:"params,omitempty"`
	CreatedAt int64           `json:"createdAt"`
}

// AgentResponse carries the agent's JSON-RPC answer for an AgentRequest.
type AgentResponse struct {
	RequestID string          `json:"requestId"`
	AgentKind string          `json:"agentKind,omitempty"` // stamped on the resolved event (ENV-1)
	Result    json.RawMessage `json:"result,omitempty"`
	Error     string          `json:"error,omitempty"`
}

// RecordFork publishes the fork lineage as a status event (EVENT_MODEL C5:
// status carries cwd/origin/parent_session_id).
func (b *Bridge) RecordFork(parent, fork *acpclient.Session) {
	b.recordUIEvent(UIEvent{
		Type:      "status",
		SessionID: fork.ID,
		Payload: map[string]any{
			"state":             "idle",
			"cwd":               fork.CWD,
			"origin":            fork.Origin,
			"parent_session_id": fork.ParentSessionID,
		},
	})
}

// TaskEvent publishes one task lifecycle event (EVENT_MODEL v0.2 type
// "task", C6) into the ring and over the live WS feed.
func (b *Bridge) TaskEvent(taskID, action, sessionID, agentKind, title, errText string) {
	b.recordUIEvent(UIEvent{
		Type:      "task",
		Agent:     agentRef(agentKind),
		SessionID: sessionID,
		Payload: map[string]any{
			"task_id":    taskID,
			"action":     action,
			"session_id": sessionID,
			"agent_kind": agentKind,
			"title":      title,
			"error":      errText,
		},
	})
}

// SendAgentRequest broadcasts a Hub→agent command on the WS bus. The bus is
// stateless: correlation happens by requestId at the sender.
func (b *Bridge) SendAgentRequest(req AgentRequest) (AgentRequest, error) {
	if req.Method == "" {
		return req, fmt.Errorf("agent request: method is required")
	}
	if req.RequestID == "" {
		uid, err := newAgentRequestID()
		if err != nil {
			return req, err
		}
		req.RequestID = uid
	}
	if req.AgentKind == "" {
		req.AgentKind = acpclient.AgentKindClaudeCode
	}
	if req.CreatedAt == 0 {
		req.CreatedAt = time.Now().Unix()
	}
	b.broadcast(map[string]any{
		"type":      "agent.request",
		"ts":        time.Now().Unix(),
		"requestId": req.RequestID,
		"agentKind": req.AgentKind,
		"method":    req.Method,
		"params":    req.Params,
	})
	b.recordUIEvent(UIEvent{Type: "task", Agent: &UIEventAgent{Kind: req.AgentKind, Version: Version, InstanceID: "hub"},
		Payload: map[string]any{"action": "dispatch", "method": req.Method, "request_id": req.RequestID}})
	return req, nil
}

// ResolveAgentResponse broadcasts the agent's answer for a pending request.
func (b *Bridge) ResolveAgentResponse(resp AgentResponse) {
	b.broadcast(map[string]any{
		"type":      "agent.response",
		"ts":        time.Now().Unix(),
		"requestId": resp.RequestID,
		"result":    resp.Result,
		"error":     resp.Error,
	})
	b.recordUIEvent(UIEvent{Type: "task",
		Agent:   agentRef(resp.AgentKind),
		Payload: map[string]any{"action": "finished", "request_id": resp.RequestID, "error": resp.Error}})
}

// newAgentRequestID generates a random Hub→agent request id.
func newAgentRequestID() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("failed to generate agent request id: %w", err)
	}
	return "rq_" + hex.EncodeToString(buf), nil
}

// Subscribe registers a WS event stream; the returned channel receives JSON
// event lines. Unsubscribe by closing via Unsubscribe (the channel is never
// closed by the bridge).
func (b *Bridge) Subscribe() chan []byte {
	ch := make(chan []byte, 64)
	b.subMu.Lock()
	b.subs[ch] = struct{}{}
	b.subMu.Unlock()
	return ch
}

// Unsubscribe removes a subscriber.
func (b *Bridge) Unsubscribe(ch chan []byte) {
	b.subMu.Lock()
	delete(b.subs, ch)
	b.subMu.Unlock()
}

// broadcast fans an event out to all subscribers, dropping events for slow
// consumers rather than blocking adapters.
func (b *Bridge) broadcast(evt any) {
	raw, err := json.Marshal(evt)
	if err != nil {
		return
	}
	raw = append(raw, '\n')
	b.subMu.Lock()
	defer b.subMu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- raw:
		default:
			// Slow subscriber: drop the event instead of stalling the adapter.
		}
	}
}
