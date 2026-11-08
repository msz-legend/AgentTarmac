package acpclient

import (
	"context"
	"encoding/json"
	"testing"
)

// TestCodexAdapterKindAndGate verifies the Codex adapter registers under its
// own kind, classifies Codex-style tool names and namespaces approval memory
// by agent_kind (a Codex verdict never applies to Claude Code).
func TestCodexAdapterKindAndGate(t *testing.T) {
	db, err := InitDB(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	store := NewStore(db)
	sink := &recordingSink{}

	codex, err := NewCodexAdapter(store, sink)
	if err != nil {
		t.Fatalf("NewCodexAdapter: %v", err)
	}
	if codex.AgentKind() != AgentKindCodex {
		t.Fatalf("agent kind = %q, want %q", codex.AgentKind(), AgentKindCodex)
	}

	mustHandle(t, codex, "session/new", frame(t, map[string]any{"sessionId": "cx-1", "cwd": "D:\\x"}))

	// Codex-style write tool → medium risk → pending approval.
	res := mustHandle(t, codex, "tool_call", frame(t, map[string]any{
		"sessionId": "cx-1", "toolCallId": "t1", "name": "apply_patch", "kind": "edit",
		"rawInput": map[string]string{"patch": "*** Begin Patch"},
	}))
	if res["outcome"] != "pending_approval" || res["risk"] != RiskMedium {
		t.Fatalf("apply_patch gate: %v", res)
	}
	uid := res["requestId"].(string)
	// Permanent verdict requires the strict two-step confirmation (decision B).
	res1, err := codex.ResolvePermission(uid, PermissionDecision{Decision: DecisionAllowAlways})
	if err != nil || res1.Applied || !res1.RequiresConfirmation {
		t.Fatalf("first allow_always: %+v err=%v", res1, err)
	}
	res2, err := codex.ResolvePermission(uid, PermissionDecision{Decision: DecisionAllowAlways, Confirm: true})
	if err != nil || !res2.Applied || res2.Outcome != OutcomeApproved {
		t.Fatalf("double confirm: %+v err=%v", res2, err)
	}

	// The permanent verdict becomes a policy rule (PRD F3); rules match on
	// tool + arg pattern per the frozen event model (no kind isolation —
	// tool names are agent-specific), with agent_kind recorded for display.
	storedRules, err := store.ListRules(context.Background())
	if err != nil || len(storedRules) != 1 {
		t.Fatalf("rules after double confirm: %+v err=%v", storedRules, err)
	}
	if r := storedRules[0]; r.Tool != "apply_patch" || r.ArgPattern != "" || r.Scope != "always" || r.AgentKind != AgentKindCodex {
		// apply_patch carried no command/file_path, so the rule is a bare-tool
		// rule (empty pattern = matches any argument).
		t.Fatalf("rule shape: %+v", r)
	}
	// No legacy allow memory is written anymore.
	if mem, _ := store.GetApprovalMemory(context.Background(), AgentKindCodex, "apply_patch", "*"); mem != nil {
		t.Fatalf("allow memory should be replaced by rules: %+v", mem)
	}

	// Codex shell commands classify through the same pipeline.
	res = mustHandle(t, codex, "tool_call", frame(t, map[string]any{
		"sessionId": "cx-1", "toolCallId": "t2", "name": "shell", "kind": "execute",
		"rawInput": map[string]string{"command": "git status"},
	}))
	if res["outcome"] != "auto_allowed" || res["risk"] != RiskLow {
		t.Fatalf("codex shell read-only: %v", res)
	}
}

// TestInboundRequestPermission verifies the adapter handles real ACP agents
// asking for permission: the request routes into the approval pipeline and the
// result carries the Hub request id plus the agent's option list.
func TestInboundRequestPermission(t *testing.T) {
	a, sink, store := newTestAdapter(t)
	ctx := context.Background()

	mustHandle(t, a, "session/new", frame(t, map[string]any{"sessionId": "s", "cwd": "D:\\x"}))

	res := mustHandle(t, a, "session/request_permission", frame(t, map[string]any{
		"sessionId": "s",
		"toolCall": map[string]any{
			"toolCallId": "call_1",
			"title":      "shell",
			"kind":       "execute",
			"rawInput":   map[string]string{"command": "cargo build --release"},
		},
		"options": []map[string]string{
			{"optionId": "proceed_once", "name": "Allow once", "kind": "allow_once"},
			{"optionId": "cancel", "name": "Reject", "kind": "reject_once"},
		},
	}))
	if res["requestId"] == nil {
		t.Fatalf("inbound request_permission result: %v", res)
	}
	opts := res["options"].([]any)
	if len(opts) != 2 || opts[0].(map[string]any)["optionId"] != "proceed_once" {
		t.Fatalf("agent options not echoed: %v", opts)
	}

	// Session paused for approval; request recorded.
	sess, _ := store.GetSession(ctx, "s")
	if sess.Status != StatusAwaitingApproval {
		t.Fatalf("session status: %s", sess.Status)
	}
	pr, err := store.GetPermissionRequestByUID(ctx, res["requestId"].(string))
	if err != nil || pr.ToolName != "shell" || pr.CommandPattern != "cargo build" {
		t.Fatalf("permission request row: %+v err=%v", pr, err)
	}
	if sink.last("session/request_permission") == nil {
		t.Fatal("outbound frame missing")
	}

	// Resolution resumes the session and rejects the call.
	got, err := a.ResolvePermission(pr.RequestUID, PermissionDecision{Decision: DecisionRejectOnce})
	if err != nil || !got.Applied || got.Decision != DecisionRejectOnce {
		t.Fatalf("resolution: %+v err=%v", got, err)
	}
	sess, _ = store.GetSession(ctx, "s")
	if sess.Status != StatusRunning {
		t.Fatalf("session after resolution: %s", sess.Status)
	}
	tc, _ := store.GetToolCallByAgentID(ctx, "s", "call_1")
	if tc.ToolStatus != ToolRejected {
		t.Fatalf("tool call status: %s", tc.ToolStatus)
	}

	// Unknown session → error; closed session → error.
	if _, err := a.HandleFrame("session/request_permission", frame(t, map[string]any{"sessionId": "ghost"})); err == nil {
		t.Fatal("unknown session accepted")
	}
}

// TestPermissionResolutionCarriesDecision verifies the resolution struct now
// carries the normalized decision (needed by the stdio connector to map onto
// agent option ids).
func TestPermissionResolutionCarriesDecision(t *testing.T) {
	a, _, _ := newTestAdapter(t)
	mustHandle(t, a, "session/new", frame(t, map[string]any{"sessionId": "s", "cwd": "D:\\x"}))
	res := mustHandle(t, a, "tool_call", frame(t, map[string]any{
		"sessionId": "s", "toolCallId": "t1", "name": "Bash", "kind": "execute",
		"rawInput": map[string]string{"command": "npm install x"},
	}))
	got, err := a.ResolvePermission(res["requestId"].(string), PermissionDecision{Decision: "allow-session"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Decision != DecisionAllowSession {
		t.Fatalf("decision = %q, want %q", got.Decision, DecisionAllowSession)
	}
}

var _ = json.Marshal
