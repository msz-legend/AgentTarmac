package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
)

// WeCom AI bot (智能机器人) long-connection transport.
//
// Protocol (official): developer.work.weixin.qq.com/document/path/101463
//
//	wss://openws.work.weixin.qq.com
//	1. {"cmd":"aibot_subscribe","headers":{"req_id":R},"body":{"bot_id":B,"secret":S}}
//	   -> {"headers":{"req_id":R},"errcode":0,"errmsg":"ok"}
//	2. {"cmd":"aibot_send_msg","headers":{"req_id":R2},
//	    "body":{"chatid":C,"chat_type":1|2,"msgtype":"markdown","markdown":{"content":T}}}
//	   -> {"headers":{"req_id":R2},"errcode":0,"errmsg":"ok"}
//
// Responses are correlated by headers.req_id; unrelated frames (callbacks,
// pings) are ignored. Each notification opens a one-shot connection
// (subscribe -> send -> close), so no heartbeat is needed: the whole lifecycle
// is well under the 30s ping interval. Official limits: 30 msg/min,
// 1000 msg/hour, markdown.content <= 20480 UTF-8 bytes, and the user must have
// sent the bot at least one message in the target chat before proactive push.
//
// We deliberately use gorilla/websocket (a full RFC 6455 handshake) rather
// than a hand-rolled client: the WeCom endpoint rejects some incomplete
// handshakes with close 1006.

const (
	wecomAibotDefaultWSURL = "wss://openws.work.weixin.qq.com"
	wecomAibotMaxContent   = 20480
	wecomAibotStepTimeout  = 10 * time.Second
)

// WecomAibotConfig holds the long-connection credentials. WSURL is optional
// and exists for tests / private deployments only; empty means the official
// endpoint. ChatType: 1 = single chat (chatid is a userid), 2 = group chat,
// 0 = omit the field and let the server infer.
type WecomAibotConfig struct {
	BotID    string
	Secret   string
	ChatID   string
	ChatType int
	WSURL    string
}

type wecomAibotTransport struct {
	cfg    WecomAibotConfig
	dialer *websocket.Dialer
}

// NewWecomAibotTransport builds the long-connection channel.
func NewWecomAibotTransport(cfg WecomAibotConfig) *wecomAibotTransport {
	if strings.TrimSpace(cfg.WSURL) == "" {
		cfg.WSURL = wecomAibotDefaultWSURL
	}
	cfg.BotID = strings.TrimSpace(cfg.BotID)
	cfg.Secret = strings.TrimSpace(cfg.Secret)
	cfg.ChatID = strings.TrimSpace(cfg.ChatID)
	return &wecomAibotTransport{
		cfg: cfg,
		dialer: &websocket.Dialer{
			HandshakeTimeout: wecomAibotStepTimeout,
			Proxy:            http.ProxyFromEnvironment,
		},
	}
}

// Tail masks the credential: only the last 6 chars of the BotID are exposed.
// The secret is never included.
func (t *wecomAibotTransport) Tail() string {
	id := t.cfg.BotID
	if len(id) > 6 {
		return id[len(id)-6:]
	}
	return id
}

type wecomAibotFrame struct {
	Headers struct {
		ReqID string `json:"req_id"`
	} `json:"headers"`
	ErrCode *int   `json:"errcode"`
	ErrMsg  string `json:"errmsg"`
}

// Post delivers one markdown message over a one-shot long connection.
func (t *wecomAibotTransport) Post(ctx context.Context, text string) error {
	if t.cfg.BotID == "" || t.cfg.Secret == "" || t.cfg.ChatID == "" {
		return fmt.Errorf("wecom aibot not configured (need bot id, secret and chat id)")
	}
	// Keep the whole exchange inside the caller's deadline; notifications are
	// fire-and-forget side paths and must never hang the daemon.
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	conn, resp, err := t.dialer.DialContext(ctx, t.cfg.WSURL, nil)
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	if err != nil {
		return fmt.Errorf("wecom aibot connect: %w", err)
	}
	defer conn.Close()

	if err := t.call(ctx, conn, "aibot_subscribe", map[string]any{
		"bot_id": t.cfg.BotID,
		"secret": t.cfg.Secret,
	}); err != nil {
		return fmt.Errorf("wecom aibot subscribe: %w", err)
	}

	body := map[string]any{
		"chatid":   t.cfg.ChatID,
		"msgtype":  "markdown",
		"markdown": map[string]any{"content": truncateUTF8(text, wecomAibotMaxContent)},
	}
	if t.cfg.ChatType == 1 || t.cfg.ChatType == 2 {
		body["chat_type"] = t.cfg.ChatType
	}
	if err := t.call(ctx, conn, "aibot_send_msg", body); err != nil {
		return fmt.Errorf("wecom aibot send: %w", err)
	}
	return nil
}

// call writes one command frame and waits for the ack carrying the same req_id.
func (t *wecomAibotTransport) call(ctx context.Context, conn *websocket.Conn, cmd string, body any) error {
	reqID, err := randomReqID()
	if err != nil {
		return err
	}
	frame := map[string]any{
		"cmd":     cmd,
		"headers": map[string]any{"req_id": reqID},
		"body":    body,
	}
	if err := withWriteDeadline(ctx, conn); err != nil {
		return err
	}
	if err := conn.WriteJSON(frame); err != nil {
		return fmt.Errorf("%s write: %w", cmd, err)
	}
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("%s: %w", cmd, err)
		}
		if err := withReadDeadline(ctx, conn); err != nil {
			return err
		}
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("%s read: %w", cmd, err)
		}
		var ack wecomAibotFrame
		if err := json.Unmarshal(raw, &ack); err != nil {
			continue
		}
		if ack.Headers.ReqID != reqID {
			continue // unrelated callback / event frame
		}
		if ack.ErrCode == nil {
			return fmt.Errorf("%s: ack missing errcode", cmd)
		}
		if *ack.ErrCode != 0 {
			return fmt.Errorf("errcode %d: %s", *ack.ErrCode, ack.ErrMsg)
		}
		return nil
	}
}

func randomReqID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate req_id: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// deadlineFrom returns the earlier of ctx's deadline and now+stepTimeout.
func deadlineFrom(ctx context.Context) time.Time {
	step := time.Now().Add(wecomAibotStepTimeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(step) {
		return dl
	}
	return step
}

func withWriteDeadline(ctx context.Context, conn *websocket.Conn) error {
	return conn.SetWriteDeadline(deadlineFrom(ctx))
}

func withReadDeadline(ctx context.Context, conn *websocket.Conn) error {
	return conn.SetReadDeadline(deadlineFrom(ctx))
}

// truncateUTF8 caps a string at max bytes without splitting a rune.
func truncateUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	b := []byte(s[:max])
	for len(b) > 0 && !utf8.Valid(b) {
		b = b[:len(b)-1]
	}
	return string(b)
}
