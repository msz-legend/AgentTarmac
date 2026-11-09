package server

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/agenttarmac/agenttarmac/internal/acpclient"
	"github.com/agenttarmac/agenttarmac/internal/platform"
	"github.com/agenttarmac/agenttarmac/internal/pty"
	"github.com/agenttarmac/agenttarmac/internal/push"
	"github.com/agenttarmac/agenttarmac/internal/runner"
)

// Server is the AgentHub daemon: HTTP + WS API with Bearer auth over the ACP
// bridge, plus the embedded Web console (Hub UI).
type Server struct {
	bridge        *Bridge
	store         *acpclient.Store
	token         string
	log           *log.Logger
	notifier      *FeishuNotifier
	wecom         *WecomNotifier
	push          *push.Manager
	runner        *runner.Runner
	tasks         TaskQueue
	platform      platform.Provider
	retentionDays int
	http          *http.Server
	ptySessions   map[string]*pty.Session
	ptyMu         sync.Mutex
	fsBrowser     *fsBrowser
}

//go:embed web
var webFS embed.FS

// NewServer builds the daemon around a bridge and store. token is the Bearer
// credential required on every route except /healthz and the static console.
func NewServer(addr string, bridge *Bridge, store *acpclient.Store, token string, logger *log.Logger) *Server {
	if logger == nil {
		logger = log.Default()
	}
	s := &Server{
		bridge:      bridge,
		store:       store,
		token:       token,
		log:         logger,
		platform:    platform.New(),
		ptySessions: make(map[string]*pty.Session),
	}
	hs := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	s.http = hs
	return s
}

// SetNotifier attaches a Feishu notifier; permission requests are pushed
// asynchronously from EmitFrame.
func (s *Server) SetNotifier(n *FeishuNotifier) { s.notifier = n }

// SetPush attaches the Web Push manager (PRD F9/M1.5: 关标签页仍可达).
func (s *Server) SetPush(m *push.Manager) { s.push = m }

// SetWecomNotifier attaches the WeCom channel (PRD M2).
func (s *Server) SetWecomNotifier(n *WecomNotifier) { s.wecom = n }

// SetRunner attaches the native session resume runner (PRD F7).
func (s *Server) SetRunner(r *runner.Runner) { s.runner = r }

// TaskQueue is the F6 task surface the server needs (subset of *tasks.Queue).
type TaskQueue interface {
	Enqueue(ctx context.Context, agentKind, cwd, prompt string) (*acpclient.Task, error)
	GetTask(ctx context.Context, id string) (*acpclient.Task, error)
	ListTasks(ctx context.Context, state string) ([]*acpclient.Task, error)
	Retry(ctx context.Context, id string) (*acpclient.Task, error)
	Cancel(ctx context.Context, id string) (*acpclient.Task, error)
	Pause()
	Resume()
	Paused() bool
}

// SetTasks attaches the F6 task queue.
func (s *Server) SetTasks(q TaskQueue) { s.tasks = q }

// SetRetention records the session retention policy (settings view, 决策 G).
func (s *Server) SetRetention(days int) { s.retentionDays = days }

// SetFSBrowser attaches the server-side directory browser / recent-CWD memory.
// Called once at daemon boot with the same -data directory as the store.
func (s *Server) SetFSBrowser(dataDir string) {
	s.fsBrowser = newFsBrowser(dataDir)
}

