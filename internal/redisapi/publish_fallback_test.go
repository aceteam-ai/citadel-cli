package redisapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

// newConnectedThenBrokenWSClient returns a WSClient whose `connected` flag is
// true (so Client.Publish's IsConnected() gate takes the WebSocket branch)
// but whose underlying connection is already closed, so the very next write
// fails with a genuine transport error rather than "not connected".
//
// This is deliberately built WITHOUT calling Connect(): Connect starts a
// background readLoop that would itself observe the closed connection, call
// handleDisconnect, and flip `connected` back to false -- racing this test's
// own attempt to exercise "IsConnected()==true, but the write errors" and
// occasionally collapsing it into the ALREADY-COVERED "not connected -> HTTP"
// path instead. Setting conn/connected directly, with no reader goroutine in
// the picture, makes the race disappear: nothing touches `connected` until
// Client.Publish's own call to WSClient.Publish does.
func newConnectedThenBrokenWSClient(t *testing.T) *WSClient {
	t.Helper()

	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		// Nothing to do server-side; the server connection is torn down by
		// srv.Close() in t.Cleanup below.
		_ = conn
	}))
	t.Cleanup(srv.Close)

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	// Close immediately: the handshake succeeded (proving the client is a
	// genuinely "connected" WS client, not one that never dialed), but every
	// subsequent write on this conn will now fail.
	_ = conn.Close()

	wsc := NewWSClient(WSClientConfig{BaseURL: srv.URL, Token: "test-token"})
	wsc.reconnectEnabled = false
	wsc.conn = conn
	wsc.connected = true
	return wsc
}

// TestPublishFallsBackToHTTPWhenWebSocketPublishErrors pins the citadel-cli#985
// fix: Client.Publish used to prefer WebSocket "when connected" and simply
// return whatever error the WS transport produced, so a WS write that failed
// mid-flight (as opposed to never being connected in the first place) lost
// the publish entirely -- exactly the shape that dropped a WriteClaimed/
// WriteEnd event and caused a false coordinator fast-fail. It must now retry
// over HTTP synchronously, in the same call, and succeed.
func TestPublishFallsBackToHTTPWhenWebSocketPublishErrors(t *testing.T) {
	httpSrv, gotPath := newContractServer(t, http.StatusOK, `{"published":true}`)
	client := newContractClient(httpSrv.URL)

	client.wsClient = newConnectedThenBrokenWSClient(t)
	t.Cleanup(func() { _ = client.wsClient.Close() })

	if !client.wsClient.IsConnected() {
		t.Fatal("fixture setup: expected the WS client to report connected before the broken-write publish")
	}

	err := client.Publish(context.Background(), "stream:v1:job-1", map[string]any{"hello": "world"})
	if err != nil {
		t.Fatalf("Publish should fall back to HTTP and succeed when the WS transport errors, got: %v", err)
	}
	if *gotPath != "/api/fabric/redis/pubsub/publish" {
		t.Errorf("expected the HTTP fallback route to be hit, got path %q", *gotPath)
	}
}

// TestPublishDoesNotFallBackWhenWebSocketSucceeds guards the "no
// double-publish" half of the same fix: when the WS write genuinely succeeds,
// Publish must return immediately and never touch the HTTP route.
func TestPublishDoesNotFallBackWhenWebSocketSucceeds(t *testing.T) {
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)

	wsc := NewWSClient(WSClientConfig{BaseURL: srv.URL, Token: "test-token"})
	wsc.reconnectEnabled = false
	if err := wsc.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = wsc.Close() })

	httpHit := false
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpHit = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"published":true}`))
	}))
	t.Cleanup(httpSrv.Close)

	client := newContractClient(httpSrv.URL)
	client.wsClient = wsc

	if err := client.Publish(context.Background(), "stream:v1:job-1", map[string]any{"hello": "world"}); err != nil {
		t.Fatalf("Publish over a healthy WS connection should succeed, got: %v", err)
	}
	if httpHit {
		t.Error("Publish must not fall back to HTTP when the WebSocket publish already succeeded (would double-publish)")
	}
}
