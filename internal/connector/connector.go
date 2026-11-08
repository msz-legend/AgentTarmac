// Package connector bridges a real ACP agent subprocess (JSON-RPC over stdio)
// to the AgentHub daemon:
//
//	agent → Hub   : notifications (session/update, tool_call, …) are POSTed to
//	                /acp/frame; agent-initiated requests (session/request_permission)
//	                route through the Hub approval gate, and the human decision
//	                is mapped back onto the agent's own option ids.
//	Hub → agent   : agent.request WS events (session/new, session/prompt, …)
//	                are written to the agent's stdin; the agent's JSON-RPC
//	                responses are posted back to the daemon, which broadcasts
//	                them as agent.response events.
//
// Unsupported client-side capabilities (fs/* beyond read/write_text_file,
// terminal/*) are answered with a JSON-RPC error instead of hanging.
package connector

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// fsReadLimit caps one fs/read_text_file answer (ACP agents read source
// files; a multi-MB read is either a binary probe or a runaway prompt).
const fsReadLimit = 2 << 20 // 2 MiB

// Config configures one connector run.
type Config struct {
	DaemonURL string // e.g. http://127.0.0.1:8799
	Token     string // Bearer token for the daemon API
	AgentKind string // defaults to claude_code
	Command   string // optional agent command; empty = passthrough stdin/stdout
	Args      []string
	CWD       string // agent working directory (surfaces in approval requests)
	Log       *log.Logger
}

// jsonrpcID is a JSON-RPC id kept verbatim (number or string).
type jsonrpcID = json.RawMessage

type rpcMsg struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      jsonrpcID       `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}

// pendingPerm tracks an agent permission request awaiting a Hub decision.
type pendingPerm struct {
	AgentID jsonrpcID
	Options []agentOption
}

type agentOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name,omitempty"`
	Kind     string `json:"kind,omitempty"`
}

// pendingForward tracks a Hub→agent request awaiting the agent's JSON-RPC answer.
type pendingForward struct {
	DaemonRequestID string
	Method          string
	Params          json.RawMessage
	SentAt          time.Time
}

// Connector wires one agent process to one daemon.
type Connector struct {
	cfg    Config
	client *http.Client
	log    *log.Logger

	writeMu sync.Mutex
	stdin   io.Writer

	forwardMu   sync.Mutex
	forwarded   map[string]*pendingForward // local jsonrpc id (as string) → forward
	forwardSeq  int64
	permMu      sync.Mutex
	permissions map[string]*pendingPerm // Hub requestId → pending perm

	readyOnce sync.Once
	ready     chan struct{}
}

// New creates a connector.
func New(cfg Config) *Connector {
	if cfg.AgentKind == "" {
		cfg.AgentKind = "claude_code"
	}
	if cfg.Log == nil {
		cfg.Log = log.Default()
	}
	return &Connector{
		cfg:         cfg,
		client:      &http.Client{Timeout: 30 * time.Second},
		log:         cfg.Log,
		forwarded:   map[string]*pendingForward{},
		permissions: map[string]*pendingPerm{},
		ready:       make(chan struct{}),
	}
}

// Ready closes when the connector's WS subscription to the daemon is live
// (agent.request events are guaranteed to arrive after that).
func (c *Connector) Ready() <-chan struct{} { return c.ready }

// Run drives the connector until ctx is cancelled or the agent's stdout ends.
// stdin/stdout are the agent's stdio (the command's pipes, or the connector's
// own stdio in passthrough mode).
func (c *Connector) Run(ctx context.Context, stdin io.Writer, stdout io.Reader) error {
	c.stdin = stdin
	errCh := make(chan error, 2)

	// ONE scanner for the agent's stdout, shared by the handshake and the
	// main loop — two readers on one pipe would split lines (found by the
	// fs-gate test: the second scanner raced the first and orphaned lines).
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	// 0. ACP handshake: the HOST (this connector) initializes the agent
	// before any session traffic — real agents (opencode acp / hermes acp)
	// reject session/new until they are initialized. Fake/test agents that
	// don't care get a harmless extra request.
	if err := c.initializeAgent(ctx, scanner); err != nil {
		return fmt.Errorf("acp initialize: %w", err)
	}

	// 1. Agent stdout → daemon (same scanner the handshake used).
	go func() {
		errCh <- c.readAgentLines(ctx, scanner)
	}()

	// 2. Daemon WS events → agent stdin.
	errCh <- c.wsLoop(ctx)

	return <-errCh
}

