package server

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
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
	// CSP (R7-4): single embedded page needs unsafe-inline for script/style,
	// but connections are locked to same-origin (API + WS on the served host).
	// object-src 'none' / frame-ancestors 'none' harden against clickjacking
	// and plugin execution; connect-src is host-relative so alternate -addr
	// deployments stay functional.
	host := r.Host
	scheme := "ws"
	if r.TLS != nil {
		scheme = "wss"
	}
	w.Header().Set("Content-Security-Policy",
		"default-src 'self'; "+
			"script-src 'self' 'unsafe-inline'; "+
			"style-src 'self' 'unsafe-inline'; "+
			"connect-src 'self' "+scheme+"://"+host+"; "+
			"img-src 'self'; font-src 'self'; object-src 'none'; "+
			"base-uri 'self'; frame-ancestors 'none'")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(html))
}

// handleEvents serves the in-memory event ring (UI 稿 §11: the console
// replays the feed on load, then follows the live WS).
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	// Persisted stream first (vibe-trial B5: history survives restarts);
	// query params: ?limit=N (default 200), ?session=<id> filter.
	limit := queryInt(r, "limit", 200)
	sessionID := r.URL.Query().Get("session")
	if s.store != nil {
		if recs, err := s.store.ListUIEvents(r.Context(), limit, sessionID); err == nil && len(recs) > 0 {
			out := make([]UIEvent, 0, len(recs))
			for i := len(recs) - 1; i >= 0; i-- { // oldest first, like the ring
				e := recs[i]
				var env struct {
					Payload map[string]any `json:"payload"`
				}
				_ = json.Unmarshal(e.Payload, &env)
				ui := UIEvent{
					V:         2,
					EventID:   fmt.Sprintf("evt_%d", e.ID),
					TS:        time.Unix(e.Ts, 0).UTC().Format("2006-01-02T15:04:05.000Z07:00"),
					SessionID: e.SessionID,
					Type:      e.Type,
					Payload:   env.Payload,
				}
				if e.AgentKind != "" {
					ui.Agent = &UIEventAgent{Kind: e.AgentKind, Version: Version, InstanceID: "hub"}
				}
				out = append(out, ui)
			}
			writeJSON(w, http.StatusOK, map[string]any{"events": out})
			return
		}
	}
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

