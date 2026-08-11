package redisapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// No test in this file measures elapsed time. Wall-clock assertions on a
// contended CI runner under -race are flaky in the direction that HIDES a
// regression: an overloaded machine overshoots a short sleep enough that it
// looks like a long one, so a backoff that has collapsed to nothing still
// passes. The same reasoning is written out above backoffSleep in cmd/work.go,
// which exists because that mistake was already shipped here once.
//
// So the jitter tests assert on the value nextReconnectDelay RETURNS, with the
// randomness injected, and the Close test asserts on a dial COUNT with a budget
// several times the backoff rather than on when the dial happened.

// TestConnectRacesKeepaliveTeardownOnTheBackoffSchedule is the test that
// integrating #728 and #734 actually requires, and that neither PR had.
//
// #743's race test deliberately called connectLocked under connMu instead of the
// exported Connect, because on that branch a Connect landing inside a reconnect
// window would start a SECOND readLoop on the same gorilla Conn (#740) and the
// test would fail for a reason #728 does not fix. #745's loopsOnce removes that
// obstacle, so this can drive the real path.
//
// #745's own keepalive tests do not prove #728 absent either: in them both
// writes to reconnectBackoff land on the same goroutine, so -race is clean for
// the wrong reason.
//
// Here the two writes are on genuinely different goroutines, reached the way
// production reaches them:
//
//   - Every connection the server accepts goes silent, so every one dies at the
//     read deadline. That is the #734 path, and it runs handleDisconnect ->
//     go reconnect() -> nextReconnectDelay. Before #734 this fired almost never.
//   - A background loop calls the exported Connect whenever IsConnected() is
//     false, which is the exact shape of Client.EnableWebSocket under #723's
//     background retry. Connect -> connectLocked writes the same field.
//
// Nothing orders those two goroutines against each other. Any channel or mutex
// used to sequence them would establish the happens-before edge that hides the
// race being asserted. The test therefore asserts nothing itself; the race
// detector is the assertion.
//
// It is ALSO the regression test for #740, which is why it is worth driving the
// exported Connect rather than connectLocked. Restoring the pre-loopsOnce bare
// `go c.readLoop()` in Connect makes this test report the exact trace #740 was
// filed with: two readLoop goroutines inside gorilla's NextReader on the same
// Conn. Without that, #740 would be closed here with no coverage at all, and a
// later refactor that dropped loopsOnce would reintroduce it silently.
func TestConnectRacesKeepaliveTeardownOnTheBackoffSchedule(t *testing.T) {
	srv := newSilentWSServer(t)

	c := NewWSClient(WSClientConfig{BaseURL: srv.URL, Token: "test-token"})
	c.pingInterval = 20 * time.Millisecond
	c.pongWait = 100 * time.Millisecond
	c.reconnectEnabled = true
	c.reconnectBackoff = time.Millisecond
	c.maxBackoff = 2 * time.Millisecond
	t.Cleanup(func() { _ = c.Close() })

	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if !c.IsConnected() {
				// Errors are uninteresting: a losing dial is a normal outcome
				// of racing a reconnect, and the subject is the field access.
				_ = c.Connect(context.Background())
			}
			time.Sleep(200 * time.Microsecond)
		}
	}()

	// The budget only decides how many teardown cycles the detector gets to
	// look at (pongWait is 100ms, so this is roughly twenty). Nothing is
	// asserted about how long anything took.
	time.Sleep(2 * time.Second)

	close(stop)
	wg.Wait()
}

// TestReconnectDelayIsNeverShorterThanTheSchedule is the property that decided
// the jitter scheme.
//
// The delay is drawn from [base, 2*base): additive and upward-only. Full jitter,
// rand(0, base), was rejected precisely because it fails this: its floor is zero
// and its mean is half the schedule, so a node that has just been answered 429
// could redial immediately and fleet-wide retry pressure doubles. That is the
// #443 shape, and #728 names too-aggressive as the direction that matters.
func TestReconnectDelayIsNeverShorterThanTheSchedule(t *testing.T) {
	// The extremes of the draw bracket the whole window.
	for name, frac := range map[string]float64{"lowest draw": 0, "highest draw": 0.999999} {
		t.Run(name, func(t *testing.T) {
			c := NewWSClient(WSClientConfig{BaseURL: "https://example.invalid", Token: "t"})
			c.reconnectBackoff = 100 * time.Millisecond
			c.maxBackoff = 400 * time.Millisecond
			c.jitterFrac = func() float64 { return frac }

			// The schedule the delays must bracket, including the clamp.
			bases := []time.Duration{
				100 * time.Millisecond,
				200 * time.Millisecond,
				400 * time.Millisecond,
				400 * time.Millisecond,
				400 * time.Millisecond,
			}
			for i, base := range bases {
				got := c.nextReconnectDelay()
				if got < base {
					t.Fatalf("attempt %d: delay %v is SHORTER than the schedule step %v; jitter must never make a retry more aggressive than it was before", i+1, got, base)
				}
				if got >= 2*base {
					t.Fatalf("attempt %d: delay %v is outside the [%v, %v) window", i+1, got, base, 2*base)
				}
			}
		})
	}
}

