package redisapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// newDrainingWSServer returns an httptest server that upgrades any request and
// then reads until the peer goes away. Draining matters: if the server never
// reads, kernel socket buffers fill, every writer blocks, and the test hangs
// instead of exercising the write path.
func newDrainingWSServer(t *testing.T) *httptest.Server {
	t.Helper()

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
	return srv
}

// newTestWSClient connects a WSClient to srv with reconnection disabled.
//
// Reconnect is off on purpose: reconnect() mutates reconnectBackoff without
// holding connMu, which is a separate (real, unfixed) data race in this file.
// Letting it run would make -race report something unrelated to the write path
// under test.
func newTestWSClient(t *testing.T, srv *httptest.Server) *WSClient {
	t.Helper()

	c := NewWSClient(WSClientConfig{BaseURL: srv.URL, Token: "test-token"})
	c.reconnectEnabled = false

	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	return c
}

// TestSendMessageConcurrentWriters drives many goroutines through the same
// write path that the heartbeat publisher, job streaming, the consume-loop ack
// and reconnect resubscription all share.
//
// gorilla permits exactly one concurrent writer per connection; more than one
// raises "panic: concurrent write to websocket connection" from flushFrame,
// which takes down the whole worker process (#720). Before the writeMu fix
// this test panics or trips -race; the payload is deliberately several KB so
// each WriteMessage is long enough to overlap the next.
func TestSendMessageConcurrentWriters(t *testing.T) {
	srv := newDrainingWSServer(t)
	c := newTestWSClient(t, srv)

	payload := map[string]any{"blob": strings.Repeat("x", 8192)}

	const (
		writers   = 50
		perWriter = 50
	)

	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			for range perWriter {
				// Errors are not the subject here: the connection may be torn
				// down by a sibling goroutine's failure. The assertion is that
				// concurrent writes do not panic or race.
				_ = c.Publish(context.Background(), fmt.Sprintf("chan-%d", i), payload)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// TestCloseDuringConcurrentWrites covers the second write site: Close sends a
// close frame and calls SetWriteDeadline, both of which are gorilla write
// methods (SetWriteDeadline is a bare field assignment on the Conn, so it
// races an in-flight WriteMessage under -race). Close must therefore take the
// same writeMu, and must still finish promptly.
func TestCloseDuringConcurrentWrites(t *testing.T) {
	srv := newDrainingWSServer(t)
	c := newTestWSClient(t, srv)

	payload := map[string]any{"blob": strings.Repeat("y", 8192)}

	const writers = 25

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := range writers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = c.Publish(context.Background(), fmt.Sprintf("chan-%d", i), payload)
			}
		}(i)
	}

	// Let the writers get in flight, then close underneath them.
	time.Sleep(50 * time.Millisecond)

	closed := make(chan error, 1)
	go func() { closed <- c.Close() }()

	select {
	case err := <-closed:
		if err != nil {
			t.Errorf("close: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Error("Close did not return; shutdown is blocked behind a writer (#312)")
	}

	close(stop)
	wg.Wait()
}

// TestWedgedWriteTimesOutInsteadOfParkingPublishers is the counterweight to the
// write mutex. Serializing writes means a single blocked WriteMessage no longer
// stalls only its own caller, it stalls every publisher queued behind writeMu:
// heartbeat, stream chunks, acks, chat presence. Bounding the write with a
// deadline is what keeps that from turning a panic into a silent stall.
//
// The server here completes the handshake and then never reads, so socket
// buffers fill and writes block for real. Every publisher must come back with
// an error rather than parking. Without the deadline they park forever and this
// test fails on its own timeout.
func TestWedgedWriteTimesOutInsteadOfParkingPublishers(t *testing.T) {
	upgrader := websocket.Upgrader{}
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		<-release // never read: let the socket buffers fill
	}))
	defer srv.Close()
	defer close(release)

	c := NewWSClient(WSClientConfig{BaseURL: srv.URL, Token: "test-token"})
	c.reconnectEnabled = false
	// Production uses defaultWriteTimeout (15s). Scaled down so the test does
	// not spend that long proving the bound exists.
	c.writeTimeout = 250 * time.Millisecond

	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = c.Close() }()

	payload := map[string]any{"blob": strings.Repeat("w", 1<<18)} // 256KB per frame

	const (
		publishers    = 8
		maxPerWriter  = 200
		overallBudget = 30 * time.Second
	)

	results := make(chan error, publishers)
	for i := range publishers {
		go func(i int) {
			var last error
			for range maxPerWriter {
				last = c.Publish(context.Background(), fmt.Sprintf("wedged-%d", i), payload)
				if last != nil {
					break
				}
			}
			results <- last
		}(i)
	}

	budget := time.After(overallBudget)
	failed := 0
	for range publishers {
		select {
		case err := <-results:
			if err != nil {
				failed++
			}
		case <-budget:
			t.Fatal("publishers parked behind a wedged write: WriteMessage is not bounded by a write deadline")
		}
	}

	if failed == 0 {
		t.Fatal("expected the wedged socket to surface a write error, got none")
	}
}