// truncateTitle clips a prompt into a session title (UTF-8 safe enough for
// v1: cut on byte boundary then drop a partial trailing rune).
func truncateTitle(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := s[:n]
	for i := 0; i < 3 && len(cut) > 0; i++ {
		if r := []byte(cut)[len(cut)-1]; r&0xC0 != 0x80 {
			break
		}
		cut = cut[:len(cut)-1]
	}
	return cut + "…"
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
	// Honest flags (vibe-trial B6): desktop mirrors web-push availability,
	// task_completion mirrors whether an IM webhook can actually receive
	// the task events; browser-granted switches stay client-side.
	writeJSON(w, http.StatusOK, map[string]any{
		"desktop":          s.push != nil,
		"feishu":           s.notifier != nil,
		"feishu_tail":      tail,
		"wecom":            s.wecom != nil,
		"wecom_tail":       wecomTail,
		"task_completion":  s.notifier != nil || s.wecom != nil,
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
	// Honor the requested channel when both are configured (vibe-trial S2:
	// the channel param used to be ignored and errors leaked the webhook
	// URL, which carries the signed secret).
	channel := strings.TrimSpace(r.URL.Query().Get("channel"))
	if channel == "" {
		channel = channelFromBody(r)
	}
	if channel == "wecom" && s.wecom == nil {
		writeError(w, http.StatusNotImplemented, "企微机器人未配置（-wecom-webhook / -wecom-aibot-*）")
		return
	}
	if channel == "feishu" && s.notifier == nil {
		writeError(w, http.StatusNotImplemented, "飞书 webhook 未配置（-feishu-webhook）")
		return
	}
	if channel == "" && s.wecom != nil {
		channel = "wecom"
	}
	if channel == "" {
		if s.notifier == nil {
			writeError(w, http.StatusNotImplemented, "no webhook notifier configured")
			return
		}
		channel = "feishu"
	}
	var err error
	if channel == "wecom" {
		err = s.wecom.Test(r.Context())
	} else {
		err = s.notifier.Test(r.Context())
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, sanitizeNotifyErr(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "channel": channel})
}

// channelFromBody pulls {"channel": "..."} out of the request body when the
// query string did not carry one.
func channelFromBody(r *http.Request) string {
	var body struct {
		Channel string `json:"channel"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(io.LimitReader(r.Body, 4*1024)).Decode(&body)
	}
	return strings.TrimSpace(body.Channel)
}

// sanitizeNotifyErr strips URLs (webhook URLs embed the signing secret) and
// caps the length before echoing a notifier error to the console.
func sanitizeNotifyErr(msg string) string {
	if i := strings.Index(msg, "http"); i >= 0 {
		msg = msg[:i] + "<url 已隐藏>"
	}
	msg = strings.TrimSpace(msg)
	if len(msg) > 200 {
		msg = msg[:200] + "…"
	}
	return msg
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
		ID:        fmt.Sprintf("%s-fork-%s", id, shortHex()),
		AgentKind: sess.AgentKind,
		CWD:       sess.CWD,
		Model:     sess.Model,
		Provider:  sess.Provider,
		// Ghost hygiene (vibe-trial B8): a fork has no agent behind it, so it
		// must not advertise origin=managed/active like a live dispatch —
		// that used to make it indistinguishable and even continuable.
		Status:          acpclient.StatusAwaitingInput,
		Origin:          "fork",
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
// terminalUnavailable explains why the terminal button cannot work for a
// given session (vibe-trial S17/D8: the old path handed out
// `codex resume task-*` — the Hub's synthetic id, which no real agent
// knows — so the command failed the moment it was pasted).
func terminalUnavailable(sess *acpclient.Session) (string, bool) {
	if sess.Origin == "managed" && strings.HasPrefix(sess.ID, "task-") && sess.AgentSessionID == "" {
		return "托管任务的会话 ID 由 Hub 合成，agent 不认识它，无法直接 resume；请使用「继续此会话」（重新派发）或在系统终端以 agent 自己的会话 ID 恢复", true
	}
	return "", false
}

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
	if why, bad := terminalUnavailable(sess); bad {
		writeError(w, http.StatusConflict, why)
		return
	}
	// R8: native resume targets the agent's own session id (agentSessionId,
	// captured from session/new); fall back to the Hub's task-* id only when
	// the agent never reported one (legacy runs).
	resumeID := sess.AgentSessionID
	if resumeID == "" {
		resumeID = sess.ID
	}
	cmd, ok := nativeResumeCommand(sess.AgentKind, resumeID)
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
	if why, bad := terminalUnavailable(sess); bad {
		writeError(w, http.StatusConflict, why)
		return
	}
	// R8: native resume targets the agent's own session id.
	resumeID := sess.AgentSessionID
	if resumeID == "" {
		resumeID = sess.ID
	}
	cmd, ok := nativeResumeCommand(sess.AgentKind, resumeID)
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
			// vibe-trial C2: the old check compared r.Host (the SERVER's own
			// host) against hardcoded :8799 constants — every Origin passed
			// on the default port, and every legit request failed on any
			// other port (terminal-ws 401 on :18899). Same-origin policy
			// now lives in one place: sameOriginOK.
			return sameOriginOK(r)
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

// handleFavicon serves the same mark as the inline data-URI in index.html, so
// WebViews that still probe /favicon.ico do not produce a 404.
func (s *Server) handleFavicon(w http.ResponseWriter, r *http.Request) {
	svg := `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32"><rect width="32" height="32" rx="7" fill="#e0a458"/><text x="16" y="23" font-size="21" font-family="sans-serif" font-weight="700" text-anchor="middle" fill="#1c1714">A</text></svg>`
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, svg)
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
		Text   string `json:"text"`
		Prompt string `json:"prompt"` // accepted alias (vibe-trial S4)
	}
	// Session existence first (vibe-trial S4: an unknown session used to
	// report the confusing "text is required").
	sess, err := s.store.GetSession(r.Context(), id)
	if err != nil {
		writeBridgeError(w, err)
		return
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		text = strings.TrimSpace(body.Prompt)
	}
	if text == "" {
		writeError(w, http.StatusBadRequest, "text is required")
		return
	}
	// A fork has no agent behind it — continuing one used to redispatch a
	// task with a ghost parent (vibe-trial B8).
	if sess.Origin == "fork" {
		writeError(w, http.StatusConflict, "分叉副本没有接入的 agent，无法继续；请对父会话继续或直接派发新任务")
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
			// Session control C-4: when the agent's REAL session id is known
			// (captured by C-1 runners), a continue is a RESUME TURN on the
			// same session row — `claude --resume <agentId>` / `codex exec
			// resume <agentId>` continue the same conversation, and events
			// keep flowing into this session (turns/answer accumulate).
			if sess.AgentSessionID != "" &&
				(sess.AgentKind == acpclient.AgentKindClaudeCode || sess.AgentKind == acpclient.AgentKindCodex) {
				t, err := s.tasks.EnqueueInSession(r.Context(), sess.AgentKind, sess.CWD, text, id)
				if err != nil {
					writeError(w, http.StatusConflict, err.Error())
					return
				}
				writeJSON(w, http.StatusOK, map[string]any{
					"ok": true, "mode": "session-resume", "task_id": t.ID,
					"session_id":        id,
					"agent_session_id":  sess.AgentSessionID,
				})
				return
			}
			// Create the child session row FIRST so the lineage is durable
			// regardless of scheduling (vibe-trial A10: writing it after
			// Enqueue raced the queue's async tryStart and silently lost).
			// tryStart's existing-session branch reuses this row.
			childID := "task-" + shortHex()
			child := &acpclient.Session{
				ID:              childID,
				AgentKind:       sess.AgentKind,
				CWD:             sess.CWD,
				Status:          acpclient.StatusActive,
				Origin:          acpclient.OriginManaged,
				ParentSessionID: id,
				Title:           truncateTitle(text, 80),
			}
			if err := s.store.CreateSession(r.Context(), child); err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
			t, err := s.tasks.EnqueueInSession(r.Context(), sess.AgentKind, sess.CWD, text, childID)
			if err != nil {
				writeError(w, http.StatusConflict, err.Error())
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"ok": true, "mode": "task-redispatch", "task_id": t.ID,
				"session_id":        childID,
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
