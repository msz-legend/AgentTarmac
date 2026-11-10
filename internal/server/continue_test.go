package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/agenttarmac/agenttarmac/internal/acpclient"
)

// fakeTaskQueue is a recording TaskQueue: it validates that "继续此会话" on an
// F6 task-* session re-dispatches instead of broadcasting to the command bus.
type fakeTaskQueue struct {
	calls []struct{ kind, cwd, prompt string }
	err   error
}

func (q *fakeTaskQueue) Enqueue(_ context.Context, kind, cwd, prompt string) (*acpclient.Task, error) {
	q.calls = append(q.calls, struct{ kind, cwd, prompt string }{kind, cwd, prompt})
	if q.err != nil {
		return nil, q.err
	}
	return &acpclient.Task{ID: "task_deadbeef", AgentKind: kind, CWD: cwd, Prompt: prompt, State: acpclient.TaskQueued}, nil
}

// EnqueueInSession mirrors the real queue's preset-session variant (the
// continue flow pre-creates the child session row for lineage, A10).
func (q *fakeTaskQueue) EnqueueInSession(ctx context.Context, kind, cwd, prompt, sessionID string) (*acpclient.Task, error) {
	t, err := q.Enqueue(ctx, kind, cwd, prompt)
	if err != nil {
		return nil, err
	}
	t.SessionID = sessionID
	return t, nil
}
func (q *fakeTaskQueue) GetTask(context.Context, string) (*acpclient.Task, error) {
	return nil, nil
}
func (q *fakeTaskQueue) ListTasks(context.Context, string) ([]*acpclient.Task, error) {
	return nil, nil
}
func (q *fakeTaskQueue) Retry(context.Context, string) (*acpclient.Task, error) {
	return nil, nil
}
func (q *fakeTaskQueue) Cancel(context.Context, string) (*acpclient.Task, error) {
	return nil, nil
}
func (q *fakeTaskQueue) Pause()       {}
func (q *fakeTaskQueue) Resume()      {}
func (q *fakeTaskQueue) Paused() bool { return false }

// newContinueDaemon wires a daemon with a recording task queue attached.
func newContinueDaemon(t *testing.T) (*httptest.Server, *acpclient.Store, *Bridge, *fakeTaskQueue) {
	t.Helper()
	db, err := acpclient.InitDB(t.TempDir() + "/continue.db")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	store := acpclient.NewStore(db)
	bridge := NewBridge(nil)
	srv := NewServer("127.0.0.1:0", bridge, store, testToken, nil)
	q := &fakeTaskQueue{}
	srv.SetTasks(q)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, store, bridge, q
}

// TestContinueTaskRedispatch is the regression test for the real-user bug found
// in the CU full-path walk: F6 task-* sessions are one-shot headless CLI runs
// whose process has already exited, so "继续此会话" must re-dispatch a fresh
// task in the same workspace instead of broadcasting session/prompt (which
// reached unrelated connectors and failed with -32602).
func TestContinueTaskRedispatch(t *testing.T) {
	ts, store, bridge, q := newContinueDaemon(t)
	_ = bridge

	sess := &acpclient.Session{
		ID: "task-913477f3", AgentKind: acpclient.AgentKindClaudeCode,
		CWD: `D:\DevTools\AgentHub`, Status: acpclient.StatusCompleted,
		Origin: acpclient.OriginManaged,
	}
	if err := store.CreateSession(context.Background(), sess); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	resp, body := postJSON(t, ts.URL+"/api/sessions/task-913477f3/continue", testToken,
		map[string]any{"text": "  请再回复一行：round two ok  "})
	if resp.StatusCode != 200 {
		t.Fatalf("continue = %d body=%v", resp.StatusCode, body)
	}
	if body["ok"] != true || body["mode"] != "task-redispatch" {
		t.Fatalf("mode = %v, want task-redispatch; body=%v", body["mode"], body)
	}
	if body["task_id"] != "task_deadbeef" {
		t.Fatalf("task id wrong: %v", body)
	}
	// A10: the child session is pre-created by the handler and its id — not
	// one derived from the task id — comes back; the lineage is durable.
	childID, _ := body["session_id"].(string)
	if childID == "" || childID == "task-deadbeef" {
		t.Fatalf("session_id should be the pre-created child id: %v", body)
	}
	if body["parent_session_id"] != "task-913477f3" {
		t.Fatalf("parent_session_id = %v", body["parent_session_id"])
	}
	child, err := store.GetSession(context.Background(), childID)
	if err != nil || child.ParentSessionID != "task-913477f3" {
		t.Fatalf("child lineage not persisted: %+v err=%v", child, err)
	}
	if len(q.calls) != 1 {
		t.Fatalf("Enqueue called %d times, want 1", len(q.calls))
	}
	got := q.calls[0]
	// Re-dispatch must reuse the original workspace/kind and carry trimmed text.
	if got.kind != acpclient.AgentKindClaudeCode || got.cwd != `D:\DevTools\AgentHub` ||
		got.prompt != "请再回复一行：round two ok" {
		t.Fatalf("Enqueue args = %+v", got)
	}
}

