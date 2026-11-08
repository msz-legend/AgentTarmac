package relay

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestRelayEndToEnd walks PRD D8's chain: the Hub dials the relay outbound,
// a browser request arrives at the relay, gets piped to the Hub handler and
// the answer streams back — nothing stored, nothing exposed but the pipe.
func TestRelayEndToEnd(t *testing.T) {
	// The "Hub": a plain handler with a Bearer-protected route.
	hubMux := http.NewServeMux()
	hubMux.HandleFunc("GET /api/echo", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer hub-token" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"from":"hub"}`))
	})

	// The relay: token-authed hub channel.
	relaySrv := httptest.NewServer(New("relay-token", nil).Handler())
	defer relaySrv.Close()

	// Hub dials OUT (ws:// in tests; TLS is a deployment concern).
	dialer := &Dialer{
		RelayURL: "ws" + strings.TrimPrefix(relaySrv.URL, "http") + "/relay/hub/my-hub?token=relay-token",
		Handler:  hubMux,
		Log:      nil,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = dialer.Run(ctx) }()
	time.Sleep(300 * time.Millisecond)

	// Browser → relay → hub → back.
	req, _ := http.NewRequest("GET", relaySrv.URL+"/my-hub/api/echo", nil)
	req.Header.Set("Authorization", "Bearer hub-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("through relay: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"from":"hub"`) {
		t.Fatalf("relayed response: %d %s", resp.StatusCode, body)
	}

	// The Bearer token is enforced end-to-end (the relay forwards it).
	badReq, _ := http.NewRequest("GET", relaySrv.URL+"/my-hub/api/echo", nil)
	badResp, err := http.DefaultClient.Do(badReq)
	if err != nil {
		t.Fatalf("bad auth relay: %v", err)
	}
	badResp.Body.Close()
	if badResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized relayed request = %d, want 401", badResp.StatusCode)
	}

	// Offline hub → 502 (no queueing, no storage).
	offlineReq, _ := http.NewRequest("GET", relaySrv.URL+"/ghost/api/echo", nil)
	offlineResp, err := http.DefaultClient.Do(offlineReq)
	if err != nil {
		t.Fatalf("offline hub relay: %v", err)
	}
	offlineResp.Body.Close()
	if offlineResp.StatusCode != http.StatusBadGateway {
		t.Fatalf("offline hub = %d, want 502", offlineResp.StatusCode)
	}
}
