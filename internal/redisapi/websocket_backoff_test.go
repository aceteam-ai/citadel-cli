package redisapi

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestReconnectBackoffAccessIsSynchronized is the regression test for issue
// #728: reconnect() read and advanced reconnectBackoff with no lock at all,
// while connectLocked wrote the same field under connMu.
//
// The colliding pair is Connect() versus an in-flight reconnect(), not two
// reconnects: handleDisconnect's connection-pointer guard means a second caller
// failing on the SAME connection returns before it can spawn a second reconnect
// goroutine, because c.conn is already nil by then. Issue
// #723 made that pair ordinary rather than exotic: enableWebSocketWithRetry
// keeps calling Client.EnableWebSocket in the background, EnableWebSocket
// forwards to Connect whenever IsConnected() is false, and IsConnected() is
// false for the whole reconnect window (up to maxBackoff).
//
// The test drives that pair directly. Each round starts disconnected with a
// short schedule so reconnect takes its read-modify-write path, then races one
// reconnect against one connect. The two goroutines are deliberately unordered:
// any channel or lock used to sequence them would establish a happens-before
// edge and hide the very race being asserted. Rounds are joined, so the setup
// between them is single-goroutine.
//
// The racing goroutine calls connectLocked under connMu, which is exactly what
// Connect does minus its loop startup. That was originally a workaround: on the
// #728 branch alone, calling the exported Connect here would have started a
// SECOND read loop on the same gorilla Conn (citadel-cli#740), and the test
// would have failed for a reason #728 does not fix.
//
// #740 is fixed now (loopsOnce), so that constraint is gone and the exported
// Connect is safe to race. This test is deliberately kept at the accessor level
// anyway: it is the narrow, fast check that the read-modify-write itself is
// guarded. TestConnectRacesKeepaliveTeardownOnTheBackoffSchedule in
// websocket_reconnect_test.go is the wider one, driving the exported Connect
// against a reconnect that a real read-deadline expiry started.
func TestReconnectBackoffAccessIsSynchronized(t *testing.T) {
	srv := newDrainingWSServer(t)

	c := NewWSClient(WSClientConfig{BaseURL: srv.URL, Token: "test-token"})
	t.Cleanup(func() { _ = c.Close() })

	ctx := context.Background()

	const rounds = 40
	for i := 0; i < rounds; i++ {
		c.connMu.Lock()
		if c.conn != nil {
			_ = c.conn.Close()
			c.conn = nil
		}
		c.connected = false
		c.reconnectBackoff = time.Millisecond
		c.maxBackoff = time.Millisecond
		c.connMu.Unlock()

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			c.reconnect()
		}()
		go func() {
			defer wg.Done()
			c.connMu.Lock()
			err := c.connectLocked(ctx)
			c.connMu.Unlock()
			if err != nil {
				t.Errorf("connect: %v", err)
			}
		}()
		wg.Wait()
	}
}

// TestNextReconnectDelayAdvancesAndClamps pins the schedule itself: each call
// returns the current delay and leaves the next one doubled, until maxBackoff
// caps it.
//
// It asserts on returned values, never on elapsed time. An earlier backoff test
// elsewhere in this codebase asserted on the wall clock and was flaky in the
// dangerous direction: an overshoot was indistinguishable from a correct wait,
// so the failure this whole area is about (a backoff that is too SHORT) is
// exactly the one such a test cannot see.
// The jitter is stubbed to its zero draw so this keeps pinning the SCHEDULE,
// which is what it is about. The jitter that sits on top of the schedule has
// its own tests in websocket_reconnect_test.go.
func TestNextReconnectDelayAdvancesAndClamps(t *testing.T) {
	c := NewWSClient(WSClientConfig{BaseURL: "https://example.invalid", Token: "t"})
	c.reconnectBackoff = 100 * time.Millisecond
	c.maxBackoff = 400 * time.Millisecond
	c.jitterFrac = func() float64 { return 0 }

	want := []time.Duration{
		100 * time.Millisecond,
		200 * time.Millisecond,
		400 * time.Millisecond,
		400 * time.Millisecond,
		400 * time.Millisecond,
	}
	for i, w := range want {
		if got := c.nextReconnectDelay(); got != w {
			t.Fatalf("call %d: nextReconnectDelay() = %v, want %v", i+1, got, w)
		}
	}
}

