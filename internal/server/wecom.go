package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/agenttarmac/agenttarmac/internal/acpclient"
)

// wecomTransport abstracts the two official WeCom delivery protocols:
//   - group-bot Webhook (HTTP POST, legacy "群机器人")
//   - AI bot long connection (WebSocket: aibot_subscribe -> aibot_send_msg,
//     "智能机器人" BotID + Secret; developer.work.weixin.qq.com/document/path/101463)
//
// Tail returns a masked identifier (last 6 chars) for the settings view. It
// must never expose the secret.
type wecomTransport interface {
	Post(ctx context.Context, text string) error
	Tail() string
}

// WecomNotifier is the WeCom (企业微信) channel (PRD D17/M2): same policy as
// Feishu — high-risk approvals and session failures only, daemon-side
// delivery, 30s quiet window, outbox dedup. The transport is pluggable so the
// same policy covers both the group-bot webhook and the AI-bot long connection.
type WecomNotifier struct {
	transport wecomTransport
	store     *acpclient.Store
	log       *log.Logger

	quietMu    sync.Mutex
	quietUntil map[string]time.Time
}

func newWecomNotifier(t wecomTransport, store *acpclient.Store, logger *log.Logger) *WecomNotifier {
	if logger == nil {
		logger = log.Default()
	}
	return &WecomNotifier{
		transport:  t,
		store:      store,
		log:        logger,
		quietUntil: map[string]time.Time{},
	}
}

// NewWecomNotifier creates the legacy group-bot Webhook channel.
func NewWecomNotifier(webhookURL string, store *acpclient.Store, logger *log.Logger) *WecomNotifier {
	return newWecomNotifier(&wecomWebhookTransport{
		url:    webhookURL,
		client: &http.Client{Timeout: 10 * time.Second},
	}, store, logger)
}

// NewWecomAibotNotifier creates the AI-bot (智能机器人) long-connection channel.
func NewWecomAibotNotifier(cfg WecomAibotConfig, store *acpclient.Store, logger *log.Logger) *WecomNotifier {
	return newWecomNotifier(NewWecomAibotTransport(cfg), store, logger)
}

// WebhookTail returns the last 6 chars of a masked identifier for display.
func (n *WecomNotifier) WebhookTail() string {
	if n == nil || n.transport == nil {
		return ""
	}
	return n.transport.Tail()
}

func (n *WecomNotifier) suppressed(kind, sessionID string) bool {
	n.quietMu.Lock()
	defer n.quietMu.Unlock()
	key := kind + "\x00" + sessionID
	now := time.Now()
	if until, ok := n.quietUntil[key]; ok && now.Before(until) {
		return true
	}
	n.quietUntil[key] = now.Add(quietWindow)
	return false
}

// NotifyPermissionRequest pushes one high-risk approval.
func (n *WecomNotifier) NotifyPermissionRequest(sessionID, toolName, risk, reason string) {
	if n == nil || n.transport == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if n.suppressed("perm", sessionID) {
		return
	}
	body := fmt.Sprintf("AgentHub 审批请求\n会话: %s\n工具: %s\n风险: %s\n原因: %s\n\n请在控制台处理。", sessionID, toolName, risk, reason)
	n.send(ctx, body, "wecom|perm|"+sessionID+"|"+toolName, sessionID)
}

// NotifySessionFailed pushes a session failure.
func (n *WecomNotifier) NotifySessionFailed(sessionID, reason string) {
	if n == nil || n.transport == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if n.suppressed("failed", sessionID) {
		return
	}
	body := fmt.Sprintf("AgentHub 会话失败\n会话: %s\n原因: %s", sessionID, reason)
	n.send(ctx, body, "wecom|failed|"+sessionID+"|"+reason, sessionID)
}

// Test sends a test message from the settings view.
func (n *WecomNotifier) Test(ctx context.Context) error {
	if n == nil || n.transport == nil {
		return fmt.Errorf("wecom notifier not configured")
	}
	return n.transport.Post(ctx, "AgentHub 通知测试：企微通道正常。")
}

func (n *WecomNotifier) send(ctx context.Context, body, tailHash, sessionID string) {
	if n.store != nil {
		if dup, err := n.store.HasNotificationWithTailHash(ctx, tailHash, 24*time.Hour); err == nil && dup {
			return
		}
	}
	if err := n.transport.Post(ctx, body); err != nil {
		if n.log != nil {
			n.log.Printf("wecom notify failed: %v", err)
		}
		return
	}
	if n.store != nil {
		if _, err := n.store.InsertNotificationWithDedup(ctx, &acpclient.Notification{
			SessionID:        sessionID,
			NotificationType: "wecom",
			Title:            "AgentHub 通知",
			Body:             body,
			TailHash:         tailHash,
		}); err != nil && n.log != nil {
			n.log.Printf("wecom record failed: %v", err)
		}
	}
}

/* ---------- 企业微信群机器人（Webhook） ---------- */

type wecomWebhookTransport struct {
	url    string
	client *http.Client
}

func (t *wecomWebhookTransport) Tail() string {
	if len(t.url) > 6 {
		return t.url[len(t.url)-6:]
	}
	return t.url
}

func (t *wecomWebhookTransport) Post(ctx context.Context, text string) error {
	if t.url == "" {
		return fmt.Errorf("wecom webhook not configured")
	}
	raw, _ := json.Marshal(map[string]any{"msgtype": "text", "text": map[string]string{"content": text}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var out struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		if resp.StatusCode >= 300 {
			return fmt.Errorf("wecom webhook → %d", resp.StatusCode)
		}
		return nil
	}
	if out.ErrCode != 0 {
		return fmt.Errorf("wecom error %d: %s", out.ErrCode, out.ErrMsg)
	}
	return nil
}
