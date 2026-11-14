// Fake `claude` CLI for deterministic AgentHub managed-task trials.
// Emits `claude -p --output-format stream-json --verbose` shaped lines.
//
// Env FAKE_CLAUDE_MODE:
//
//	ok        (default) tool_use -> tool_result -> result(usage) then exit 0
//	highrisk  tool_use with a rm -rf style command, sleeps FAKE_DELAY_MS
//	          (default 15000) so QA can post a permission decision, then
//	          tool_result -> result
//	hang      emit init then sleep forever (for cancel tests)
//	fail      emit init then exit 3 with a stderr message
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"
)

func emit(v any) {
	b, _ := json.Marshal(v)
	fmt.Println(string(b))
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func main() {
	mode := os.Getenv("FAKE_CLAUDE_MODE")
	if mode == "" {
		mode = "ok"
	}
	// Prompt markers override the env mode so one daemon serves all scenarios.
	prompt := ""
	resumeID := ""
	args := os.Args[1:]
	for i, a := range args {
		if a == "-p" && i+1 < len(args) {
			prompt = args[i+1]
		}
		if a == "--resume" && i+1 < len(args) {
			resumeID = args[i+1] // session control C-5: resume keeps the id
		}
	}
	for _, m := range []string{"highrisk", "hang", "fail", "ok"} {
		if len(prompt) >= len(m)+2 && contains(prompt, "["+m+"]") {
			mode = m
		}
	}
	// A resume turn reuses the SAME agent session id — this is what proves
	// history continuation end-to-end (a fresh run would mint a new pid id).
	sessionID := resumeID
	if sessionID == "" {
		sessionID = fmt.Sprintf("fake-cc-%d", os.Getpid())
	}
	emit(map[string]any{"type": "system", "subtype": "init", "session_id": sessionID, "tools": []string{"Bash"}})

	switch mode {
	case "hang":
		select {}
	case "fail":
		fmt.Fprintln(os.Stderr, "fake-claude: simulated boot failure")
		os.Exit(3)
	}

	delay := 15000
	if v, err := strconv.Atoi(os.Getenv("FAKE_DELAY_MS")); err == nil {
		delay = v
	}

	command := "echo hello-from-fake-claude"
	if mode == "highrisk" {
		command = "rm -rf ./evil-target"
	}
	emit(map[string]any{"type": "assistant", "message": map[string]any{
		"content": []any{map[string]any{
			"type": "tool_use", "id": "toolu_fake_1", "name": "Bash",
			"input": map[string]any{"command": command},
		}}}})
	if mode == "highrisk" {
		time.Sleep(time.Duration(delay) * time.Millisecond)
	}
	emit(map[string]any{"type": "user", "message": map[string]any{
		"content": []any{map[string]any{
			"type": "tool_result", "tool_use_id": "toolu_fake_1",
			"content": "hello-from-fake-claude",
		}}}})
	emit(map[string]any{"type": "result", "subtype": "success", "is_error": false,
		"result": "fake-claude final answer for: vibe trial",
		"usage":  map[string]any{"input_tokens": 12, "output_tokens": 7}})
}
