package server

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agenttarmac/agenttarmac/internal/acpclient"
	"github.com/agenttarmac/agenttarmac/internal/platform"
	"github.com/agenttarmac/agenttarmac/internal/pty"
	"github.com/agenttarmac/agenttarmac/internal/push"
	"github.com/agenttarmac/agenttarmac/internal/runner"

	"github.com/gorilla/websocket"
)

// The web console (UI 稿 v0.4 §11) is a single embedded page. The daemon
// injects window.__TARMAC_BOOT__ (token/version/WS addr) at serve time so the
// operator never pastes credentials; APIs and WS stay Bearer-protected.

var (
	webIndexOnce sync.Once
	webIndexHTML []byte
)

// handleIndex serves the console with the BOOT injection.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	webIndexOnce.Do(func() {
		raw, err := fs.ReadFile(webFS, "web/index.html")
		if err != nil {
			webIndexHTML = []byte("<!doctype html><title>AgentHub</title><p>console embed error</p>")
			return
		}
		webIndexHTML = raw
	})
	boot, err := json.Marshal(map[string]any{
		"token":   s.token,
		"version": Version,
		"addr":    "ws://" + r.Host + "/ws",
	})
	if err != nil {
		boot = []byte(`{"token":"","version":"","addr":""}`)
	}
	html := strings.Replace(string(webIndexHTML), "/*__BOOT__*/null", string(boot), 1)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(html))
}

// handleEvents serves the in-memory event ring (UI 稿 §11: the console
// replays the feed on load, then follows the live WS).
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"events": s.bridge.UIEvents()})
}

// handleAgents lists the registered agent kinds (roster tile).
func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request) {
	kinds := s.bridge.AgentKinds()
	agents := make([]map[string]any, 0, len(kinds))
	for _, k := range kinds {
		agents = append(agents, map[string]any{
			"kind":        k,
			"version":     Version,
			"instance_id": "hub",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": agents})
}

// handleRules exposes the policy rule table (PRD F3 / EVENT_MODEL v0.2
// §3.1): rules created by allow_session / allow_always verdicts.
func (s *Server) handleRules(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListRules(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	type ruleView struct {
		ID          string `json:"id"`
		Tool        string `json:"tool"`
		ArgPattern  string `json:"arg_pattern,omitempty"`
		Scope       string `json:"scope"`
		Effect      string `json:"effect"`
		AgentKind   string `json:"agent_kind,omitempty"`
		SessionID   string `json:"session_id,omitempty"`
		CreatedFrom string `json:"created_from,omitempty"`
	}
	rules := make([]ruleView, 0, len(list))
	for _, r := range list {
		rules = append(rules, ruleView{
			ID:          r.ID,
			Tool:        r.Tool,
			ArgPattern:  r.ArgPattern,
			Scope:       r.Effect + " · " + r.Scope,
			Effect:      r.Effect,
			AgentKind:   r.AgentKind,
			SessionID:   r.SessionID,
			CreatedFrom: r.CreatedFrom,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"rules": rules})
}

// handleDeleteRule revokes one policy rule (规则 view 删除).
func (s *Server) handleDeleteRule(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ok, err := s.store.RemoveRule(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "unknown rule "+id)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// mustJSON marshals v, returning null on failure (never used with
// unmarshalable values).
func mustJSON(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("null")
	}
	return raw
}

// handleNotifyStatus reports the notification channels (settings view).
func (s *Server) handleNotifyStatus(w http.ResponseWriter, r *http.Request) {
	tail := ""
	if s.notifier != nil {
		tail = s.notifier.WebhookTail()
	}
	wecomTail := ""
	if s.wecom != nil {
		wecomTail = s.wecom.WebhookTail()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"desktop":          true,
		"feishu":           s.notifier != nil,
		"feishu_tail":      tail,
		"wecom":            s.wecom != nil,
		"wecom_tail":       wecomTail,
		"task_completion":  true,
		"retention_days":   s.retentionDays,
		"version":          Version,
		"push":             s.push != nil,
		"push_subscribers": s.pushCount(),
		"vapid_public_key": s.vapidPublicKey(),
	})
}

// handleNotifyTest sends a test message over the configured notifier
// (WeCom takes precedence when set; otherwise Feishu).
func (s *Server) handleNotifyTest(w http.ResponseWriter, r *http.Request) {
	if s.wecom != nil {
		if err := s.wecom.Test(r.Context()); err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "channel": "wecom"})
		return
	}
	if s.notifier == nil {
		writeError(w, http.StatusNotImplemented, "no webhook notifier configured")
		return
	}
	if err := s.notifier.Test(r.Context()); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "channel": "feishu"})
}

