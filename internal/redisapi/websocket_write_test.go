package redisapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
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

// newWedgedWSServer returns an httptest server that completes the WebSocket
// handshake and then never reads, so the client's socket buffers fill and its
// writes block for real.
func newWedgedWSServer(t *testing.T) *httptest.Server {
	t.Helper()

	upgrader := websocket.Upgrader{}
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		<-release
	}))
	// Cleanups run LIFO, so the handler is released before the server is shut
	// down; otherwise srv.Close() would block waiting on the parked handler.
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
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
	srv := newWedgedWSServer(t)

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

// TestCloseReturnsWhileWriterIsWedged exercises the tryLockWrite timeout branch,
// which the other Close test never reaches (its writes complete in
// microseconds, so Close always takes writeMu on the first try).
//
// Here a writer is genuinely stuck holding writeMu with a write timeout far
// longer than the test. Close must give up on the close handshake and shut down
// anyway rather than block behind it, which is the guarantee #312 established
// and the one most at risk from making Close take the write lock at all.
func TestCloseReturnsWhileWriterIsWedged(t *testing.T) {
	srv := newWedgedWSServer(t)

	c := NewWSClient(WSClientConfig{BaseURL: srv.URL, Token: "test-token"})
	c.reconnectEnabled = false
	// Long enough that the wedged writer will not release writeMu on its own
	// during this test: Close has to break the tie.
	c.writeTimeout = 60 * time.Second

	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}

	payload := map[string]any{"blob": strings.Repeat("v", 1<<18)}

	var completed atomic.Int64
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for {
			if err := c.Publish(context.Background(), "wedged", payload); err != nil {
				return
			}
			completed.Add(1)
		}
	}()

	// Wait for the writer to stop making progress, i.e. to be blocked inside
	// WriteMessage holding writeMu.
	prev := int64(-1)
	for range 100 {
		time.Sleep(50 * time.Millisecond)
		cur := completed.Load()
		if cur > 0 && cur == prev {
			break
		}
		prev = cur
	}
	if completed.Load() == 0 {
		t.Fatal("writer never completed a publish; the fixture is not wedging")
	}

	closed := make(chan error, 1)
	go func() { closed <- c.Close() }()

	select {
	case err := <-closed:
		if err != nil {
			t.Errorf("close: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Close blocked behind a wedged writer; the bounded acquire is not working (#312)")
	}

	<-writerDone
}
