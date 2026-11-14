// Fake `codex` CLI for deterministic AgentHub managed-task trials.
// Emits `codex exec --json` shaped JSONL events.
//
// Env FAKE_CODEX_MODE:
//
//	ok    (default) item.started/completed(command_execution) +
//	      item.completed(agent_message) + turn.completed(usage), exit 0
//	hang  emit one event then sleep forever
//	fail  exit 4 with a stderr message
package main

import (
	"encoding/json"
	"fmt"
	"os"
)

func emit(v any) {
	b, _ := json.Marshal(v)
	fmt.Println(string(b))
}

func main() {
	mode := os.Getenv("FAKE_CODEX_MODE")
	if mode == "" {
		mode = "ok"
	}
	// Prompt markers override the env mode so one daemon serves all scenarios.
	prompt := ""
	resumeID := ""
	if n := len(os.Args); n > 1 {
		// Fresh turn: `exec --json <prompt>` (prompt is the last arg).
		// Resume turn (session control C-5): `exec resume <id> --json <prompt>`.
		args := os.Args[1:]
		for i, a := range args {
			if a == "resume" && i+1 < len(args) {
				resumeID = args[i+1]
			}
		}
		if len(args) >= 2 {
			prompt = args[len(args)-1]
		}
		for _, m := range []string{"hang", "fail", "ok"} {
			if len(prompt) >= len(m)+2 && contains(prompt, "["+m+"]") {
				mode = m
			}
		}
	}
	if mode == "fail" {
		fmt.Fprintln(os.Stderr, "fake-codex: simulated boot failure")
		os.Exit(4)
	}
	// session_configured opens every codex exec run and carries the REAL
	// session id — a resume turn reuses the caller's id (proves history
	// continuation); a fresh run mints a new pid-based one.
	sessionID := resumeID
	if sessionID == "" {
		sessionID = fmt.Sprintf("fake-codex-%d", os.Getpid())
	}
	emit(map[string]any{"type": "session_configured", "session_id": sessionID})
	emit(map[string]any{"type": "item.started", "item": map[string]any{
		"type": "command_execution", "id": "item_1", "command": "echo hello-from-fake-codex"}})
	switch mode {
	case "hang":
		select {}
	}
	emit(map[string]any{"type": "item.completed", "item": map[string]any{
		"type": "command_execution", "id": "item_1", "command": "echo hello-from-fake-codex",
		"exit_code": 0, "aggregated_output": "hello-from-fake-codex"}})
	emit(map[string]any{"type": "item.completed", "item": map[string]any{
		"type": "agent_message", "id": "item_2", "text": "fake-codex final answer"}})
	emit(map[string]any{"type": "turn.completed", "usage": map[string]any{
		"input_tokens": 21, "cached_input_tokens": 0, "output_tokens": 9}})
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
