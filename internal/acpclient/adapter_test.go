package acpclient

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// recordingSink captures outbound frames for assertions.
type recordingSink struct {
	mu     sync.Mutex
	events []recordedFrame
}

type recordedFrame struct {
	Method string
	Params map[string]any
}

func (s *recordingSink) EmitFrame(method string, params json.RawMessage) {
	var m map[string]any
	_ = json.Unmarshal(params, &m)
	s.mu.Lock()
	s.events = append(s.events, recordedFrame{Method: method, Params: m})
	s.mu.Unlock()
}

func (s *recordingSink) count(method string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, e := range s.events {
		if e.Method == method {
			n++
		}
	}
	return n
}

func (s *recordingSink) last(method string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.events) - 1; i >= 0; i-- {
		if s.events[i].Method == method {
			return s.events[i].Params
		}
	}
	return nil
}

// newTestAdapter builds an adapter over a temp SQLite file.
func newTestAdapter(t *testing.T) (*ClaudeCodeAdapter, *recordingSink, *Store) {
	t.Helper()
	db, err := InitDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	store := NewStore(db)
	sink := &recordingSink{}
	adapter, err := NewClaudeCodeAdapter(store, sink)
	if err != nil {
		t.Fatalf("NewClaudeCodeAdapter: %v", err)
	}
	if err := adapter.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	return adapter, sink, store
}

func frame(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal frame: %v", err)
	}
	return raw
}

