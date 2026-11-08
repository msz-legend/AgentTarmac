package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agenttarmac/agenttarmac/internal/acpclient"
)

// TestWebConsoleServed verifies the embedded console is served at / with the
// BOOT injection (UI 稿 §11.2: the daemon hands the page its own token).
func TestWebConsoleServed(t *testing.T) {
	ts, _, _ := newTestDaemon(t)

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET / = %d", resp.StatusCode)
	}
	page := string(raw)
	for _, want := range []string{"AgentHub", "__TARMAC_BOOT__", "view-desk", "denyModal", "放行台"} {
		if !strings.Contains(page, want) {
			t.Errorf("console page missing %q", want)
		}
	}
	if !strings.Contains(page, `"token":"`) {
		t.Error("BOOT injection did not carry the API token")
	}
	if strings.Contains(page, "/*__BOOT__*/null") {
		t.Error("BOOT placeholder not replaced")
	}
}

// TestMemoryRevokeEndpoint verifies DELETE /api/approval-memory/{id}.
func TestMemoryRevokeEndpoint(t *testing.T) {
	ts, _, store := newTestDaemon(t)
	ctx := context.Background()

	if err := store.UpsertApprovalMemory(ctx, &acpclient.ApprovalMemory{
		AgentKind: acpclient.AgentKindClaudeCode, Tool: "Bash", CommandPattern: "npm install", Decision: "allow",
	}); err != nil {
		t.Fatalf("seed memory: %v", err)
	}
	mem, _ := store.GetApprovalMemory(ctx, acpclient.AgentKindClaudeCode, "Bash", "npm install")

	req, _ := http.NewRequest("DELETE", ts.URL+"/api/approval-memory/"+strconv.FormatInt(mem.ID, 10), nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("DELETE status = %d", resp.StatusCode)
	}
	if again, _ := store.GetApprovalMemory(ctx, acpclient.AgentKindClaudeCode, "Bash", "npm install"); again != nil {
		t.Error("memory survived revoke")
	}

	// Unknown id → 404.
	req2, _ := http.NewRequest("DELETE", ts.URL+"/api/approval-memory/999", nil)
	req2.Header.Set("Authorization", "Bearer "+testToken)
	resp2, _ := http.DefaultClient.Do(req2)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("DELETE unknown id = %d, want 404", resp2.StatusCode)
	}
}