// TestReconnectDelaySpreadSurvivesAtTheMaximumBackoff pins the case that made
// the "do not clamp the jittered result to maxBackoff" decision.
//
// A long control-plane outage puts every node in the fleet at the ceiling, which
// is exactly when a lockstep redial is most damaging against a rate-limited
// endpoint. Clamping the jittered delay to maxBackoff would make every node at
// the ceiling sleep exactly maxBackoff, collapsing the spread to zero in the
// worst case. maxBackoff therefore caps the SCHEDULE, not the sleep.
func TestReconnectDelaySpreadSurvivesAtTheMaximumBackoff(t *testing.T) {
	c := NewWSClient(WSClientConfig{BaseURL: "https://example.invalid", Token: "t"})
	c.maxBackoff = time.Minute
	// Pinned at the ceiling: nextReconnectDelay leaves it there.
	c.reconnectBackoff = c.maxBackoff

	ceiling := c.maxBackoff
	const draws = 200
	var sawLow, sawHigh bool
	for i := 0; i < draws; i++ {
		d := c.nextReconnectDelay()
		if d < ceiling {
			t.Fatalf("draw %d: delay %v is shorter than the ceiling %v", i, d, ceiling)
		}
		if d >= 2*ceiling {
			t.Fatalf("draw %d: delay %v is outside the [%v, %v) window", i, d, ceiling, 2*ceiling)
		}
		if d < ceiling+ceiling/4 {
			sawLow = true
		}
		if d > ceiling+3*ceiling/4 {
			sawHigh = true
		}
	}
	// With 200 uniform draws, missing either quarter is a ~1e-25 event, so this
	// is a statement about the code and not about luck.
	if !sawLow || !sawHigh {
		t.Fatalf("delays at the ceiling did not spread across the window (saw a low draw: %v, saw a high draw: %v); every node pinned at maxBackoff would redial in lockstep", sawLow, sawHigh)
	}
}

// TestJitterDoesNotContaminateTheStoredSchedule keeps the randomness in the
// returned sleep and out of the state.
//
// If the jittered value were stored back, randomness would compound across
// attempts, connectLocked's reset would no longer mean exactly "back to one
// second", and the schedule would stop being something a reader can predict from
// the constants. Only the sleep is random.
func TestJitterDoesNotContaminateTheStoredSchedule(t *testing.T) {
	c := NewWSClient(WSClientConfig{BaseURL: "https://example.invalid", Token: "t"})
	c.reconnectBackoff = 100 * time.Millisecond
	c.maxBackoff = 400 * time.Millisecond
	// A mid-window draw, so a contaminated schedule diverges immediately.
	c.jitterFrac = func() float64 { return 0.5 }

	want := []time.Duration{
		200 * time.Millisecond,
		400 * time.Millisecond,
		400 * time.Millisecond,
		400 * time.Millisecond,
	}
	for i, w := range want {
		_ = c.nextReconnectDelay()
		c.connMu.RLock()
		got := c.reconnectBackoff
		c.connMu.RUnlock()
		if got != w {
			t.Fatalf("after call %d the stored schedule = %v, want %v: the jitter is leaking into the state", i+1, got, w)
		}
	}
}

// TestCloseDuringTheBackoffPreventsAFurtherDial is the #741 regression.
//
// reconnect checked c.done at the top of the loop and then slept for the whole
// backoff without re-checking it, so a Close landing inside that sleep still let
// the loop dial afterwards. On success it set connected = true and c.conn on a
// client the caller had already closed, so IsConnected() reported true after
// Close returned and the socket it opened was left dangling.
//
// It is fixed here rather than filed because the keepalive changes its odds: the
// window used to open almost never and now opens on every read-deadline expiry,
// and the jitter above lengthens the sleep it lives in.
//
// The assertion is a dial count under a budget several times the backoff, never
// an elapsed-time comparison. A broken implementation reaches the second dial at
// about one second; a correct one never reaches it at all.
func TestCloseDuringTheBackoffPreventsAFurtherDial(t *testing.T) {
	var dials atomic.Int64
	upgrader := websocket.Upgrader{}
	release := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		dials.Add(1)
		<-release
	}))
	// LIFO: release the parked handlers before the server shuts down.
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	c := NewWSClient(WSClientConfig{BaseURL: srv.URL, Token: "test-token"})
	c.reconnectEnabled = true
	// The read deadline must not be what ends this test.
	c.pingInterval = time.Second
	c.pongWait = time.Minute
	// One second of backoff, entered deterministically: Close lands about a
	// hundredth of the way into it, and the check below waits five times it.
	c.reconnectBackoff = time.Second
	c.maxBackoff = time.Second
	c.jitterFrac = func() float64 { return 0 }

	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if got := dials.Load(); got != 1 {
		t.Fatalf("dials after the first connect = %d, want 1", got)
	}

	// Tear the connection down the way a read error would, which schedules the
	// reconnect and starts its backoff sleep.
	c.connMu.RLock()
	conn := c.conn
	c.connMu.RUnlock()
	c.handleDisconnect(conn)

	// Close well inside the backoff window.
	time.Sleep(10 * time.Millisecond)
	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if waitFor(func() bool { return dials.Load() >= 2 }, 5*time.Second) {
		t.Fatalf("reconnect dialed after Close (%d dials): done is not re-checked after the backoff sleep (#741)", dials.Load())
	}
	if c.IsConnected() {
		t.Fatal("IsConnected() is true after Close: a reconnect marked a closed client connected")
	}
}
