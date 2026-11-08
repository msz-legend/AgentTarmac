package connector_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// wsReader wraps a WS connection with deadline-based reads and predicate waits.
type wsReader struct {
	conn *websocket.Conn
}

func wsDial(tsURL string) (*websocket.Conn, *http.Response, error) {
	url := "ws" + strings.TrimPrefix(tsURL, "http") + "/ws?token=" + testToken
	return websocket.DefaultDialer.Dial(url, nil)
}

// testWS returns a subscribed WS connection after consuming the hello event.
func testWS(t *testing.T, ts *httptest.Server) *wsReader {
	t.Helper()
	conn, _, err := wsDial(ts.URL)
	if err != nil {
		t.Fatalf("dial ws: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	w := &wsReader{conn: conn}
	first := w.next(t, 5*time.Second)
	if first["type"] != "hello" {
		t.Fatalf("expected hello, got %v", first)
	}
	return w
}

func (w *wsReader) next(t *testing.T, d time.Duration) map[string]any {
	t.Helper()
	type result struct {
		evt map[string]any
		err error
	}
	ch := make(chan result, 1)
	go func() {
		w.conn.SetReadDeadline(time.Now().Add(d))
		_, raw, err := w.conn.ReadMessage()
		if err != nil {
			ch <- result{err: err}
			return
		}
		var evt map[string]any
		if err := json.Unmarshal(raw, &evt); err != nil {
			ch <- result{err: err}
			return
		}
		ch <- result{evt: evt}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("ws read: %v", r.err)
		}
		return r.evt
	case <-time.After(d + 2*time.Second):
		t.Fatalf("ws read timed out after %s", d)
		return nil
	}
}

func (w *wsReader) waitFor(t *testing.T, d time.Duration, pred func(map[string]any) bool, what string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatalf("timeout waiting for ws event: %s", what)
		}
		evt := w.next(t, remaining)
		if pred(evt) {
			return evt
		}
	}
}
