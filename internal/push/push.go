// Package push implements Web Push notifications (PRD F9/M1.5): the daemon
// holds VAPID keys, stores browser push subscriptions and delivers high-risk
// approvals / session failures even when every Hub tab is closed.
package push

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
)

// Subscription is one browser push subscription (PushSubscription JSON).
type Subscription struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

// Manager owns the VAPID keys and the subscription registry.
type Manager struct {
	mu      sync.Mutex
	private string // base64url VAPID private key
	public  string // base64url VAPID public key
	subs    []Subscription
	path    string // <data>/push-subs.json (subscriptions survive restarts)
	log     *log.Logger
}

// NewManager loads (or creates) the VAPID keypair from keyPath and the
// subscription registry from subPath.
func NewManager(keyPath, subPath string, logger *log.Logger) (*Manager, error) {
	if logger == nil {
		logger = log.Default()
	}
	m := &Manager{path: subPath, log: logger}
	raw, err := os.ReadFile(keyPath)
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, m); err != nil {
			return nil, fmt.Errorf("push: parse vapid keys: %w", err)
		}
	case os.IsNotExist(err):
		priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, err
		}
		privBytes := priv.D.FillBytes(make([]byte, 32))
		m.private = base64.RawURLEncoding.EncodeToString(privBytes)
		m.public = base64.RawURLEncoding.EncodeToString(append([]byte{4}, append(priv.X.Bytes(), priv.Y.Bytes()...)...))
		out, _ := json.Marshal(m)
		if err := os.WriteFile(keyPath, out, 0o600); err != nil {
			return nil, err
		}
	default:
		return nil, err
	}
	if raw, err := os.ReadFile(subPath); err == nil {
		_ = json.Unmarshal(raw, &m.subs)
	}
	return m, nil
}

// PublicKey returns the base64url VAPID public key handed to browsers.
func (m *Manager) PublicKey() string { return m.public }

// Subscribe registers a push subscription (idempotent on endpoint).
func (m *Manager) Subscribe(s Subscription) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, existing := range m.subs {
		if existing.Endpoint == s.Endpoint {
			return
		}
	}
	m.subs = append(m.subs, s)
	m.persist()
}

// Unsubscribe drops a subscription whose endpoint the push server rejected.
func (m *Manager) Unsubscribe(endpoint string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := m.subs[:0]
	for _, s := range m.subs {
		if s.Endpoint != endpoint {
			out = append(out, s)
		}
	}
	m.subs = out
	m.persist()
}

// Count reports the number of registered subscriptions.
func (m *Manager) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.subs)
}

func (m *Manager) persist() {
	out, err := json.Marshal(m.subs)
	if err != nil {
		return
	}
	_ = os.WriteFile(m.path, out, 0o600)
}

// Notify delivers one summary notification to every subscription. Dead
// endpoints (410) are unsubscribed. Failures are logged, never fatal.
func (m *Manager) Notify(title, body, tag string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	subs := append([]Subscription(nil), m.subs...)
	m.mu.Unlock()

	payload, _ := json.Marshal(map[string]string{"title": title, "body": body, "tag": tag})
	for _, s := range subs {
		sub := &webpush.Subscription{Endpoint: s.Endpoint, Keys: webpush.Keys{
			P256dh: s.Keys.P256dh,
			Auth:   s.Keys.Auth,
		}}
		resp, err := webpush.SendNotification(payload, sub, &webpush.Options{
			Subscriber:      "agenthub@localhost",
			VAPIDPrivateKey: m.private,
			TTL:             int((5 * time.Minute).Seconds()),
		})
		if err != nil {
			m.log.Printf("push: %v", err)
			continue
		}
		if resp.StatusCode == 404 || resp.StatusCode == 410 {
			m.Unsubscribe(s.Endpoint)
		}
		resp.Body.Close()
	}
}
