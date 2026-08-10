package redisapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// wsUpgradeServer serves the WebSocket route, rejecting the first `failures`
// upgrade attempts before accepting. reject is the status written on a rejected
// attempt; rejectBody/rejectHeader let a test shape a 429.
type wsUpgradeServer struct {
	failures      int32
	attempts      int32
	rejectStatus  int
	rejectBody    string
	rejectHeaders map[string]string
}

func (s *wsUpgradeServer) handler() http.HandlerFunc {
	upgrader := websocket.Upgrader{}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/fabric/redis/ws" {
			http.NotFound(w, r)
			return
		}
		n := atomic.AddInt32(&s.attempts, 1)
		if n <= atomic.LoadInt32(&s.failures) {
			for k, v := range s.rejectHeaders {
				w.Header().Set(k, v)
			}
			status := s.rejectStatus
			if status == 0 {
				status = http.StatusBadGateway
			}
			w.WriteHeader(status)
			if s.rejectBody != "" {
				_, _ = w.Write([]byte(s.rejectBody))
			}
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		// Hold the connection open; the client only needs it to be established.
		go func() {
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					conn.Close()
					return
				}
			}
		}()
	}
}

// TestEnableWebSocketRetryableAfterFailedFirstConnect is the regression test for
// issue #723: a failed FIRST connect used to nil out the WebSocket client, so
// there was nothing left to retry and the process stayed on the (broken) HTTP
// publish fallback forever. A second call must be able to succeed.
func TestEnableWebSocketRetryableAfterFailedFirstConnect(t *testing.T) {
	ws := &wsUpgradeServer{failures: 1}
	srv := httptest.NewServer(ws.handler())
	defer srv.Close()

	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "t"})
	defer c.Close()

	ctx := context.Background()

	if err := c.EnableWebSocket(ctx); err == nil {
		t.Fatal("first EnableWebSocket succeeded, want failure (server rejects attempt 1)")
	}
	if c.WebSocket() == nil {
		t.Fatal("WebSocket client was discarded after a failed connect; nothing is left to retry (#723)")
	}
	if got := c.PubSubTransport(); got != PubSubTransportHTTP {
		t.Errorf("PubSubTransport after failure = %q, want %q", got, PubSubTransportHTTP)
	}

	if err := c.EnableWebSocket(ctx); err != nil {
		t.Fatalf("second EnableWebSocket failed, want success on retry: %v", err)
	}
	if got := c.PubSubTransport(); got != PubSubTransportWebSocket {
		t.Errorf("PubSubTransport after successful retry = %q, want %q", got, PubSubTransportWebSocket)
	}
	if !c.IsWebSocketConnected() {
		t.Error("IsWebSocketConnected() = false after a successful retry")
	}
}

// TestEnableWebSocketIdempotentWhenConnected proves a repeated call on a live
// connection is a cheap no-op rather than a second dial. The background retry
// loop calls this in a loop, so a non-idempotent version would re-dial (and
// start a second read loop) on every tick.
func TestEnableWebSocketIdempotentWhenConnected(t *testing.T) {
	ws := &wsUpgradeServer{}
	srv := httptest.NewServer(ws.handler())
	defer srv.Close()

	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "t"})
	defer c.Close()

	if err := c.EnableWebSocket(context.Background()); err != nil {
		t.Fatalf("EnableWebSocket: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := c.EnableWebSocket(context.Background()); err != nil {
			t.Fatalf("repeat EnableWebSocket: %v", err)
		}
	}
	if got := atomic.LoadInt32(&ws.attempts); got != 1 {
		t.Errorf("upgrade attempts = %d, want 1 (repeat calls must not re-dial)", got)
	}
}

// TestEnableWebSocketRefusedAfterClose keeps a still-running retry loop from
// resurrecting the transport after shutdown has begun.
func TestEnableWebSocketRefusedAfterClose(t *testing.T) {
	ws := &wsUpgradeServer{}
	srv := httptest.NewServer(ws.handler())
	defer srv.Close()

	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "t"})
	_ = c.Close()

	if err := c.EnableWebSocket(context.Background()); err == nil {
		t.Fatal("EnableWebSocket succeeded after Close, want refusal")
	}
	if atomic.LoadInt32(&ws.attempts) != 0 {
		t.Error("EnableWebSocket dialed after Close")
	}
}

// TestWebSocketUpgrade429IsTypedRateLimit is what makes the retry honor the
// server's backoff. The dial path is not doRequest, so without explicit typing a
// rate-limited handshake comes back untyped and a retry loop falls into its
// generic backoff -- polling far tighter than retry_after asked, which is the
// quota-burning behaviour of #443.
func TestWebSocketUpgrade429IsTypedRateLimit(t *testing.T) {
	ws := &wsUpgradeServer{
		failures:     1,
		rejectStatus: http.StatusTooManyRequests,
		rejectBody:   `{"error":"Rate limit exceeded","limit":50000,"window":"day","retry_after":86400}`,
	}
	srv := httptest.NewServer(ws.handler())
	defer srv.Close()

	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "t"})
	defer c.Close()

	err := c.EnableWebSocket(context.Background())
	if err == nil {
		t.Fatal("EnableWebSocket succeeded, want a 429 failure")
	}
	rle, ok := AsRateLimitError(err)
	if !ok {
		t.Fatalf("429 upgrade rejection is not a *RateLimitError: %v", err)
	}
	if got := rle.Wait(time.Now()); got != 24*time.Hour {
		t.Errorf("Wait() = %s, want 24h from retry_after", got)
	}
	if rle.Limit != 50000 || rle.Window != "day" {
		t.Errorf("limit/window = %d/%q, want 50000/day", rle.Limit, rle.Window)
	}
}

// TestWebSocketUpgrade429UsesRetryAfterHeader covers the edge-proxy case: a 429
// with no JSON body at all, only the standard header.
func TestWebSocketUpgrade429UsesRetryAfterHeader(t *testing.T) {
	ws := &wsUpgradeServer{
		failures:      1,
		rejectStatus:  http.StatusTooManyRequests,
		rejectHeaders: map[string]string{"Retry-After": "120"},
	}
	srv := httptest.NewServer(ws.handler())
	defer srv.Close()

	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "t"})
	defer c.Close()

	err := c.EnableWebSocket(context.Background())
	rle, ok := AsRateLimitError(err)
	if !ok {
		t.Fatalf("429 upgrade rejection is not a *RateLimitError: %v", err)
	}
	if got := rle.Wait(time.Now()); got != 2*time.Minute {
		t.Errorf("Wait() = %s, want 2m from the Retry-After header", got)
	}
}