// initializeAgent performs the ACP initialize handshake over the shared
// scanner. Pre-reply notifications are processed through the normal path;
// the reply (id=1) is asserted and everything after stays in the scanner for
// the main loop. agentCapabilities are informational for v1 — the connector
// advertises fs read/write unconditionally and gates writes Hub-side.
func (c *Connector) initializeAgent(ctx context.Context, scanner *bufio.Scanner) error {
	const initTimeout = 30 * time.Second
	if err := c.writeAgent(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": 1,
			"clientCapabilities": map[string]any{
				"fs":       map[string]any{"readTextFile": true, "writeTextFile": true},
				"terminal": false,
			},
		},
	}); err != nil {
		return err
	}
	deadline := time.After(initTimeout)
	for {
		select {
		case <-deadline:
			return fmt.Errorf("agent did not answer initialize within %s", initTimeout)
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return err
			}
			return fmt.Errorf("agent stdout closed during initialize")
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var msg rpcMsg
		if json.Unmarshal([]byte(line), &msg) != nil {
			continue
		}
		if msg.ID != nil && string(msg.ID) == "1" {
			if msg.Error != nil {
				return fmt.Errorf("agent rejected initialize: %s", string(msg.Error))
			}
			c.log.Printf("acp initialize handshake ok")
			return nil
		}
		// Early notification/request before the initialize reply.
		if err := c.handleAgentLine(ctx, []byte(line)); err != nil {
			c.log.Printf("agent line dropped: %v", err)
		}
	}
}

// readAgentLines is the shared line-processing loop over an already-created
// scanner (used by readAgentLoop and the initialize drain).
func (c *Connector) readAgentLines(ctx context.Context, scanner *bufio.Scanner) error {
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if err := c.handleAgentLine(ctx, []byte(line)); err != nil {
			return err
		}
	}
	return scanner.Err()
}

// ------------------------------------------------------------- agent → daemon

func (c *Connector) readAgentLoop(ctx context.Context, stdout io.Reader) error {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	err := c.readAgentLines(ctx, scanner)
	if err != nil {
		c.log.Printf("agent stdout loop failed: %v", err)
	} else {
		c.log.Printf("agent stdout closed")
	}
	return err
}

func (c *Connector) handleAgentLine(ctx context.Context, line []byte) error {
	var msg rpcMsg
	if err := json.Unmarshal(line, &msg); err != nil {
		return fmt.Errorf("unmarshal jsonrpc: %w", err)
	}

	// A response to one of our forwarded requests.
	if msg.Method == "" && len(msg.ID) > 0 {
		c.resolveForward(ctx, msg)
		return nil
	}
	if msg.Method == "" {
		return fmt.Errorf("message without method or id")
	}

	// Agent-initiated request (has an id): permission requests route through
	// the Hub gate; unsupported client capabilities fail fast.
	if len(msg.ID) > 0 {
		return c.handleAgentRequest(ctx, msg)
	}

	// Notification: record it in the Hub, nothing to answer.
	return c.postFrame(ctx, msg.Method, msg.Params, nil)
}

