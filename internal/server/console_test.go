package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/agenttarmac/agenttarmac/internal/acpclient"
	"github.com/agenttarmac/agenttarmac/internal/rules"
)

// postFrameJSON is a tiny helper: post one frame and return the HTTP response.
func postFrameJSON(t *testing.T, tsURL, token, method string, params any) *http.Response {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"method": method, "params": params})
	req, err := http.NewRequest("POST", tsURL+"/acp/frame", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST frame: %v", err)
	}
	return resp
}

// TestConsoleDataEndpoints walks the console contract (UI 稿 §12.3 adapted):
// /api/events ring, /api/agents, /api/rules alias over approval memory and
// /api/notify/status.
func TestConsoleDataEndpoints(t *testing.T) {
	ts, bridge, store := newTestDaemon(t)
	ctx := context.Background()

	// Seed: one session, one gated tool call (pending approval), one decision.
	if resp := postFrameJSON(t, ts.URL, testToken, "session/new",
		map[string]any{"sessionId": "c1", "cwd": "D:\\x", "configOptions": map[string]string{}}); resp.StatusCode != 200 {
		t.Fatalf("session/new = %d", resp.StatusCode)
	}
	resp := postFrameJSON(t, ts.URL, testToken, "tool_call",
		map[string]any{"sessionId": "c1", "toolCallId": "tc1", "name": "Bash", "kind": "execute",
			"rawInput": map[string]string{"command": "npm install x"}})
	if resp.StatusCode != 200 {
		t.Fatalf("tool_call = %d", resp.StatusCode)
	}
	var frameResp struct {
		Result struct {
			RequestID string `json:"requestId"`
		} `json:"result"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&frameResp)
	resp.Body.Close()
	uid := frameResp.Result.RequestID
	if uid == "" {
		t.Fatal("tool_call returned no requestId")
	}

	// /api/agents lists the registered kinds (the test daemon registers
	// claude_code only; the real daemon also registers codex).
	_, agents := getJSON(t, ts.URL+"/api/agents", testToken)
	list := agents["agents"].([]any)
	if len(list) != 1 {
		t.Fatalf("agents = %v", agents)
	}
	if a0 := list[0].(map[string]any); a0["kind"] != acpclient.AgentKindClaudeCode || a0["instance_id"] != "hub" {
		t.Fatalf("agent shape: %v", a0)
	}

	// /api/events carries the permission_request and the session status.
	_, events := getJSON(t, ts.URL+"/api/events", testToken)
	evs := events["events"].([]any)
	if len(evs) == 0 {
		t.Fatal("event ring empty")
	}
	var sawPermReq bool
	for _, raw := range evs {
		e := raw.(map[string]any)
		if e["type"] == "permission_request" && e["payload"].(map[string]any)["tool"] == "Bash" {
			sawPermReq = true
		}
	}
	if !sawPermReq {
		t.Error("permission_request event missing from ring")
	}

	// /api/rules reflects the policy rule table (PRD F3); deleting via
	// /api/rules works.
	if err := store.AddRule(ctx, rules.Rule{
		ID: "rule_test1", Tool: "Bash", ArgPattern: "npm install", Scope: "always",
		Effect: rules.EffectAllow, CreatedFrom: "pr_test", AgentKind: acpclient.AgentKindClaudeCode,
	}); err != nil {
		t.Fatalf("seed rule: %v", err)
	}
	_, rules := getJSON(t, ts.URL+"/api/rules", testToken)
	rl := rules["rules"].([]any)
	if len(rl) != 1 {
		t.Fatalf("rules = %v", rules)
	}
	r0 := rl[0].(map[string]any)
	if r0["tool"] != "Bash" || r0["arg_pattern"] != "npm install" || r0["scope"] != "allow · always" {
		t.Fatalf("rule mapping wrong: %v", r0)
	}
	delReq, err := http.NewRequest("DELETE", ts.URL+"/api/rules/"+r0["id"].(string), nil)
	if err != nil {
		t.Fatalf("new delete: %v", err)
	}
	delReq.Header.Set("Authorization", "Bearer "+testToken)
	del, err := http.DefaultClient.Do(delReq)
	if err != nil {
		t.Fatalf("DELETE /api/rules: %v", err)
	}
	del.Body.Close()
	if del.StatusCode != 200 {
		t.Fatalf("DELETE /api/rules = %d", del.StatusCode)
	}
	if remaining, _ := store.ListRules(ctx); len(remaining) != 0 {
		t.Error("rule deletion did not clear the rules table")
	}

	// /api/notify/status reports channels; /api/notify/test is 501 without a
	// webhook and consistent when set.
	_, st := getJSON(t, ts.URL+"/api/notify/status", testToken)
	if _, ok := st["feishu"]; !ok {
		t.Fatalf("notify status shape: %v", st)
	}
	respTest, _ := postJSON(t, ts.URL+"/api/notify/test", testToken, map[string]any{})
	if respTest.StatusCode != http.StatusNotImplemented {
		t.Fatalf("notify test without webhook = %d, want 501", respTest.StatusCode)
	}

	// Resolving the request records permission_resolved with spec vocabulary.
	if _, err := bridge.ResolvePermission("", uid, acpclient.PermissionDecision{Decision: acpclient.DecisionAllowOnce}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	_, events = getJSON(t, ts.URL+"/api/events", testToken)
	evs = events["events"].([]any)
	var sawResolved bool
	for _, raw := range evs {
		e := raw.(map[string]any)
		p, _ := e["payload"].(map[string]any)
		if e["type"] == "permission_resolved" && p["request_id"] == uid {
			sawResolved = true
			if p["outcome"] != "allowed_once" || p["decided_by"] != "hub" || p["rule_scope"] != "none" {
				t.Errorf("permission_resolved vocabulary: %v", p)
			}
		}
	}
	if !sawResolved {
		t.Error("permission_resolved event missing from ring")
	}
}

// TestConsoleDecisionDoubleConfirm verifies the console's allowAlways path
// against the daemon's two-step confirmation (decision B).
func TestConsoleDecisionDoubleConfirm(t *testing.T) {
	ts, _, _ := newTestDaemon(t)

	if resp := postFrameJSON(t, ts.URL, testToken, "session/new",
		map[string]any{"sessionId": "c2", "cwd": "D:\\x"}); resp.StatusCode != 200 {
		t.Fatalf("session/new = %d", resp.StatusCode)
	}
	resp := postFrameJSON(t, ts.URL, testToken, "tool_call",
		map[string]any{"sessionId": "c2", "toolCallId": "tc1", "name": "Bash", "kind": "execute",
			"rawInput": map[string]string{"command": "cargo build"}})
	var frameResp struct {
		Result struct {
			RequestID string `json:"requestId"`
		} `json:"result"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&frameResp)
	resp.Body.Close()
	uid := frameResp.Result.RequestID

	// First click: requires confirmation, request stays pending.
	r1, out1 := postJSON(t, ts.URL+"/api/permission-requests/"+uid+"/decision", testToken,
		map[string]any{"decision": "allow_always"})
	if r1.StatusCode != 200 || out1["requiresConfirmation"] != true {
		t.Fatalf("first allow_always: %d %v", r1.StatusCode, out1)
	}
	// Second click with confirm=true: applied.
	r2, out2 := postJSON(t, ts.URL+"/api/permission-requests/"+uid+"/decision", testToken,
		map[string]any{"decision": "allow_always", "confirm": true})
	if r2.StatusCode != 200 || out2["applied"] != true {
		t.Fatalf("second allow_always: %d %v", r2.StatusCode, out2)
	}
}

// TestConsoleIndexInTestDaemonMakesSureAuthStillApplies: the console page is
// open, but every data API still requires the Bearer token.
func TestConsoleIndexInTestDaemonMakesSureAuthStillApplies(t *testing.T) {
	ts, _, _ := newTestDaemon(t)
	for _, path := range []string{"/api/events", "/api/agents", "/api/rules", "/api/notify/status"} {
		resp, _ := getJSON(t, ts.URL+path, "")
		_ = resp
	}
	// getJSON never returns status; do a raw check here.
	for _, path := range []string{"/api/events", "/api/agents", "/api/rules"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET %s without token = %d, want 401", path, resp.StatusCode)
		}
	}
}
