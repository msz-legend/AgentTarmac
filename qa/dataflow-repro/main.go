// Fake ACP stdio server for deterministic AgentHub data-flow reproduction.
//
// Env:
//   FAKE_LOG   path to append every frame received from Hub (stdin)
//   FAKE_MODE  "normal" (responds to session/prompt with a result) |
//              "hold"   (never responds to session/prompt; keeps stdout open)
//
// Behaviour: after `initialize` it replies with a result AND sends two
// unsolicited `session/update` notifications, which is what real opencode /
// hermes ACP servers do. That is deliberate: it exposes both
//   P0-A  Hub never sends session/new
//   P0-B  json.Decoder pre-read buffering starves the later bufio.Scanner
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

type frame struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int            `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func main() {
	logPath := os.Getenv("FAKE_LOG")
	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakeacp: cannot open log %s: %v\n", logPath, err)
		os.Exit(1)
	}
	defer lf.Close()

	logLine := func(s string) {
		fmt.Fprintln(lf, s)
		_ = lf.Sync()
	}
	logLine(fmt.Sprintf("START pid=%d mode=%s args=%v", os.Getpid(), os.Getenv("FAKE_MODE"), os.Args))

	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	writeJSON := func(v any) {
		b, _ := json.Marshal(v)
		fmt.Fprint(os.Stdout, string(b), "\n")
		_ = os.Stdout.Sync()
	}

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		logLine("IN: " + line)
		var msg frame
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}
		id := msg.ID
		if id == nil {
			zero := 0
			id = &zero
		}
		switch msg.Method {
		case "initialize":
			logLine("METHOD=initialize -> result + 2 unsolicited session/update")
			writeJSON(map[string]any{"jsonrpc": "2.0", "id": *id, "result": map[string]any{
				"protocolVersion": 1,
				"agentInfo":       map[string]any{"name": "fake", "version": "1.0.0"},
				"capabilities": map[string]any{
					"loadSession":        false,
					"promptCapabilities": map[string]any{"image": false, "embeddedContext": false},
					"mcpCapabilities":    map[string]any{},
				},
			}})
			writeJSON(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
				"sessionId": "ses_FAKE_0001",
				"update":    map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "agent thinking..."}},
			}})
			writeJSON(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
				"sessionId": "ses_FAKE_0001",
				"update":    map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "more progress"}},
			}})
		case "session/new":
			logLine("METHOD=session/new -> ses_FAKE_NEW_001")
			writeJSON(map[string]any{"jsonrpc": "2.0", "id": *id, "result": map[string]any{"sessionId": "ses_FAKE_NEW_001"}})
		case "session/prompt":
			logLine(fmt.Sprintf("METHOD=session/prompt id=%d -> %s", *id,
				map[bool]string{true: "HOLD (no response)", false: "respond with result"}[os.Getenv("FAKE_MODE") == "hold"]))
			if os.Getenv("FAKE_MODE") == "hold" {
				continue
			}
			writeJSON(map[string]any{"jsonrpc": "2.0", "id": *id, "result": map[string]any{"stopReason": "end_turn"}})
			logLine("EXIT after session/prompt response")
			os.Exit(0)
		case "session/request_permission":
			logLine("METHOD=session/request_permission (logged only)")
		default:
			logLine("METHOD=" + msg.Method + " (unhandled)")
		}
	}
	logLine(fmt.Sprintf("STDIN EOF (scanner err=%v)", sc.Err()))
}
