// internal/terminal/server_race_test.go
//go:build !windows

// Regression test for citadel #729 (same bug class as #720/PR #725):
// internal/terminal/server.go used to write to the WebSocket connection from
// two independent goroutines with no synchronization -- the PTY->WebSocket
// relay goroutine and handleConnection's own WebSocket->PTY main loop, which
// replies to resize/ping messages. gorilla/websocket permits exactly one
// concurrent writer per connection and panics with "concurrent write to
// websocket connection" otherwise. This dials a real WebSocket against a real
// PTY session and forces both goroutines to write at once; run with -race to
// catch the underlying data race, not just the panic.
package terminal

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestHandleConnectionConcurrentWrites(t *testing.T) {
	const port = 17881
	cfg := &Config{
		Host:           "127.0.0.1",
		Port:           port,
		MaxConnections: 10,
		IdleTimeout:    30 * time.Minute,
		OrgID:          "org-1",
		Shell:          "/bin/sh",
		RateLimitRPS:   1000,
		RateLimitBurst: 1000,
	}
	auth := NewMockTokenValidator()
	auth.AddValidToken("tok_test", &TokenInfo{UserID: "alice", OrgID: "org-1"})

	s := NewServer(cfg, auth)
	s.SetSilent()
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop(context.Background())
	time.Sleep(100 * time.Millisecond)

	u := fmt.Sprintf("ws://127.0.0.1:%d/terminal?token=tok_test", port)
	conn, resp, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if resp != nil {
		resp.Body.Close()
	}
	defer conn.Close()

	// The test's own client-side conn is itself a single *websocket.Conn
	// shared by several goroutines below; serialize ITS writes too so a
	// self-inflicted client-side panic can never be mistaken for the
	// server-side bug this test targets.
	var sendMu sync.Mutex
	send := func(msg *Message) {
		data, err := msg.Marshal()
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		sendMu.Lock()
		defer sendMu.Unlock()
		if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
			t.Fatalf("client write: %v", err)
		}
	}

	// Kick off a firehose of PTY output: `yes` prints continuously until
	// killed, keeping the server's PTY->WebSocket relay goroutine writing
	// nonstop for the rest of the test.
	send(NewInputMessage([]byte("yes\n")))

	// Drain server->client messages on a background goroutine so the
	// server's writes never block on a full client-side receive buffer.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	// Concurrently hammer resize and ping messages from the client. Each one
	// makes handleConnection's WebSocket->PTY main loop write a reply
	// (resize ack via session.Resize, or a pong), which races the PTY relay
	// goroutine's continuous output writes on the exact same *websocket.Conn.
	const (
		writers   = 20
		perWriter = 25
	)
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < perWriter; j++ {
				if j%5 == 0 {
					send(NewPingMessage())
				} else {
					send(NewResizeMessage(uint16(80+i), uint16(24+j%10)))
				}
			}
		}(i)
	}
	wg.Wait()

	// Stop the output firehose (Ctrl-C) and tear down the connection.
	send(NewInputMessage([]byte{0x03}))
	conn.Close()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for client read loop to exit after close")
	}
}
