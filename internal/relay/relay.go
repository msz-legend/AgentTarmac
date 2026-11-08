// Package relay implements PRD D8's self-hosted public relay: the Hub makes
// an OUTBOUND WebSocket connection to the relay (no public IP / port
// forwarding needed), phone/PC browsers hit the relay, and the relay pipes
// bytes between them without persisting anything (§9 红线 3: 不落地).
//
// Wire format (JSON lines over the hub's WS):
//
//	→ {"id":"n","method":"GET","path":"/api/sessions","header":{...},"body":"base64"}
//	← {"id":"n","status":200,"header":{...},"body":"base64"}
//
// Security (§9): the hub authenticates to the relay with a relay token; the
// browser authenticates to the Hub with the usual Bearer token (the relay
// forwards it untouched and never needs to understand it). TLS terminates at
// the relay (operator supplies certs).
package relay

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Server is the self-hosted relay.
type Server struct {
	token string // relay token the hub must present (?token=)
	log   *log.Logger

	mu   sync.RWMutex
	hubs map[string]*hubConn // hubID → live hub connection
	wait map[string]chan *respMsg
	seq  int64
}

type hubConn struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

type reqMsg struct {
	ID     string            `json:"id"`
	Method string            `json:"method"`
	Path   string            `json:"path"`
	Header map[string]string `json:"header,omitempty"`
	Body   string            `json:"body,omitempty"` // base64
}

type respMsg struct {
	ID     string            `json:"id"`
	Status int               `json:"status"`
	Header map[string]string `json:"header,omitempty"`
	Body   string            `json:"body,omitempty"`
}

// New creates the relay server.
func New(token string, logger *log.Logger) *Server {
	if logger == nil {
		logger = log.Default()
	}
	return &Server{token: token, log: logger, hubs: map[string]*hubConn{}, wait: map[string]chan *respMsg{}}
}

// Handler mounts the relay routes:
//
//	GET /relay/hub/{hubID}   — hub outbound connection (WS, token-authed)
//	/{hubID}/{rest}          — browser → hub forwarding
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("relay ok"))
	})
	mux.HandleFunc("/relay/hub/", s.handleHubConnect)
	mux.HandleFunc("/", s.handleForward)
	return mux
}

var up = websocket.Upgrader{
	CheckOrigin:      func(*http.Request) bool { return true },
	Subprotocols:     []string{"agenthub-relay-v1"},
	HandshakeTimeout: 10 * time.Second,
}

// handleHubConnect accepts the hub's outbound connection.
func (s *Server) handleHubConnect(w http.ResponseWriter, r *http.Request) {
	if s.token != "" && r.URL.Query().Get("token") != s.token {
		http.Error(w, "relay: bad token", http.StatusUnauthorized)
		return
	}
	hubID := strings.Trim(strings.TrimPrefix(r.URL.Path, "/relay/hub/"), "/")
	if hubID == "" {
		http.Error(w, "relay: hub id required", http.StatusBadRequest)
		return
	}
	conn, err := up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	s.log.Printf("relay: hub %q connected from %s", hubID, r.RemoteAddr)
	s.mu.Lock()
	if old := s.hubs[hubID]; old != nil {
		_ = old.conn.Close()
	}
	s.hubs[hubID] = &hubConn{conn: conn}
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		if s.hubs[hubID] != nil && s.hubs[hubID].conn == conn {
			delete(s.hubs, hubID)
		}
		s.mu.Unlock()
		_ = conn.Close()
		s.log.Printf("relay: hub %q disconnected", hubID)
	}()

	// Reader: responses come back on this connection.
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var resp respMsg
		if json.Unmarshal(raw, &resp) != nil {
			continue
		}
		s.mu.Lock()
		ch := s.wait[resp.ID]
		delete(s.wait, resp.ID)
		s.mu.Unlock()
		if ch != nil {
			ch <- &resp
		}
	}
}

