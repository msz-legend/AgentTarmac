package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/agenttarmac/agenttarmac/internal/acpclient"
)

const testToken = "test-token-0123456789abcdef"

// newTestDaemon wires a full daemon (bridge + adapter + HTTP server) over a
// temp SQLite file and returns the HTTP test server.
func newTestDaemon(t *testing.T) (*httptest.Server, *Bridge, *acpclient.Store) {
	t.Helper()
	db, err := acpclient.InitDB(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	store := acpclient.NewStore(db)

	bridge := NewBridge(nil)
	srv := NewServer("127.0.0.1:0", bridge, store, testToken, nil)
	adapter, err := acpclient.NewClaudeCodeAdapter(store, srv)
	if err != nil {
		t.Fatalf("NewClaudeCodeAdapter: %v", err)
	}
	if err := bridge.Register(adapter); err != nil {
		t.Fatalf("Register: %v", err)
	}

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, bridge, store
}

func postJSON(t *testing.T, url, token string, body any) (*http.Response, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req, err := http.NewRequest("POST", url, bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp, out
}

func getJSON(t *testing.T, url, token string) (*http.Response, map[string]any) {
	t.Helper()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp, out
}

// TestHealthzNoAuth verifies the liveness probe is open.
func TestHealthzNoAuth(t *testing.T) {
	ts, bridge, _ := newTestDaemon(t)
	if kinds := bridge.AgentKinds(); len(kinds) != 1 || kinds[0] != acpclient.AgentKindClaudeCode {
		t.Fatalf("agent kinds: %v", kinds)
	}
	resp, out := getJSON(t, ts.URL+"/healthz", "")
	if resp.StatusCode != 200 {
		t.Fatalf("healthz status %d", resp.StatusCode)
	}
	if out["status"] != "ok" {
		t.Fatalf("healthz body: %v", out)
	}
}

// TestAuthBoundary verifies Bearer enforcement on every protected route.
func TestAuthBoundary(t *testing.T) {
	ts, _, _ := newTestDaemon(t)

	for _, path := range []string{"/api/sessions", "/api/permission-requests", "/api/approvals", "/api/approval-memory"} {
		resp, _ := getJSON(t, ts.URL+path, "")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET %s without token = %d, want 401", path, resp.StatusCode)
		}
		resp, _ = getJSON(t, ts.URL+path, "wrong-token")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET %s with wrong token = %d, want 401", path, resp.StatusCode)
		}
	}

	// POST /acp/frame: no token / wrong token → 401.
	resp, _ := postJSON(t, ts.URL+"/acp/frame", "", map[string]any{"method": "session/new"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("POST /acp/frame without token = %d, want 401", resp.StatusCode)
	}
	resp, _ = postJSON(t, ts.URL+"/acp/frame", "wrong-token", map[string]any{"method": "session/new"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("POST /acp/frame with wrong token = %d, want 401", resp.StatusCode)
	}

	// POST with the right token passes auth (may 400 on the frame itself).
	resp, out := postJSON(t, ts.URL+"/acp/frame", testToken, map[string]any{"method": "session/new", "params": map[string]string{}})
	if resp.StatusCode == http.StatusUnauthorized {
		t.Fatalf("valid token rejected: %v", out)
	}

	// WS auth: bad token rejected at upgrade, good token accepted.
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer wrong")
	if _, err := dialWS(t, ts, hdr, ""); err == nil {
		t.Error("WS with wrong token accepted")
	}
	if _, err := dialWS(t, ts, nil, testToken); err != nil {
		t.Errorf("WS with query token rejected: %v", err)
	}
}

func dialWS(t *testing.T, ts *httptest.Server, hdr http.Header, queryToken string) (*websocket.Conn, error) {
	t.Helper()
	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	if queryToken != "" {
		url += "?token=" + queryToken
	}
	conn, _, err := websocket.DefaultDialer.Dial(url, hdr)
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { conn.Close() })
	return conn, nil
}

// readEvent reads one WS event with a deadline.
func readEvent(t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, raw, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read ws event: %v", err)
	}
	var evt map[string]any
	if err := json.Unmarshal(raw, &evt); err != nil {
		t.Fatalf("unmarshal ws event: %v", err)
	}
	return evt
}