// handleAgentRequest routes an agent-initiated JSON-RPC request.
func (c *Connector) handleAgentRequest(ctx context.Context, msg rpcMsg) error {
	switch {
	case msg.Method == "session/request_permission":
		return c.forwardPermissionRequest(ctx, msg)
	case msg.Method == "fs/read_text_file":
		return c.fsReadTextFile(ctx, msg)
	case msg.Method == "fs/write_text_file":
		return c.fsWriteTextFile(ctx, msg)
	case isUnsupportedClientCapability(msg.Method):
		return c.writeAgent(map[string]any{
			"jsonrpc": "2.0",
			"id":      msg.ID,
			"error": map[string]any{
				"code":    -32601,
				"message": "agenthub connector v1 does not implement " + msg.Method,
			},
		})
	default:
		// Record the request in the Hub and answer with the frame result.
		var result json.RawMessage
		err := c.postFrame(ctx, msg.Method, msg.Params, &result)
		if err != nil {
			return c.writeAgent(map[string]any{
				"jsonrpc": "2.0",
				"id":      msg.ID,
				"error":   map[string]any{"code": -32000, "message": err.Error()},
			})
		}
		return c.writeAgent(map[string]any{
			"jsonrpc": "2.0",
			"id":      msg.ID,
			"result":  result,
		})
	}
}