// handleSessionFork creates a Hub-side fork of a session (PRD F7/D14:
// fork is the explicit secondary action; D15: agent-context resume for
// CC/Codex CLI arrives with the M2 native-resume work — the fork records
// lineage and lets the operator continue in a fresh session).
func (s *Server) handleSessionFork(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, err := s.store.GetSession(r.Context(), id)
	if err != nil {
		writeBridgeError(w, err)
		return
	}
	fork := &acpclient.Session{
		ID:              fmt.Sprintf("%s-fork-%s", id, shortHex()),
		AgentKind:       sess.AgentKind,
		CWD:             sess.CWD,
		Model:           sess.Model,
		Provider:        sess.Provider,
		Status:          acpclient.StatusActive,
		Origin:          sess.Origin,
		ParentSessionID: sess.ID,
		Title:           "fork: " + sess.Title,
	}
	if err := s.store.CreateSession(r.Context(), fork); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.bridge.RecordFork(sess, fork)
	writeJSON(w, http.StatusOK, map[string]any{"sessionId": fork.ID, "parentSessionId": fork.ParentSessionID})
}

// nativeResumeCommand builds the system-terminal resume command for agent
// kinds with native interactive resume (EVENT_MODEL §6 table); ACP-only
// kinds have none, so the terminal button hides for them (PRD F8).
func nativeResumeCommand(kind, sessionID string) (string, bool) {
	switch kind {
	case acpclient.AgentKindClaudeCode:
		return fmt.Sprintf("claude --resume %s", sessionID), true
	case acpclient.AgentKindCodex:
		return fmt.Sprintf("codex resume %s", sessionID), true
	default:
		return "", false
	}
}

// handleSessionTerminal opens the OS terminal in the session cwd resuming the
// session (PRD F8/D15: 一键拉起系统终端).
func (s *Server) handleSessionTerminal(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, err := s.store.GetSession(r.Context(), id)
	if err != nil {
		writeBridgeError(w, err)
		return
	}
	cmd, ok := nativeResumeCommand(sess.AgentKind, sess.ID)
	if !ok {
		writeError(w, http.StatusConflict, "该 Agent 类型无原生交互式恢复命令，请在 Hub 内查看（PRD F8）")
		return
	}
	if err := s.platform.OpenSessionTerminal(r.Context(), platform.TerminalRequest{CWD: sess.CWD, Command: cmd}); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "command": cmd, "cwd": sess.CWD})
}