// TestContinueManagedACPContentBlocks guards the other managed path: ACP
// session/prompt must carry content blocks, not a bare string.
func TestContinueManagedACPContentBlocks(t *testing.T) {
	ts, store, bridge, q := newContinueDaemon(t)
	sess := &acpclient.Session{
		ID: "acp:live-1", AgentKind: acpclient.AgentKindClaudeCode,
		CWD: ".", Status: acpclient.StatusActive, Origin: acpclient.OriginManaged,
	}
	if err := store.CreateSession(context.Background(), sess); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	ch := bridge.Subscribe()
	defer bridge.Unsubscribe(ch)

	resp, body := postJSON(t, ts.URL+"/api/sessions/acp:live-1/continue", testToken,
		map[string]any{"text": "hello"})
	if resp.StatusCode != 200 || body["mode"] != "managed" {
		t.Fatalf("continue = %d body=%v", resp.StatusCode, body)
	}
	if len(q.calls) != 0 {
		t.Fatalf("non-task managed session must not re-dispatch; calls=%d", len(q.calls))
	}

	// Walk the broadcast frames and find the agent.request carrying our params.
	for {
		select {
		case raw := <-ch:
			var evt struct {
				Type   string          `json:"type"`
				Params json.RawMessage `json:"params"`
			}
			if err := json.Unmarshal(raw, &evt); err != nil || evt.Type != "agent.request" {
				continue
			}
			var p struct {
				SessionID string `json:"sessionId"`
				Prompt    []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"prompt"`
			}
			if err := json.Unmarshal(evt.Params, &p); err != nil {
				t.Fatalf("params decode: %v (%s)", err, evt.Params)
			}
			if p.SessionID != "acp:live-1" || len(p.Prompt) != 1 ||
				p.Prompt[0].Type != "text" || p.Prompt[0].Text != "hello" {
				t.Fatalf("bad ACP prompt shape: %s", evt.Params)
			}
			return
		default:
			t.Fatal("no agent.request frame broadcast")
		}
	}
}

// TestContinueValidation still rejects empty text and unknown sessions.
func TestContinueValidation(t *testing.T) {
	ts, _, _, q := newContinueDaemon(t)
	// Session existence is validated before the body (vibe-trial S4: an
	// unknown session used to report the misleading "text is required").
	if resp, _ := postJSON(t, ts.URL+"/api/sessions/no-such/continue", testToken,
		map[string]any{"text": "hi"}); resp.StatusCode == 200 || resp.StatusCode == 400 {
		t.Fatalf("unknown session = %d, want 404-class", resp.StatusCode)
	}
	if resp, _ := postJSON(t, ts.URL+"/api/sessions/missing/continue", testToken,
		map[string]any{"text": "   "}); resp.StatusCode == 200 {
		t.Fatal("empty text on unknown session must not return 200")
	}
	if len(q.calls) != 0 {
		t.Fatalf("no re-dispatch expected for invalid input, got %d", len(q.calls))
	}
}