// forwardPermissionRequest posts the request to the Hub gate and parks it
// until a human decision arrives via WS.
func (c *Connector) forwardPermissionRequest(ctx context.Context, msg rpcMsg) error {
	var params struct {
		SessionID string        `json:"sessionId"`
		Options   []agentOption `json:"options"`
	}
	_ = json.Unmarshal(msg.Params, &params)

	fail := func(errMsg string) error {
		return c.writeAgent(map[string]any{
			"jsonrpc": "2.0",
			"id":      msg.ID,
			"error":   map[string]any{"code": -32000, "message": errMsg},
		})
	}

	raw, err := c.postFrameRaw(ctx, msg.Method, msg.Params)
	if err != nil {
		return fail(err.Error())
	}
	var frameResp struct {
		Result struct {
			RequestID string `json:"requestId"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &frameResp); err != nil || frameResp.Result.RequestID == "" {
		return fail("permission request did not return a requestId")
	}

	c.permMu.Lock()
	c.permissions[frameResp.Result.RequestID] = &pendingPerm{AgentID: msg.ID, Options: params.Options}
	c.permMu.Unlock()
	c.log.Printf("permission request %s parked (session %s)", frameResp.Result.RequestID, params.SessionID)
	return nil
}

func isUnsupportedClientCapability(method string) bool {
	return strings.HasPrefix(method, "fs/") || strings.HasPrefix(method, "terminal/")
}

// fsReadTextFile answers the ACP host callback fs/read_text_file with the
// file's text (read-only, low risk — no approval needed). Content is capped
// at fsReadLimit so a huge file cannot exhaust the connector.
func (c *Connector) fsReadTextFile(ctx context.Context, msg rpcMsg) error {
	var params struct {
		SessionID string `json:"sessionId"`
		Path      string `json:"path"`
		Line      *int   `json:"line,omitempty"`
		Limit     *int   `json:"limit,omitempty"`
	}
	_ = json.Unmarshal(msg.Params, &params)
	fail := func(code int, m string) error {
		return c.writeAgent(map[string]any{
			"jsonrpc": "2.0", "id": msg.ID,
			"error": map[string]any{"code": code, "message": m},
		})
	}
	if params.Path == "" {
		return fail(-32602, "fs/read_text_file: path is required")
	}
	raw, err := os.ReadFile(params.Path)
	if err != nil {
		return fail(-32000, "fs/read_text_file: "+err.Error())
	}
	content := string(raw)
	if params.Line != nil || params.Limit != nil {
		lines := strings.Split(content, "\n")
		start, end := 0, len(lines)
		if params.Line != nil {
			start = *params.Line
			if start < 0 || start >= len(lines) {
				return fail(-32000, "fs/read_text_file: line out of range")
			}
			end = len(lines)
			if params.Limit != nil && start+*params.Limit < end {
				end = start + *params.Limit
			}
		}
		content = strings.Join(lines[start:end], "\n")
	}
	if len(content) > fsReadLimit {
		content = content[:fsReadLimit]
	}
	return c.writeAgent(map[string]any{
		"jsonrpc": "2.0", "id": msg.ID,
		"result": map[string]any{"content": content},
	})
}

// fsWriteTextFile answers the ACP host callback fs/write_text_file. Writing
// is a MEDIUM-risk action (PRD D11), so the write first routes through the
// Hub's approval gate (same pipeline as tool calls — PRD F3) and only lands
// on disk when the verdict allows it. Denied / timed out → JSON-RPC error.
func (c *Connector) fsWriteTextFile(ctx context.Context, msg rpcMsg) error {
	var params struct {
		SessionID string `json:"sessionId"`
		Path      string `json:"path"`
		Content   string `json:"content"`
	}
	_ = json.Unmarshal(msg.Params, &params)
	fail := func(code int, m string) error {
		return c.writeAgent(map[string]any{
			"jsonrpc": "2.0", "id": msg.ID,
			"error": map[string]any{"code": code, "message": m},
		})
	}
	if params.Path == "" {
		return fail(-32602, "fs/write_text_file: path is required")
	}

	// Approval gate: same intake surface the hooks use, blocking until the
	// human (or a policy rule) decides. Fail-closed on any error.
	verdict, err := c.gateFileWrite(ctx, params.SessionID, params.Path, len(params.Content))
	if err != nil {
		return fail(-32000, "fs/write_text_file: approval gate unavailable: "+err.Error())
	}
	if !verdict {
		return fail(-32000, "fs/write_text_file: denied by AgentHub approval gate")
	}
	if err := os.MkdirAll(filepath.Dir(params.Path), 0o755); err != nil {
		return fail(-32000, "fs/write_text_file: "+err.Error())
	}
	if err := os.WriteFile(params.Path, []byte(params.Content), 0o644); err != nil {
		return fail(-32000, "fs/write_text_file: "+err.Error())
	}
	return c.writeAgent(map[string]any{
		"jsonrpc": "2.0", "id": msg.ID, "result": map[string]any{},
	})
}

// gateFileWrite runs one blocking approval decision for a file write. The
// intake endpoint fail-closes on hub unreachability, so a transport error is
// reported (never treated as an allow).
func (c *Connector) gateFileWrite(ctx context.Context, sessionID, path string, size int) (bool, error) {
	body := map[string]any{
		"session_id":  sessionID,
		"tool":        "Write",
		"tool_name":   "Write",
		"file_path":   path,
		"args_digest": fmt.Sprintf("write %d bytes to %s", size, path),
		"cwd":         c.cfg.CWD,
		"agent_kind":  c.cfg.AgentKind,
		"reason":      "ACP fs/write_text_file",
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return false, err
	}
	c.log.Printf("fs write gate: POSTing intake for %s", path)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimSuffix(c.cfg.DaemonURL, "/")+"/api/hook/intake", bytes.NewReader(raw))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	// The intake blocks until a human decides (up to the 2-minute fail-closed
	// deadline), far beyond the shared 30s client — use a dedicated one.
	gateClient := &http.Client{Timeout: 150 * time.Second}
	resp, err := gateClient.Do(req)
	if err != nil {
		c.log.Printf("fs write gate: intake POST failed: %v", err)
		return false, err
	}
	defer resp.Body.Close()
	var out struct {
		Outcome string `json:"outcome"`
		Note    string `json:"note"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return false, err
	}
	c.log.Printf("fs write gate verdict for %s: %s (%s)", path, out.Outcome, out.Note)
	return out.Outcome == "allowed_once", nil
}

// resolveForward matches the agent's JSON-RPC response to a Hub→agent request
// and posts the answer back to the daemon.
func (c *Connector) resolveForward(ctx context.Context, msg rpcMsg) {
	key := string(msg.ID)
	c.forwardMu.Lock()
	pf, ok := c.forwarded[key]
	if ok {
		delete(c.forwarded, key)
	}
	c.forwardMu.Unlock()
	if !ok {
		c.log.Printf("agent response for unknown id %s ignored", key)
		return
	}

	body := map[string]any{"requestId": pf.DaemonRequestID}
	if len(msg.Result) > 0 {
		body["result"] = msg.Result
	}
	if len(msg.Error) > 0 {
		body["error"] = string(msg.Error)
	}
	if err := c.postDaemon(ctx, "POST", "/api/agent-requests/"+pf.DaemonRequestID+"/response", body, nil); err != nil {
		c.log.Printf("agent response for %s lost: %v", pf.DaemonRequestID, err)
		return
	}

	// After the agent created a session, register it in the Hub too so the
	// session list, approval gating and audit all cover it.
	if pf.Method == "session/new" && len(msg.Result) > 0 && len(msg.Error) == 0 {
		var res struct {
			SessionID string `json:"sessionId"`
		}
		if json.Unmarshal(msg.Result, &res) == nil && res.SessionID != "" {
			params := mergeSessionID(pf.Params, res.SessionID)
			if err := c.postFrame(ctx, "session/new", params, nil); err != nil {
				c.log.Printf("hub registration for session %s failed: %v", res.SessionID, err)
			}
		}
	}

	// Real ACP agents signal turn completion via the prompt response, not via
	// a notification — close the Hub-side state machine here.
	if pf.Method == "session/prompt" {
		var params struct {
			SessionID string `json:"sessionId"`
		}
		_ = json.Unmarshal(pf.Params, &params)
		if params.SessionID != "" {
			if len(msg.Error) > 0 {
				update, _ := json.Marshal(map[string]any{
					"sessionId": params.SessionID,
					"status":    "failed",
					"error":     truncate(string(msg.Error), 500),
				})
				if err := c.postFrame(ctx, "session/update", update, nil); err != nil {
					c.log.Printf("turn-failure frame for %s lost: %v", params.SessionID, err)
				}
			} else {
				update, _ := json.Marshal(map[string]any{
					"sessionId":  params.SessionID,
					"isComplete": true,
				})
				if err := c.postFrame(ctx, "session/update", update, nil); err != nil {
					c.log.Printf("turn-completion frame for %s lost: %v", params.SessionID, err)
				}
			}
		}
	}
}

func mergeSessionID(params json.RawMessage, sessionID string) json.RawMessage {
	var m map[string]any
	if len(params) > 0 {
		_ = json.Unmarshal(params, &m)
	}
	if m == nil {
		m = map[string]any{}
	}
	m["sessionId"] = sessionID
	out, err := json.Marshal(m)
	if err != nil {
		return json.RawMessage(`{"sessionId":"` + sessionID + `"}`)
	}
	return out
}

// ---------------------------------------------------------------- daemon → agent

func (c *Connector) wsLoop(ctx context.Context) error {
	backoff := 2 * time.Second
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err := c.wsOnce(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		c.log.Printf("ws disconnected (%v), retrying in %s", err, backoff)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (c *Connector) wsOnce(ctx context.Context) error {
	wsURL := strings.Replace(c.cfg.DaemonURL, "http", "ws", 1) + "/ws?token=" + c.cfg.Token
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		return err
	}
	defer conn.Close()

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := conn.SetReadDeadline(time.Now().Add(90 * time.Second)); err != nil {
			return err
		}
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		var evt struct {
			Type string          `json:"type"`
			Raw  json.RawMessage `json:"-"`
		}
		if err := json.Unmarshal(raw, &evt); err != nil {
			continue
		}
		switch evt.Type {
		case "hello":
			c.readyOnce.Do(func() { close(c.ready) })
			c.log.Printf("ws connected to %s", c.cfg.DaemonURL)
		case "agent.request":
			if err := c.handleAgentRequestEvent(ctx, raw); err != nil {
				c.log.Printf("agent request handling failed: %v", err)
			}
		case "permission.resolved":
			if err := c.handlePermissionResolved(ctx, raw); err != nil {
				c.log.Printf("permission resolution mapping failed: %v", err)
			}
		}
	}
}

type wsEnvelope struct {
	Type      string          `json:"type"`
	RequestID string          `json:"requestId"`
	Method    string          `json:"method"`
	Params    json.RawMessage `json:"params"`
	Result    json.RawMessage `json:"result"`
	Error     string          `json:"error"`
	Decision  string          `json:"decision"`
	Outcome   string          `json:"outcome"`
	Applied   bool            `json:"applied"`
	AgentKind string          `json:"agentKind"`
}

// handleAgentRequestEvent forwards a Hub→agent command to the agent. For
// session/prompt the Hub-side bookkeeping frame is posted first so an invalid
// turn aborts before the agent runs anything.
func (c *Connector) handleAgentRequestEvent(ctx context.Context, raw []byte) error {
	var evt wsEnvelope
	if err := json.Unmarshal(raw, &evt); err != nil {
		return err
	}

	// 只转发与本 connector kind 匹配的请求，避免多个 connector
	// 同时连 Hub 时把请求错误转发给不该处理的 agent。
	// evt.AgentKind 为空时放行（向后兼容旧调用）；非空且不匹配则丢弃。
	if evt.AgentKind != "" && evt.AgentKind != c.cfg.AgentKind {
		c.log.Printf("dropped %s for kind %s (this connector: %s)",
			evt.Method, evt.AgentKind, c.cfg.AgentKind)
		return nil
	}

	switch evt.Method {
	case "session/prompt":
		// Hub-side bookkeeping expects prompt as plain text while the agent
		// speaks the ACP content-block array — flatten for the frame, forward
		// the original params to the agent verbatim.
		if err := c.postFrame(ctx, "session/prompt", flattenPromptText(evt.Params), nil); err != nil {
			return c.respondDaemonError(ctx, evt.RequestID, err)
		}
	case "session/new", "session/cancel", "":
		// forwarded as-is (session/new registration happens on response)
		if evt.Method == "session/new" {
			// Some agents (opencode) Zod-validate session/new and require an
			// mcpServers array — default it to empty when the Hub omits it.
			evt.Params = mergeJSONKey(evt.Params, "mcpServers", []any{})
		}
	default:
		// Unknown Hub command: still forward (the agent may support it).
	}

	id := c.nextForwardID(evt.RequestID, evt.Method, evt.Params)
	if err := c.writeAgent(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  evt.Method,
		"params":  evt.Params,
	}); err != nil {
		return c.respondDaemonError(ctx, evt.RequestID, err)
	}
	c.log.Printf("forwarded %s (hub %s)", evt.Method, evt.RequestID)
	return nil
}