// EmitFrame implements acpclient.Sink: outbound adapter frames are broadcast
// to WS subscribers and recorded into the console event ring. Permission
// requests additionally trigger the Feishu notifier (decision F) when one is
// configured.
func (s *Server) EmitFrame(method string, params json.RawMessage) {
	s.bridge.broadcast(map[string]any{
		"type":   "frame.out",
		"ts":     time.Now().Unix(),
		"method": method,
		"params": params,
	})
	s.bridge.RecordFrameOut(method, params)
	if method == "session/request_permission" && s.notifier != nil {
		var p struct {
			SessionID      string `json:"sessionId"`
			RequestID      string `json:"requestId"`
			Name           string `json:"name"`
			Risk           string `json:"risk"`
			CommandPattern string `json:"commandPattern"`
			Reason         string `json:"reason"`
		}
		if json.Unmarshal(params, &p) == nil {
			// PRD D17/F9 policy: only HIGH-risk pending approvals go to IM;
			// medium/low stay desktop-only. Delivery is async and never
			// blocks the approval gate.
			if p.Risk == acpclient.RiskHigh {
				go s.notifier.NotifyPermissionRequest(p.SessionID, p.RequestID, p.Name, p.Risk, p.CommandPattern, p.Reason)
			}
		}
	}
	if method == "session/request_permission" && s.wecom != nil {
		var p struct {
			SessionID string `json:"sessionId"`
			Name      string `json:"name"`
			Risk      string `json:"risk"`
			Reason    string `json:"reason"`
		}
		if json.Unmarshal(params, &p) == nil && p.Risk == acpclient.RiskHigh {
			go s.wecom.NotifyPermissionRequest(p.SessionID, p.Name, p.Risk, p.Reason)
		}
	}
	if method == "session/request_permission" && s.push != nil {
		var p struct {
			SessionID string `json:"sessionId"`
			Name      string `json:"name"`
			Risk      string `json:"risk"`
		}
		if json.Unmarshal(params, &p) == nil && p.Risk == acpclient.RiskHigh {
			// PRD F9 acceptance: high-risk approvals reach the operator even
			// with every Hub tab closed (Service Worker + Web Push).
			go s.push.Notify("AgentHub · 待放行", labelSnippet(p.Name, p.Risk, p.SessionID), "agenthub:perm:"+p.SessionID)
		}
	}
	if method == "session/update" {
		var p struct {
			SessionID string `json:"sessionId"`
			Status    string `json:"status"`
			Error     string `json:"error"`
		}
		if json.Unmarshal(params, &p) == nil && p.Status == acpclient.StatusFailed {
			// PRD F9: 任务失败立即推送（IM + push 通道）。
			if s.notifier != nil {
				go s.notifier.NotifySessionFailed(p.SessionID, p.Error)
			}
			if s.wecom != nil {
				go s.wecom.NotifySessionFailed(p.SessionID, p.Error)
			}
			if s.push != nil {
				go s.push.Notify("AgentHub · 会话失败", snippet(p.SessionID, p.Error), "agenthub:failed:"+p.SessionID)
			}
		}
	}
}

// labelSnippet builds a notification summary (no command text — PRD F9
// security rule).
func labelSnippet(tool, risk, sessionID string) string {
	s := tool + " · risk:" + risk
	if len(sessionID) > 8 {
		s += " · " + sessionID[:8]
	}
	return s
}

func snippet(sessionID, reason string) string {
	s := sessionID
	if len(s) > 12 {
		s = s[:12]
	}
	if reason != "" {
		s += " · " + reason
	}
	return s
}

