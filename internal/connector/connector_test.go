package connector_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agenttarmac/agenttarmac/internal/acpclient"
	"github.com/agenttarmac/agenttarmac/internal/connector"
	"github.com/agenttarmac/agenttarmac/internal/server"
)

const testToken = "connector-test-token"

// newTestDaemon wires a daemon with the claude_code adapter over a temp DB.
func newTestDaemon(t *testing.T) *httptest.Server {
	ts, _ := newTestDaemonKinds(t, acpclient.AgentKindClaudeCode)
	return ts
}

// newTestDaemonKinds registers one BaseAdapter per kind (shared behavior,
// kind-scoped memory — same as the production daemon). Returns the server
// (multi-kind: the fs gate runs on the opencode adapter).
func newTestDaemonKinds(t *testing.T, kinds ...string) (*httptest.Server, *acpclient.Store) {
	t.Helper()
	db, err := acpclient.InitDB(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	store := acpclient.NewStore(db)
	bridge := server.NewBridge(nil)
	bridge.SetStore(store)
	srv := server.NewServer("127.0.0.1:0", bridge, store, testToken, nil)
	for _, kind := range kinds {
		adapter, err := acpclient.NewBaseAdapter(kind, store, srv)
		if err != nil {
			t.Fatalf("NewBaseAdapter(%s): %v", kind, err)
		}
		if err := bridge.Register(adapter); err != nil {
			t.Fatalf("Register(%s): %v", kind, err)
		}
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, store
}

// fakeAgent is an in-process ACP agent speaking JSON-RPC over pipes. It is the
// SINGLE reader of its stdin; the test observes what it received via the
// received channel instead of touching the pipe directly.
type fakeAgent struct {
	in       io.Reader
	out      io.Writer
	errCh    chan error
	received chan string // method of each inbound request/notification ("" for responses)
}

func newFakeAgent(in io.Reader, out io.Writer) *fakeAgent {
	return &fakeAgent{
		in:       in,
		out:      out,
		errCh:    make(chan error, 8),
		received: make(chan string, 32),
	}
}

func (f *fakeAgent) fail(format string, args ...any) {
	select {
	case f.errCh <- fmt.Errorf(format, args...):
	default:
	}
}

func (f *fakeAgent) note(method string) {
	select {
	case f.received <- method:
	default:
	}
}

func (f *fakeAgent) write(v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = f.out.Write(append(raw, '\n'))
	return err
}

// readLine is only called from the run goroutine (single reader).
func (f *fakeAgent) readLine(timeout time.Duration) (map[string]any, error) {
	type result struct {
		line map[string]any
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		var raw json.RawMessage
		if err := json.NewDecoder(f.in).Decode(&raw); err != nil {
			ch <- result{err: err}
			return
		}
		var line map[string]any
		if err := json.Unmarshal(raw, &line); err != nil {
			ch <- result{err: fmt.Errorf("non-object message %s: %w", truncateStr(string(raw), 200), err)}
			return
		}
		ch <- result{line: line}
	}()
	select {
	case r := <-ch:
		return r.line, r.err
	case <-time.After(timeout):
		return nil, fmt.Errorf("fake agent read timed out after %s", timeout)
	}
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// run executes the agent script: answer initialize + session/new; on
// session/prompt, ask for permission like a real ACP agent, verify the Hub's
// decision mapping, then complete the turn.
func (f *fakeAgent) run() {
	const readTimeout = 15 * time.Second
	for {
		msg, err := f.readLine(readTimeout)
		if err != nil {
			// Pipe closed at teardown is expected; report anything else.
			if !strings.Contains(err.Error(), "io: read/write on closed pipe") &&
				!strings.Contains(err.Error(), "file already closed") {
				f.fail("%v", err)
			}
			return
		}
		method, _ := msg["method"].(string)
		if method == "" {
			f.note("") // a response line (e.g. to our permission request)
		} else {
			f.note(method)
		}
		switch method {
		case "initialize":
			_ = f.write(map[string]any{
				"jsonrpc": "2.0", "id": msg["id"],
				"result": map[string]any{"protocolVersion": "acp/v1"},
			})
		case "session/new":
			_ = f.write(map[string]any{
				"jsonrpc": "2.0", "id": msg["id"],
				"result": map[string]any{"sessionId": "fake-sess-1"},
			})
		case "session/prompt":
			// 1. Ask for permission like a real agent bridge would.
			_ = f.write(map[string]any{
				"jsonrpc": "2.0", "id": "agent-perm-1",
				"method": "session/request_permission",
				"params": map[string]any{
					"sessionId": "fake-sess-1",
					"toolCall": map[string]any{
						"toolCallId": "tc-perm-1", "title": "shell", "kind": "execute",
						"rawInput": map[string]string{"command": "cargo build --release"},
					},
					"options": []map[string]string{
						{"optionId": "proceed_once", "name": "Allow once", "kind": "allow_once"},
						{"optionId": "cancel", "name": "Reject", "kind": "reject_once"},
					},
				},
			})
			// 2. The next line must be the permission resolution.
			permResp, err := f.readLine(readTimeout)
			if err != nil {
				f.fail("permission response: %v", err)
				return
			}
			f.note("")
			if permResp["id"] != "agent-perm-1" {
				f.fail("expected permission response id agent-perm-1, got %v", permResp["id"])
				return
			}
			result, _ := permResp["result"].(map[string]any)
			outcome, _ := result["outcome"].(map[string]any)
			if outcome["outcome"] != "selected" || outcome["optionId"] != "proceed_once" {
				f.fail("permission mapping wrong: %v", outcome)
				return
			}
			// 3. Report activity and complete the turn.
			_ = f.write(map[string]any{
				"jsonrpc": "2.0",
				"method":  "session/update",
				"params":  map[string]any{"sessionId": "fake-sess-1", "status": "running"},
			})
			_ = f.write(map[string]any{
				"jsonrpc": "2.0", "id": msg["id"],
				"result": map[string]any{"stopReason": "end_turn"},
			})
		default:
			// Unknown forwarded requests get a plain error response.
			if id, ok := msg["id"]; ok {
				_ = f.write(map[string]any{
					"jsonrpc": "2.0", "id": id,
					"error": map[string]any{"code": -32601, "message": "fake agent: unsupported " + method},
				})
			}
		}
	}
}

// drainFakeAgentErrors surfaces fake-agent assertion failures to the test.
func drainFakeAgentErrors(t *testing.T, f *fakeAgent) {
	t.Helper()
	for {
		select {
		case err := <-f.errCh:
			t.Errorf("fake agent: %v", err)
		default:
			return
		}
	}
}

// waitForReceived waits until the fake agent received the given method.
func waitForReceived(t *testing.T, f *fakeAgent, method string) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case got := <-f.received:
			if got == method {
				return
			}
		case <-deadline:
			t.Fatalf("fake agent never received %q", method)
		}
	}
}

