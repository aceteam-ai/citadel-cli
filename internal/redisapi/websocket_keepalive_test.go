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

// newSilentWSServer upgrades the connection and then does nothing at all: it
// never sends a message and, because it never calls ReadMessage, it never
// answers a ping either. From the client's side that is indistinguishable from
// a peer that vanished without a FIN reaching us, which is the half-open case
// #734 is about.
func newSilentWSServer(t *testing.T) *httptest.Server {
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
	// LIFO: release the parked handler before shutting the server down,
	// otherwise srv.Close() waits on it forever.
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	return srv
}

// waitFor polls cond until it holds or the budget runs out.
func waitFor(cond func() bool, budget time.Duration) bool {
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

// newKeepaliveTestClient builds a client with the keepalive scaled down so a
// test does not have to wait out the production 45s read deadline.
//
// The scaling keeps production's 1:4 ping-to-deadline ratio but stays coarse on
// purpose. CI runs these on a shared, heavily contended runner under -race, and
// a sub-second budget would turn an ordinary scheduling hiccup into a red build
// that gets rerun rather than diagnosed.
func newKeepaliveTestClient(srv *httptest.Server) *WSClient {
	c := NewWSClient(WSClientConfig{BaseURL: srv.URL, Token: "test-token"})
	c.reconnectEnabled = false
	c.pingInterval = 250 * time.Millisecond
	c.pongWait = time.Second
	return c
}

// TestReadDeadlineTearsDownASilentConnection is the core #734 regression.
//
// Without a read deadline, readLoop blocks in ReadMessage forever against a
// peer that has gone away without a FIN. connected stays true, Publish writes
// into a dead socket and reports success, handleDisconnect never fires, and
// citadel status happily reports pubsub_transport: websocket while nothing is
// flowing. The client must notice on its own.
func TestReadDeadlineTearsDownASilentConnection(t *testing.T) {
	srv := newSilentWSServer(t)
	c := newKeepaliveTestClient(srv)

	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = c.Close() }()

	if !c.IsConnected() {
		t.Fatal("IsConnected() = false right after a successful connect")
	}

	if !waitFor(func() bool { return !c.IsConnected() }, 5*time.Second) {
		t.Fatal("still reporting connected against a silent peer: the read deadline never fired (#734)")
	}

	// The whole point of tearing down is that the caller falls back to HTTP
	// instead of publishing into a dead socket and being told it worked.
	if err := c.Publish(context.Background(), "chan", map[string]any{"a": 1}); err == nil {
		t.Fatal("Publish succeeded on a torn-down connection; the silent-success bug is still reachable")
	}
}

// TestKeepaliveHoldsAQuietButHealthyConnection is the counterweight. A
// connection with no application traffic at all must stay up indefinitely,
// because our own pings are answered. It fails if the ping ticker does not run
// or if the pong handler does not push the read deadline out, which is the
// obvious way to "fix" #734 and end up with a node that reconnects every 45s.
func TestKeepaliveHoldsAQuietButHealthyConnection(t *testing.T) {
	// A draining server sits in ReadMessage, so gorilla's default ping handler
	// answers our pings. No application messages are ever exchanged.
	srv := newDrainingWSServer(t)
	c := newKeepaliveTestClient(srv)

	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = c.Close() }()

	// Five times the read deadline with nothing but keepalive traffic.
	deadline := time.Now().Add(5 * c.pongWait)
	for time.Now().Before(deadline) {
		if !c.IsConnected() {
			t.Fatal("healthy but idle connection was torn down: keepalive is not extending the read deadline")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestPingsReachTheServer pins the ping ticker itself. The previous test would
// also pass if the server happened to be chatty; this one asserts the frames
// are actually on the wire at roughly the configured cadence.
func TestPingsReachTheServer(t *testing.T) {
	var pings atomic.Int64

	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		// Overriding the handler replaces gorilla's automatic pong, so answer
		// by hand; otherwise the client would rightly declare us dead.
		conn.SetPingHandler(func(appData string) error {
			pings.Add(1)
			return conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(time.Second))
		})
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)

	c := newKeepaliveTestClient(srv)
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = c.Close() }()

	// pingInterval is 250ms, so a 5s budget for three pings is very slack.
	if !waitFor(func() bool { return pings.Load() >= 3 }, 5*time.Second) {
		t.Fatalf("server saw %d pings, want at least 3: the ping ticker is not running", pings.Load())
	}
}

