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

func TestHealthyWebSocketMaxAgeReconnectsAndRestoresSubscriptions(t *testing.T) {
	var attempts, subscriptions atomic.Int32
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		attempts.Add(1)
		for {
			var msg WSMessage
			if err := conn.ReadJSON(&msg); err != nil {
				return
			}
			if msg.Type == "subscribe" {
				subscriptions.Add(1)
			}
		}
	}))
	t.Cleanup(server.Close)
	client := NewWSClient(WSClientConfig{BaseURL: server.URL, Token: "synthetic-token"})
	client.maxConnectionAge = 200 * time.Millisecond
	client.reconnectBackoff = 10 * time.Millisecond
	client.maxBackoff = 50 * time.Millisecond
	client.jitterFrac = func() float64 { return 0 }
	client.pingInterval = 50 * time.Millisecond
	client.pongWait = 5 * time.Second
	t.Cleanup(func() { client.Close() })
	if err := client.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := client.Subscribe(context.Background(), "synthetic-channel"); err != nil {
		t.Fatal(err)
	}
	if !waitFor(func() bool { return attempts.Load() >= 2 && subscriptions.Load() >= 2 && client.IsConnected() }, 10*time.Second) {
		t.Fatal("healthy socket never rotated/restored its subscriptions")
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	client.connMu.RLock()
	timerStopped := client.connectionAgeTimer == nil
	client.connMu.RUnlock()
	if !timerStopped {
		t.Fatal("close retained the connection-age timer")
	}
	afterClose := attempts.Load()
	time.Sleep(400 * time.Millisecond)
	if attempts.Load() != afterClose || client.IsConnected() {
		t.Fatal("connection-age timer resurrected the closed client")
	}
}