func (c *Connector) nextForwardID(daemonRequestID, method string, params json.RawMessage) jsonrpcID {
	c.forwardMu.Lock()
	defer c.forwardMu.Unlock()
	c.forwardSeq++
	id := json.RawMessage(fmt.Sprintf("%d", 1000+c.forwardSeq))
	c.forwarded[string(id)] = &pendingForward{
		DaemonRequestID: daemonRequestID,
		Method:          method,
		Params:          params,
		SentAt:          time.Now(),
	}
	return id
}

func (c *Connector) respondDaemonError(ctx context.Context, requestID string, err error) error {
	return c.postDaemon(ctx, "POST", "/api/agent-requests/"+requestID+"/response",
		map[string]any{"error": err.Error()}, nil)
}

// handlePermissionResolved maps the Hub decision back onto the agent's own
// option ids and answers the parked agent request.
func (c *Connector) handlePermissionResolved(ctx context.Context, raw []byte) error {
	var evt wsEnvelope
	if err := json.Unmarshal(raw, &evt); err != nil {
		return err
	}
	if !evt.Applied {
		return nil // double-confirm interstitial; nothing to answer yet
	}
	c.permMu.Lock()
	p, ok := c.permissions[evt.RequestID]
	if ok {
		delete(c.permissions, evt.RequestID)
	}
	c.permMu.Unlock()
	if !ok {
		return nil // not ours (e.g. raised by the Hub UI directly)
	}

	allow := evt.Outcome == "approved"
	outcome := map[string]any{"outcome": "cancelled"}
	if opt := pickAgentOption(p.Options, evt.Decision, allow); opt != "" {
		outcome = map[string]any{"outcome": "selected", "optionId": opt}
	}
	return c.writeAgent(map[string]any{
		"jsonrpc": "2.0",
		"id":      p.AgentID,
		"result":  map[string]any{"outcome": outcome},
	})
}