// TestSessionExportEndpoint verifies the decision-G export download.
func TestSessionExportEndpoint(t *testing.T) {
	ts, _, _ := newTestDaemon(t)
	frameURL := ts.URL + "/acp/frame"

	postJSON(t, frameURL, testToken, map[string]any{
		"method": "session/new",
		"params": map[string]any{"sessionId": "exp-1", "cwd": "D:\\x"},
	})
	postJSON(t, frameURL, testToken, map[string]any{
		"method": "tool_call",
		"params": map[string]any{"sessionId": "exp-1", "toolCallId": "tc1", "name": "Read", "kind": "read",
			"rawInput": map[string]string{"file_path": "D:\\x\\a.go"}},
	})

	req, _ := http.NewRequest("GET", ts.URL+"/api/sessions/exp-1/export", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("export status = %d", resp.StatusCode)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Errorf("content-disposition = %q", cd)
	}
	var bundle struct {
		Session   map[string]any `json:"session"`
		ToolCalls []any          `json:"toolCalls"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&bundle); err != nil {
		t.Fatalf("decode bundle: %v", err)
	}
	if bundle.Session["id"] != "exp-1" || len(bundle.ToolCalls) != 1 {
		t.Fatalf("bundle: %+v", bundle)
	}

	// Unknown session → 404.
	req2, _ := http.NewRequest("GET", ts.URL+"/api/sessions/ghost/export", nil)
	req2.Header.Set("Authorization", "Bearer "+testToken)
	resp2, _ := http.DefaultClient.Do(req2)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("export ghost = %d, want 404", resp2.StatusCode)
	}
}

// TestNotificationsEndpoint verifies the notification outbox listing.
func TestNotificationsEndpoint(t *testing.T) {
	ts, _, store := newTestDaemon(t)
	if err := store.CreateSession(context.Background(), &acpclient.Session{ID: "s1", CWD: ".", Status: acpclient.StatusActive}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := store.InsertNotificationWithDedup(context.Background(), &acpclient.Notification{
		SessionID: "s1", NotificationType: "feishu", Title: "审批请求", TailHash: "h1",
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	resp, out := getJSON(t, ts.URL+"/api/notifications", testToken)
	if resp.StatusCode != 200 {
		t.Fatalf("notifications status %d", resp.StatusCode)
	}
	records := out["notifications"].([]any)
	if len(records) != 1 {
		t.Fatalf("notifications: %v", out)
	}
}

// TestFeishuNotifier verifies the webhook push, the signature parameters and
// the tail-hash de-duplication (decision F).
func TestFeishuNotifier(t *testing.T) {
	_, _, store := newTestDaemon(t)
	if err := store.CreateSession(context.Background(), &acpclient.Session{ID: "s1", CWD: ".", Status: acpclient.StatusActive}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	var mu sync.Mutex
	var calls []struct {
		url  string
		body map[string]any
	}
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		calls = append(calls, struct {
			url  string
			body map[string]any
		}{r.URL.String(), body})
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0})
	}))
	defer hook.Close()

	notifier := NewFeishuNotifier(hook.URL, "test-secret", store, nil)
	notifier.NotifyPermissionRequest("s1", "pr_abc", "Bash", "medium", "npm install", "审批记忆未命中")
	waitForCond(t, 5*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(calls) == 1
	}, "first webhook call")
	mu.Lock()
	url1 := calls[0].url
	mu.Unlock()
	if !strings.Contains(url1, "timestamp=") || !strings.Contains(url1, "sign=") {
		t.Errorf("signed URL missing params: %s", url1)
	}

	// Same request again → deduplicated, no second webhook call, one row.
	notifier.NotifyPermissionRequest("s1", "pr_abc", "Bash", "medium", "npm install", "审批记忆未命中")
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	n := len(calls)
	mu.Unlock()
	if n != 1 {
		t.Fatalf("webhook called %d times, want 1 (dedup broken)", n)
	}
	records, _ := store.ListNotifications(context.Background(), 10)
	if len(records) != 1 {
		t.Fatalf("notification rows = %d, want 1", len(records))
	}
	if records[0].NotificationType != "feishu" || records[0].TailHash == "" {
		t.Errorf("notification row: %+v", records[0])
	}
}

// TestNotifierHookOnPermissionFrame verifies the notifier hook fires on
// session/request_permission frames for HIGH-risk operations only
// (PRD D17/F9 policy: medium/low stay desktop-only).
func TestNotifierHookOnPermissionFrame(t *testing.T) {
	_, bridge, store := newTestDaemon(t)

	var mu sync.Mutex
	hits := 0
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0})
	}))
	defer hook.Close()

	// Rebuild the daemon's server notifier: the test daemon shares the store.
	srv2 := NewServer("127.0.0.1:0", bridge, store, testToken, nil)
	srv2.SetNotifier(NewFeishuNotifier(hook.URL, "", store, nil))

	// Exercise the hook directly to keep the test focused.
	srv2.EmitFrame("session/request_permission", json.RawMessage(`{
		"requestId":"pr_hook0","sessionId":"s1","name":"Bash","risk":"medium",
		"commandPattern":"npm install","reason":"test"
	}`))
	srv2.EmitFrame("session/request_permission", json.RawMessage(`{
		"requestId":"pr_hook1","sessionId":"s1","name":"Bash","risk":"high",
		"commandPattern":"rm -rf","reason":"test"
	}`))
	waitForCond(t, 5*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return hits >= 1
	}, "webhook hit from notifier hook")
	// Exactly one hit: the medium-risk frame must not reach the webhook.
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if hits != 1 {
		t.Fatalf("webhook hits = %d, want exactly 1 (high risk only per PRD D17)", hits)
	}
}

// TestAgentRequestBus verifies the Hub→agent request/response bus endpoints.
func TestAgentRequestBus(t *testing.T) {
	ts, _, _ := newTestDaemon(t)
	conn, err := dialWS(t, ts, nil, testToken)
	if err != nil {
		t.Fatalf("dial ws: %v", err)
	}
	readEvent := func(d time.Duration) map[string]any {
		t.Helper()
		conn.SetReadDeadline(time.Now().Add(d))
		_, raw, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("ws read: %v", err)
		}
		var evt map[string]any
		if err := json.Unmarshal(raw, &evt); err != nil {
			t.Fatalf("ws unmarshal: %v", err)
		}
		return evt
	}
	waitEvent := func(d time.Duration, pred func(map[string]any) bool, what string) map[string]any {
		t.Helper()
		deadline := time.Now().Add(d)
		for {
			if time.Now().After(deadline) {
				t.Fatalf("timeout waiting for ws event: %s", what)
			}
			evt := readEvent(time.Until(deadline))
			if pred(evt) {
				return evt
			}
		}
	}
	// hello confirms the subscription is live.
	if evt := readEvent(5 * time.Second); evt["type"] != "hello" {
		t.Fatalf("expected hello, got %v", evt)
	}

	resp, out := postJSON(t, ts.URL+"/api/agent-requests", testToken, map[string]any{
		"method": "session/new",
		"params": map[string]any{"cwd": "D:\\x"},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("agent-requests: %d %v", resp.StatusCode, out)
	}
	rid := out["requestId"].(string)

	evt := waitEvent(5*time.Second, func(e map[string]any) bool {
		return e["type"] == "agent.request" && e["requestId"] == rid
	}, "agent.request event")
	if evt["method"] != "session/new" {
		t.Fatalf("agent.request event: %v", evt)
	}

	resp, out = postJSON(t, ts.URL+"/api/agent-requests/"+rid+"/response", testToken, map[string]any{
		"result": map[string]any{"sessionId": "s-1"},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("agent response: %d %v", resp.StatusCode, out)
	}
	evt = waitEvent(5*time.Second, func(e map[string]any) bool {
		return e["type"] == "agent.response" && e["requestId"] == rid
	}, "agent.response event")
	result, _ := evt["result"].(map[string]any)
	if result["sessionId"] != "s-1" {
		t.Fatalf("agent.response event: %v", evt)
	}
}

// ------------------------------------------------------------------ helpers

func waitForCond(t *testing.T, d time.Duration, cond func() bool, what string) {
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