// handleSessionTerminalWS is the browser-embedded terminal endpoint (PRD: xterm.js +
// ConPTY + WebSocket). It opens a ConPTY session running the agent's native resume
// command in the session cwd and bridges PTY <-> WebSocket bidirectionally.
// It reuses the same session/auth model as the rest of the console.
func (s *Server) handleSessionTerminalWS(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, err := s.store.GetSession(r.Context(), id)
	if err != nil {
		writeBridgeError(w, err)
		return
	}
	cmd, ok := nativeResumeCommand(sess.AgentKind, sess.ID)
	if !ok {
		writeError(w, http.StatusConflict, "该 Agent 类型无内嵌交互式终端，请使用系统终端（PRD F8）")
		return
	}
	// conpty.Start needs a real executable; claude/codex live as *.ps1 wrappers
	// in the npm bin dir, so run via powershell on Windows.
	ptyCmd := `powershell.exe -NoProfile -Command "` + cmd + `"`
	ptySess, err := pty.NewSession(r.Context(), ptyCmd, 120, 30)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.ptyMu.Lock()
	s.ptySessions[id] = ptySess
	s.ptyMu.Unlock()
	defer func() {
		s.ptyMu.Lock()
		delete(s.ptySessions, id)
		s.ptyMu.Unlock()
		_ = ptySess.Close()
	}()

	upgrader := websocket.Upgrader{
		ReadBufferSize:  4 * 1024,
		WriteBufferSize: 4 * 1024,
		CheckOrigin: func(r *http.Request) bool {
			// 仅允许同源或本地回环，防止跨站 WebSocket 劫持（PRD §9）。
			host := r.Host
			if host == "" {
				host = r.URL.Host
			}
			switch host {
			case "127.0.0.1:8799", "localhost:8799":
				return true
			default:
				// 允许显式配置的额外源（逗号分隔，env AGENTHUB_WS_ALLOW_ORIGINS）
				if allowed := os.Getenv("AGENTHUB_WS_ALLOW_ORIGINS"); allowed != "" {
					for _, o := range strings.Split(allowed, ",") {
						if o := strings.TrimSpace(o); o != "" && o == host {
							return true
						}
					}
				}
				return false
			}
		},
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer conn.Close()

	writeToWS := func(b []byte) error {
		return conn.WriteMessage(websocket.BinaryMessage, b)
	}
	readFromWS := func() ([]byte, error) {
		_, msg, err := conn.ReadMessage()
		return msg, err
	}

	ctx := r.Context()
	if err := pty.Pump(ctx, ptySess, writeToWS, readFromWS); err != nil && err != context.Canceled {
		s.log.Printf("pty pump ended for session %s: %v", id, err)
	}
}
func (s *Server) pushCount() int {
	if s.push == nil {
		return 0
	}
	return s.push.Count()
}

// vapidPublicKey exposes the VAPID public key for browser subscriptions.
func (s *Server) vapidPublicKey() string {
	if s.push == nil {
		return ""
	}
	return s.push.PublicKey()
}

// shortHex generates a short random suffix for fork ids.
func shortHex() string {
	buf := make([]byte, 3)
	if _, err := rand.Read(buf); err != nil {
		return strconv.FormatInt(time.Now().UnixNano()%0xffffff, 16)
	}
	return hex.EncodeToString(buf)
}

// handlePushSubscribe registers a browser push subscription (PRD F9/M1.5).
func (s *Server) handlePushSubscribe(w http.ResponseWriter, r *http.Request) {
	if s.push == nil {
		writeError(w, http.StatusNotImplemented, "web push not configured")
		return
	}
	var sub push.Subscription
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&sub); err != nil {
		writeError(w, http.StatusBadRequest, "invalid subscription: "+err.Error())
		return
	}
	if sub.Endpoint == "" {
		writeError(w, http.StatusBadRequest, "endpoint is required")
		return
	}
	s.push.Subscribe(sub)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleVendor serves the small, fixed allowlist of locally vendored terminal
// assets. A literal allowlist (not a joined path) keeps path traversal out.
func (s *Server) handleVendor(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var contentType string
	switch name {
	case "xterm.js", "addon-fit.js":
		contentType = "text/javascript; charset=utf-8"
	case "xterm.css":
		contentType = "text/css; charset=utf-8"
	default:
		http.NotFound(w, r)
		return
	}
	raw, err := fs.ReadFile(webFS, "web/vendor/"+name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

// handleSW serves the Service Worker (same-origin requirement for push).
func (s *Server) handleSW(w http.ResponseWriter, r *http.Request) {
	raw, err := fs.ReadFile(webFS, "web/sw.js")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Service-Worker-Allowed", "/")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

// handleSessionContinue is PRD F7's "继续此会话". Three session flavours need
// different transport:
//
//   - F6 task-queue sessions (origin=managed, id "task-*"): the driving CLI was
//     a one-shot headless run that already exited, so no connector owns the
//     session id. Re-dispatch a fresh task in the same cwd / agent kind, which
//     reuses the work context and shows up in the task ledger.
//   - Connector-hosted managed ACP sessions (origin=managed, other ids): ride
//     the Hub→agent command bus, with prompt as ACP content blocks.
//   - Attached claude_code / codex sessions: resume through the vendor's
//     native headless command (internal/runner).
func (s *Server) handleSessionContinue(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		writeError(w, http.StatusBadRequest, "text is required")
		return
	}
	sess, err := s.store.GetSession(r.Context(), id)
	if err != nil {
		writeBridgeError(w, err)
		return
	}
	if sess.Origin == acpclient.OriginManaged {
		// F6 tasks are one-shot CLI invocations. Broadcasting session/prompt
		// for their synthetic id reaches unrelated connectors, which reject the
		// unknown session (and a bare-string prompt) and mark the turn failed.
		// Re-dispatching in the same workspace is the honest "reuse" semantic.
		if strings.HasPrefix(id, "task-") {
			if s.tasks == nil {
				writeError(w, http.StatusConflict, "任务队列不可用，无法重新派发该会话")
				return
			}
			t, err := s.tasks.Enqueue(r.Context(), sess.AgentKind, sess.CWD, text)
			if err != nil {
				writeError(w, http.StatusConflict, err.Error())
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"ok": true, "mode": "task-redispatch", "task_id": t.ID,
				"session_id":        "task-" + strings.TrimPrefix(t.ID, "task_"),
				"parent_session_id": id,
			})
			return
		}
		// Connector-hosted managed ACP: forward over the command bus. ACP
		// session/prompt takes content blocks, not a bare string; the connector
		// flattens them into text for Hub bookkeeping and forwards the original
		// params to the agent verbatim.
		sent, err := s.bridge.SendAgentRequest(AgentRequest{
			AgentKind: sess.AgentKind,
			Method:    "session/prompt",
			Params: mustJSON(map[string]any{
				"sessionId": id,
				"prompt":    []any{map[string]any{"type": "text", "text": text}},
			}),
		})
		if err != nil {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "session_id": id, "mode": "managed", "requestId": sent.RequestID})
		return
	}
	if s.runner == nil || !runner.SupportsResume(sess.AgentKind) {
		writeError(w, http.StatusConflict, "该会话不支持原生续跑（仅 attached 的 claude_code/codex）")
		return
	}
	newID, started, err := s.runner.Continue(r.Context(), sess.AgentKind, sess.ID, sess.CWD, body.Text)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if !started {
		writeError(w, http.StatusConflict, "该会话已有续跑在进行中")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "session_id": newID, "mode": "native-resume"})
}

