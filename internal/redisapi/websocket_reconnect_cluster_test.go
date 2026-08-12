package redisapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// These three tests cover the behaviour added by the #746/#747/#748 fix, which
// the pre-existing suite did not: TestWebSocketUpgrade429* prove the 429 is
// TYPED, and TestConnectRaces... proves the field access is race-clean, but
// nothing asserted that reconnect HONORS the retry_after, that a racing Connect
// does not leave reconnect dialing a second (orphaned) connection, or that the
// reconnect callbacks fire exactly once when Connect wins the race.
//
// Like the rest of websocket_reconnect_test.go, none of these assert on elapsed
// time: a loaded -race runner overshoots short sleeps in the direction that
// HIDES a regression. They assert on dial COUNTS and callback COUNTS with a
// budget several times the schedule, and poll for a value rather than timing a
// sleep. See the note at the top of websocket_reconnect_test.go.

// TestReconnectHonorsServerRetryAfterInsteadOfLocalSchedule is the #747
// regression. connectLocked types a 429 upgrade rejection with the server's
// retry_after; reconnect used to discard it and fall into its own doubling
// schedule, polling far tighter than the server asked -- the #443 shape, where
// a tight retry loop burned a node's daily Redis-API quota and locked it out
// for about a day.
//
// The server always rejects with a 24h retry_after while the client's own
// schedule is a single millisecond. Honoring the hint means exactly ONE dial
// inside the budget (then parked for 24h); discarding it means the local 1ms
// schedule fires dozens to hundreds of times in the same window. The dial count
// is the discriminator.
func TestReconnectHonorsServerRetryAfterInsteadOfLocalSchedule(t *testing.T) {
	ws := &wsUpgradeServer{
		failures:     1 << 30, // always reject
		rejectStatus: http.StatusTooManyRequests,
		rejectBody:   `{"error":"Rate limit exceeded","limit":50000,"window":"day","retry_after":86400}`,
	}
	srv := httptest.NewServer(ws.handler())
	defer srv.Close()

	c := NewWSClient(WSClientConfig{BaseURL: srv.URL, Token: "t"})
	// Manually driven below; keep handleDisconnect from spawning a second
	// reconnect goroutine that would pollute the dial count.
	c.reconnectEnabled = false
	// A tiny LOCAL schedule: without the fix reconnect falls through to this
	// and redials hundreds of times inside the budget; with the fix it honors
	// the server's 24h retry_after and parks after a single dial.
	c.reconnectBackoff = time.Millisecond
	c.maxBackoff = 2 * time.Millisecond
	c.jitterFrac = func() float64 { return 0 }
	t.Cleanup(func() { _ = c.Close() })

	go c.reconnect()

	// Budget several hundred times the local schedule. Asserted on the dial
	// COUNT, not on how long anything took.
	time.Sleep(400 * time.Millisecond)
	_ = c.Close() // release the honored sleep so reconnect exits

	if got := atomic.LoadInt32(&ws.attempts); got != 1 {
		t.Fatalf("upgrade attempts = %d, want 1: reconnect ignored the server's retry_after and fell into its own %v schedule (the #443 quota burn, #747)", got, time.Millisecond)
	}
}

// TestReconnectDoesNotRedialWhenConnectWinsDuringBackoff is the #748 orphan
// regression. reconnect checked c.connected only BEFORE its backoff sleep and
// never again, then called connectLocked unconditionally. A Connect() landing
// during the sleep set c.conn = A; reconnect then woke and dialed B, overwriting
// c.conn without closing A -- an FD and a server-side connection leaked per
// occurrence. #734's keepalive made reconnect run on every read-deadline expiry,
// so these windows opened routinely.
//
// The racing Connect is Client.EnableWebSocket's #723 background retry landing
// mid-reconnect. With the post-sleep re-check the server sees exactly one
// upgrade (Connect's); without it, a second.
func TestReconnectDoesNotRedialWhenConnectWinsDuringBackoff(t *testing.T) {
	ws := &wsUpgradeServer{} // failures 0: always accepts, counts attempts
	srv := httptest.NewServer(ws.handler())
	defer srv.Close()

	c := NewWSClient(WSClientConfig{BaseURL: srv.URL, Token: "t"})
	c.reconnectEnabled = false
	// A fixed backoff long enough that reconnect is reliably still asleep when
	// the racing Connect lands 50ms in. jitterFrac 0 pins the sleep at exactly
	// this base rather than [base, 2*base).
	c.reconnectBackoff = 500 * time.Millisecond
	c.maxBackoff = time.Second
	c.jitterFrac = func() float64 { return 0 }
	var callbacks int32
	c.OnReconnect(func() { atomic.AddInt32(&callbacks, 1) })
	t.Cleanup(func() { _ = c.Close() })

	// reconnect enters its backoff sleep: nothing is connected yet, so
	// bailIfAlreadyReconnected does not short-circuit and it will reach the
	// post-sleep dial.
	go c.reconnect()

	// A racing Connect wins well inside the 500ms window.
	time.Sleep(50 * time.Millisecond)
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("racing Connect: %v", err)
	}

	// Poll (do not time) for reconnect to have woken and fired its callback for
	// the winning connection. Budget covers the 500ms sleep with wide margin.
	deadline := time.Now().Add(3 * time.Second)
	for atomic.LoadInt32(&callbacks) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	if got := atomic.LoadInt32(&ws.attempts); got != 1 {
		t.Fatalf("server upgrades = %d, want 1: reconnect re-dialed after Connect won the race, orphaning a connection (#748)", got)
	}
	if got := atomic.LoadInt32(&callbacks); got != 1 {
		t.Fatalf("OnReconnect fired %d times, want exactly 1: reconnect owns firing the callbacks for the connection that won the race (#748 callback ownership)", got)
	}
}

// TestReconnectBailFiresCallbacksWhenConnectAlreadyWon pins the callback
// ownership decision of #748 directly and deterministically. Connect() itself
// never fires OnReconnect (only real reconnects do), so when a racing Connect
// has already re-established the connection, reconnect is the one piece of code
// responsible for firing the callbacks -- otherwise a consumer that re-arms in
// OnReconnect (re-subscribing at a higher layer) ends up connected with nothing
// flowing, the exact failure #734 exists to prevent. When nothing has
// reconnected, it must fire nothing and let reconnect go on to dial.
func TestReconnectBailFiresCallbacksWhenConnectAlreadyWon(t *testing.T) {
	c := NewWSClient(WSClientConfig{BaseURL: "https://example.invalid", Token: "t"})
	t.Cleanup(func() { _ = c.Close() })

	var callbacks int32
	c.OnReconnect(func() { atomic.AddInt32(&callbacks, 1) })

	// Disconnected: bail is false and NOTHING fires -- reconnect must proceed
	// to dial.
	if c.bailIfAlreadyReconnected() {
		t.Fatal("bailIfAlreadyReconnected() = true while disconnected, want false")
	}
	if got := atomic.LoadInt32(&callbacks); got != 0 {
		t.Fatalf("callbacks fired %d times while disconnected, want 0", got)
	}

	// A racing Connect won: bail is true and the callbacks fire exactly once.
	c.connMu.Lock()
	c.connected = true
	c.connMu.Unlock()

	if !c.bailIfAlreadyReconnected() {
		t.Fatal("bailIfAlreadyReconnected() = false after a racing Connect, want true")
	}
	if got := atomic.LoadInt32(&callbacks); got != 1 {
		t.Fatalf("callbacks fired %d times when Connect won the race, want exactly 1 (#748)", got)
	}
}
