package server

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/agenttarmac/agenttarmac/internal/acpclient"
)

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, d time.Duration, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

// intakeResult carries one completed intake HTTP round trip.
type intakeResult struct {
	status int
	body   string
}

// postIntake POSTs one hook intake body.
func postIntake(t *testing.T, tsURL string, body string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest("POST", tsURL+"/api/hook/intake", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST intake: %v", err)
	}
	raw := make([]byte, 0, 4096)
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		raw = append(raw, buf[:n]...)
		if err != nil {
			break
		}
	}
	resp.Body.Close()
	return resp, string(raw)
}

// TestHookIntakeEndToEnd walks the PRD F3 hook path: a Claude Code
// PreToolUse hook parks on the Hub, the human allows-always (double
// confirmed), the hook receives the verdict, and a second identical call is
// auto-allowed by the policy rule (decided_by=policy) without prompting.
func TestHookIntakeEndToEnd(t *testing.T) {
	ts, _, _ := newTestDaemon(t)

	// 1. Hook intake for a medium-risk command: blocks in the background.
	done := make(chan intakeResult, 1)
	go func() {
		resp, body := postIntake(t, ts.URL, `{
			"session_id": "cc-hook-1", "tool_name": "Bash",
			"command": "cargo build --release",
			"args_digest": "{\"command\":\"cargo build --release\"}",
			"cwd": "D:\\work\\proj", "agent_kind": "claude_code"
		}`)
		done <- intakeResult{resp.StatusCode, body}
	}()

	// 2. The gate raised a permission request; wait for it in the ring.
	var uid string
	waitFor(t, 5*time.Second, func() bool {
		_, evs := getJSON(t, ts.URL+"/api/events", testToken)
		for _, raw := range evs["events"].([]any) {
			e := raw.(map[string]any)
			if e["type"] == "permission_request" {
				p := e["payload"].(map[string]any)
				if p["tool"] == "Bash" && p["rule_supported"] == true {
					uid = p["request_id"].(string)
					return true
				}
			}
		}
		return false
	}, "permission_request from hook intake")

	// The hook session auto-registered as attached (PRD F7).
	_, sessions := getJSON(t, ts.URL+"/api/sessions", testToken)
	var origin string
	for _, raw := range sessions["sessions"].([]any) {
		s := raw.(map[string]any)
		if s["id"] == "cc-hook-1" {
			origin = s["origin"].(string)
		}
	}
	if origin != "attached" {
		t.Fatalf("hook session origin = %q, want attached", origin)
	}

	// 3. Human verdict: allow_always with the double confirmation.
	resp, out := postJSON(t, ts.URL+"/api/permission-requests/"+uid+"/decision", testToken,
		map[string]any{"decision": "allow_always"})
	if resp.StatusCode != 200 || out["requiresConfirmation"] != true {
		t.Fatalf("first allow_always: %d %v", resp.StatusCode, out)
	}
	resp, out = postJSON(t, ts.URL+"/api/permission-requests/"+uid+"/decision", testToken,
		map[string]any{"decision": "allow_always", "confirm": true})
	if resp.StatusCode != 200 || out["applied"] != true {
		t.Fatalf("second allow_always: %d %v", resp.StatusCode, out)
	}

	// 4. The parked hook receives the verdict.
	select {
	case r := <-done:
		if r.status != 200 {
			t.Fatalf("intake status %d: %s", r.status, r.body)
		}
		if !strings.Contains(r.body, `"outcome":"allowed_once"`) || !strings.Contains(r.body, `"rule_scope":"always"`) {
			t.Fatalf("intake verdict body: %s", r.body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("intake never returned")
	}

	// 5. Second identical call: policy auto-allow, no round trip.
	resp, body := postIntake(t, ts.URL, `{
		"session_id": "cc-hook-1", "tool_name": "Bash",
		"command": "cargo build --release",
		"args_digest": "{}", "cwd": "D:\\work\\proj", "agent_kind": "claude_code"
	}`)
	if resp.StatusCode != 200 || !strings.Contains(body, `"outcome":"allowed_once"`) {
		t.Fatalf("policy intake: %d %s", resp.StatusCode, body)
	}

	// 6. The permission_resolved event carries decided_by=hub for the human
	// verdict (policy auto-allows don't re-ask, so this is the only one).
	_, evs := getJSON(t, ts.URL+"/api/events", testToken)
	var sawResolved bool
	for _, raw := range evs["events"].([]any) {
		e := raw.(map[string]any)
		if e["type"] == "permission_resolved" {
			p := e["payload"].(map[string]any)
			if p["request_id"] == uid && p["decided_by"] == acpclient.DecidedByHub && p["rule_scope"] == acpclient.ScopeAlways {
				sawResolved = true
			}
		}
	}
	if !sawResolved {
		t.Error("permission_resolved (hub, always) missing")
	}
}

// TestHookIntakeDenyWithNote verifies option ④: the hook receives
// outcome=rejected and the 复验 note travels into the audit trail.
func TestHookIntakeDenyWithNote(t *testing.T) {
	ts, _, store := newTestDaemon(t)

	done := make(chan intakeResult, 1)
	go func() {
		resp, body := postIntake(t, ts.URL, `{
			"session_id": "cc-hook-2", "tool_name": "Bash",
			"command": "npm publish --access public",
			"args_digest": "{}", "cwd": "D:\\x", "agent_kind": "claude_code"
		}`)
		done <- intakeResult{resp.StatusCode, body}
	}()

	var uid string
	waitFor(t, 5*time.Second, func() bool {
		_, evs := getJSON(t, ts.URL+"/api/events", testToken)
		for _, raw := range evs["events"].([]any) {
			e := raw.(map[string]any)
			if e["type"] == "permission_request" {
				p := e["payload"].(map[string]any)
				if p["tool"] == "Bash" && p["command"] == "npm publish --access public" {
					uid = p["request_id"].(string)
					return true
				}
			}
		}
		return false
	}, "permission_request for npm publish")

	resp, _ := postJSON(t, ts.URL+"/api/permission-requests/"+uid+"/decision", testToken,
		map[string]any{"decision": "reject_once", "note": "先走 CI 再发布"})
	if resp.StatusCode != 200 {
		t.Fatalf("reject decision: %d", resp.StatusCode)
	}

	select {
	case r := <-done:
		if !strings.Contains(r.body, `"outcome":"rejected"`) || !strings.Contains(r.body, "先走 CI") {
			t.Fatalf("intake verdict: %d %s", r.status, r.body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("intake never returned")
	}

	approvals, err := store.ListApprovals(t.Context(), 10)
	if err != nil {
		t.Fatalf("list approvals: %v", err)
	}
	found := false
	for _, a := range approvals {
		if a.Decision == "deny" && strings.Contains(a.Notes, "先走 CI") {
			found = true
		}
	}
	if !found {
		t.Error("复验 note missing from approval history")
	}
}
