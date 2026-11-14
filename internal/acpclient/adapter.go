package acpclient

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/agenttarmac/agenttarmac/internal/rules"
)

// Frame vocabulary spoken by the adapters (Hub-internal ACP v2 draft subset):
//
// Inbound (agent runtime → Hub), dispatched via HandleFrame:
//
//	initialize                request  — protocol handshake
//	session/new               request  — create a hosted session
//	session/prompt            request  — submit a user prompt (starts a turn)
//	tool_call                 notif.   — agent reports a tool invocation (gated here)
//	tool_result               notif.   — agent reports a tool outcome (recorded)
//	session/update            notif.   — agent status transition / completion
//	session/request_permission request — agent asks the client for permission
//	                                   (e.g. a real ACP agent bridge); the Hub
//	                                   routes it through the same approval gate
//
// Outbound (Hub → Hub UI / WS listeners), emitted via Sink:
//
//	session/update               — status transitions emitted by the adapter
//	session/request_permission   — approval request awaiting a human decision
//	tool_call/approved           — tool call passed the approval gate
//	tool_call/rejected           — tool call denied by the gate

// Sink receives outbound frames emitted by an adapter (the daemon bridge
// broadcasts them to WebSocket listeners and the log).
type Sink interface {
	EmitFrame(method string, params json.RawMessage)
}

// NopSink discards outbound frames (tests, headless use).
type NopSink struct{}

// EmitFrame implements Sink by discarding.
func (NopSink) EmitFrame(string, json.RawMessage) {}

// BaseAdapter implements the Hub-side ACP v2 draft adapter shared by every
// agent kind (decision A). It persists every frame, runs the approval gate on
// tool calls (decisions B and C) and drives the session state machine.
// Kind-specific adapters (Claude Code, Codex) are thin wrappers selecting
// their agent_kind.
type BaseAdapter struct {
	kind  string
	store *Store
	sink  Sink

	mu       sync.Mutex
	sessions map[string]*sessionState
	pending  map[string]*pendingPermission
	waiters  map[string]chan Decision // requestUID → parked hook intake
	frameSeq uint64
}

// sessionState is the in-memory projection of one hosted session. The DB row
// remains the source of truth (session-scoped policy now lives in the rules
// table, PRD F3).
type sessionState struct {
	sess      *Session
	toolCalls map[string]*ToolCall // by agent tool call id
}

// pendingPermission is an in-flight approval request awaiting a human decision.
type pendingPermission struct {
	DBID            int64
	RequestUID      string
	SessionID       string
	ToolCallDBID    int64
	ToolCallAgentID string
	ToolName        string
	Pattern         string
	MainArg         string // rule arg_pattern derivation input (PRD Q-F4)
	Risk            RiskLevel
	AwaitingConfirm bool
	CreatedAt       time.Time
}

// NewBaseAdapter creates a generic adapter for the given agent kind. store
// must be an opened database (InitDB); sink receives outbound frames and may
// be nil.
func NewBaseAdapter(kind string, store *Store, sink Sink) (*BaseAdapter, error) {
	if kind == "" {
		return nil, fmt.Errorf("adapter kind must not be empty")
	}
	if store == nil || store.db == nil {
		return nil, fmt.Errorf("%s adapter requires an initialized store", kind)
	}
	if sink == nil {
		sink = NopSink{}
	}
	return &BaseAdapter{
		kind:     kind,
		store:    store,
		sink:     sink,
		sessions: map[string]*sessionState{},
		pending:  map[string]*pendingPermission{},
		waiters:  map[string]chan Decision{},
	}, nil
}

// AgentKind reports the registry key used by the daemon bridge.
func (a *BaseAdapter) AgentKind() string { return a.kind }

// Initialize validates the adapter wiring. Kept as an explicit step so the
// daemon can fail fast at startup.
func (a *BaseAdapter) Initialize(ctx context.Context) error {
	if _, err := a.store.ListSessions(ctx); err != nil {
		return fmt.Errorf("adapter self-check failed: %w", err)
	}
	return nil
}

// Finalize releases in-memory state. Sessions stay persisted; approval
// memories are permanent by design.
func (a *BaseAdapter) Finalize() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sessions = map[string]*sessionState{}
	a.pending = map[string]*pendingPermission{}
	return nil
}

// HandleFrame is the main entry point for ACP message handling. It dispatches
// incoming frames to the appropriate handler and returns the request result
// (nil for notifications).
func (a *BaseAdapter) HandleFrame(method string, rawMsg json.RawMessage) (json.RawMessage, error) {
	switch method {
	case "initialize":
		return a.handleInitialize(rawMsg)
	case "session/new":
		return a.handleSessionNew(rawMsg)
	case "session/prompt":
		return a.handleSessionPrompt(rawMsg)
	case "tool_call":
		return a.handleToolCall(rawMsg)
	case "tool_result":
		return a.handleToolResult(rawMsg)
	case "session/update":
		return a.handleSessionUpdate(rawMsg)
	case "session/request_permission":
		return a.handleSessionRequestPermission(rawMsg)
	default:
		return nil, fmt.Errorf("unknown ACP method: %s", method)
	}
}

// HookIntake is the body a PreToolUse / PermissionRequest hook POSTs to the
// daemon (PRD F3; field names follow Claude Code's PreToolUse input, plus the
// Codex PermissionRequest surface).
type HookIntake struct {
	SessionID      string `json:"session_id"`
	Tool           string `json:"tool"`
	ToolName       string `json:"tool_name"` // Claude Code's actual field name
	Command        string `json:"command"`
	FilePath       string `json:"file_path"` // Write/Edit/MultiEdit/NotebookEdit
	ArgsDigest     string `json:"args_digest"`
	Cwd            string `json:"cwd"`
	PermissionMode string `json:"permission_mode"`
	ToolUseID      string `json:"tool_use_id"`
	AgentKind      string `json:"agent_kind"` // claude_code (default) | codex | …
	Reason         string `json:"reason"`     // names the hook surface, overrides the default
}

// ToolNameOrTool returns the effective tool name across the two field names.
func (h HookIntake) ToolNameOrTool() string {
	if h.ToolName != "" {
		return h.ToolName
	}
	return h.Tool
}

// AgentKindOr returns the effective agent kind (default claude_code).
func (h HookIntake) AgentKindOr(def string) string {
	if h.AgentKind != "" {
		return h.AgentKind
	}
	if def != "" {
		return def
	}
	return AgentKindClaudeCode
}

// IntakeTimeout bounds how long a hook waits for a human verdict before the
// Hub fails closed (PRD F3: the hub-hook default is 2 minutes).
const IntakeTimeout = 2 * time.Minute