// ListenAndServe runs the daemon until Shutdown is called.
func (s *Server) ListenAndServe() error {
	err := s.http.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown gracefully stops the daemon.
func (s *Server) Shutdown() error {
	return s.http.Close()
}

// Addr reports the configured listen address.
func (s *Server) Addr() string { return s.http.Addr }

// Handler assembles the route table (exported for tests).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.handleHealthz)

	mux.HandleFunc("POST /acp/frame", s.auth(s.handleFrame))
	mux.HandleFunc("POST /api/hook/intake", s.auth(s.handleHookIntake))
	mux.HandleFunc("POST /api/agent-requests", s.auth(s.handleAgentRequest))
	mux.HandleFunc("POST /api/agent-requests/{id}/response", s.auth(s.handleAgentResponse))
	mux.HandleFunc("GET /api/sessions", s.auth(s.handleSessions))
	mux.HandleFunc("GET /api/sessions/{id}/export", s.auth(s.handleSessionExport))
	mux.HandleFunc("POST /api/sessions/{id}/fork", s.auth(s.handleSessionFork))
	mux.HandleFunc("POST /api/sessions/{id}/continue", s.auth(s.handleSessionContinue))
	mux.HandleFunc("POST /api/tasks", s.auth(s.handleTaskEnqueue))
	mux.HandleFunc("GET /api/tasks", s.auth(s.handleTaskList))
	mux.HandleFunc("POST /api/tasks/{id}/retry", s.auth(s.handleTaskRetry))
	mux.HandleFunc("POST /api/tasks/{id}/cancel", s.auth(s.handleTaskCancel))
	mux.HandleFunc("POST /api/tasks/queue/pause", s.auth(s.handleTaskPause))
	mux.HandleFunc("POST /api/tasks/queue/resume", s.auth(s.handleTaskResume))
	mux.HandleFunc("POST /api/sessions/{id}/terminal", s.auth(s.handleSessionTerminal))
	mux.HandleFunc("GET /api/sessions/{id}/terminal-ws", s.auth(s.handleSessionTerminalWS))
	mux.HandleFunc("GET /api/tool-calls", s.auth(s.handleToolCalls))
	mux.HandleFunc("GET /api/permission-requests", s.auth(s.handlePermissionRequests))
	mux.HandleFunc("POST /api/permission-requests/{id}/decision", s.auth(s.handlePermissionDecision))
	mux.HandleFunc("GET /api/approvals", s.auth(s.handleApprovals))
	mux.HandleFunc("GET /api/approval-memory", s.auth(s.handleApprovalMemory))
	mux.HandleFunc("DELETE /api/approval-memory/{id}", s.auth(s.handleDeleteApprovalMemory))
	mux.HandleFunc("GET /api/notifications", s.auth(s.handleNotifications))

	// Web console data endpoints (UI 稿 §12.3 contract, adapted to this
	// daemon's vocabulary).
	mux.HandleFunc("GET /api/events", s.auth(s.handleEvents))
	mux.HandleFunc("GET /api/agents", s.auth(s.handleAgents))
	mux.HandleFunc("GET /api/rules", s.auth(s.handleRules))
	mux.HandleFunc("DELETE /api/rules/{id}", s.auth(s.handleDeleteRule))
	mux.HandleFunc("GET /api/notify/status", s.auth(s.handleNotifyStatus))
	mux.HandleFunc("POST /api/notify/test", s.auth(s.handleNotifyTest))
	mux.HandleFunc("POST /api/push/subscribe", s.auth(s.handlePushSubscribe))
	mux.HandleFunc("GET /sw.js", s.handleSW)
	// Locally vendored terminal assets: no CDN dependency, so the console
	// renders (including the embedded terminal) offline and on mobile.
	mux.HandleFunc("GET /vendor/{name}", s.handleVendor)
	// Some browsers/WebViews still probe /favicon.ico despite the inline data
	// URI in index.html. Serve the same mark so no 404 shows up in the log.
	mux.HandleFunc("GET /favicon.ico", s.handleFavicon)

	// File-system browser for task dispatch working-directory selection
	// (PC + Web). Read-only browse + persistent recent-directory memory.
	mux.HandleFunc("GET /api/fs/roots", s.auth(s.handleFSRoots))
	mux.HandleFunc("GET /api/fs/list", s.auth(s.handleFSList))
	mux.HandleFunc("GET /api/fs/recent", s.auth(s.handleFSRecentList))
	mux.HandleFunc("POST /api/fs/recent", s.auth(s.handleFSRecentAdd))

	mux.HandleFunc("GET /ws", s.auth(s.handleWS))

	// Embedded Web console: the index carries a BOOT injection (token +
	// version) so the operator never pastes credentials; APIs and the WS
	// stream stay Bearer-protected.
	mux.HandleFunc("GET /{$}", s.handleIndex)

	return mux
}

// ------------------------------------------------------------------- routes

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":     "ok",
		"agentKinds": s.bridge.AgentKinds(),
		"time":       time.Now().Format(time.RFC3339),
	})
}

// handleFrame accepts one inbound ACP frame.
func (s *Server) handleFrame(w http.ResponseWriter, r *http.Request) {
	var f InboundFrame
	if err := json.NewDecoder(r.Body).Decode(&f); err != nil {
		writeError(w, http.StatusBadRequest, "invalid frame body: "+err.Error())
		return
	}
	result, err := s.bridge.DispatchFrame(f)
	if err != nil {
		writeBridgeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"method": f.Method,
		"result": result,
	})
}