// TestReadDeadlineDisconnectTriggersReconnect closes the loop: detecting death
// is only useful if it feeds the existing reconnect path. A node that notices
// and then sits there is no better off than one that never noticed.
func TestReadDeadlineDisconnectTriggersReconnect(t *testing.T) {
	var attempts atomic.Int64
	upgrader := websocket.Upgrader{}
	release := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		if attempts.Add(1) == 1 {
			// First connection goes silent: never reads, so never pongs.
			<-release
			return
		}
		// Every later connection behaves, answering pings from ReadMessage.
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	c := NewWSClient(WSClientConfig{BaseURL: srv.URL, Token: "test-token"})
	c.pingInterval = 250 * time.Millisecond
	c.pongWait = time.Second
	// reconnectEnabled defaults on for a configured BaseURL; make it explicit,
	// since the whole subject of this test is the reconnect.
	c.reconnectEnabled = true
	c.reconnectBackoff = 50 * time.Millisecond

	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = c.Close() }()

	if !waitFor(func() bool { return attempts.Load() >= 2 && c.IsConnected() }, 15*time.Second) {
		t.Fatalf("no reconnect after a read-deadline disconnect: attempts=%d connected=%v",
			attempts.Load(), c.IsConnected())
	}

	// The reconnected socket must be armed too. Without this the test would
	// pass on a connection that merely happened to be up at the instant we
	// looked: connectLocked, not Connect, is what has to arm the keepalive,
	// because reconnect never goes through Connect. An unarmed connection has
	// no read deadline and no pings, which is the original bug restored on
	// every socket after the first.
	settled := attempts.Load()
	deadline := time.Now().Add(3 * c.pongWait)
	for time.Now().Before(deadline) {
		if !c.IsConnected() {
			t.Fatal("reconnected connection dropped again: it is not being kept alive")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := attempts.Load(); got != settled {
		t.Fatalf("dial attempts kept climbing (%d -> %d) against a healthy peer: reconnect is flapping", settled, got)
	}
}

// TestKeepaliveIsRearmedOnEveryReconnect pins that connectLocked, not Connect,
// owns the arming.
//
// Reconnect never goes through Connect, so arming there would leave every
// socket after the first with no read deadline and no handlers. That is the
// original bug restored one connection later, and it is invisible to a test
// that only checks the reconnected socket stays up: an unarmed connection is
// MORE stable, not less, because nothing can ever declare it dead. The
// observable difference is whether the client keeps noticing. Here every
// connection the server accepts goes silent, so a correctly armed client
// detects and redials over and over; an armed-once client stalls at two.
func TestKeepaliveIsRearmedOnEveryReconnect(t *testing.T) {
	var attempts atomic.Int64
	upgrader := websocket.Upgrader{}
	release := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		attempts.Add(1)
		<-release
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	c := NewWSClient(WSClientConfig{BaseURL: srv.URL, Token: "test-token"})
	c.pingInterval = 250 * time.Millisecond
	c.pongWait = time.Second
	c.reconnectEnabled = true
	c.reconnectBackoff = 50 * time.Millisecond

	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = c.Close() }()

	// Three accepted connections means the SECOND one was detected as dead too,
	// which can only happen if the reconnect path armed it.
	if !waitFor(func() bool { return attempts.Load() >= 3 }, 25*time.Second) {
		t.Fatalf("server accepted %d connections, want at least 3: the keepalive is not re-armed after a reconnect", attempts.Load())
	}
}

// TestPingsAreSafeAlongsideConcurrentWrites checks the load-bearing claim that
// the ping needs no writeMu.
//
// gorilla permits ONE concurrent writer and panics from flushFrame otherwise,
// taking the worker process with it (#720). WriteControl is documented safe
// alongside other methods and the vendored source backs that up, but the claim
// is worth an actual test: a ping every millisecond against 50 publishers
// pushing 8KB frames. Running the fix with WriteMessage(PingMessage) in place
// of WriteControl panics here.
func TestPingsAreSafeAlongsideConcurrentWrites(t *testing.T) {
	srv := newDrainingWSServer(t)

	c := NewWSClient(WSClientConfig{BaseURL: srv.URL, Token: "test-token"})
	c.reconnectEnabled = false
	c.pingInterval = time.Millisecond
	// Long enough that the read deadline is not what ends this test.
	c.pongWait = time.Minute

	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}

	payload := map[string]any{"blob": strings.Repeat("p", 8192)}

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
				_ = c.Publish(context.Background(), fmt.Sprintf("ping-race-%d", i), payload)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}
