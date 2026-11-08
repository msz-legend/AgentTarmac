package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

// aibotMock is a minimal stand-in for wss://openws.work.weixin.qq.com. It
// records every inbound frame and replies with a correlated ack.
type aibotMock struct {
	t        *testing.T
	frames   []map[string]any
	subCode  int
	sendCode int
	noise    bool // emit an unrelated req_id frame before each ack
	closed   bool
}

func (m *aibotMock) handler(w http.ResponseWriter, r *http.Request) {
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	conn, err := up.Upgrade(w, r, nil)
	if err != nil {
		m.t.Errorf("upgrade: %v", err)
		return
	}
	defer conn.Close()
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var frame map[string]any
		if err := json.Unmarshal(raw, &frame); err != nil {
			m.t.Errorf("bad frame: %v", err)
			return
		}
		m.frames = append(m.frames, frame)
		headers, _ := frame["headers"].(map[string]any)
		reqID, _ := headers["req_id"].(string)
		if m.noise {
			_ = conn.WriteJSON(map[string]any{
				"headers": map[string]any{"req_id": "unrelated-noise"},
				"errcode": 0, "errmsg": "ok",
			})
		}
		code := 0
		if frame["cmd"] == "aibot_subscribe" {
			code = m.subCode
		} else if frame["cmd"] == "aibot_send_msg" {
			code = m.sendCode
		}
		_ = conn.WriteJSON(map[string]any{
			"headers": map[string]any{"req_id": reqID},
			"errcode": code, "errmsg": "mock",
		})
		if frame["cmd"] == "aibot_send_msg" {
			m.closed = true
			return
		}
	}
}

func newAibotMockServer(t *testing.T, mock *aibotMock) (*httptest.Server, string) {
	ts := httptest.NewServer(http.HandlerFunc(mock.handler))
	t.Cleanup(ts.Close)
	return ts, "ws" + strings.TrimPrefix(ts.URL, "http")
}

// Fake BotID for unit tests only — never a real credential. Suffix JW275Q
// matches the masking assertions below.
const aibotTestBotID = "aibsFAKEUNITTESTONLYJW275Q"

func aibotTestConfig(wsURL string) WecomAibotConfig {
	return WecomAibotConfig{
		BotID:    aibotTestBotID,
		Secret:   "unit-test-secret",
		ChatID:   "unit-test-chat",
		ChatType: 1,
		WSURL:    wsURL,
	}
}

// TestWecomAibotPostHappyPath verifies the official two-frame protocol:
// aibot_subscribe (bot_id+secret) then aibot_send_msg (chatid/chat_type/
// markdown), correlated by headers.req_id.
func TestWecomAibotPostHappyPath(t *testing.T) {
	mock := &aibotMock{t: t, subCode: 0, sendCode: 0, noise: true}
	_, wsURL := newAibotMockServer(t, mock)
	tr := NewWecomAibotTransport(aibotTestConfig(wsURL))
	if err := tr.Post(context.Background(), "AgentHub 通知测试：企微通道正常。"); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if len(mock.frames) != 2 {
		t.Fatalf("frames = %d, want 2", len(mock.frames))
	}
	sub := mock.frames[0]
	if sub["cmd"] != "aibot_subscribe" {
		t.Fatalf("frame0 cmd = %v", sub["cmd"])
	}
	subBody, _ := sub["body"].(map[string]any)
	if subBody["bot_id"] != aibotTestBotID || subBody["secret"] != "unit-test-secret" {
		t.Fatalf("subscribe body = %v", subBody)
	}
	subHeaders, _ := sub["headers"].(map[string]any)
	subReqID, _ := subHeaders["req_id"].(string)
	if subReqID == "" {
		t.Fatal("subscribe missing req_id")
	}
	send := mock.frames[1]
	if send["cmd"] != "aibot_send_msg" {
		t.Fatalf("frame1 cmd = %v", send["cmd"])
	}
	sendBody, _ := send["body"].(map[string]any)
	if sendBody["chatid"] != "unit-test-chat" {
		t.Errorf("chatid = %v", sendBody["chatid"])
	}
	if sendBody["chat_type"] != float64(1) {
		t.Errorf("chat_type = %v", sendBody["chat_type"])
	}
	if sendBody["msgtype"] != "markdown" {
		t.Errorf("msgtype = %v", sendBody["msgtype"])
	}
	md, _ := sendBody["markdown"].(map[string]any)
	if md["content"] != "AgentHub 通知测试：企微通道正常。" {
		t.Errorf("markdown.content = %v", md["content"])
	}
	sendHeaders, _ := send["headers"].(map[string]any)
	sendReqID, _ := sendHeaders["req_id"].(string)
	if sendReqID == "" || sendReqID == subReqID {
		t.Errorf("send req_id %q must be present and differ from subscribe %q", sendReqID, subReqID)
	}
	if tr.Tail() != "JW275Q" {
		t.Errorf("Tail() = %q, want masked bot id tail", tr.Tail())
	}
}

