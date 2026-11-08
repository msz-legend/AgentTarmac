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

// WecomNotifier is the WeCom (企业微信) group-bot channel (PRD D17/M2):
// same policy as Feishu — high-risk approvals and session failures only,
// daemon-side delivery, 30s quiet window, outbox dedup.
type WecomNotifier struct {
	webhookURL string // https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=…
	store      *acpclient.Store
	client     *http.Client
	log        *log.Logger

	quietMu    sync.Mutex
	quietUntil map[string]time.Time
}

// NewWecomNotifier creates the WeCom channel.
func NewWecomNotifier(webhookURL string, store *acpclient.Store, logger *log.Logger) *WecomNotifier {
	if logger == nil {
		logger = log.Default()
	}
	return &WecomNotifier{
		webhookURL: webhookURL,
		store:      store,
		client:     &http.Client{Timeout: 10 * time.Second},
		log:        logger,
		quietUntil: map[string]time.Time{},
	}
}

// WebhookTail returns the last 6 chars for masked display.
func (n *WecomNotifier) WebhookTail() string {
	if n == nil || n.webhookURL == "" {
		return ""
	}
	if len(n.webhookURL) > 6 {
		return n.webhookURL[len(n.webhookURL)-6:]
	}
	return n.webhookURL
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
	if n == nil || n.webhookURL == "" {
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
	if n == nil || n.webhookURL == "" {
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
	if n == nil || n.webhookURL == "" {
		return fmt.Errorf("wecom webhook not configured")
	}
	return n.post(ctx, "AgentHub 通知测试：企微通道正常。")
}

func (n *WecomNotifier) send(ctx context.Context, body, tailHash, sessionID string) {
	if n.store != nil {
		if dup, err := n.store.HasNotificationWithTailHash(ctx, tailHash, 24*time.Hour); err == nil && dup {
			return
		}
	}
	if err := n.post(ctx, body); err != nil {
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

func (n *WecomNotifier) post(ctx context.Context, text string) error {
	raw, _ := json.Marshal(map[string]any{"msgtype": "text", "text": map[string]string{"content": text}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.webhookURL, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.client.Do(req)
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