// TestConnectorEndToEnd walks the full loop: Hub→agent session/new (with Hub
// registration from the agent's response), prompt forwarding, agent-initiated
// permission request mapped to the agent's option id, and turn completion.
func TestConnectorEndToEnd(t *testing.T) {
	ts := newTestDaemon(t)
	ws := testWS(t, ts)

	agentOutR, agentOutW := io.Pipe()
	agentInR, agentInW := io.Pipe()
	fake := newFakeAgent(agentInR, agentOutW)
	go fake.run()

	cfg := connector.Config{
		DaemonURL: ts.URL,
		Token:     testToken,
		AgentKind: acpclient.AgentKindClaudeCode,
		Log:       log.New(os.Stderr, "connector ", log.Ltime|log.Lmicroseconds),
	}
	conn := connector.New(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go func() { _ = conn.Run(ctx, agentInW, agentOutR) }()

	select {
	case <-conn.Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("connector ws never became ready")
	}

	// 1. Hub → agent: session/new.
	rq1 := postAgentRequest(t, ts, map[string]any{
		"method": "session/new",
		"params": map[string]any{"cwd": "D:\\work\\proj"},
	})
	waitForReceived(t, fake, "session/new")

	// The connector registers the agent-created session in the Hub.
	waitFor(t, 5*time.Second, func() bool {
		return sessionExists(t, ts, "fake-sess-1")
	}, "hub registration of fake-sess-1")

	// 2. The agent's answer comes back as agent.response.
	evt := ws.waitFor(t, 5*time.Second, func(e map[string]any) bool {
		return e["type"] == "agent.response" && e["requestId"] == rq1
	}, "agent.response for session/new")
	result, _ := evt["result"].(map[string]any)
	if result["sessionId"] != "fake-sess-1" {
		t.Fatalf("agent.response result: %v", result)
	}

	// 3. Hub → agent: session/prompt (agent will ask for permission).
	rq2 := postAgentRequest(t, ts, map[string]any{
		"method": "session/prompt",
		"params": map[string]any{"sessionId": "fake-sess-1", "prompt": "build the project"},
	})
	waitForReceived(t, fake, "session/prompt")

	// 4. The agent's permission request becomes a Hub permission request.
	var hubReqID string
	waitFor(t, 5*time.Second, func() bool {
		reqs := listPendingPermissionRequests(t, ts)
		if len(reqs) == 0 {
			return false
		}
		first, _ := reqs[0].(map[string]any)
		hubReqID, _ = first["requestId"].(string)
		return hubReqID != ""
	}, "hub permission request from agent")

	// 5. Human decision (allow_once) maps onto the agent's proceed_once.
	resp, out := postJSON(t, ts.URL+"/api/permission-requests/"+hubReqID+"/decision",
		map[string]any{"decision": "allow_once"})
	if resp.StatusCode != 200 || out["applied"] != true {
		t.Fatalf("decision: %d %v", resp.StatusCode, out)
	}

	// 6. The fake agent asserts the mapping (selected / proceed_once) itself
	// and completes the turn; its answer returns as agent.response...
	evt = ws.waitFor(t, 10*time.Second, func(e map[string]any) bool {
		return e["type"] == "agent.response" && e["requestId"] == rq2
	}, "agent.response for session/prompt")
	if result, _ := evt["result"].(map[string]any); result["stopReason"] != "end_turn" {
		t.Fatalf("prompt response: %v", result)
	}

	// 7. ...the connector closes the turn in the Hub state machine...
	evt = ws.waitFor(t, 5*time.Second, func(e map[string]any) bool {
		if e["type"] != "frame.out" {
			return false
		}
		params, _ := e["params"].(map[string]any)
		return e["method"] == "session/update" && params != nil && params["isComplete"] == true
	}, "turn-completion frame.out")
	params, _ := evt["params"].(map[string]any)
	if params["sessionId"] != "fake-sess-1" {
		t.Fatalf("completion frame session: %v", params)
	}

	// 8. ...and the session ends up completed with one turn.
	waitFor(t, 5*time.Second, func() bool {
		s := getSession(t, ts, "fake-sess-1")
		return s != nil && s["status"] == "completed" && s["turns"] == float64(1)
	}, "session completed with 1 turn")

	drainFakeAgentErrors(t, fake)
}

// ------------------------------------------------------------------ helpers

func postJSON(t *testing.T, url string, body any) (*http.Response, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", url, strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func getJSON(t *testing.T, url string) map[string]any {
	t.Helper()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out
}

func postAgentRequest(t *testing.T, ts *httptest.Server, body map[string]any) string {
	t.Helper()
	resp, out := postJSON(t, ts.URL+"/api/agent-requests", body)
	if resp.StatusCode != 200 {
		t.Fatalf("agent-requests: %d %v", resp.StatusCode, out)
	}
	id, _ := out["requestId"].(string)
	if id == "" {
		t.Fatalf("no requestId: %v", out)
	}
	return id
}

func sessionExists(t *testing.T, ts *httptest.Server, id string) bool {
	out := getJSON(t, ts.URL+"/api/sessions")
	sessions, _ := out["sessions"].([]any)
	for _, s := range sessions {
		if m, _ := s.(map[string]any); m["id"] == id {
			return true
		}
	}
	return false
}

func getSession(t *testing.T, ts *httptest.Server, id string) map[string]any {
	out := getJSON(t, ts.URL+"/api/sessions")
	sessions, _ := out["sessions"].([]any)
	for _, s := range sessions {
		if m, _ := s.(map[string]any); m["id"] == id {
			return m
		}
	}
	return nil
}

func listPendingPermissionRequests(t *testing.T, ts *httptest.Server) []any {
	out := getJSON(t, ts.URL+"/api/permission-requests?outcome=pending")
	reqs, _ := out["permissionRequests"].([]any)
	return reqs
}

// getJSON2 fetches any JSON endpoint (diagnostics variant without Fatal on
// status).
func getJSON2(t *testing.T, url string) (*http.Response, map[string]any) {
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, nil
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

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

// TestConnectorFsCapabilities drives the ACP host callbacks synchronously:
// read returns the file content, write lands on disk after an allow_once
// verdict, a rejected write surfaces as a JSON-RPC error, and terminal/*
// still fails fast. Uses the opencode kind to prove non-claude kinds work.
func TestConnectorFsCapabilities(t *testing.T) {
	ts, _ := newTestDaemonKinds(t, acpclient.AgentKindClaudeCode, acpclient.AgentKindOpenCode)

	dir := t.TempDir()
	readPath := filepath.Join(dir, "read-me.txt")
	if err := os.WriteFile(readPath, []byte("fs-connector-test-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	writePath := filepath.Join(dir, "sub", "written.txt")

	agentOutR, agentOutW := io.Pipe()
	agentInR, agentInW := io.Pipe()
	fake := newFakeAgent(agentInR, agentOutW)

	cfg := connector.Config{
		DaemonURL: ts.URL,
		Token:     testToken,
		AgentKind: acpclient.AgentKindOpenCode,
		CWD:       dir,
		Log:       log.New(os.Stderr, "fs-conn ", log.Ltime|log.Lmicroseconds),
	}
	conn := connector.New(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Bootstrap goroutine: answer the ACP initialize handshake so Run can
	// reach Ready(); the ordered fs traffic below stays on the main goroutine.
	go func() {
		msg, err := fake.readLine(15 * time.Second)
		if err != nil {
			fake.fail("initialize read: %v", err)
			return
		}
		if msg["method"] != "initialize" {
			fake.fail("expected initialize, got %v", msg["method"])
			return
		}
		_ = fake.write(map[string]any{
			"jsonrpc": "2.0", "id": msg["id"],
			"result": map[string]any{"protocolVersion": 1},
		})
	}()

	go func() { _ = conn.Run(ctx, agentInW, agentOutR) }()
	select {
	case <-conn.Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("connector ws never became ready")
	}

	// The test plays the agent: it writes requests to agentOutW and reads the
	// connector's answers from agentInR (via fake.readLine). Hub→agent
	// requests (session/new) are answered with the registered session id.
	// sendAgentReq fires one agent→host request.
	sendAgentReq := func(id, method string, params map[string]any) {
		raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		if _, err := agentOutW.Write(append(raw, '\n')); err != nil {
			t.Fatal(err)
		}
	}
	answerHubReq := func(wantMethod string) {
		t.Helper()
		msg, err := fake.readLine(15 * time.Second)
		if err != nil {
			t.Fatalf("read %s request: %v", wantMethod, err)
		}
		if msg["method"] != wantMethod {
			t.Fatalf("expected %s request, got %v", wantMethod, msg["method"])
		}
		id := msg["id"]
		result := map[string]any{}
		if wantMethod == "session/new" {
			result = map[string]any{"sessionId": "fake-sess-1"}
		}
		_ = fake.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	}

	// Register a session first (the fs gate keys on the session id).
	postAgentRequest(t, ts, map[string]any{
		"method": "session/new",
		"params": map[string]any{"cwd": dir},
	})
	answerHubReq("session/new")
	waitFor(t, 5*time.Second, func() bool {
		return sessionExists(t, ts, "fake-sess-1")
	}, "hub registration of fake-sess-1")

	// 1. fs/read_text_file → content round-trips.
	sendAgentReq("fs-read-1", "fs/read_text_file", map[string]any{"sessionId": "fake-sess-1", "path": readPath})
	ans, err := fake.readLine(15 * time.Second)
	if err != nil {
		t.Fatalf("fs read answer: %v", err)
	}
	res, _ := ans["result"].(map[string]any)
	if res == nil || res["content"] != "fs-connector-test-content" {
		t.Fatalf("fs read result wrong: %v", ans)
	}

	// 2. fs/write_text_file → intake gate → allow_once → file on disk.
	sendAgentReq("fs-write-1", "fs/write_text_file", map[string]any{
		"sessionId": "fake-sess-1", "path": writePath, "content": "written by connector",
	})
	decideNext := func(decision string, note string) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			reqs := listPendingPermissionRequests(t, ts)
			if len(reqs) > 0 {
				first, _ := reqs[0].(map[string]any)
				rid, _ := first["requestId"].(string)
				resp, out := postJSON(t, ts.URL+"/api/permission-requests/"+rid+"/decision",
					map[string]any{"decision": decision, "note": note})
				if resp.StatusCode == 200 && out["applied"] == true {
					return
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Errorf("approval for %s never surfaced", decision)
		return
	}
	go decideNext("allow_once", "")
	ans, err = fake.readLine(30 * time.Second)
	if err != nil {
		t.Fatalf("fs write answer: %v", err)
	}
	if _, hasErr := ans["error"]; hasErr {
		t.Fatalf("allowed write returned error: %v", ans)
	}
	waitFor(t, 5*time.Second, func() bool {
		raw, err := os.ReadFile(writePath)
		return err == nil && string(raw) == "written by connector"
	}, "allowed fs write landed on disk")

	// 3. Second write → reject_once → JSON-RPC error, file unchanged.
	sendAgentReq("fs-write-2", "fs/write_text_file", map[string]any{
		"sessionId": "fake-sess-1", "path": writePath, "content": "should not land",
	})
	go decideNext("reject_once", "fs test deny")
	ans, err = fake.readLine(30 * time.Second)
	if err != nil {
		t.Fatalf("fs denied answer: %v", err)
	}
	errObj, _ := ans["error"].(map[string]any)
	if errObj == nil || !strings.Contains(fmt.Sprint(errObj["message"]), "denied by AgentHub approval gate") {
		t.Fatalf("denied write error wrong: %v", ans)
	}
	content, err := os.ReadFile(writePath)
	if err != nil || string(content) != "written by connector" {
		t.Fatalf("denied write changed the file: %v %s", err, content)
	}

	// 4. terminal/* still fails fast (regression).
	sendAgentReq("term-1", "terminal/execute", map[string]any{"sessionId": "fake-sess-1", "command": "ls"})
	ans, err = fake.readLine(15 * time.Second)
	if err != nil {
		t.Fatalf("terminal answer: %v", err)
	}
	errObj, _ = ans["error"].(map[string]any)
	if errObj == nil || !strings.Contains(fmt.Sprint(errObj["message"]), "does not implement") {
		t.Fatalf("terminal error wrong: %v", ans)
	}

	drainFakeAgentErrors(t, fake)
}