func TestWecomAibotChatTypeOmitted(t *testing.T) {
	mock := &aibotMock{t: t}
	_, wsURL := newAibotMockServer(t, mock)
	cfg := aibotTestConfig(wsURL)
	cfg.ChatType = 0
	if err := NewWecomAibotTransport(cfg).Post(context.Background(), "x"); err != nil {
		t.Fatalf("Post: %v", err)
	}
	send := mock.frames[1]
	body, _ := send["body"].(map[string]any)
	if _, ok := body["chat_type"]; ok {
		t.Errorf("chat_type must be omitted when ChatType=0, got %v", body["chat_type"])
	}
}

func TestWecomAibotSubscribeRejected(t *testing.T) {
	mock := &aibotMock{t: t, subCode: 853000}
	_, wsURL := newAibotMockServer(t, mock)
	err := NewWecomAibotTransport(aibotTestConfig(wsURL)).Post(context.Background(), "x")
	if err == nil || !strings.Contains(err.Error(), "853000") {
		t.Fatalf("want subscribe errcode 853000 in error, got %v", err)
	}
	if len(mock.frames) != 1 {
		t.Errorf("must not send aibot_send_msg after a failed subscribe, frames=%d", len(mock.frames))
	}
}

func TestWecomAibotSendRejected(t *testing.T) {
	mock := &aibotMock{t: t, sendCode: 93000}
	_, wsURL := newAibotMockServer(t, mock)
	err := NewWecomAibotTransport(aibotTestConfig(wsURL)).Post(context.Background(), "x")
	if err == nil || !strings.Contains(err.Error(), "93000") {
		t.Fatalf("want send errcode 93000 in error, got %v", err)
	}
}

func TestWecomAibotMissingConfig(t *testing.T) {
	cases := []WecomAibotConfig{
		{Secret: "s", ChatID: "c"},
		{BotID: "b", ChatID: "c"},
		{BotID: "b", Secret: "s"},
	}
	for i, cfg := range cases {
		err := NewWecomAibotTransport(cfg).Post(context.Background(), "x")
		if err == nil || !strings.Contains(err.Error(), "not configured") {
			t.Errorf("case %d: want not-configured error, got %v", i, err)
		}
	}
}

func TestWecomAibotTruncateUTF8(t *testing.T) {
	if got := truncateUTF8("hello", 10); got != "hello" {
		t.Errorf("short string = %q", got)
	}
	s := strings.Repeat("中", 100) // 300 bytes
	got := truncateUTF8(s, 10)
	if len(got) != 9 {
		t.Errorf("truncated bytes = %d, want 9 (3 whole runes)", len(got))
	}
	if !strings.Contains(got, "中") {
		t.Errorf("truncated = %q", got)
	}
}

// TestWecomAibotNotifierWiring proves the notifier surface (Test/WebhookTail)
// reaches the long-connection transport, and that /api/notify/test reports it.
func TestWecomAibotNotifierWiring(t *testing.T) {
	mock := &aibotMock{t: t}
	_, wsURL := newAibotMockServer(t, mock)
	n := NewWecomAibotNotifier(aibotTestConfig(wsURL), nil, nil)
	if got := n.WebhookTail(); got != "JW275Q" {
		t.Errorf("WebhookTail() = %q", got)
	}
	if err := n.Test(context.Background()); err != nil {
		t.Fatalf("Test: %v", err)
	}
	if len(mock.frames) != 2 {
		t.Fatalf("frames = %d, want 2", len(mock.frames))
	}
	var nilNotifier *WecomNotifier
	if err := nilNotifier.Test(context.Background()); err == nil {
		t.Error("nil notifier must report not configured")
	}
}