// handleForward pipes a browser request to the hub and streams the answer
// back. Nothing is stored (§9 红线 3).
func (s *Server) handleForward(w http.ResponseWriter, r *http.Request) {
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 2)
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, "relay: use /{hubID}/…", http.StatusNotFound)
		return
	}
	hubID, path := parts[0], "/"+parts[1]

	s.mu.RLock()
	hub := s.hubs[hubID]
	s.mu.RUnlock()
	if hub == nil {
		http.Error(w, "relay: hub "+hubID+" offline", http.StatusBadGateway)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		http.Error(w, "relay: read body", http.StatusBadRequest)
		return
	}
	header := map[string]string{}
	for k, vs := range r.Header {
		if strings.EqualFold(k, "Authorization") || strings.EqualFold(k, "Content-Type") {
			header[k] = strings.Join(vs, ",")
		}
	}
	s.mu.Lock()
	s.seq++
	seq := s.seq
	ch := make(chan *respMsg, 1)
	s.wait[formatID(seq)] = ch
	s.mu.Unlock()

	req := reqMsg{ID: formatID(seq), Method: r.Method, Path: path, Header: header, Body: base64.StdEncoding.EncodeToString(body)}
	raw, _ := json.Marshal(req)
	hub.mu.Lock()
	err = hub.conn.WriteMessage(websocket.TextMessage, raw)
	hub.mu.Unlock()
	if err != nil {
		s.mu.Lock()
		delete(s.wait, req.ID)
		s.mu.Unlock()
		http.Error(w, "relay: hub write failed", http.StatusBadGateway)
		return
	}

	select {
	case resp := <-ch:
		for k, v := range resp.Header {
			w.Header().Set(k, v)
		}
		if resp.Header["Content-Type"] == "" {
			w.Header().Set("Content-Type", "application/json")
		}
		raw, _ := base64.StdEncoding.DecodeString(resp.Body)
		w.WriteHeader(resp.Status)
		_, _ = w.Write(raw)
	case <-time.After(30 * time.Second):
		s.mu.Lock()
		delete(s.wait, req.ID)
		s.mu.Unlock()
		http.Error(w, "relay: hub timeout", http.StatusGatewayTimeout)
	case <-r.Context().Done():
	}
}

func formatID(seq int64) string {
	return "req-" + strconv.FormatInt(seq, 10)
}

// Dialer is the hub-side outbound connection to the relay. Requests arriving
// on the relay are served by handler and answered back over the same WS.
type Dialer struct {
	RelayURL string // wss://host/relay/hub/{id}?token=…
	Handler  http.Handler
	Log      *log.Logger

	mu sync.Mutex // serializes response writes on one WS connection
}

// Run dials the relay and serves forwarded requests until ctx is cancelled,
// reconnecting with capped backoff (500ms → 8s, same as the console WS).
func (d *Dialer) Run(ctx context.Context) error {
	backoff := 500 * time.Millisecond
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err := d.once(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.Log != nil {
			d.Log.Printf("relay link down (%v), retrying in %s", err, backoff)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 8*time.Second {
			backoff *= 2
		}
	}
}

func (d *Dialer) once(ctx context.Context) error {
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, d.RelayURL, nil)
	if err != nil {
		return err
	}
	defer conn.Close()
	if d.Log != nil {
		d.Log.Printf("relay link up: %s", d.RelayURL)
	}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		var req reqMsg
		if json.Unmarshal(raw, &req) != nil {
			continue
		}
		go d.serve(conn, req)
	}
}

// serve executes one forwarded request against the local handler and writes
// the response back.
func (d *Dialer) serve(conn *websocket.Conn, req reqMsg) {
	defer func() { _ = recover() }()
	body, _ := base64.StdEncoding.DecodeString(req.Body)
	r, err := http.NewRequest(req.Method, "http://local"+req.Path, bytes.NewReader(body))
	if err != nil {
		return
	}
	for k, v := range req.Header {
		r.Header.Set(k, v)
	}
	rec := newCaptureRecorder(r)
	d.Handler.ServeHTTP(rec, r)

	raw := rec.body
	resp := respMsg{
		ID:     req.ID,
		Status: rec.status,
		Header: map[string]string{"Content-Type": rec.header.Get("Content-Type")},
		Body:   base64.StdEncoding.EncodeToString(raw),
	}
	out, _ := json.Marshal(resp)
	d.mu.Lock()
	_ = conn.WriteMessage(websocket.TextMessage, out)
	d.mu.Unlock()
}

// captureRecorder implements http.ResponseWriter for relayed requests.
type captureRecorder struct {
	header http.Header
	status int
	body   []byte
	req    *http.Request
}

func newCaptureRecorder(r *http.Request) *captureRecorder {
	return &captureRecorder{header: http.Header{}, status: 200, req: r}
}

func (c *captureRecorder) Header() http.Header  { return c.header }
func (c *captureRecorder) WriteHeader(code int) { c.status = code }
func (c *captureRecorder) Write(p []byte) (int, error) {
	c.body = append(c.body, p...)
	return len(p), nil
}
