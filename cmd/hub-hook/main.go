// Command hub-hook is the Go single-file hook injector (PRD D6: 用户机器
// 零运行时依赖). One binary, two roles:
//
//	hub-hook                — run as a Claude Code PreToolUse hook (stdin JSON
//	                          in, allow JSON/exit 0 or deny/exit 2 out; every
//	                          non-low-risk call is forwarded to the Hub and
//	                          blocks until the human verdict arrives; fails
//	                          closed when the Hub is unreachable)
//	hub-hook install        — register/unregister the hook in Claude Code's
//	hub-hook uninstall        settings.json (or Codex's hooks.json with
//	                          --codex), preserving unrelated keys
//
// Options: --url / $AGENTHUB_URL (default http://127.0.0.1:8799), --token /
// $AGENTHUB_TOKEN, --timeout-ms / $AGENTHUB_TIMEOUT_MS, --codex.
package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	managedMarker = "hub-hook"
	lowRiskTools  = "Read,Grep,Glob,LS,TodoRead,TodoWrite,NotebookRead"
)

type options struct {
	url       string
	token     string
	timeoutMS int
	codex     bool
}

func opts() options {
	// Accept both `hub-hook install --url …` and `hub-hook --url … install`:
	// split argv into a leading subcommand (if present) and flags, then parse
	// the flags with a FlagSet over the remainder.
	args := os.Args[1:]
	sub := ""
	if len(args) > 0 && (args[0] == "install" || args[0] == "uninstall") {
		sub = args[0]
		args = args[1:]
	}
	var o options
	fs := flag.NewFlagSet("hub-hook", flag.ContinueOnError)
	fs.StringVar(&o.url, "url", envOr("AGENTHUB_URL", "http://127.0.0.1:8799"), "Hub address")
	fs.StringVar(&o.token, "token", envOr("AGENTHUB_TOKEN", ""), "Hub access token")
	fs.IntVar(&o.timeoutMS, "timeout-ms", atoiOr(envOr("AGENTHUB_TIMEOUT_MS", "120000"), 120000), "verdict wait deadline")
	fs.BoolVar(&o.codex, "codex", false, "install/uninstall the Codex PermissionRequest hook")
	_ = fs.Parse(args)
	subcommand = sub
	return o
}

// subcommand holds the leading positional subcommand ("" = hook role).
var subcommand string

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func atoiOr(s string, def int) int {
	n := 0
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil || n <= 0 {
		return def
	}
	return n
}

func main() {
	o := opts()
	switch subcommand {
	case "install":
		if err := install(o); err != nil {
			fail("install: %v", err)
		}
		fmt.Println("AgentHub hook installed")
		return
	case "uninstall":
		if err := uninstall(o); err != nil {
			fail("uninstall: %v", err)
		}
		fmt.Println("AgentHub hook removed")
		return
	case "":
		runHook(o)
	default:
		fail("unknown subcommand %q", subcommand)
	}
}

// runHook is the PreToolUse / PermissionRequest role.
func runHook(o options) {
	raw := readStdin()
	var input struct {
		ToolName string `json:"tool_name"`
		Tool     string `json:"tool"`
	}
	_ = json.Unmarshal(raw, &input)
	tool := input.ToolName
	if tool == "" {
		tool = input.Tool
	}
	if tool == "" || isLowRisk(tool) {
		allow("AgentHub: low-risk tool")
	}

	payload := map[string]any{
		"session_id":  jsonPath(raw, "session_id"),
		"tool":        tool,
		"tool_name":   tool,
		"args_digest": digest(raw),
		"cwd":         jsonPath(raw, "cwd"),
		"agent_kind":  agentKind(o),
	}
	if command := jsonPath(raw, "tool_input.command"); command != "" {
		payload["command"] = command
	}
	if fp := jsonPath(raw, "tool_input.file_path"); fp != "" {
		payload["file_path"] = fp
	}
	if fp := jsonPath(raw, "tool_input.path"); fp != "" && jsonPath(raw, "file_path") == "" {
		payload["file_path"] = fp
	}
	body, _ := json.Marshal(payload)

	client := &http.Client{
		Timeout: time.Duration(o.timeoutMS) * time.Millisecond,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		},
	}
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(o.url, "/")+"/api/hook/intake", bytes.NewReader(body))
	if err != nil {
		deny("AgentHub: request build failed: " + err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	if o.token != "" {
		req.Header.Set("Authorization", "Bearer "+o.token)
	}
	resp, err := client.Do(req)
	if err != nil {
		deny(fmt.Sprintf("AgentHub: hub unreachable (%s): %v", o.url, err))
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		text, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		deny(fmt.Sprintf("AgentHub: hub intake failed %d %s", resp.StatusCode, text))
	}
	var decision struct {
		Outcome   string `json:"outcome"`
		RuleScope string `json:"rule_scope"`
		Note      string `json:"note"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&decision); err != nil {
		deny("AgentHub: hub returned an unreadable verdict")
	}
	if decision.Outcome == "allowed_once" {
		scope := ""
		if decision.RuleScope != "" && decision.RuleScope != "none" {
			scope = fmt.Sprintf(" (rule_scope=%s)", decision.RuleScope)
		}
		note := ""
		if decision.Note != "" {
			note = " — " + decision.Note
		}
		allow("AgentHub: allowed" + scope + note)
	}
	deny("AgentHub: denied" + noteSuffix(decision.Note))
}

func noteSuffix(note string) string {
	if note == "" {
		return ""
	}
	return " — " + note
}

func agentKind(o options) string {
	if o.codex {
		return "codex"
	}
	return "claude_code"
}

func allow(reason string) {
	out, _ := json.Marshal(map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":            "PreToolUse",
			"permissionDecision":       "allow",
			"permissionDecisionReason": reason,
		},
	})
	fmt.Println(string(out))
	os.Exit(0)
}

func deny(reason string) {
	fmt.Fprintln(os.Stderr, reason)
	out, _ := json.Marshal(map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":            "PreToolUse",
			"permissionDecision":       "deny",
			"permissionDecisionReason": reason,
		},
	})
	fmt.Println(string(out))
	os.Exit(2) // Claude Code's "block this tool call" signal — fail closed
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "hub-hook: "+format+"\n", args...)
	os.Exit(1)
}

func readStdin() []byte {
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	if err != nil {
		return nil
	}
	return raw
}

func digest(raw []byte) string {
	s := string(raw)
	if len(s) > 4000 {
		s = s[:4000]
	}
	return s
}

// jsonPath reads "a.b.c" paths from the hook input without panicking on
// missing keys.
func jsonPath(raw []byte, path string) string {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	cur := m
	parts := strings.Split(path, ".")
	for i, p := range parts {
		v, ok := cur[p]
		if !ok {
			return ""
		}
		if i == len(parts)-1 {
			s, _ := v.(string)
			return s
		}
		next, ok := v.(map[string]any)
		if !ok {
			return ""
		}
		cur = next
	}
	return ""
}

func isLowRisk(tool string) bool {
	for _, t := range strings.Split(lowRiskTools, ",") {
		if strings.EqualFold(strings.TrimSpace(t), tool) {
			return true
		}
	}
	return false
}
