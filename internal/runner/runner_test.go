package runner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeSink struct {
	mu     sync.Mutex
	frames []string // "method|key bits"
}

func (f *fakeSink) Dispatch(agentKind, method string, params json.RawMessage) error {
	f.mu.Lock()
	f.frames = append(f.frames, method+"|"+string(params))
	f.mu.Unlock()
	return nil
}

func (f *fakeSink) has(substr string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, fr := range f.frames {
		if strings.Contains(fr, substr) {
			return true
		}
	}
	return false
}

// fakeClaude is a portable stand-in for `claude --resume … --output-format
// stream-json`: it emits one assistant tool_use line and a result line.
func fakeClaude(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "fake-claude.txt")
	script := filepath.Join(dir, "fake-claude.bat")
	if _, err := os.Stat("cmd"); err == nil {
	}
	// Windows-safe: a .cmd file that echoes fixed JSONL.
	content := "@echo off\r\n" +
		"echo {\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"tool_use\",\"id\":\"tu_1\",\"name\":\"Bash\",\"input\":{\"command\":\"git status\"}}]}}\r\n" +
		"echo {\"type\":\"user\",\"message\":{\"content\":[{\"type\":\"tool_result\",\"tool_use_id\":\"tu_1\",\"is_error\":false,\"content\":\"ok\"}]}}\r\n" +
		"echo {\"type\":\"result\",\"subtype\":\"success\",\"result\":\"done\"}\r\n"
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = p
	return script
}

func TestContinueCCNativeResume(t *testing.T) {
	sink := &fakeSink{}
	r := New(sink, nil)
	fake := fakeClaude(t)
	r.SetCommandBuilder(func(kind, sessionID, prompt string) (string, []string) {
		if kind != "claude_code" || sessionID != "cc-1" || !strings.Contains(prompt, "新任务") {
			t.Fatalf("unexpected resume args: %s %s %s", kind, sessionID, prompt)
		}
		return "cmd", []string{"/c", fake}
	})

	_, started, err := r.Continue(context.Background(), "claude_code", "cc-1", t.TempDir(), "带上新任务继续")
	if err != nil || !started {
		t.Fatalf("continue: started=%v err=%v", started, err)
	}
	// Wait for the async run to finish.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if sink.has("\"status\":\"completed\"") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(sink.frames) == 0 {
		t.Fatal("no frames dispatched at all")
	}
	t.Logf("frames: %v", sink.frames)
	if !sink.has("tool_call") || !sink.has("\"name\":\"Bash\"") {
		t.Fatal("tool_call frame missing from resume output")
	}
	if !sink.has("tool_result") {
		t.Fatal("tool_result frame missing")
	}
	if !sink.has("\"status\":\"completed\"") {
		t.Fatal("completion status missing")
	}
}

func TestContinueSingleInFlight(t *testing.T) {
	sink := &fakeSink{}
	r := New(sink, nil)
	r.SetCommandBuilder(func(kind, sessionID, prompt string) (string, []string) {
		// A command that never finishes: block on stdin.
		return "cmd", []string{"/c", "pause"}
	})
	_, started, err := r.Continue(context.Background(), "claude_code", "s1", ".", "x")
	if err != nil || !started {
		t.Fatalf("first continue: %v %v", started, err)
	}
	_, started, err = r.Continue(context.Background(), "claude_code", "s1", ".", "y")
	if err == nil || started {
		t.Fatalf("second continue must be rejected while in flight: %v %v", started, err)
	}
}

func TestSupportsResume(t *testing.T) {
	if !SupportsResume("claude_code") || !SupportsResume("codex") {
		t.Fatal("CC/Codex must support native resume")
	}
	if SupportsResume("dsh") || SupportsResume("") {
		t.Fatal("ACP-only kinds must not claim native resume (PRD F8)")
	}
	var _ = errors.New
}
