// Fake ACP stdio server for deterministic AgentHub data-flow trials (v2).
//
// Env:
//   FAKE_LOG        path to append every frame received from Hub (stdin)
//   FAKE_MODE       normal (default): responds to session/prompt with a result
//                   hold: never responds to session/prompt; keeps stdout open
//                   perm: before answering session/prompt, sends a
//                         session/request_permission server request and waits
//                         for the Hub's response (logged), then answers.
//   FAKE_SESSIONID  agent session id reported in session/new result
//                   (default ses_FAKE_0001)
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
	Result  json.RawMessage `json:"result"`
	Error   json.RawMessage `json:"error"`
}

func main() {
	logPath := os.Getenv("FAKE_LOG")
	if logPath == "" {
		logPath = "fake.log"
	}
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
	mode := os.Getenv("FAKE_MODE")
	if mode == "" {
		mode = "normal"
	}
	agentSessionID := os.Getenv("FAKE_SESSIONID")
	if agentSessionID == "" {
		agentSessionID = "ses_FAKE_0001"
	}
	logLine(fmt.Sprintf("START pid=%d mode=%s args=%v", os.Getpid(), mode, os.Args))

	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	writeJSON := func(v any) {
		b, _ := json.Marshal(v)
		fmt.Fprint(os.Stdout, string(b), "\n")
		_ = os.Stdout.Sync()
	}

	type pending struct {
		method string
		frame  frame
	}
	// deferredPrompt holds the session/prompt request while waiting on a
	// permission round-trip (perm mode).
	var deferredPrompt *pending

	for in.Scan() {
		line := strings.TrimSpace(in.Text())
		if line == "" {
			continue
		}
		logLine("IN: " + line)
		var msg frame
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}
		// Response to our session/request_permission (perm mode): log the
		// verdict the Hub actually sent back, then continue the prompt.
		if msg.Method == "" && msg.ID != nil && deferredPrompt != nil {
			logLine(fmt.Sprintf("PERMISSION_RESPONSE id=%d result=%s err=%s",
				*msg.ID, string(msg.Result), string(msg.Error)))
			// Answer the deferred session/prompt now (normal behaviour).
			pid := deferredPrompt.frame.ID
			logLine(fmt.Sprintf("METHOD=session/prompt id=%d -> respond with result (after permission)", *pid))
			writeJSON(map[string]any{"jsonrpc": "2.0", "id": *pid, "result": map[string]any{"stopReason": "end_turn"}})
			logLine("EXIT after session/prompt response")
			os.Exit(0)
		}

		id := msg.ID
		if id == nil {
			zero := 0
			id = &zero
		}
		switch msg.Method {
		case "initialize":
			logLine("METHOD=initialize -> result")
			writeJSON(map[string]any{"jsonrpc": "2.0", "id": *id, "result": map[string]any{
				"protocolVersion": 1,
				"agentInfo":       map[string]any{"name": "fake", "version": "2.0.0"},
				"capabilities": map[string]any{
					"loadSession":        false,
					"promptCapabilities": map[string]any{"image": false, "embeddedContext": false},
					"mcpCapabilities":    map[string]any{},
				},
			}})
		case "session/new":
			logLine("METHOD=session/new -> " + agentSessionID)
			writeJSON(map[string]any{"jsonrpc": "2.0", "id": *id, "result": map[string]any{"sessionId": agentSessionID}})
		case "session/prompt":
			// Per-prompt markers override the process-wide mode so one daemon
			// serves all scenarios.
			var pp struct {
				Prompt []struct {
					Text string `json:"text"`
				} `json:"prompt"`
			}
			_ = json.Unmarshal(msg.Params, &pp)
			for _, b := range pp.Prompt {
				switch {
				case strings.Contains(b.Text, "[perm]"):
					mode = "perm"
				case strings.Contains(b.Text, "[hold]"):
					mode = "hold"
				}
			}
			if mode == "perm" {
				logLine(fmt.Sprintf("METHOD=session/prompt id=%d -> defer, sending session/request_permission", *id))
				f := msg
				deferredPrompt = &pending{method: msg.Method, frame: f}
				writeJSON(map[string]any{"jsonrpc": "2.0", "id": 4242, "method": "session/request_permission", "params": map[string]any{
					"sessionId": agentSessionID,
					"toolCall":  map[string]any{"toolCallId": "tc_fake_1", "title": "Bash", "kind": "execute", "rawInput": map[string]any{"command": "echo perm-probe"}},
					"options": []any{
						map[string]any{"optionId": "opt_once", "name": "Allow once", "kind": "allow_once"},
						map[string]any{"optionId": "opt_session", "name": "Allow for session", "kind": "allow_session"},
						map[string]any{"optionId": "opt_always", "name": "Always allow", "kind": "allow_always"},
						map[string]any{"optionId": "opt_reject", "name": "Reject", "kind": "reject_once"},
					},
					"reason": "fakeacp perm-mode probe",
				}})
				continue
			}
			logLine(fmt.Sprintf("METHOD=session/prompt id=%d -> %s", *id,
				map[bool]string{true: "HOLD (no response)", false: "respond with result"}[mode == "hold"]))
			if mode == "hold" {
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
	logLine(fmt.Sprintf("STDIN EOF (scanner err=%v)", in.Err()))
}