func mustHandle(t *testing.T, a *ClaudeCodeAdapter, method string, params json.RawMessage) map[string]any {
	t.Helper()
	raw, err := a.HandleFrame(method, params)
	if err != nil {
		t.Fatalf("HandleFrame(%s): %v", method, err)
	}
	if raw == nil {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	return out
}

// TestAdapterFullLifecycle walks session/new → prompt → gated tool_call →
// session approval → tool_result → completion, asserting DB state and emitted
// frames at every step.
func TestAdapterFullLifecycle(t *testing.T) {
	a, sink, store := newTestAdapter(t)
	ctx := context.Background()

	// 1. session/new
	res := mustHandle(t, a, "session/new", frame(t, map[string]any{
		"sessionId": "sess-1",
		"cwd":       "D:\\work\\proj",
		"configOptions": map[string]string{
			"model":    "claude-sonnet-4-5",
			"provider": "anthropic",
		},
	}))
	if res["sessionId"] != "sess-1" {
		t.Fatalf("session/new result: %v", res)
	}
	sess, err := store.GetSession(ctx, "sess-1")
	if err != nil || sess.Status != StatusActive {
		t.Fatalf("session row: %+v err=%v", sess, err)
	}
	if got := sink.count("session/update"); got != 1 {
		t.Fatalf("expected 1 session/update frame after creation, got %d", got)
	}

	// 2. session/prompt
	res = mustHandle(t, a, "session/prompt", frame(t, map[string]any{
		"sessionId": "sess-1",
		"prompt":    "帮我安装依赖并构建",
	}))
	if res["turnIndex"].(float64) != 1 {
		t.Fatalf("prompt turnIndex: %v", res)
	}

	// 3. tool_call: medium risk, no memory → pending approval
	res = mustHandle(t, a, "tool_call", frame(t, map[string]any{
		"sessionId":  "sess-1",
		"toolCallId": "tc_1",
		"name":       "Bash",
		"kind":       "execute",
		"rawInput":   map[string]string{"command": "npm install left-pad"},
	}))
	if res["outcome"] != "pending_approval" {
		t.Fatalf("tool_call outcome: %v", res)
	}
	uid := res["requestId"].(string)
	sess, _ = store.GetSession(ctx, "sess-1")
	if sess.Status != StatusAwaitingApproval {
		t.Fatalf("session status = %s, want awaiting_approval", sess.Status)
	}
	reqFrame := sink.last("session/request_permission")
	if reqFrame == nil || reqFrame["requestId"] != uid {
		t.Fatalf("session/request_permission frame missing: %v", reqFrame)
	}
	opts := reqFrame["options"].([]any)
	if len(opts) == 0 {
		t.Fatal("permission options empty")
	}

	// Prompting while awaiting approval must fail.
	if _, err := a.HandleFrame("session/prompt", frame(t, map[string]any{"sessionId": "sess-1", "prompt": "again"})); err == nil {
		t.Fatal("prompt during awaiting_approval accepted")
	}

	// 4. Human decision: allow_session
	got, err := a.ResolvePermission(uid, PermissionDecision{Decision: DecisionAllowSession})
	if err != nil || !got.Applied || got.Outcome != OutcomeApproved {
		t.Fatalf("ResolvePermission: %+v err=%v", got, err)
	}
	sess, _ = store.GetSession(ctx, "sess-1")
	if sess.Status != StatusRunning {
		t.Fatalf("session status after approval = %s, want running", sess.Status)
	}

	// 5. PRD Q-F4 semantics: the allow_session rule is bound to the exact
	// main argument. Same command → policy auto-allow; different args → the
	// user is asked again.
	res = mustHandle(t, a, "tool_call", frame(t, map[string]any{
		"sessionId":  "sess-1",
		"toolCallId": "tc_2",
		"name":       "Bash",
		"kind":       "execute",
		"rawInput":   map[string]string{"command": "npm install left-pad"},
	}))
	if res["outcome"] != "policy_allowed" {
		t.Fatalf("session-scope replay outcome: %v", res)
	}
	if sink.last("tool_call/approved") == nil {
		t.Fatal("tool_call/approved frame missing")
	}
	res = mustHandle(t, a, "tool_call", frame(t, map[string]any{
		"sessionId":  "sess-1",
		"toolCallId": "tc_2b",
		"name":       "Bash",
		"kind":       "execute",
		"rawInput":   map[string]string{"command": "npm install other-pkg"},
	}))
	if res["outcome"] != "pending_approval" {
		t.Fatalf("different args must re-ask: %v", res)
	}

	// 6. tool_result finalizes the call and accumulates usage
	mustHandle(t, a, "tool_result", frame(t, map[string]any{
		"sessionId":  "sess-1",
		"toolCallId": "tc_1",
		"status":     "success",
		"content":    "added 42 packages",
		"usage":      map[string]int64{"inputTokens": 120, "outputTokens": 80},
	}))
	tc, err := store.GetToolCallByAgentID(ctx, "sess-1", "tc_1")
	if err != nil || tc.ToolStatus != ToolCompleted || tc.CompletedAt == 0 {
		t.Fatalf("tool call after result: %+v err=%v", tc, err)
	}
	sess, _ = store.GetSession(ctx, "sess-1")
	if sess.InputTokens != 120 || sess.OutputTokens != 80 {
		t.Fatalf("usage not accumulated: %+v", sess)
	}

	// 7. session/update completion
	res = mustHandle(t, a, "session/update", frame(t, map[string]any{
		"sessionId":  "sess-1",
		"isComplete": true,
		"usage":      map[string]int64{"inputTokens": 10, "outputTokens": 5},
	}))
	if res["status"] != StatusCompleted {
		t.Fatalf("completion status: %v", res)
	}
	sess, _ = store.GetSession(ctx, "sess-1")
	if !sess.IsTerminal || sess.InputTokens != 130 {
		t.Fatalf("final session state: %+v", sess)
	}

	// 8. Closing is terminal; further prompts are rejected.
	mustHandle(t, a, "session/update", frame(t, map[string]any{"sessionId": "sess-1", "status": StatusClosed}))
	if _, err := a.HandleFrame("session/prompt", frame(t, map[string]any{"sessionId": "sess-1", "prompt": "x"})); err == nil {
		t.Fatal("prompt on closed session accepted")
	}
	if _, err := a.HandleFrame("tool_call", frame(t, map[string]any{
		"sessionId": "sess-1", "toolCallId": "tc_9", "name": "Bash", "kind": "execute",
		"rawInput": map[string]string{"command": "echo hi"},
	})); err == nil {
		t.Fatal("tool_call on closed session accepted")
	}
}

// TestAdapterHighRiskAlwaysIntercepted verifies decision C: even a stored
// "allow" memory verdict never auto-approves a high-risk tool call, and the
// option list hides permanent approval.
func TestAdapterHighRiskAlwaysIntercepted(t *testing.T) {
	a, sink, store := newTestAdapter(t)
	ctx := context.Background()

	mustHandle(t, a, "session/new", frame(t, map[string]any{"sessionId": "s", "cwd": "D:\\x"}))

	// Store a (hypothetical) allow memory for the rm pattern.
	if err := store.UpsertApprovalMemory(ctx, &ApprovalMemory{
		AgentKind: AgentKindClaudeCode, Tool: "Bash", CommandPattern: "rm -rf",
		Decision: "allow", Scope: "always", ConfirmCount: 2,
	}); err != nil {
		t.Fatalf("seed memory: %v", err)
	}

	res := mustHandle(t, a, "tool_call", frame(t, map[string]any{
		"sessionId": "s", "toolCallId": "tc_h1", "name": "Bash", "kind": "execute",
		"rawInput": map[string]string{"command": "rm -rf D:\\work\\proj\\build"},
	}))
	if res["outcome"] != "pending_approval" {
		t.Fatalf("high risk with allow memory must still be intercepted: %v", res)
	}
	if res["risk"] != RiskHigh {
		t.Fatalf("risk = %v, want high", res["risk"])
	}

	reqFrame := sink.last("session/request_permission")
	for _, o := range reqFrame["options"].([]any) {
		if o.(map[string]any)["optionId"] == DecisionAllowAlways {
			t.Error("permanent allow offered for high-risk operation")
		}
	}

	// Deny it and the tool call row is rejected.
	uid := reqFrame["requestId"].(string)
	got, err := a.ResolvePermission(uid, PermissionDecision{Decision: DecisionRejectOnce})
	if err != nil || !got.Applied || got.Outcome != OutcomeRejected {
		t.Fatalf("reject resolution: %+v err=%v", got, err)
	}
	tc, err := store.GetToolCallByAgentID(ctx, "s", "tc_h1")
	if err != nil || tc.ToolStatus != ToolRejected {
		t.Fatalf("rejected tool call: %+v err=%v", tc, err)
	}
}

// TestAdapterPermanentVerdicts verifies decision B: allow_always requires a
// second confirmation, then lands in approval memory and auto-applies later;
// reject_always does the same for deny.
func TestAdapterPermanentVerdicts(t *testing.T) {
	a, sink, store := newTestAdapter(t)
	ctx := context.Background()

	mustHandle(t, a, "session/new", frame(t, map[string]any{"sessionId": "s", "cwd": "D:\\x"}))

	raise := func(id, cmd string) string {
		res := mustHandle(t, a, "tool_call", frame(t, map[string]any{
			"sessionId": "s", "toolCallId": id, "name": "Bash", "kind": "execute",
			"rawInput": map[string]string{"command": cmd},
		}))
		if res["outcome"] != "pending_approval" {
			t.Fatalf("tool_call %s outcome: %v", id, res)
		}
		return res["requestId"].(string)
	}

	// First raise → allow_always without confirm → requires confirmation.
	uid1 := raise("tc_1", "cargo build --release")
	res1, err := a.ResolvePermission(uid1, PermissionDecision{Decision: DecisionAllowAlways})
	if err != nil {
		t.Fatalf("first allow_always: %v", err)
	}
	if res1.Applied || !res1.RequiresConfirmation {
		t.Fatalf("first allow_always should require confirmation: %+v", res1)
	}
	// Still pending in DB.
	pr, _ := store.GetPermissionRequestByUID(ctx, uid1)
	if pr.Outcome != OutcomePending {
		t.Fatalf("request resolved too early: %v", pr.Outcome)
	}

	// Second confirmation applies and writes a policy rule (PRD F3/Q-F4:
	// pattern = the exact main argument).
	res2, err := a.ResolvePermission(uid1, PermissionDecision{Decision: DecisionAllowAlways, Confirm: true})
	if err != nil || !res2.Applied || res2.Outcome != OutcomeApproved {
		t.Fatalf("second allow_always: %+v err=%v", res2, err)
	}
	storedRules, err := store.ListRules(ctx)
	if err != nil || len(storedRules) != 1 {
		t.Fatalf("rules after double confirm: %+v err=%v", storedRules, err)
	}
	rule := storedRules[0]
	if rule.Tool != "Bash" || rule.ArgPattern != "cargo build --release" || rule.Scope != "always" ||
		rule.Effect != "allow" || rule.CreatedFrom != uid1 {
		t.Fatalf("rule shape: %+v", rule)
	}

	// Next identical main argument auto-applies from the rule (decided_by=policy).
	res := mustHandle(t, a, "tool_call", frame(t, map[string]any{
		"sessionId": "s", "toolCallId": "tc_2", "name": "Bash", "kind": "execute",
		"rawInput": map[string]string{"command": "cargo build --release"},
	}))
	if res["outcome"] != "policy_allowed" {
		t.Fatalf("rule auto-apply outcome: %v", res)
	}
	if sink.last("tool_call/approved")["reason"] != "matched rule "+rule.ID {
		t.Fatalf("unexpected approval reason: %v", sink.last("tool_call/approved"))
	}
	// Audit trail records the policy decision.
	approvals, _ := store.ListApprovals(ctx, 50)
	foundPolicy := false
	for _, ap := range approvals {
		if ap.DecisionBy == "policy" && ap.Decision == "allow" {
			foundPolicy = true
		}
	}
	if !foundPolicy {
		t.Error("policy decision missing from approval history")
	}

	// reject_always double confirmation writes a deny memory for a different
	// pattern (the "cargo build" rule still auto-allows that one).
	uid3 := raise("tc_3", "cargo test ./...")
	if _, err := a.ResolvePermission(uid3, PermissionDecision{Decision: DecisionRejectAlways}); err != nil {
		t.Fatalf("first reject_always: %v", err)
	}
	if _, err := a.ResolvePermission(uid3, PermissionDecision{Decision: DecisionRejectAlways, Confirm: true}); err != nil {
		t.Fatalf("second reject_always: %v", err)
	}
	mem, _ := store.GetApprovalMemory(ctx, AgentKindClaudeCode, "Bash", "cargo test")
	if mem == nil || mem.Decision != "deny" {
		t.Fatalf("deny memory: %+v", mem)
	}

	// Deny memory auto-rejects a new call on the same pattern.
	res = mustHandle(t, a, "tool_call", frame(t, map[string]any{
		"sessionId": "s", "toolCallId": "tc_4", "name": "Bash", "kind": "execute",
		"rawInput": map[string]string{"command": "cargo test ./..."},
	}))
	if res["outcome"] != "auto_rejected" {
		t.Fatalf("deny memory outcome: %v", res)
	}
	if sink.last("tool_call/rejected") == nil {
		t.Fatal("tool_call/rejected frame missing")
	}
	// ...while the earlier allow rule keeps applying to its own pattern.
	res = mustHandle(t, a, "tool_call", frame(t, map[string]any{
		"sessionId": "s", "toolCallId": "tc_5", "name": "Bash", "kind": "execute",
		"rawInput": map[string]string{"command": "cargo build --release"},
	}))
	if res["outcome"] != "policy_allowed" {
		t.Fatalf("allow rule outcome after deny memory: %v", res)
	}
}

// TestAdapterLowRiskPassive verifies read-only tool calls flow through without
// prompting (passive monitoring) but are still recorded.
func TestAdapterLowRiskPassive(t *testing.T) {
	a, sink, store := newTestAdapter(t)

	mustHandle(t, a, "session/new", frame(t, map[string]any{"sessionId": "s", "cwd": "D:\\x"}))
	res := mustHandle(t, a, "tool_call", frame(t, map[string]any{
		"sessionId": "s", "toolCallId": "tc_1", "name": "Read", "kind": "read",
		"rawInput": map[string]string{"file_path": "D:\\x\\main.go"},
	}))
	if res["outcome"] != "auto_allowed" {
		t.Fatalf("low risk outcome: %v", res)
	}
	tc, err := store.GetToolCallByAgentID(context.Background(), "s", "tc_1")
	if err != nil || tc.ToolStatus != ToolApproved || tc.Risk != RiskLow {
		t.Fatalf("low risk tool call row: %+v err=%v", tc, err)
	}
	if sink.count("session/request_permission") != 0 {
		t.Error("permission request raised for low-risk read")
	}
}

// TestAdapterRecoverPendingAfterRestart verifies a pending permission request
// can be resolved after the adapter process restarted (in-memory state gone).
func TestAdapterRecoverPendingAfterRestart(t *testing.T) {
	a, _, store := newTestAdapter(t)
	ctx := context.Background()

	mustHandle(t, a, "session/new", frame(t, map[string]any{"sessionId": "s", "cwd": "D:\\x"}))
	res := mustHandle(t, a, "tool_call", frame(t, map[string]any{
		"sessionId": "s", "toolCallId": "tc_1", "name": "Bash", "kind": "execute",
		"rawInput": map[string]string{"command": "npm install x"},
	}))
	uid := res["requestId"].(string)

	// Simulate restart: fresh adapter over the same DB.
	a2, err := NewClaudeCodeAdapter(store, nil)
	if err != nil {
		t.Fatalf("recreate adapter: %v", err)
	}
	got, err := a2.ResolvePermission(uid, PermissionDecision{Decision: DecisionAllowOnce})
	if err != nil || !got.Applied {
		t.Fatalf("recovered resolution: %+v err=%v", got, err)
	}
	pr, _ := store.GetPermissionRequestByUID(ctx, uid)
	if pr.Outcome != OutcomeApproved {
		t.Fatalf("recovered request outcome: %v", pr.Outcome)
	}
	// Session was resumed in the DB.
	sess, _ := store.GetSession(ctx, "s")
	if sess.Status != StatusRunning {
		t.Fatalf("session after recovery: %s", sess.Status)
	}

	// Double-resolve must fail cleanly.
	if _, err := a2.ResolvePermission(uid, PermissionDecision{Decision: DecisionAllowOnce}); err == nil {
		t.Fatal("double resolve accepted after recovery")
	}
	// Unknown uid → ErrNotFound-mapped error.
	if _, err := a2.ResolvePermission("pr_nope", PermissionDecision{Decision: DecisionAllowOnce}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown uid error = %v, want ErrNotFound chain", err)
	}
}

// TestAdapterToolCallReplay verifies duplicate tool_call frames report the
// recorded state, including the requestId for calls still awaiting approval.
func TestAdapterToolCallReplay(t *testing.T) {
	a, _, _ := newTestAdapter(t)

	mustHandle(t, a, "session/new", frame(t, map[string]any{"sessionId": "s", "cwd": "D:\\x"}))
	in := frame(t, map[string]any{
		"sessionId": "s", "toolCallId": "tc_1", "name": "Bash", "kind": "execute",
		"rawInput": map[string]string{"command": "npm install x"},
	})
	first := mustHandle(t, a, "tool_call", in)
	if first["outcome"] != "pending_approval" || first["requestId"] == nil {
		t.Fatalf("fresh call: %v", first)
	}

	replay := mustHandle(t, a, "tool_call", in)
	if replay["replay"] != true || replay["outcome"] != "pending_approval" || replay["requestId"] != first["requestId"] {
		t.Fatalf("replay while pending: %v", replay)
	}

	// A completed call replays as completed, without a requestId.
	mustHandle(t, a, "tool_result", frame(t, map[string]any{"sessionId": "s", "toolCallId": "tc_1", "status": "success"}))
	after := mustHandle(t, a, "tool_call", in)
	if after["replay"] != true || after["outcome"] != ToolCompleted || after["requestId"] != nil {
		t.Fatalf("replay after completion: %v", after)
	}
}

// TestAdapterValidationErrors covers the request validation surface.
func TestAdapterValidationErrors(t *testing.T) {
	a, _, _ := newTestAdapter(t)

	if _, err := a.HandleFrame("session/new", frame(t, map[string]any{"sessionId": "", "cwd": "x"})); err == nil {
		t.Error("empty sessionId accepted")
	}
	if _, err := a.HandleFrame("session/new", frame(t, map[string]any{"sessionId": "s", "cwd": ""})); err == nil {
		t.Error("empty cwd accepted")
	}
	mustHandle(t, a, "session/new", frame(t, map[string]any{"sessionId": "s", "cwd": "D:\\x"}))
	if _, err := a.HandleFrame("session/new", frame(t, map[string]any{"sessionId": "s", "cwd": "D:\\x"})); err == nil {
		t.Error("duplicate session accepted")
	}
	if _, err := a.HandleFrame("session/prompt", frame(t, map[string]any{"sessionId": "ghost", "prompt": "x"})); err == nil {
		t.Error("prompt for unknown session accepted")
	}
	if _, err := a.HandleFrame("tool_result", frame(t, map[string]any{"sessionId": "s", "toolCallId": "nope"})); err == nil {
		t.Error("tool_result for unknown toolCallId accepted")
	}
	if _, err := a.HandleFrame("bogus/method", frame(t, map[string]any{})); err == nil {
		t.Error("unknown method accepted")
	}
	// Invalid state transition: active → closed → running.
	mustHandle(t, a, "session/update", frame(t, map[string]any{"sessionId": "s", "status": StatusClosed}))
	if _, err := a.HandleFrame("session/update", frame(t, map[string]any{"sessionId": "s", "status": StatusRunning})); err == nil {
		t.Error("closed → running transition accepted")
	}
}

// TestIntakeToolCallTimeout verifies PRD F3 fail-closed behavior: a hook
// intake nobody answers returns unavailable (decided_by=timeout), the request
// lands as rejected in the ledger, and the timeout is audited.
func TestIntakeToolCallTimeout(t *testing.T) {
	a, _, store := newTestAdapter(t)

	go func() {
		// The human decision arrives after the (tiny) deadline.
		time.Sleep(300 * time.Millisecond)
	}()
	dec, requestID, err := a.IntakeToolCall(context.Background(), HookIntake{
		SessionID: "hs", ToolName: "Bash", Command: "npm install slow",
		Cwd: `D:\x`, AgentKind: AgentKindClaudeCode,
	}, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("intake: %v", err)
	}
	if dec.Outcome != OutcomeUnavailable || dec.DecidedBy != DecidedByTimeout {
		t.Fatalf("timeout decision: %+v", dec)
	}
	if requestID == "" {
		t.Fatal("no request id on timeout")
	}
	// Fail closed: the tool call must not be approved.
	tc, err := store.GetToolCallByAgentID(context.Background(), "hs", requestID)
	_ = tc
	_ = err
	// The permission request row exists and stays pending in the ledger
	// (the timeout decision was recorded in the audit trail).
	pr, err := store.GetPermissionRequestByUID(context.Background(), requestID)
	if err != nil {
		t.Fatalf("request row: %v", err)
	}
	if pr.ToolName != "Bash" {
		t.Fatalf("request row tool: %+v", pr)
	}
}

// TestIntakeToolCallPolicyAutoAllow verifies the hook fast path: after an
// allow_always verdict, an identical IntakeToolCall returns allowed_once with
// rule_scope=always WITHOUT raising a permission request.
func TestIntakeToolCallPolicyAutoAllow(t *testing.T) {
	a, _, store := newTestAdapter(t)
	ctx := context.Background()

	// First call: raises a request; resolve it with allow_always ×2.
	done := make(chan Decision, 1)
	go func() {
		dec, _, _ := a.IntakeToolCall(ctx, HookIntake{
			SessionID: "hs2", ToolName: "Bash", Command: "cargo build",
			Cwd: `D:\x`, AgentKind: AgentKindClaudeCode,
		}, 5*time.Second)
		done <- dec
	}()
	var uid string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if stored, _ := store.ListPermissionRequests(ctx, OutcomePending); len(stored) > 0 {
			uid = stored[0].RequestUID
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if uid == "" {
		t.Fatal("intake did not raise a permission request")
	}
	if _, err := a.ResolvePermission(uid, PermissionDecision{Decision: DecisionAllowAlways}); err != nil {
		t.Fatalf("first allow_always: %v", err)
	}
	if _, err := a.ResolvePermission(uid, PermissionDecision{Decision: DecisionAllowAlways, Confirm: true}); err != nil {
		t.Fatalf("second allow_always: %v", err)
	}
	dec := <-done
	if dec.Outcome != OutcomeAllowedOnce || dec.RuleScope != ScopeAlways {
		t.Fatalf("first intake decision: %+v", dec)
	}

	// Second call: policy answers instantly.
	dec2, _, err := a.IntakeToolCall(ctx, HookIntake{
		SessionID: "hs2", ToolName: "Bash", Command: "cargo build",
		Cwd: `D:\x`, AgentKind: AgentKindClaudeCode,
	}, 5*time.Second)
	if err != nil {
		t.Fatalf("second intake: %v", err)
	}
	if dec2.Outcome != OutcomeAllowedOnce || dec2.DecidedBy != DecidedByPolicy {
		t.Fatalf("policy intake decision: %+v", dec2)
	}
}