// pickAgentOption selects the agent option matching the Hub decision, with
// graceful fallbacks when the agent offers a different vocabulary.
func pickAgentOption(opts []agentOption, decision string, allow bool) string {
	want := map[string][]string{
		"allow_once":    {"allow_once"},
		"allow_session": {"allow_once", "allow_always"},
		"allow_always":  {"allow_always"},
		"reject_once":   {"reject_once"},
		"reject_always": {"reject_always"},
	}[decision]
	for _, w := range want {
		for _, o := range opts {
			if o.Kind == w {
				return o.OptionID
			}
		}
	}
	// Fallback: first option of the right polarity.
	for _, o := range opts {
		if allow && strings.HasPrefix(o.Kind, "allow") {
			return o.OptionID
		}
		if !allow && (strings.HasPrefix(o.Kind, "reject") || o.Kind == "cancel") {
			return o.OptionID
		}
	}
	return ""
}

// ------------------------------------------------------------------- plumbing

func (c *Connector) writeAgent(v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.stdin == nil {
		return fmt.Errorf("agent stdin not wired")
	}
	_, err = c.stdin.Write(raw)
	return err
}

// postFrame posts one inbound frame; when out is non-nil it receives the
// frame's result.
func (c *Connector) postFrame(ctx context.Context, method string, params json.RawMessage, out *json.RawMessage) error {
	body := map[string]any{"agentKind": c.cfg.AgentKind, "method": method, "params": params}
	var resp json.RawMessage
	if err := c.postDaemon(ctx, "POST", "/acp/frame", body, &resp); err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	var parsed struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(resp, &parsed); err != nil {
		return err
	}
	*out = parsed.Result
	return nil
}

