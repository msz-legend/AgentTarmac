package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/agenttarmac/agenttarmac/internal/acpclient"
)

// FeishuNotifier pushes alerts to a Feishu custom-bot webhook (decision F):
// high-risk pending approvals and session failures (PRD D17). Every push is
// recorded in the notification outbox with tail-hash de-duplication, and a
// 30s quiet window merges same-kind alerts per session (PRD M1.5 静默期).
type FeishuNotifier struct {
	webhookURL string
	secret     string
	store      *acpclient.Store
	client     *http.Client
	log        *log.Logger

	quietMu    sync.Mutex
	quietUntil map[string]time.Time
}

// NewFeishuNotifier creates the notifier. secret may be empty for bots
// without signature verification.
func NewFeishuNotifier(webhookURL, secret string, store *acpclient.Store, logger *log.Logger) *FeishuNotifier {
	if logger == nil {
		logger = log.Default()
	}
	return &FeishuNotifier{
		webhookURL: webhookURL,
		secret:     secret,
		store:      store,
		client:     &http.Client{Timeout: 10 * time.Second},
		log:        logger,
		quietUntil: map[string]time.Time{},
	}
}

// quietWindow is the M1.5 notification merge window (D17: 默认 30s).
const quietWindow = 30 * time.Second

// suppressed reports whether a (kind, session) alert is inside the merge
// window; otherwise it opens the window.
func (n *FeishuNotifier) suppressed(kind, sessionID string) bool {
	n.quietMu.Lock()
	defer n.quietMu.Unlock()
	key := kind + "\x00" + sessionID
	now := time.Now()
	if until, ok := n.quietUntil[key]; ok && now.Before(until) {
		return true
	}
	n.quietUntil[key] = now.Add(quietWindow)
	if len(n.quietUntil) > 512 {
		for k, u := range n.quietUntil {
			if now.After(u) {
				delete(n.quietUntil, k)
			}
		}
	}
	return false
}

// NotifyPermissionRequest pushes one high-risk permission request. Safe to
// call from a goroutine; failures are logged, never fatal.
func (n *FeishuNotifier) NotifyPermissionRequest(sessionID, requestID, toolName, risk, pattern, reason string) {
	if n == nil || n.webhookURL == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	title := "AgentHub 审批请求"
	body := fmt.Sprintf("会话: %s\n工具: %s\n风险: %s\n模式: %s\n原因: %s\n请求: %s\n\n请在 AgentHub 控制台的「审批」页处理。",
		sessionID, toolName, risk, pattern, reason, requestID)

	if n.suppressed("perm", sessionID) {
		return
	}
	tailHash := sha256Hex("feishu|perm|" + requestID)
	if n.store != nil {
		if dup, err := n.store.HasNotificationWithTailHash(ctx, tailHash, 24*time.Hour); err == nil && dup {
			return
		}
	}

	if err := n.post(ctx, title, body); err != nil {
		n.log.Printf("feishu notify failed: %v", err)
		return
	}
	if n.store != nil {
		if _, err := n.store.InsertNotificationWithDedup(ctx, &acpclient.Notification{
			SessionID:        sessionID,
			NotificationType: "feishu",
			Title:            title,
			Body:             body,
			TailHash:         tailHash,
		}); err != nil {
			n.log.Printf("feishu notification record failed: %v", err)
		}
	}
	n.log.Printf("feishu notification sent for request %s", requestID)
}

// NotifySessionFailed pushes a session-failure alert (PRD F9/D17).
func (n *FeishuNotifier) NotifySessionFailed(sessionID, reason string) {
	if n == nil || n.webhookURL == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	title := "AgentHub 会话失败"
	body := fmt.Sprintf("会话: %s\n原因: %s\n\n请在 AgentHub 控制台查看详情。", sessionID, reason)

	if n.suppressed("failed", sessionID) {
		return
	}
	tailHash := sha256Hex("feishu|failed|" + sessionID + "|" + reason)
	if n.store != nil {
		if dup, err := n.store.HasNotificationWithTailHash(ctx, tailHash, 24*time.Hour); err == nil && dup {
			return
		}
	}
	if err := n.post(ctx, title, body); err != nil {
		n.log.Printf("feishu failure alert failed: %v", err)
		return
	}
	if n.store != nil {
		if _, err := n.store.InsertNotificationWithDedup(ctx, &acpclient.Notification{
			SessionID:        sessionID,
			NotificationType: "feishu",
			Title:            title,
			Body:             body,
			TailHash:         tailHash,
		}); err != nil {
			n.log.Printf("feishu failure record failed: %v", err)
		}
	}
}

// WebhookTail returns the last 6 characters of the webhook URL for masked
// display in the settings view (只回显尾号).
func (n *FeishuNotifier) WebhookTail() string {
	if n == nil || n.webhookURL == "" {
		return ""
	}
	if len(n.webhookURL) > 6 {
		return n.webhookURL[len(n.webhookURL)-6:]
	}
	return n.webhookURL
}

// Test sends a test message from the settings view (发送测试).
func (n *FeishuNotifier) Test(ctx context.Context) error {
	if n == nil || n.webhookURL == "" {
		return fmt.Errorf("feishu webhook not configured")
	}
	return n.post(ctx, "AgentHub 通知测试", "这是一条来自 Hub 控制台的测试消息。如果你看到它，说明 webhook 通道工作正常。")
}

// post sends one text message, adding the signature parameters when a secret
// is configured (飞书自定义机器人加签).
func (n *FeishuNotifier) post(ctx context.Context, title, body string) error {
	payload := map[string]any{
		"msg_type": "text",
		"content":  map[string]string{"text": title + "\n" + body},
	}
	url := n.webhookURL
	if n.secret != "" {
		ts := time.Now().Unix()
		sign, err := feishuSign(ts, n.secret)
		if err != nil {
			return err
		}
		url = fmt.Sprintf("%s?timestamp=%d&sign=%s", n.webhookURL, ts, sign)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
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
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		if resp.StatusCode >= 300 {
			return fmt.Errorf("feishu webhook → %d", resp.StatusCode)
		}
		return nil // non-JSON 2xx: accept
	}
	if out.Code != 0 {
		return fmt.Errorf("feishu webhook error %d: %s", out.Code, out.Msg)
	}
	return nil
}

// feishuSign implements the custom-bot signature: HMAC-SHA256 keyed with
// "timestamp\nsecret" over an empty message, base64-encoded.
func feishuSign(ts int64, secret string) (string, error) {
	mac := hmac.New(sha256.New, []byte(fmt.Sprintf("%d\n%s", ts, secret)))
	if _, err := mac.Write(nil); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(mac.Sum(nil)), nil
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return base64.StdEncoding.EncodeToString(sum[:])
}