// handleTaskEnqueue dispatches one managed task (PRD F6: 派发).
func (s *Server) handleTaskEnqueue(w http.ResponseWriter, r *http.Request) {
	if s.tasks == nil {
		writeError(w, http.StatusNotImplemented, "task queue not configured")
		return
	}
	var body struct {
		AgentKind string `json:"agentKind"`
		CWD       string `json:"cwd"`
		Prompt    string `json:"prompt"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	t, err := s.tasks.Enqueue(r.Context(), body.AgentKind, body.CWD, body.Prompt)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// handleTaskList returns the task ledger.
func (s *Server) handleTaskList(w http.ResponseWriter, r *http.Request) {
	if s.tasks == nil {
		writeJSON(w, http.StatusOK, map[string]any{"tasks": nil, "paused": false})
		return
	}
	list, err := s.tasks.ListTasks(r.Context(), r.URL.Query().Get("state"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": list, "paused": s.tasks.Paused()})
}

// handleTaskRetry requeues a failed task.
func (s *Server) handleTaskRetry(w http.ResponseWriter, r *http.Request) {
	t, err := s.tasks.Retry(r.Context(), r.PathValue("id"))
	if err != nil {
		writeBridgeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// handleTaskCancel cancels a queued or running task.
func (s *Server) handleTaskCancel(w http.ResponseWriter, r *http.Request) {
	t, err := s.tasks.Cancel(r.Context(), r.PathValue("id"))
	if err != nil {
		writeBridgeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// handleTaskPause pauses queue dispatching (D13: 可暂停续跑).
func (s *Server) handleTaskPause(w http.ResponseWriter, r *http.Request) {
	s.tasks.Pause()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "paused": true})
}

// handleTaskResume re-enables queue dispatching.
func (s *Server) handleTaskResume(w http.ResponseWriter, r *http.Request) {
	s.tasks.Resume()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "paused": false})
}

// handleSessionExport streams one session's archive bundle as a JSON (or
// Markdown, PRD 5.6) download (decision G: 支持导出).
func (s *Server) handleSessionExport(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	bundle, err := s.store.ExportSession(r.Context(), id)
	if err != nil {
		writeBridgeError(w, err)
		return
	}
	if r.URL.Query().Get("format") == "md" {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"agenthub-session-%s.md\"", id))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(renderSessionMarkdown(bundle)))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"agenthub-session-%s.json\"", id))
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(bundle)
}

// renderSessionMarkdown renders the export bundle as a readable Markdown
// transcript (PRD 5.6: 导出 JSON/Markdown).
func renderSessionMarkdown(b *acpclient.SessionExport) string {
	var sb strings.Builder
	s := b.Session
	if s == nil {
		return "# AgentHub session export\n\n(empty)\n"
	}
	fmt.Fprintf(&sb, "# 会话 %s\n\n", s.ID)
	fmt.Fprintf(&sb, "- Agent: %s\n- CWD: `%s`\n- 模型: %s (%s)\n- 状态: %s\n- 回合: %d\n- Tokens: 入 %d / 出 %d\n- 标题: %s\n- 来源: %s\n",
		s.AgentKind, s.CWD, s.Model, s.Provider, s.Status, s.Turns, s.InputTokens, s.OutputTokens, s.Title, s.Origin)
	fmt.Fprintf(&sb, "\n## 工具调用\n\n| # | 工具 | 状态 | 风险 | 输入 |\n|---|------|------|------|------|\n")
	for i, tc := range b.ToolCalls {
		in := tc.RawInput
		if len(in) > 200 {
			in = in[:200] + "…"
		}
		fmt.Fprintf(&sb, "| %d | %s | %s | %s | `%s` |\n", i+1, tc.ToolName, tc.ToolStatus, tc.Risk, in)
	}
	fmt.Fprintf(&sb, "\n## 权限请求\n\n| 请求 | 工具 | 风险 | 结果 | 判决 |\n|------|------|------|------|------|\n")
	for _, pr := range b.PermissionRequests {
		fmt.Fprintf(&sb, "| %s | %s | %s | %s | %s |\n", pr.RequestUID, pr.ToolName, pr.Risk, pr.Outcome, pr.OptionID)
	}
	fmt.Fprintf(&sb, "\n## 判决历史\n\n| 判决 | 来源 | 备注 |\n|------|------|------|\n")
	for _, a := range b.Approvals {
		fmt.Fprintf(&sb, "| %s | %s | %s |\n", a.Decision, a.DecisionBy, a.Notes)
	}
	return sb.String()
}

// handleAgentRequest queues a Hub→agent command onto the WS bus; the stdio
// connector forwards it to the connected agent and posts the answer back to
// /api/agent-requests/{id}/response.
func (s *Server) handleAgentRequest(w http.ResponseWriter, r *http.Request) {
	var req AgentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid agent request body: "+err.Error())
		return
	}
	sent, err := s.bridge.SendAgentRequest(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sent)
}

// handleAgentResponse accepts the connector's answer for a Hub→agent request.
func (s *Server) handleAgentResponse(w http.ResponseWriter, r *http.Request) {
	var resp AgentResponse
	resp.RequestID = r.PathValue("id")
	if resp.RequestID == "" {
		writeError(w, http.StatusBadRequest, "request id is required")
		return
	}
	if err := json.NewDecoder(r.Body).Decode(&resp); err != nil {
		writeError(w, http.StatusBadRequest, "invalid agent response body: "+err.Error())
		return
	}
	s.bridge.ResolveAgentResponse(resp)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	sessions, err := s.store.ListSessions(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
}

func (s *Server) handleToolCalls(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("sessionId")
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, "sessionId query parameter is required")
		return
	}
	calls, err := s.store.ListToolCalls(r.Context(), sessionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"toolCalls": calls})
}

func (s *Server) handlePermissionRequests(w http.ResponseWriter, r *http.Request) {
	outcome := r.URL.Query().Get("outcome")
	reqs, err := s.store.ListPermissionRequests(r.Context(), outcome)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"permissionRequests": reqs})
}

// handlePermissionDecision applies a human verdict (decision B) to a pending
// permission request identified by its request UID.
func (s *Server) handlePermissionDecision(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("id")
	var dec acpclient.PermissionDecision
	if err := json.NewDecoder(r.Body).Decode(&dec); err != nil {
		writeError(w, http.StatusBadRequest, "invalid decision body: "+err.Error())
		return
	}
	if _, err := acpclient.NormalizeDecision(dec.Decision); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := s.bridge.ResolvePermission("", uid, dec)
	if err != nil {
		writeBridgeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleApprovals(w http.ResponseWriter, r *http.Request) {
	limit := queryInt(r, "limit", 100)
	records, err := s.store.ListApprovals(r.Context(), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"approvals": records})
}

func (s *Server) handleApprovalMemory(w http.ResponseWriter, r *http.Request) {
	mem, err := s.store.ListApprovalMemory(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"approvalMemory": mem})
}

// handleDeleteApprovalMemory revokes a persisted verdict (Hub UI 撤销 action).
func (s *Server) handleDeleteApprovalMemory(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid memory id")
		return
	}
	if err := s.store.DeleteApprovalMemory(r.Context(), id); err != nil {
		writeBridgeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleNotifications(w http.ResponseWriter, r *http.Request) {
	limit := queryInt(r, "limit", 100)
	records, err := s.store.ListNotifications(r.Context(), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"notifications": records})
}

// ------------------------------------------------------------------- websocket

var upgrader = websocket.Upgrader{
	// Local-only daemon; token auth is the real boundary.
	CheckOrigin: func(r *http.Request) bool { return true },
}

// handleWS streams bridge events (frame.in / frame.out / permission.resolved)
// to Hub UI listeners.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade already wrote the error
	}
	defer conn.Close()

	events := s.bridge.Subscribe()
	defer s.bridge.Unsubscribe(events)

	// hello tells the client the subscription is live, so listeners that care
	// about ordering can wait for it before triggering work.
	if hello, err := json.Marshal(map[string]any{"type": "hello", "ts": time.Now().Unix()}); err == nil {
		select {
		case events <- append(hello, '\n'):
		default:
		}
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Reader: drain client messages (pings/close) so writes can proceed.
		conn.SetReadLimit(1 << 20)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-done:
			return
		case evt := <-events:
			if err := conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
				return
			}
			if err := conn.WriteMessage(websocket.TextMessage, evt); err != nil {
				return
			}
		case <-ticker.C:
			if err := conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
				return
			}
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// ------------------------------------------------------------------- auth

// auth wraps a handler with Bearer token verification.
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := bearerToken(r)
		if tok == "" {
			writeError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		if subtle.ConstantTimeCompare([]byte(tok), []byte(s.token)) != 1 {
			writeError(w, http.StatusUnauthorized, "invalid bearer token")
			return
		}
		next(w, r)
	}
}

func bearerToken(r *http.Request) string {
	// WS clients cannot always set headers; allow ?token= fallback.
	if h := r.Header.Get("Authorization"); h != "" {
		if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
			return strings.TrimSpace(h[7:])
		}
		return ""
	}
	if q := r.URL.Query().Get("token"); q != "" {
		return q
	}
	return ""
}

// ------------------------------------------------------------- responses

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}

// writeBridgeError maps adapter/bridge errors onto HTTP statuses.
func writeBridgeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, acpclient.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case strings.Contains(err.Error(), "already resolved"), strings.Contains(err.Error(), "already exists"):
		writeError(w, http.StatusConflict, err.Error())
	case strings.Contains(err.Error(), "no adapter registered"):
		writeError(w, http.StatusNotFound, err.Error())
	default:
		writeError(w, http.StatusBadRequest, err.Error())
	}
}

func queryInt(r *http.Request, key string, def int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	n := 0
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil || n <= 0 {
		return def
	}
	return n
}
