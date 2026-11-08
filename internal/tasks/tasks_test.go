package tasks

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/agenttarmac/agenttarmac/internal/acpclient"
)

type memStore struct {
	mu    sync.Mutex
	tasks map[string]*acpclient.Task
}

func newMemStore() *memStore { return &memStore{tasks: map[string]*acpclient.Task{}} }

func (m *memStore) InsertTask(_ context.Context, t *acpclient.Task) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *t
	m.tasks[t.ID] = &cp
	return nil
}
func (m *memStore) GetTask(_ context.Context, id string) (*acpclient.Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	if !ok {
		return nil, acpclient.ErrNotFound
	}
	cp := *t
	return &cp, nil
}
func (m *memStore) ListTasks(_ context.Context, state string) ([]*acpclient.Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*acpclient.Task
	for _, t := range m.tasks {
		if state == "" || t.State == state {
			cp := *t
			out = append(out, &cp)
		}
	}
	return out, nil
}
func (m *memStore) QueuedTasks(ctx context.Context) ([]*acpclient.Task, error) {
	return m.ListTasks(ctx, acpclient.TaskQueued)
}
func (m *memStore) UpdateTask(_ context.Context, t *acpclient.Task) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *t
	m.tasks[t.ID] = &cp
	return nil
}
func (m *memStore) CreateSession(context.Context, *acpclient.Session) error { return nil }
func (m *memStore) UpdateSession(context.Context, *acpclient.Session) error { return nil }
func (m *memStore) GetSession(_ context.Context, id string) (*acpclient.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.tasks {
		if t.SessionID == id {
			return &acpclient.Session{
				ID:        id,
				AgentKind: t.AgentKind,
				CWD:       t.CWD,
				Status:    t.State,
				Origin:    "managed",
				Title:     t.Prompt,
			}, nil
		}
	}
	return nil, acpclient.ErrNotFound
}

type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (e *eventLog) TaskEvent(taskID, action, sessionID, agentKind, title, errText string) {
	e.mu.Lock()
	e.events = append(e.events, action+"|"+taskID)
	e.mu.Unlock()
}

func (e *eventLog) has(action string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, ev := range e.events {
		if ev == action || len(ev) > len(action) && ev[:len(action)] == action {
			return true
		}
	}
	return false
}

type nullDispatch struct{}

func (nullDispatch) Dispatch(agentKind, method string, params json.RawMessage) error { return nil }

// fake agent: a .cmd emitting one tool frame + exiting 0 (portable on
// Windows CI boxes without claude/codex installed).
func fakeAgentScript(t *testing.T) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "fake-agent.cmd")
	content := "@echo off\r\n" +
		"echo {\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"tool_use\",\"id\":\"t1\",\"name\":\"Bash\",\"input\":{\"command\":\"git status\"}}]}}\r\n" +
		"echo {\"type\":\"result\",\"subtype\":\"success\"}\r\n"
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

func TestTaskQueueEndToEnd(t *testing.T) {
	store := newMemStore()
	events := &eventLog{}
	q := New(store, events, nullDispatch{}, "", 1)
	fake := fakeAgentScript(t)
	q.SetSpawner(func(ctx context.Context, t *acpclient.Task, sessionID string) (*exec.Cmd, string, error) {
		return exec.Command("cmd", "/c", fake), "", nil
	})

	ctx := context.Background()
	task, err := q.Enqueue(ctx, acpclient.AgentKindClaudeCode, t.TempDir(), "跑一遍构建")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		fresh, _ := store.GetTask(ctx, task.ID)
		if fresh != nil && fresh.State == acpclient.TaskFinished {
			break
		}
		if fresh != nil && fresh.State == acpclient.TaskFailed {
			t.Fatalf("task failed: %s", fresh.Error)
		}
		time.Sleep(50 * time.Millisecond)
	}
	fresh, _ := store.GetTask(ctx, task.ID)
	if fresh == nil || fresh.State != acpclient.TaskFinished {
		t.Fatalf("task not finished: %+v", fresh)
	}
	if fresh.SessionID == "" {
		t.Fatal("managed session not linked")
	}
	for _, want := range []string{"queued", "started", "finished"} {
		if !events.has(want) {
			t.Errorf("task event %q missing (events: %v)", want, events.events)
		}
	}
}

func TestTaskPauseBlocksDispatch(t *testing.T) {
	store := newMemStore()
	events := &eventLog{}
	q := New(store, events, nullDispatch{}, "", 1)
	q.SetSpawner(func(ctx context.Context, t *acpclient.Task, sessionID string) (*exec.Cmd, string, error) {
		return exec.Command("cmd", "/c", "exit", "1"), "", nil // fails → retry
	})
	q.Pause()
	ctx := context.Background()
	if _, err := q.Enqueue(ctx, acpclient.AgentKindClaudeCode, ".", "paused work"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	fresh, _ := store.ListTasks(ctx, acpclient.TaskQueued)
	if len(fresh) != 1 || fresh[0].State != acpclient.TaskQueued {
		t.Fatalf("paused queue dispatched anyway: %+v", fresh)
	}
	q.Resume()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		all, _ := store.ListTasks(ctx, "")
		if len(all) > 0 && all[0].State != acpclient.TaskQueued {
			return // dispatched after resume
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("resume did not dispatch")
}