// TestDaemonE2EPermissionFlow runs the full hosted-session loop over HTTP:
// session/new → session/prompt → tool_call (pending) → decision API →
// tool_result → completion, with a WS listener observing everything.
func TestDaemonE2EPermissionFlow(t *testing.T) {
	ts, _, _ := newTestDaemon(t)

	// WS listener subscribes before anything happens.
	conn, err := dialWS(t, ts, nil, testToken)
	if err != nil {
		t.Fatalf("dial ws: %v", err)
	}
	// Wait for the hello event so the subscription is guaranteed live.
	evt := readEvent(t, conn)
	if evt["type"] != "hello" {
		t.Fatalf("expected hello event, got %v", evt)
	}

	frameURL := ts.URL + "/acp/frame"
	send := func(method string, params map[string]any) map[string]any {
		resp, out := postJSON(t, frameURL, testToken, map[string]any{"method": method, "params": params})
		if resp.StatusCode != 200 {
			t.Fatalf("%s → %d: %v", method, resp.StatusCode, out)
		}
		result := out["result"].(map[string]any)
		return result
	}

	// 1-2. session/new + prompt
	send("session/new", map[string]any{"sessionId": "e2e-1", "cwd": "D:\\work"})
	send("session/prompt", map[string]any{"sessionId": "e2e-1", "prompt": "run the build"})

	// 3. tool_call → pending approval
	res := send("tool_call", map[string]any{
		"sessionId": "e2e-1", "toolCallId": "tc_1", "name": "Bash", "kind": "execute",
		"rawInput": map[string]string{"command": "npm run build"},
	})
	uid := res["requestId"].(string)

	// 4. Pending request visible via API.
	resp, out := getJSON(t, ts.URL+"/api/permission-requests?outcome=pending", testToken)
	if resp.StatusCode != 200 {
		t.Fatalf("permission-requests status %d: %v", resp.StatusCode, out)
	}
	reqs := out["permissionRequests"].([]any)
	if len(reqs) != 1 || reqs[0].(map[string]any)["requestId"] != uid {
		t.Fatalf("pending requests: %v", out)
	}

	// 5. Decision via API (allow_session).
	dURL := ts.URL + "/api/permission-requests/" + uid + "/decision"
	resp, out = postJSON(t, dURL, testToken, map[string]any{"decision": "allow_session"})
	if resp.StatusCode != 200 {
		t.Fatalf("decision status %d: %v", resp.StatusCode, out)
	}
	if out["applied"] != true || out["outcome"] != "approved" {
		t.Fatalf("decision result: %v", out)
	}

	// Double decision → 409.
	resp, _ = postJSON(t, dURL, testToken, map[string]any{"decision": "allow_once"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("double decision status = %d, want 409", resp.StatusCode)
	}

	// 6-7. tool_result + completion.
	send("tool_result", map[string]any{"sessionId": "e2e-1", "toolCallId": "tc_1", "status": "success", "content": "built"})
	send("session/update", map[string]any{"sessionId": "e2e-1", "isComplete": true})

	// 8. Sessions API reflects the final state.
	resp, out = getJSON(t, ts.URL+"/api/sessions", testToken)
	sessions := out["sessions"].([]any)
	if len(sessions) != 1 {
		t.Fatalf("sessions: %v", out)
	}
	s0 := sessions[0].(map[string]any)
	if s0["status"] != acpclient.StatusCompleted || s0["isTerminal"] != true {
		t.Fatalf("final session state: %v", s0)
	}

	// 9. The WS listener must have seen frame.in / frame.out /
	// permission.resolved events.
	saw := map[string]bool{}
	deadline := time.After(5 * time.Second)
	for !(saw["frame.in"] && saw["frame.out"] && saw["permission.resolved"]) {
		select {
		case <-deadline:
			t.Fatalf("missing ws events; seen: %v", saw)
		default:
		}
		evt := readEvent(t, conn)
		if typ, ok := evt["type"].(string); ok {
			saw[typ] = true
		}
	}
}

// TestFrameErrorMapping verifies adapter errors surface as HTTP statuses.
func TestFrameErrorMapping(t *testing.T) {
	ts, _, _ := newTestDaemon(t)
	frameURL := ts.URL + "/acp/frame"

	// Unknown method → 400.
	resp, _ := postJSON(t, frameURL, testToken, map[string]any{"method": "nope"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown method = %d, want 400", resp.StatusCode)
	}
	// Unknown agent kind → 404.
	resp, _ = postJSON(t, frameURL, testToken, map[string]any{"agentKind": "ghost", "method": "session/new"})
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown agent kind = %d, want 404", resp.StatusCode)
	}
	// Unknown session → 404 (ErrNotFound chain from the adapter).
	resp, out := postJSON(t, frameURL, testToken, map[string]any{"method": "session/prompt", "params": map[string]any{"sessionId": "ghost", "prompt": "x"}})
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(out["error"].(string), "unknown session") {
		t.Errorf("unknown session = %d %v", resp.StatusCode, out)
	}
}

// fakeAdapter is a second adapter kind used to exercise the registry.
type fakeAdapter struct{}

func (fakeAdapter) AgentKind() string { return "fake" }
func (fakeAdapter) HandleFrame(method string, params json.RawMessage) (json.RawMessage, error) {
	return params, nil
}
func (fakeAdapter) ResolvePermission(string, acpclient.PermissionDecision) (acpclient.PermissionResolution, error) {
	return acpclient.PermissionResolution{}, nil
}
func (fakeAdapter) IntakeToolCall(context.Context, acpclient.HookIntake, time.Duration) (acpclient.Decision, string, error) {
	return acpclient.TimedOutDecision(), "", nil
}
func (fakeAdapter) Initialize(context.Context) error { return nil }

// TestBridgeRegistry verifies registration rules and fan-out.
func TestBridgeRegistry(t *testing.T) {
	bridge := NewBridge(nil)
	if err := bridge.Register(fakeAdapter{}); err != nil {
		t.Fatalf("register fake: %v", err)
	}
	if err := bridge.Register(fakeAdapter{}); err == nil {
		t.Error("duplicate registration accepted")
	}
	if kinds := bridge.AgentKinds(); len(kinds) != 1 || kinds[0] != "fake" {
		t.Fatalf("agent kinds: %v", kinds)
	}

	// Dispatch routes by kind.
	if _, err := bridge.DispatchFrame(InboundFrame{AgentKind: "fake", Method: "echo", Params: json.RawMessage(`{"x":1}`)}); err != nil {
		t.Fatalf("dispatch fake: %v", err)
	}
	if _, err := bridge.DispatchFrame(InboundFrame{AgentKind: "missing", Method: "x"}); err == nil ||
		!strings.Contains(err.Error(), "no adapter registered") {
		t.Fatalf("missing kind error: %v", err)
	}

	// Subscribers receive broadcast events.
	ch := bridge.Subscribe()
	defer bridge.Unsubscribe(ch)
	bridge.DispatchFrame(InboundFrame{AgentKind: "fake", Method: "echo", Params: json.RawMessage(`{}`)})
	select {
	case raw := <-ch:
		var evt map[string]any
		if err := json.Unmarshal(raw, &evt); err != nil || evt["type"] != "frame.in" {
			t.Fatalf("broadcast event: %s", raw)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no broadcast received")
	}
}