// IntakeToolCall is the shared synchronous decision path for hooks
// (PRD F3: CC PreToolUse / Codex PermissionRequest). It registers the tool
// call through the standard gate (persistence, events, audit) and, when the
// gate raises a permission request, blocks until a human verdict arrives or
// the deadline passes — always returning a decision (fail closed).
func (a *BaseAdapter) IntakeToolCall(ctx context.Context, in HookIntake, deadline time.Duration) (Decision, string, error) {
	tool := in.ToolNameOrTool()
	if tool == "" {
		return TimedOutDecision(), "", fmt.Errorf("tool is required")
	}

	// The hook session may not be hosted yet — auto-register it as attached
	// (PRD F7: origin=attached; the original process keeps driving it).
	a.mu.Lock()
	if _, err := a.sessionLocked(in.SessionID); err != nil {
		sess := &Session{
			ID:        in.SessionID,
			AgentKind: in.AgentKindOr(a.kind),
			CWD:       in.Cwd,
			Status:    StatusActive,
			Origin:    "attached",
		}
		if in.SessionID == "" {
			// Synthesize a stable session id for hook calls without one.
			sess.ID = "hook_" + mustRandomHex4()
		}
		if err := a.store.CreateSession(ctx, sess); err != nil {
			a.mu.Unlock()
			return TimedOutDecision(), "", err
		}
		a.sessions[sess.ID] = &sessionState{sess: sess, toolCalls: map[string]*ToolCall{}}
	}
	a.mu.Unlock()

	toolCallID := in.ToolUseID
	if toolCallID == "" {
		toolCallID = "hook_" + mustRandomHex4()
	}
	rawInput := map[string]string{}
	if in.Command != "" {
		rawInput["command"] = in.Command
	}
	if in.FilePath != "" {
		rawInput["file_path"] = in.FilePath
	}
	params, err := json.Marshal(map[string]any{
		"sessionId":  in.SessionID,
		"toolCallId": toolCallID,
		"name":       tool,
		"kind":       classifyHookKind(tool),
		"rawInput":   rawInput,
		"argsDigest": in.ArgsDigest,
	})
	if err != nil {
		return TimedOutDecision(), "", err
	}

	// Park the waiter BEFORE the gate runs so no verdict can slip past.
	ch := make(chan Decision, 1)
	res, err := a.HandleFrame("tool_call", params)
	if err != nil {
		return TimedOutDecision(), "", err
	}
	var gate struct {
		Outcome   string   `json:"outcome"`
		RequestID string   `json:"requestId"`
		Reason    string   `json:"reason"`
		Decision  Decision `json:"decision"`
	}
	_ = json.Unmarshal(res, &gate)

	if gate.Outcome != "pending_approval" {
		// Gate decided from policy / memory / risk pass-through: the gate
		// attached the full spec Decision to the frame result.
		if gate.Decision.Outcome == "" {
			gate.Decision = Decision{Outcome: OutcomeAllowedOnce, RuleScope: ScopeNone, DecidedBy: DecidedByPolicy, Note: gate.Reason}
		}
		return gate.Decision, gate.RequestID, nil
	}

	if gate.RequestID == "" {
		return TimedOutDecision(), "", fmt.Errorf("gate returned no request id")
	}
	a.mu.Lock()
	a.waiters[gate.RequestID] = ch
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.waiters, gate.RequestID)
		a.mu.Unlock()
	}()

	timer := time.NewTimer(deadline)
	defer timer.Stop()
	select {
	case dec := <-ch:
		return dec, gate.RequestID, nil
	case <-timer.C:
		// Fail closed (PRD F3): record the timeout in the audit trail and
		// resolve the request as unavailable.
		dec := TimedOutDecision()
		_ = a.store.ResolvePermissionRequest(context.Background(), a.pendingDBID(gate.RequestID), OutcomeRejected, "")
		_ = a.store.InsertApproval(context.Background(), &ApprovalRecord{
			SessionID: in.SessionID,
			Decision:  "deny", DecisionBy: DecidedByTimeout,
			Notes: "hook intake timed out after " + deadline.String(),
		})
		return dec, gate.RequestID, nil
	case <-ctx.Done():
		return TimedOutDecision(), gate.RequestID, ctx.Err()
	}
}

// pendingDBID resolves the row id of a pending request (timeout bookkeeping).
func (a *BaseAdapter) pendingDBID(requestUID string) int64 {
	if p, ok := a.pending[requestUID]; ok {
		return p.DBID
	}
	return 0
}

// classifyHookKind maps a hook tool name onto the ACP tool kind vocabulary.
func classifyHookKind(tool string) string {
	name := strings.ToLower(strings.TrimSpace(tool))
	switch {
	case terminalToolNames[name]:
		return "execute"
	case writeLikeTools[name]:
		return "edit"
	case readLikeTools[name]:
		return "read"
	default:
		return "other"
	}
}

// ------------------------------------------------------------------ handlers

func (a *BaseAdapter) handleInitialize(rawMsg json.RawMessage) (json.RawMessage, error) {
	return mustJSON(map[string]any{
		"protocolVersion": "acp/v2-draft",
		"agentKind":       a.kind,
	}), nil
}