// TestNextReconnectDelayAdvancesAtomically checks that the read, the doubling
// and the clamp are one indivisible step.
//
// handleDisconnect's connection-pointer guard means one disconnect event cannot
// spawn two reconnect goroutines: the second caller sees c.conn already nil and
// returns. Two can still coexist, but only if a Connect lands in the reconnect
// window and the connection it establishes then fails, which is the same #723
// precondition as the race above.
//
// So this is mostly about the accessor's own contract. If it were built out of a
// separate load and store, two callers could read the same value and store the
// same doubled one, losing a doubling and making the schedule quietly tighter
// than it looks. Distinct return values rule that out, and the assertion is on
// the set of values, not on order or on time.
func TestNextReconnectDelayAdvancesAtomically(t *testing.T) {
	const (
		callers = 8
		rounds  = 300
	)

	c := NewWSClient(WSClientConfig{BaseURL: "https://example.invalid", Token: "t"})
	// Above the largest expected delay (128ms), so nothing clamps and every
	// caller in a round must come away with a different value.
	c.maxBackoff = time.Second
	// Zero draw: the subject here is the read-modify-write, not the jitter, and
	// a random addend would blur the distinct-values assertion below.
	c.jitterFrac = func() float64 { return 0 }

	// A lost doubling needs the two callers to interleave, so one round proves
	// little. Repeat: rounds are joined, so each one is an independent trial and
	// the assertion stays on values rather than on timing.
	got := make([]time.Duration, callers)
	for r := 0; r < rounds; r++ {
		// Under connMu, which is the field's guard, even though the previous
		// round is already joined.
		c.connMu.Lock()
		c.reconnectBackoff = time.Millisecond
		c.connMu.Unlock()

		var wg sync.WaitGroup
		for i := 0; i < callers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				got[i] = c.nextReconnectDelay()
			}(i)
		}
		wg.Wait()

		seen := make(map[time.Duration]int, callers)
		for _, d := range got {
			seen[d]++
		}
		for i := 0; i < callers; i++ {
			want := time.Duration(1<<uint(i)) * time.Millisecond
			if seen[want] != 1 {
				t.Fatalf("round %d: delay %v handed out %d times, want exactly 1 (all delays: %v)",
					r, want, seen[want], got)
			}
		}
	}
}

// TestSuccessfulConnectResetsReconnectBackoff pins the other half of the
// schedule: a connection that comes back and PROVES itself returns the node to
// the initial delay once it is torn down, so a later blip does not start out
// already backed off to a minute.
//
// This test used to assert the reset happened immediately at Connect()
// success. #746 moved it: connectLocked resetting on every successful dial,
// regardless of what happened next, was the bug -- a peer that dials fine but
// never answers a ping then gets torn down every pongWait, the redial
// succeeds, the schedule resets to 1s, and the node redials forever at
// roughly pongWait+1s instead of backing off. The reset now happens in
// handleDisconnect, and only for a connection old enough to have proved
// itself; see the connEstablishedAt / provenConnDurationMultiplier comments on
// the reconnectBackoff field and on handleDisconnect.
func TestSuccessfulConnectResetsReconnectBackoff(t *testing.T) {
	srv := newDrainingWSServer(t)

	c := NewWSClient(WSClientConfig{BaseURL: srv.URL, Token: "test-token"})
	c.reconnectEnabled = false
	// Scaled down so the test does not have to wait out production's 45s
	// pongWait to reach the proven threshold, but keeping the same 1:4
	// ping-to-deadline ratio so the connection is actually kept alive by real
	// pong traffic (newDrainingWSServer answers pings via gorilla's default
	// handler) rather than free-running to its own read-deadline teardown
	// before the test's manual one.
	c.pingInterval = 10 * time.Millisecond
	c.pongWait = 40 * time.Millisecond
	t.Cleanup(func() { _ = c.Close() })

	c.reconnectBackoff = 42 * time.Second
	// Zero draw, so the assertions below are on the reset value itself.
	c.jitterFrac = func() float64 { return 0 }

	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}

	// Immediately after Connect, the schedule must be UNCHANGED: the
	// connection has had no chance to prove anything yet, and a dial alone
	// must not reset it (#746).
	c.connMu.RLock()
	got := c.reconnectBackoff
	c.connMu.RUnlock()
	if got != 42*time.Second {
		t.Fatalf("schedule right after Connect = %v, want unchanged 42s", got)
	}

	// Let the connection survive well past the proven threshold (kept alive by
	// real pong traffic; see the client setup above), then tear it down the
	// way a real read-deadline expiry would.
	time.Sleep(3 * time.Duration(provenConnDurationMultiplier) * c.pongWait)
	c.connMu.RLock()
	conn := c.conn
	connected := c.connected
	c.connMu.RUnlock()
	if !connected {
		t.Fatal("connection was torn down on its own before the manual handleDisconnect below; the keepalive is not holding it up as expected")
	}
	c.handleDisconnect(conn)

	c.connMu.RLock()
	got = c.reconnectBackoff
	c.connMu.RUnlock()
	if got != initialReconnectBackoff {
		t.Fatalf("schedule after a proven connection's teardown = %v, want %v", got, initialReconnectBackoff)
	}
}