// postFrameRaw posts one inbound frame and returns the full response body.
func (c *Connector) postFrameRaw(ctx context.Context, method string, params json.RawMessage) ([]byte, error) {
	body := map[string]any{"agentKind": c.cfg.AgentKind, "method": method, "params": params}
	var resp json.RawMessage
	if err := c.postDaemon(ctx, "POST", "/acp/frame", body, &resp); err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *Connector) postDaemon(ctx context.Context, method, path string, body any, out *json.RawMessage) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = strings.NewReader(string(raw))
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(c.cfg.DaemonURL, "/")+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s → %d: %s", method, path, resp.StatusCode, truncate(string(raw), 200))
	}
	if out != nil {
		*out = json.RawMessage(raw)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// mergeJSONKey sets top[key] = value in a params object (absent key only —
// an explicit Hub value always wins).
func mergeJSONKey(params json.RawMessage, key string, value any) json.RawMessage {
	var m map[string]any
	if len(params) > 0 {
		_ = json.Unmarshal(params, &m)
	}
	if m == nil {
		m = map[string]any{}
	}
	if _, exists := m[key]; !exists {
		m[key] = value
	}
	out, err := json.Marshal(m)
	if err != nil {
		return params
	}
	return out
}

// flattenPromptText converts an ACP session/prompt params object (prompt as
// an array of content blocks) into the Hub's bookkeeping shape (prompt as one
// text string). A params object whose prompt is already a string passes
// through unchanged.
func flattenPromptText(params json.RawMessage) json.RawMessage {
	var m struct {
		SessionID string          `json:"sessionId"`
		Prompt    json.RawMessage `json:"prompt"`
	}
	if json.Unmarshal(params, &m) != nil || len(m.Prompt) == 0 {
		return params
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(m.Prompt, &blocks) != nil {
		return params // already a plain string (or non-array) — keep as-is
	}
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	out, err := json.Marshal(map[string]any{
		"sessionId": m.SessionID,
		"prompt":    strings.Join(parts, "\n"),
	})
	if err != nil {
		return params
	}
	return out
}