// handleSessionNew processes the session/new ACP request: validates the
// payload, persists the session row and reports it active.
func (a *BaseAdapter) handleSessionNew(rawMsg json.RawMessage) (json.RawMessage, error) {
	var payload struct {
		SessionID string `json:"sessionId"`
		CWD       string `json:"cwd"`
		Config    struct {
			Model    string `json:"model"`
			Provider string `json:"provider"`
		} `json:"configOptions"`
	}
	if err := json.Unmarshal(rawMsg, &payload); err != nil {
		return nil, fmt.Errorf("failed to unmarshal session/new: %w", err)
	}
	if payload.SessionID == "" {
		return nil, fmt.Errorf("session/new: sessionId is required")
	}
	if payload.CWD == "" {
		return nil, fmt.Errorf("session/new: cwd is required")
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	ctx := context.Background()

	if _, err := a.store.GetSession(ctx, payload.SessionID); err == nil {
		return nil, fmt.Errorf("session/new: session %s already exists", payload.SessionID)
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}

	sess := &Session{
		ID:        payload.SessionID,
		AgentKind: a.kind,
		CWD:       payload.CWD,
		Model:     payload.Config.Model,
		Provider:  payload.Config.Provider,
		Status:    StatusActive,
		// Sessions born from an agent's own session/new are ATTACHED (PRD
		// F7/D14): the agent process lives outside the Hub (driven by the
		// connector / the user's CLI). Managed sessions are created by the
		// task queue, which sets origin=managed explicitly.
		Origin: OriginAttached,
	}
	if err := a.store.CreateSession(ctx, sess); err != nil {
		return nil, err
	}
	a.sessions[payload.SessionID] = &sessionState{
		sess:      sess,
		toolCalls: map[string]*ToolCall{},
	}

	a.emitLocked("session/update", map[string]any{
		"sessionId": sess.ID,
		"status":    StatusActive,
		"phase":     "created",
	})
	return mustJSON(map[string]any{"sessionId": sess.ID}), nil
}

// handleSessionPrompt processes the session/prompt ACP request: validates the
// session can accept a turn, bumps counters and reports running.
func (a *BaseAdapter) handleSessionPrompt(rawMsg json.RawMessage) (json.RawMessage, error) {
	var payload struct {
		SessionID     string `json:"sessionId"`
		Prompt        string `json:"prompt"`
		TurnIndex     int    `json:"turnIndex"`
		IsContinue    bool   `json:"isContinue"`
		EstimatedCost struct {
			InputTokens  int64 `json:"inputTokens"`
			OutputTokens int64 `json:"outputTokens"`
		} `json:"estimatedCost"`
	}
	if err := json.Unmarshal(rawMsg, &payload); err != nil {
		return nil, fmt.Errorf("failed to unmarshal session/prompt: %w", err)
	}
	if payload.Prompt == "" && !payload.IsContinue {
		return nil, fmt.Errorf("session/prompt: prompt is required")
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	ctx := context.Background()

	state, err := a.sessionLocked(payload.SessionID)
	if err != nil {
		return nil, err
	}
	switch state.sess.Status {
	case StatusRunning, StatusAwaitingApproval:
		return nil, fmt.Errorf("session/prompt: turn already in progress (status %s)", state.sess.Status)
	case StatusClosed:
		return nil, fmt.Errorf("session/prompt: session %s is closed", state.sess.ID)
	}

	state.sess.Status = StatusRunning
	state.sess.Turns++
	state.sess.InputTokens += payload.EstimatedCost.InputTokens
	state.sess.OutputTokens += payload.EstimatedCost.OutputTokens
	if payload.TurnIndex > state.sess.Turns {
		state.sess.Turns = payload.TurnIndex
	}
	// PRD F7: the first prompt becomes the session title (UI display).
	if state.sess.Title == "" && payload.Prompt != "" {
		title := payload.Prompt
		if len(title) > 80 {
			title = title[:80]
		}
		state.sess.Title = title
	}
	if err := a.store.UpdateSession(ctx, state.sess); err != nil {
		return nil, err
	}

	a.emitLocked("session/update", map[string]any{
		"sessionId":  state.sess.ID,
		"status":     StatusRunning,
		"turnIndex":  state.sess.Turns,
		"isContinue": payload.IsContinue,
	})
	return mustJSON(map[string]any{"sessionId": state.sess.ID, "turnIndex": state.sess.Turns}), nil
}

// handleToolCall processes the tool_call ACP notification: records the call,
// classifies its risk (decision C) and runs the approval gate (decision B):
//
//	high risk            → always intercepted, never auto-approved
//	session approval hit → auto-allowed for this session
//	memory hit (allow)   → auto-allowed, decision_by=system
//	memory hit (deny)    → auto-rejected, decision_by=system
//	low risk             → auto-allowed (passive monitoring)
//	otherwise            → permission request raised to the Hub UI
func (a *BaseAdapter) handleToolCall(rawMsg json.RawMessage) (json.RawMessage, error) {
	var payload struct {
		SessionID    string          `json:"sessionId"`
		ToolCallID   string          `json:"toolCallId"`
		ToolName     string          `json:"name"`
		ToolKind     string          `json:"kind"`
		ToolStatus   string          `json:"status"`
		ToolContent  json.RawMessage `json:"content"`
		ToolRawInput json.RawMessage `json:"rawInput"`
	}
	if err := json.Unmarshal(rawMsg, &payload); err != nil {
		return nil, fmt.Errorf("failed to unmarshal tool_call: %w", err)
	}
	if payload.ToolCallID == "" {
		return nil, fmt.Errorf("tool_call: toolCallId is required")
	}
	if payload.ToolName == "" {
		return nil, fmt.Errorf("tool_call: name is required")
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	ctx := context.Background()

	state, err := a.sessionLocked(payload.SessionID)
	if err != nil {
		return nil, err
	}
	if state.sess.Status == StatusClosed {
		return nil, fmt.Errorf("tool_call: session %s is closed", state.sess.ID)
	}
	if existing, ok := state.toolCalls[payload.ToolCallID]; ok {
		// Idempotent replay: report the recorded state instead of failing.
		// Fresh calls report the gate action (auto_allowed / ...); replays
		// report the current lifecycle state.
		outcome := existing.ToolStatus
		var requestUID string
		if existing.ToolStatus == ToolPending {
			outcome = "pending_approval"
			for _, p := range a.pending {
				if p.ToolCallDBID == existing.ID {
					requestUID = p.RequestUID
					break
				}
			}
		}
		result := map[string]any{
			"toolCallId":     payload.ToolCallID,
			"outcome":        outcome,
			"risk":           existing.Risk,
			"commandPattern": CommandPattern(existing.ToolName, json.RawMessage(existing.RawInput)),
			"replay":         true,
		}
		if requestUID != "" {
			result["requestId"] = requestUID
		}
		return mustJSON(result), nil
	}

	risk := ClassifyRisk(payload.ToolName, payload.ToolKind, payload.ToolRawInput)
	pattern := CommandPattern(payload.ToolName, payload.ToolRawInput)

	tc := &ToolCall{
		SessionID:       state.sess.ID,
		AgentToolCallID: payload.ToolCallID,
		ToolName:        payload.ToolName,
		ToolKind:        payload.ToolKind,
		ToolStatus:      ToolPending,
		Risk:            string(risk),
		ToolContent:     string(payload.ToolContent),
		RawInput:        string(payload.ToolRawInput),
	}
	if err := a.store.InsertToolCall(ctx, tc); err != nil {
		return nil, err
	}
	state.toolCalls[payload.ToolCallID] = tc

	mainArgument := mainArg(payload.ToolName, payload.ToolRawInput)
	gate := a.gateToolCallLocked(ctx, state, tc, risk, pattern, mainArgument)
	outcome, requestUID, reason := gate.Outcome, gate.RequestID, gate.Reason
	if requestUID != "" {
		// Gate raised a permission request; session pauses until decided.
		state.sess.Status = StatusAwaitingApproval
		if err := a.store.UpdateSession(ctx, state.sess); err != nil {
			return nil, err
		}
	} else {
		// Gate decided from memory / policy — persist the verdict on the tool
		// call row and record the audit entry.
		if tc.ToolStatus == ToolApproved || tc.ToolStatus == ToolRejected {
			if err := a.store.UpdateToolCallStatus(ctx, tc.ID, tc.ToolStatus); err != nil {
				return nil, err
			}
		}
		decision := "allow"
		decisionBy := "system"
		if outcome == "auto_rejected" {
			decision = "deny"
		}
		if gate.Decision.DecidedBy == DecidedByPolicy {
			decisionBy = "policy"
		}
		if err := a.recordGateAuditLocked(ctx, tc, pattern, decision, decisionBy, reason); err != nil {
			return nil, err
		}
		method := "tool_call/approved"
		if outcome == "auto_rejected" {
			method = "tool_call/rejected"
			if state.sess.Status == StatusRunning || state.sess.Status == StatusActive {
				state.sess.LastError = "tool call rejected: " + reason
				if err := a.store.UpdateSession(ctx, state.sess); err != nil {
					return nil, err
				}
			}
		}
		a.emitLocked(method, map[string]any{
			"sessionId":      state.sess.ID,
			"toolCallId":     payload.ToolCallID,
			"name":           payload.ToolName,
			"risk":           string(risk),
			"commandPattern": pattern,
			"reason":         reason,
		})
	}

	result := map[string]any{
		"toolCallId":     payload.ToolCallID,
		"risk":           string(risk),
		"commandPattern": pattern,
		"outcome":        outcome,
		"reason":         reason,
	}
	if requestUID != "" {
		result["requestId"] = requestUID
	}
	// Non-pending outcomes carry the full spec Decision (hook intake reads
	// rule_scope/decided_by from here — PRD F3).
	if gate.Decision.Outcome != "" {
		result["decision"] = gate.Decision
	}
	return mustJSON(result), nil
}

// gateOutcome is the verdict of the approval gate for one tool call.
type gateOutcome struct {
	Outcome   string // "auto_allowed" | "auto_rejected" | "pending_approval" | "policy_allowed" | "gate_error"
	RequestID string // pending request id when Outcome == "pending_approval"
	Reason    string
	Decision  Decision // populated for non-pending outcomes (hook intake uses this)
}

// mainArg is the argument a rule's arg_pattern is matched against
// (EVENT_MODEL v0.2 §3.1): shell tools on the command line, file tools on
// the target path, everything else has none (only bare-tool rules match).
func mainArg(toolName string, rawInput json.RawMessage) string {
	name := strings.ToLower(strings.TrimSpace(toolName))
	if terminalToolNames[name] {
		return extractCommand(rawInput)
	}
	if writeLikeTools[name] || readLikeTools[name] {
		if len(rawInput) != 0 {
			var p struct {
				FilePath string `json:"file_path"`
			}
			if json.Unmarshal(rawInput, &p) == nil && p.FilePath != "" {
				return p.FilePath
			}
		}
	}
	return ""
}

// gateToolCallLocked applies the approval gate to a freshly recorded tool
// call (PRD F3 / decision D16 order):
//
//  1. policy rule hit      → auto-allow, decided_by=policy (四选项②③) —
//     checked BEFORE the high-risk intercept because the verdict UI offers
//     放行 Always on high-risk cards; a rule minted from such a verdict must
//     actually fire, or the promise is false (vibe-trial A7).
//  2. high risk            → intercepted, pending human approval (决策C)
//  3. deny memory          → auto-reject (reject_always verdicts; allow-deny
//     rules arrive with the M2 escalation)
//  4. legacy allow memory  → auto-allow (pre-rule-engine verdicts)
//  5. low risk             → auto-allow (passive monitoring, 决策C)
//  6. otherwise            → permission request raised to the Hub UI
//
// Callers must hold a.mu.
func (a *BaseAdapter) gateToolCallLocked(ctx context.Context, state *sessionState, tc *ToolCall, risk RiskLevel, pattern, mainArgument string) gateOutcome {
	// 1. Policy rules from earlier ②/③ verdicts (decided_by=policy, PRD F3).
	stored, err := a.store.ListRules(ctx)
	if err != nil {
		return gateOutcome{Outcome: "gate_error", Reason: err.Error()}
	}
	if rule, matched := rules.MatchFirst(stored, a.kind, tc.ToolName, mainArgument, tc.SessionID); matched == nil {
		tc.ToolStatus = ToolApproved
		return gateOutcome{
			Outcome:  "policy_allowed",
			Reason:   "matched rule " + rule.ID,
			Decision: Decision{Outcome: OutcomeAllowedOnce, RuleScope: rule.Scope, DecidedBy: DecidedByPolicy, Note: "matched rule " + rule.ID},
		}
	}

	// 2. High-risk operations are intercepted (decision C) unless a rule
	// above already allowed them.
	if risk == RiskHigh {
		uid, err := a.raisePermissionLocked(ctx, state, tc, risk, pattern, "高危操作，需要人工审批")
		if err != nil {
			return gateOutcome{Outcome: "gate_error", Reason: err.Error()}
		}
		return gateOutcome{Outcome: "pending_approval", RequestID: uid, Reason: "high-risk operation intercepted"}
	}

	// 3. Persistent deny memory (reject_always verdicts, decision B).
	mem, err := a.store.GetApprovalMemory(ctx, a.kind, tc.ToolName, pattern)
	if err != nil {
		return gateOutcome{Outcome: "gate_error", Reason: err.Error()}
	}
	if mem != nil && mem.Decision == "deny" {
		tc.ToolStatus = ToolRejected
		return gateOutcome{
			Outcome:  "auto_rejected",
			Reason:   "approval memory: deny",
			Decision: Decision{Outcome: OutcomeRejectedEv, RuleScope: ScopeNone, DecidedBy: DecidedByPolicy, Note: "deny memory"},
		}
	}

	// 4. Legacy allow memory (verdicts written before the rule engine).
	if mem != nil && mem.Decision == "allow" {
		tc.ToolStatus = ToolApproved
		return gateOutcome{
			Outcome:  "auto_allowed",
			Reason:   "approval memory: allow",
			Decision: Decision{Outcome: OutcomeAllowedOnce, RuleScope: ScopeAlways, DecidedBy: DecidedByPolicy, Note: "approval memory: allow"},
		}
	}

	// 5. Low-risk operations pass under passive monitoring (decision C).
	if risk == RiskLow {
		tc.ToolStatus = ToolApproved
		return gateOutcome{
			Outcome:  "auto_allowed",
			Reason:   "passive monitoring (low risk)",
			Decision: Decision{Outcome: OutcomeAllowedOnce, RuleScope: ScopeNone, DecidedBy: DecidedByHub},
		}
	}

	// 6. Everything else needs a human decision.
	uid, err := a.raisePermissionLocked(ctx, state, tc, risk, pattern, "审批记忆未命中，需要人工审批")
	if err != nil {
		return gateOutcome{Outcome: "gate_error", Reason: err.Error()}
	}
	return gateOutcome{Outcome: "pending_approval", RequestID: uid, Reason: "awaiting approval"}
}

// raisePermissionLocked creates the DB record, the pending entry and the
// outbound session/request_permission frame. Callers must hold a.mu.
func (a *BaseAdapter) raisePermissionLocked(ctx context.Context, state *sessionState, tc *ToolCall, risk RiskLevel, pattern, reason string) (string, error) {
	uid, err := newRequestUID()
	if err != nil {
		return "", err
	}
	pr := &PermissionRequest{
		RequestUID:     uid,
		SessionID:      state.sess.ID,
		ToolCallID:     tc.ID,
		ToolName:       tc.ToolName,
		Risk:           string(risk),
		CommandPattern: pattern,
		Reason:         reason,
	}
	if err := a.store.InsertPermissionRequest(ctx, pr); err != nil {
		return "", err
	}
	a.pending[uid] = &pendingPermission{
		DBID:            pr.ID,
		RequestUID:      uid,
		SessionID:       state.sess.ID,
		ToolCallDBID:    tc.ID,
		ToolCallAgentID: tc.AgentToolCallID,
		ToolName:        tc.ToolName,
		Pattern:         pattern,
		MainArg:         mainArg(tc.ToolName, json.RawMessage(tc.RawInput)),
		Risk:            risk,
		CreatedAt:       time.Now(),
	}

	a.emitLocked("session/request_permission", map[string]any{
		"requestId":      uid,
		"sessionId":      state.sess.ID,
		"toolCallId":     tc.AgentToolCallID,
		"name":           tc.ToolName,
		"kind":           tc.ToolKind,
		"risk":           string(risk),
		"commandPattern": pattern,
		"mainArg":        mainArg(tc.ToolName, json.RawMessage(tc.RawInput)),
		"reason":         reason,
		"options":        PermissionOptions(risk),
	})
	return uid, nil
}

// handleSessionRequestPermission processes an inbound session/request_permission
// request from a real ACP agent bridge: the agent itself asks for permission,
// so the Hub routes it through the same approval pipeline. The result echoes
// the Hub request id (for the connector to correlate) plus the agent's option
// list (so the connector can map Hub verdicts back onto agent option ids).
func (a *BaseAdapter) handleSessionRequestPermission(rawMsg json.RawMessage) (json.RawMessage, error) {
	var payload struct {
		SessionID string `json:"sessionId"`
		ToolCall  struct {
			ToolCallID string          `json:"toolCallId"`
			Title      string          `json:"title"`
			Kind       string          `json:"kind"`
			RawInput   json.RawMessage `json:"rawInput"`
		} `json:"toolCall"`
		Options []struct {
			OptionID string `json:"optionId"`
			Name     string `json:"name"`
			Kind     string `json:"kind"`
		} `json:"options"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(rawMsg, &payload); err != nil {
		return nil, fmt.Errorf("failed to unmarshal session/request_permission: %w", err)
	}
	if payload.SessionID == "" {
		return nil, fmt.Errorf("session/request_permission: sessionId is required")
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	ctx := context.Background()

	state, err := a.sessionLocked(payload.SessionID)
	if err != nil {
		return nil, err
	}
	if state.sess.Status == StatusClosed {
		return nil, fmt.Errorf("session/request_permission: session %s is closed", state.sess.ID)
	}

	name := payload.ToolCall.Title
	if name == "" {
		name = payload.ToolCall.ToolCallID
	}
	if name == "" {
		name = "unknown"
	}
	toolCallID := payload.ToolCall.ToolCallID
	if toolCallID == "" {
		toolCallID = "rp_" + mustRandomHex4()
	}

	risk := ClassifyRisk(name, payload.ToolCall.Kind, payload.ToolCall.RawInput)
	pattern := CommandPattern(name, payload.ToolCall.RawInput)
	tc := &ToolCall{
		SessionID:       state.sess.ID,
		AgentToolCallID: toolCallID,
		ToolName:        name,
		ToolKind:        payload.ToolCall.Kind,
		ToolStatus:      ToolPending,
		Risk:            string(risk),
		RawInput:        string(payload.ToolCall.RawInput),
	}
	if err := a.store.InsertToolCall(ctx, tc); err != nil {
		return nil, err
	}
	state.toolCalls[toolCallID] = tc

	// The agent's own options take precedence (they are what the agent can
	// consume back); the Hub fallback list is used when the agent sent none.
	agentOptions := make([]map[string]any, 0, len(payload.Options))
	for _, o := range payload.Options {
		agentOptions = append(agentOptions, map[string]any{
			"optionId": o.OptionID,
			"name":     o.Name,
			"kind":     o.Kind,
		})
	}
	options := agentOptions
	if len(options) == 0 {
		hubOpts := PermissionOptions(risk)
		options = make([]map[string]any, 0, len(hubOpts))
		for _, o := range hubOpts {
			options = append(options, map[string]any{
				"optionId": o.OptionID,
				"name":     o.Label,
				"kind":     o.Kind,
			})
		}
	}

	reason := payload.Reason
	if reason == "" {
		reason = "agent 请求权限审批"
	}
	uid, err := a.raiseAgentPermissionLocked(ctx, state, tc, risk, pattern, reason, options)
	if err != nil {
		return nil, err
	}
	state.sess.Status = StatusAwaitingApproval
	if err := a.store.UpdateSession(ctx, state.sess); err != nil {
		return nil, err
	}

	return mustJSON(map[string]any{
		"requestId":  uid,
		"sessionId":  state.sess.ID,
		"toolCallId": toolCallID,
		"risk":       string(risk),
		"options":    options,
	}), nil
}

// raiseAgentPermissionLocked is raisePermissionLocked for agent-initiated
// requests: same DB/pending bookkeeping, but the outbound frame carries the
// agent-provided option list. Callers must hold a.mu.
func (a *BaseAdapter) raiseAgentPermissionLocked(ctx context.Context, state *sessionState, tc *ToolCall, risk RiskLevel, pattern, reason string, options []map[string]any) (string, error) {
	uid, err := newRequestUID()
	if err != nil {
		return "", err
	}
	pr := &PermissionRequest{
		RequestUID:     uid,
		SessionID:      state.sess.ID,
		ToolCallID:     tc.ID,
		ToolName:       tc.ToolName,
		Risk:           string(risk),
		CommandPattern: pattern,
		Reason:         reason,
	}
	if err := a.store.InsertPermissionRequest(ctx, pr); err != nil {
		return "", err
	}
	a.pending[uid] = &pendingPermission{
		DBID:            pr.ID,
		RequestUID:      uid,
		SessionID:       state.sess.ID,
		ToolCallDBID:    tc.ID,
		ToolCallAgentID: tc.AgentToolCallID,
		ToolName:        tc.ToolName,
		Pattern:         pattern,
		MainArg:         mainArg(tc.ToolName, json.RawMessage(tc.RawInput)),
		Risk:            risk,
		CreatedAt:       time.Now(),
	}

	a.emitLocked("session/request_permission", map[string]any{
		"requestId":      uid,
		"sessionId":      state.sess.ID,
		"toolCallId":     tc.AgentToolCallID,
		"name":           tc.ToolName,
		"kind":           tc.ToolKind,
		"risk":           string(risk),
		"commandPattern": pattern,
		"mainArg":        mainArg(tc.ToolName, json.RawMessage(tc.RawInput)),
		"reason":         reason,
		"options":        options,
	})
	return uid, nil
}

// recordGateAuditLocked writes an approval_history entry for gate decisions
// taken without a permission request (memory hits, low-risk pass-through), so
// the audit trail covers every automatic decision too. Callers must hold a.mu.
func (a *BaseAdapter) recordGateAuditLocked(ctx context.Context, tc *ToolCall, pattern, decision, decisionBy, reason string) error {
	return a.store.InsertApproval(ctx, &ApprovalRecord{
		PermissionRequestID: 0, // no permission_requests row for system decisions
		SessionID:           tc.SessionID,
		Decision:            decision,
		DecisionBy:          decisionBy,
		Notes: fmt.Sprintf("tool=%s toolCallId=%s pattern=%q risk=%s reason=%s",
			tc.ToolName, tc.AgentToolCallID, pattern, tc.Risk, reason),
	})
}

// ResolvePermission applies a human decision to a pending permission request
// (decision B: permanent verdicts need a second confirmation). Requests left
// pending by a daemon restart are recovered from the database.
func (a *BaseAdapter) ResolvePermission(requestUID string, dec PermissionDecision) (PermissionResolution, error) {
	decision, err := NormalizeDecision(dec.Decision)
	if err != nil {
		return PermissionResolution{}, err
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	ctx := context.Background()

	p, ok := a.pending[requestUID]
	if !ok {
		// Not in memory: either unknown, already resolved, or left pending by
		// a restart. Recover from the database.
		pr, err := a.store.GetPermissionRequestByUID(ctx, requestUID)
		if err != nil {
			return PermissionResolution{}, fmt.Errorf("resolve permission: %w", err)
		}
		if pr.Outcome != OutcomePending {
			return PermissionResolution{}, fmt.Errorf("resolve permission: request %s already resolved (outcome %s)", requestUID, pr.Outcome)
		}
		p = &pendingPermission{
			DBID:         pr.ID,
			RequestUID:   pr.RequestUID,
			SessionID:    pr.SessionID,
			ToolCallDBID: pr.ToolCallID,
			ToolName:     pr.ToolName,
			Pattern:      pr.CommandPattern,
			Risk:         RiskLevel(pr.Risk),
		}
	}
	return a.resolvePendingLocked(ctx, p, decision, dec)
}

// resolvePendingLocked applies the decision to one pending request. Callers
// must hold a.mu.
func (a *BaseAdapter) resolvePendingLocked(ctx context.Context, p *pendingPermission, decision string, dec PermissionDecision) (PermissionResolution, error) {
	// Double confirmation for permanent verdicts (decision B).
	if (decision == DecisionAllowAlways || decision == DecisionRejectAlways) && !p.AwaitingConfirm {
		p.AwaitingConfirm = true
		return PermissionResolution{
			RequestID:            p.RequestUID,
			Outcome:              OutcomePending,
			Applied:              false,
			RequiresConfirmation: true,
			Note:                 "永久判决需要二次确认，请携带 confirm=true 再次提交",
		}, nil
	}

	allow := decision == DecisionAllowOnce || decision == DecisionAllowSession || decision == DecisionAllowAlways
	outcome := OutcomeRejected
	if allow {
		outcome = OutcomeApproved
	}
	if err := a.store.ResolvePermissionRequest(ctx, p.DBID, outcome, decision); err != nil {
		return PermissionResolution{}, err
	}

	decidedBy := dec.DecidedBy
	if decidedBy == "" {
		decidedBy = "user"
	}
	notes := dec.Note
	if p.AwaitingConfirm {
		notes = joinNotes(notes, "double-confirmed")
	}
	if err := a.store.InsertApproval(ctx, &ApprovalRecord{
		PermissionRequestID: p.DBID,
		SessionID:           p.SessionID,
		Decision:            allowVerb(allow),
		DecisionBy:          decidedBy,
		Notes:               notes,
	}); err != nil {
		return PermissionResolution{}, err
	}

	// Tool call lifecycle follows the verdict — but a call that already
	// reached a terminal state is never downgraded by a late verdict
	// (vibe-trial A9: a decision arriving after the tool result used to
	// flip a completed call back to approved).
	tcStatus := ToolRejected
	if allow {
		tcStatus = ToolApproved
	}
	state := a.sessions[p.SessionID]
	if p.ToolCallDBID != 0 {
		if cur, gerr := a.store.GetToolCall(ctx, p.ToolCallDBID); gerr == nil && isTerminalToolStatus(cur.ToolStatus) {
			// Keep the recorded outcome; the verdict is already captured in
			// permission_requests + approvals below.
		} else if err := a.store.UpdateToolCallStatus(ctx, p.ToolCallDBID, tcStatus); err != nil {
			return PermissionResolution{}, err
		}
	}
	// Keep the in-memory projection in sync with the DB row.
	if state != nil && p.ToolCallAgentID != "" {
		if tc, ok := state.toolCalls[p.ToolCallAgentID]; ok && !isTerminalToolStatus(tc.ToolStatus) {
			tc.ToolStatus = tcStatus
		}
	}

	// Scoped allowances become policy rules (PRD F3 / Q-F4: pattern is the
	// exact main argument, a literal glob); reject_always stays in approval
	// memory (deny rules are the M2 escalation).
	if decision == DecisionAllowSession || decision == DecisionAllowAlways {
		scope := ScopeSession
		if decision == DecisionAllowAlways {
			scope = ScopeAlways
		}
		rule := rules.Rule{
			ID:          "rule_" + strings.TrimPrefix(p.RequestUID, "pr_"),
			Tool:        p.ToolName,
			ArgPattern:  p.MainArg,
			Scope:       scope,
			Effect:      rules.EffectAllow,
			SessionID:   p.SessionID,
			CreatedFrom: p.RequestUID,
			AgentKind:   a.kind,
		}
		if err := a.store.AddRule(ctx, rule); err != nil {
			return PermissionResolution{}, err
		}
		// Keep approval_memory in step with always-scoped rules so the pair
		// can be revoked together (vibe-trial B12: rules and memory used to
		// diverge silently). Session-scoped grants stay rules-only — memory
		// has no session dimension and would leak the grant globally.
		if decision == DecisionAllowAlways {
			_ = a.writeAllowMemoryLocked(ctx, p)
		}
	} else {
		if err := a.writeMemoryIfNeededLocked(ctx, p, decision, allow); err != nil {
			return PermissionResolution{}, err
		}
	}

	// Resume the session unless it closed while we waited.
	sess := (*Session)(nil)
	if state != nil {
		sess = state.sess
	} else if p.SessionID != "" {
		if loaded, err := a.store.GetSession(ctx, p.SessionID); err == nil {
			sess = loaded
		}
	}
	if sess != nil && sess.Status == StatusAwaitingApproval {
		sess.Status = StatusRunning
		if err := a.store.UpdateSession(ctx, sess); err != nil {
			return PermissionResolution{}, err
		}
	}
	delete(a.pending, p.RequestUID)

	// Emit the AUTHORITATIVE session status (vibe-trial R4-F1: a hardcoded
	// "running" here landed after the task's finished event whenever the
	// human verdict came late, and live boards replayed back to running).
	finalStatus := StatusRunning
	if sess != nil {
		finalStatus = sess.Status
	}
	a.emitLocked("session/update", map[string]any{
		"sessionId":           p.SessionID,
		"status":              finalStatus,
		"permissionRequestId": p.RequestUID,
		"permissionOutcome":   outcome,
		"toolCallId":          p.ToolCallAgentID,
	})
	// Deliver the verdict to a waiting hook intake (POST /api/hook/intake),
	// if one is parked on this request. The spec Decision uses the frozen
	// outcome vocabulary.
	if ch, ok := a.waiters[p.RequestUID]; ok {
		d := RejectDecision(dec.Note)
		if allow {
			d = AllowOnceDecision()
			d.Note = dec.Note
			switch decision {
			case DecisionAllowSession:
				d.RuleScope = ScopeSession
			case DecisionAllowAlways:
				d.RuleScope = ScopeAlways
			}
		}
		select {
		case ch <- d:
		default:
		}
	}
	return PermissionResolution{
		RequestID: p.RequestUID,
		Outcome:   outcome,
		Applied:   true,
		Decision:  decision,
		Note:      notes,
	}, nil
}

// isTerminalToolStatus reports whether a tool call has reached a final
// outcome that a late verdict must not overwrite (vibe-trial A9).
func isTerminalToolStatus(s string) bool {
	return s == ToolCompleted || s == ToolFailed || s == ToolRejected
}

// answerCap bounds the accumulated answer per session (bytes).
const answerCap = 32 * 1024

// appendAnswer appends one streamed chunk / result line to the session's
// answer attribute, rune-safe under answerCap. Once the cap is reached a
// truncation marker is appended once and further chunks are dropped.
func appendAnswer(cur, chunk string) string {
	if len(cur) >= answerCap {
		if strings.HasSuffix(cur, "…(截断)") {
			return cur
		}
		return truncateRunes(cur, answerCap) + "…(截断)"
	}
	next := cur
	if next != "" {
		next += "\n"
	}
	next += chunk
	if len(next) > answerCap {
		return truncateRunes(next, answerCap) + "…(截断)"
	}
	return next
}

// truncateRunes cuts a string to at most n bytes without splitting a rune.
func truncateRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := s[:n]
	for len(cut) > 0 && cut[len(cut)-1]&0xC0 == 0x80 {
		cut = cut[:len(cut)-1] // drop continuation bytes of a split rune
	}
	return cut
}

// writeAllowMemoryLocked upserts an allow row into approval_memory for an
// always-scoped rule (vibe-trial B12: rules and memory now move together).
func (a *BaseAdapter) writeAllowMemoryLocked(ctx context.Context, p *pendingPermission) error {
	mem, err := a.store.GetApprovalMemory(ctx, a.kind, p.ToolName, p.Pattern)
	if err != nil {
		return err
	}
	confirmCount := 2 // first click + second confirmation
	if mem != nil {
		confirmCount = mem.ConfirmCount + 1
	}
	return a.store.UpsertApprovalMemory(ctx, &ApprovalMemory{
		AgentKind:      a.kind,
		Tool:           p.ToolName,
		CommandPattern: p.Pattern,
		Decision:       "allow",
		Scope:          "always",
		ConfirmCount:   confirmCount,
	})
}

// writeMemoryIfNeededLocked persists a reject_always verdict into approval
// memory once the double confirmation is complete (allow rules went to the
// rules table instead; deny rules are the M2 escalation). Callers must hold
// a.mu.
func (a *BaseAdapter) writeMemoryIfNeededLocked(ctx context.Context, p *pendingPermission, decision string, allow bool) error {
	if allow || decision != DecisionRejectAlways {
		return nil
	}
	verdict := "deny"
	mem, err := a.store.GetApprovalMemory(ctx, a.kind, p.ToolName, p.Pattern)
	if err != nil {
		return err
	}
	confirmCount := 2 // first click + second confirmation
	if mem != nil {
		confirmCount = mem.ConfirmCount + 1
	}
	return a.store.UpsertApprovalMemory(ctx, &ApprovalMemory{
		AgentKind:      a.kind,
		Tool:           p.ToolName,
		CommandPattern: p.Pattern,
		Decision:       verdict,
		Scope:          "always",
		ConfirmCount:   confirmCount,
	})
}

// handleToolResult processes the tool_result ACP notification: finalizes the
// tool call row, accumulates usage and reports the session running again.
func (a *BaseAdapter) handleToolResult(rawMsg json.RawMessage) (json.RawMessage, error) {
	var payload struct {
		SessionID  string          `json:"sessionId"`
		ToolCallID string          `json:"toolCallId"`
		ToolName   string          `json:"name"`
		ToolKind   string          `json:"kind"`
		ToolStatus string          `json:"status"`
		Content    json.RawMessage `json:"content"`
		ToolError  string          `json:"error"`
		Usage      struct {
			InputTokens  int64 `json:"inputTokens"`
			OutputTokens int64 `json:"outputTokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rawMsg, &payload); err != nil {
		return nil, fmt.Errorf("failed to unmarshal tool_result: %w", err)
	}
	if payload.ToolCallID == "" {
		return nil, fmt.Errorf("tool_result: toolCallId is required")
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	ctx := context.Background()

	state, err := a.sessionLocked(payload.SessionID)
	if err != nil {
		return nil, err
	}
	tc, err := a.store.GetToolCallByAgentID(ctx, state.sess.ID, payload.ToolCallID)
	if err != nil {
		return nil, fmt.Errorf("tool_result: unknown toolCallId %s", payload.ToolCallID)
	}

	status := normalizeToolResultStatus(payload.ToolStatus, payload.ToolError)
	rawOutput := string(payload.Content)
	if payload.ToolError != "" {
		rawOutput = joinNotes(rawOutput, "error: "+payload.ToolError)
	}
	if err := a.store.UpdateToolCallResult(ctx, tc.ID, status, rawOutput); err != nil {
		return nil, err
	}
	// Keep the in-memory projection in sync with the DB row (storing the same
	// pointer again is harmless; after hydration the map may not have it yet).
	tc.ToolStatus = status
	tc.RawOutput = rawOutput
	state.toolCalls[payload.ToolCallID] = tc

	state.sess.InputTokens += payload.Usage.InputTokens
	state.sess.OutputTokens += payload.Usage.OutputTokens
	if status == ToolFailed && payload.ToolError != "" {
		state.sess.LastError = payload.ToolError
	}
	if state.sess.Status == StatusRunning || state.sess.Status == StatusAwaitingApproval {
		state.sess.Status = StatusRunning
	}
	if err := a.store.UpdateSession(ctx, state.sess); err != nil {
		return nil, err
	}

	a.emitLocked("session/update", map[string]any{
		"sessionId":  state.sess.ID,
		"status":     state.sess.Status,
		"toolCallId": payload.ToolCallID,
		"toolStatus": status,
		"usage": map[string]int64{
			"inputTokens":  state.sess.InputTokens,
			"outputTokens": state.sess.OutputTokens,
		},
	})
	return mustJSON(map[string]any{"toolCallId": payload.ToolCallID, "status": status}), nil
}

// handleSessionUpdate processes the session/update ACP notification: applies
// the state machine, accumulates usage and relays the update to listeners.
func (a *BaseAdapter) handleSessionUpdate(rawMsg json.RawMessage) (json.RawMessage, error) {
	var payload struct {
		SessionID  string          `json:"sessionId"`
		Status     string          `json:"status"`
		Content    string          `json:"content"`
		IsComplete bool            `json:"isComplete"`
		Usage      json.RawMessage `json:"usage"`
		Plan       string          `json:"plan"`
		Error      string          `json:"error"`
	}
	if err := json.Unmarshal(rawMsg, &payload); err != nil {
		return nil, fmt.Errorf("failed to unmarshal session/update: %w", err)
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	ctx := context.Background()

	state, err := a.sessionLocked(payload.SessionID)
	if err != nil {
		return nil, err
	}

	status := payload.Status
	if payload.IsComplete {
		status = StatusCompleted
	}
	if status != "" {
		if !isValidTransition(state.sess.Status, status) {
			return nil, fmt.Errorf("session/update: invalid transition %s -> %s", state.sess.Status, status)
		}
		state.sess.Status = status
	}
	if payload.Error != "" {
		state.sess.LastError = payload.Error
		if state.sess.Status != StatusClosed && status == "" {
			state.sess.Status = StatusFailed
		}
	}
	if len(payload.Usage) > 0 {
		var usage struct {
			InputTokens  int64 `json:"inputTokens"`
			OutputTokens int64 `json:"outputTokens"`
		}
		if err := json.Unmarshal(payload.Usage, &usage); err == nil {
			state.sess.InputTokens += usage.InputTokens
			state.sess.OutputTokens += usage.OutputTokens
		}
	}
	// Accumulate the agent's answer (user request 2026-11-14: 看到回话的具体
	// 内容 — streamed chunks and the final result line append into one
	// session attribute; capped rune-safe at 32KB). Plan text is not part of
	// the answer.
	if payload.Content != "" {
		state.sess.Answer = appendAnswer(state.sess.Answer, payload.Content)
	}
	if status == StatusCompleted {
		state.sess.IsTerminal = true
	}
	if status == StatusClosed {
		state.sess.IsClosed = true
		state.sess.IsTerminal = true
	}
	if err := a.store.UpdateSession(ctx, state.sess); err != nil {
		return nil, err
	}

	// Relay the update (with the authoritative status) to Hub listeners.
	a.emitLocked("session/update", map[string]any{
		"sessionId":  state.sess.ID,
		"status":     state.sess.Status,
		"isComplete": payload.IsComplete,
		"plan":       payload.Plan,
		"content":    payload.Content,
		"usage": map[string]int64{
			"inputTokens":  state.sess.InputTokens,
			"outputTokens": state.sess.OutputTokens,
		},
	})
	return mustJSON(map[string]any{"sessionId": state.sess.ID, "status": state.sess.Status}), nil
}

// ------------------------------------------------------------ state machine

// adapterStateTransitions defines the legal session/update transitions.
var adapterStateTransitions = map[string][]string{
	StatusActive:           {StatusRunning, StatusAwaitingApproval, StatusAwaitingInput, StatusCompleted, StatusFailed, StatusClosed},
	StatusRunning:          {StatusRunning, StatusAwaitingApproval, StatusAwaitingInput, StatusCompleted, StatusFailed, StatusClosed},
	StatusAwaitingApproval: {StatusRunning, StatusAwaitingInput, StatusCompleted, StatusFailed, StatusClosed},
	StatusAwaitingInput:    {StatusRunning, StatusAwaitingApproval, StatusCompleted, StatusFailed, StatusClosed},
	StatusCompleted:        {StatusRunning, StatusClosed},
	StatusFailed:           {StatusRunning, StatusClosed},
	StatusClosed:           {},
}

func isValidTransition(from, to string) bool {
	for _, s := range adapterStateTransitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

func normalizeToolResultStatus(status, errText string) string {
	switch status {
	case "success", "completed", "ok":
		return ToolCompleted
	case "failed", "error", "cancelled", "canceled":
		return ToolFailed
	case "":
		if errText != "" {
			return ToolFailed
		}
		return ToolCompleted
	default:
		return status
	}
}

func allowVerb(allow bool) string {
	if allow {
		return "allow"
	}
	return "deny"
}

func joinNotes(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}

// sessionLocked returns the in-memory state for a session, lazily hydrating
// from the DB after a process restart. Callers must hold a.mu.
func (a *BaseAdapter) sessionLocked(sessionID string) (*sessionState, error) {
	if state, ok := a.sessions[sessionID]; ok {
		return state, nil
	}
	sess, err := a.store.GetSession(context.Background(), sessionID)
	if err != nil {
		return nil, fmt.Errorf("unknown session %s: %w", sessionID, ErrNotFound)
	}
	state := &sessionState{
		sess:      sess,
		toolCalls: map[string]*ToolCall{},
	}
	a.sessions[sessionID] = state
	return state, nil
}

// emitLocked sends an outbound frame; never fails the handler.
func (a *BaseAdapter) emitLocked(method string, payload any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	a.frameSeq++
	a.sink.EmitFrame(method, raw)
}

// newRequestUID generates a random permission request id.
func newRequestUID() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("failed to generate request id: %w", err)
	}
	return "pr_" + hex.EncodeToString(buf), nil
}

// mustRandomHex4 generates a short random suffix; failures fall back to a
// timestamp-derived value rather than erroring id generation paths.
func mustRandomHex4() string {
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%08x", time.Now().UnixNano()&0xffffffff)
	}
	return hex.EncodeToString(buf)
}

func mustJSON(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return raw
}