// handleHookIntake is the blocking call a Claude Code PreToolUse / Codex
// PermissionRequest hook makes (PRD F3). Flow: decode → route to the
// agent-kind adapter → gate (rules → memory → risk → human) → wait for the
// verdict (or time out, failing closed) → return the verdict as JSON.
func (s *Server) handleHookIntake(w http.ResponseWriter, r *http.Request) {
	var in acpclient.HookIntake
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	a, err := s.bridge.AdapterFor(in.AgentKindOr(acpclient.AgentKindClaudeCode))
	if err != nil {
		// Unknown agent kind: fail closed so the agent never runs ungated.
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"outcome":    acpclient.OutcomeUnavailable,
			"rule_scope": acpclient.ScopeNone,
			"note":       err.Error(),
		})
		return
	}
	dec, requestID, err := a.IntakeToolCall(r.Context(), in, acpclient.IntakeTimeout)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"outcome":    acpclient.OutcomeUnavailable,
			"rule_scope": acpclient.ScopeNone,
			"note":       "intake failed: " + err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"request_id": requestID,
		"outcome":    dec.Outcome,
		"rule_scope": dec.RuleScope,
		"note":       dec.Note,
	})
}

// ensure embed is used through webFS in server.go (compile-time guard).
var _ = embed.FS{}
