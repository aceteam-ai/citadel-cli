package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/redisapi"
)

// recordingConnector fails a fixed number of times then succeeds, recording the
// wall-clock time of every attempt so a test can assert the loop backs off
// rather than hot-looping.
type recordingConnector struct {
	mu        sync.Mutex
	failures  int
	attemptAt []time.Time
	err       error
}

func (r *recordingConnector) Connect(_ context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.attemptAt = append(r.attemptAt, time.Now())
	if len(r.attemptAt) <= r.failures {
		if r.err != nil {
			return r.err
		}
		return errors.New("WebSocket connection failed: connection refused")
	}
	return nil
}

func (r *recordingConnector) attempts() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.attemptAt)
}

func (r *recordingConnector) gaps() []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []time.Duration
	for i := 1; i < len(r.attemptAt); i++ {
		out = append(out, r.attemptAt[i].Sub(r.attemptAt[i-1]))
	}
	return out
}

// logCollector captures the messages enableWebSocketWithRetry emits, so a test
// can assert the failure and the recovery are BOTH visible. The pre-fix code
// logged the failure at Debug only, which is why a node in this state produced
// no evidence at default verbosity (issue #723).
type logCollector struct {
	mu    sync.Mutex
	lines []string
}

func (l *logCollector) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logCollector) contains(sub string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, ln := range l.lines {
		if strings.Contains(ln, sub) {
			return true
		}
	}
	return false
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return cond()
}

// TestEnableWebSocketWithRetry_FirstConnectFailsThenSucceeds is the core
// regression for #723: a failed startup connect must not be terminal.
func TestEnableWebSocketWithRetry_FirstConnectFailsThenSucceeds(t *testing.T) {
	withFastBackoff(t)

	conn := &recordingConnector{failures: 1}
	logs := &logCollector{}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	returned := make(chan (<-chan struct{}), 1)
	go func() { returned <- enableWebSocketWithRetry(ctx, conn, logs.logf) }()

	var done <-chan struct{}
	select {
	case done = <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("enableWebSocketWithRetry blocked; it must return immediately and retry in the background")
	}
	defer func() { <-done }()

	if !waitFor(t, 3*time.Second, func() bool { return conn.attempts() >= 2 }) {
		t.Fatalf("connector attempted %d time(s); the failed first connect was never retried (#723)", conn.attempts())
	}

	if !waitFor(t, 3*time.Second, func() bool { return logs.contains("connected on retry") }) {
		t.Error("recovery was never logged; a node that self-heals must say so")
	}
	if !logs.contains("falling back to HTTP") {
		t.Error("the initial failure was not logged at Log level; the degraded state stays invisible")
	}

	// Once connected the loop must stop attempting: no steady-state dialing.
	settled := conn.attempts()
	time.Sleep(50 * time.Millisecond)
	if got := conn.attempts(); got != settled {
		t.Errorf("connector still being dialed after success (%d -> %d); the retry loop must exit", settled, got)
	}
}

// TestEnableWebSocketWithRetry_SucceedsFirstTry proves the healthy path is
// unchanged: one synchronous attempt, no goroutine, no log noise.
func TestEnableWebSocketWithRetry_SucceedsFirstTry(t *testing.T) {
	withFastBackoff(t)

	conn := &recordingConnector{failures: 0}
	logs := &logCollector{}

	done := enableWebSocketWithRetry(context.Background(), conn, logs.logf)
	select {
	case <-done:
	default:
		t.Fatal("healthy path started a background retry loop")
	}

	if got := conn.attempts(); got != 1 {
		t.Errorf("attempts = %d, want exactly 1 on the healthy path", got)
	}
	time.Sleep(20 * time.Millisecond)
	if got := conn.attempts(); got != 1 {
		t.Errorf("attempts = %d after settling, want 1 (no background loop should have started)", got)
	}
	if len(logs.lines) != 0 {
		t.Errorf("healthy path logged %v, want silence", logs.lines)
	}
}

// TestEnableWebSocketWithRetry_BacksOffRatherThanHotLooping is the #443 guard:
// the retry must not become a tight dial loop that burns the node's daily
// Redis-API quota and self-DoSes the node it is trying to reconnect.
func TestEnableWebSocketWithRetry_BacksOffRatherThanHotLooping(t *testing.T) {
	// Deliberately NOT withFastBackoff: this test asserts on growth, so it needs
	// bounds it controls precisely.
	origInitial, origMax := connectBackoffInitial, connectBackoffMax
	connectBackoffInitial = 20 * time.Millisecond
	connectBackoffMax = time.Second
	t.Cleanup(func() { connectBackoffInitial, connectBackoffMax = origInitial, origMax })

	conn := &recordingConnector{failures: 1 << 30} // never succeeds
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := enableWebSocketWithRetry(ctx, conn, func(string, ...any) {})
	defer func() { <-done }()

	// Attempt 1 is the synchronous one; the background loop then attempts
	// immediately and sleeps 20/40/80ms (+/-20% jitter) between its own tries.
	// Five attempts is ~140ms of sleeping.
	if !waitFor(t, 3*time.Second, func() bool { return conn.attempts() >= 5 }) {
		t.Fatalf("only %d attempts; the retry loop is not running", conn.attempts())
	}
	cancel()

	gaps := conn.gaps()
	if len(gaps) < 4 {
		t.Fatalf("recorded %d gaps, want >= 4", len(gaps))
	}

	// gaps[0] spans the synchronous attempt to the loop's first attempt (no
	// sleep by design). Every gap after that must be at least the jittered floor
	// of its nominal backoff (jitter is +/-20%, so 0.8x). A hot loop shows gaps
	// near zero throughout.
	nominal := connectBackoffInitial
	for i := 1; i <= 3; i++ {
		floor := time.Duration(float64(nominal) * 0.8)
		if gaps[i] < floor {
			t.Errorf("gap %d = %s, want >= %s (hot loop: backoff is not being applied)", i, gaps[i], floor)
		}
		nominal *= 2
	}

	// And the backoff must actually GROW, not stay flat at the initial value.
	if gaps[3] <= gaps[1] {
		t.Errorf("backoff is not growing: gap 1 = %s, gap 3 = %s", gaps[1], gaps[3])
	}
}

// TestEnableWebSocketWithRetry_HonorsRateLimitRetryAfter proves the 429 hint
// reaches the retry policy: a server asking for a long wait must not be polled
// tighter than it asked (#443), and the wait must still yield to shutdown.
func TestEnableWebSocketWithRetry_HonorsRateLimitRetryAfter(t *testing.T) {
	origInitial, origMax, origChunk := connectBackoffInitial, connectBackoffMax, connectRateLimitChunk
	connectBackoffInitial = time.Millisecond
	connectBackoffMax = 5 * time.Millisecond
	connectRateLimitChunk = 10 * time.Millisecond
	t.Cleanup(func() {
		connectBackoffInitial, connectBackoffMax, connectRateLimitChunk = origInitial, origMax, origChunk
	})

	conn := &recordingConnector{
		failures: 1 << 30,
		err:      &redisapi.RateLimitError{StatusCode: 429, RetryAfter: time.Hour},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := enableWebSocketWithRetry(ctx, conn, func(string, ...any) {})
	defer func() { <-done }()

	// One attempt happens synchronously, one more from the loop's first pass;
	// after that the 1h retry_after must hold it. Nothing near a hot loop.
	time.Sleep(200 * time.Millisecond)
	if got := conn.attempts(); got > 2 {
		t.Errorf("attempts = %d within 200ms against a 1h retry_after; the server backoff is being ignored", got)
	}
	cancel()
}

// TestEnableWebSocketWithRetry_StopsOnContextCancel: a cancelled context is a
// clean shutdown, not a reason to keep dialing.
func TestEnableWebSocketWithRetry_StopsOnContextCancel(t *testing.T) {
	withFastBackoff(t)

	conn := &recordingConnector{failures: 1 << 30}
	ctx, cancel := context.WithCancel(context.Background())

	done := enableWebSocketWithRetry(ctx, conn, func(string, ...any) {})
	waitFor(t, time.Second, func() bool { return conn.attempts() >= 2 })
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("retry loop did not exit after cancellation")
	}

	settled := conn.attempts()
	time.Sleep(50 * time.Millisecond)
	if got := conn.attempts(); got != settled {
		t.Errorf("retry loop kept dialing after cancellation (%d -> %d)", settled, got)
	}
}

// TestParsePubSubTransport covers what `citadel status` reads off the running
// worker. The "absent" cases must report unknown rather than default to a
// transport: claiming "http" when the field simply was not there (older worker,
// direct-Redis mode, no worker at all) would invent an outage.
func TestParsePubSubTransport(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
		ok   bool
	}{
		{"websocket", `{"worker":{"consuming":true,"pubsub_transport":"websocket"}}`, "websocket", true},
		{"http fallback", `{"worker":{"consuming":true,"pubsub_transport":"http"}}`, "http", true},
		{"no worker section", `{"version":"v1"}`, "", false},
		{"older worker, field absent", `{"worker":{"consuming":true}}`, "", false},
		{"malformed", `not json`, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parsePubSubTransport([]byte(tc.body))
			if got != tc.want || ok != tc.ok {
				t.Errorf("parsePubSubTransport(%s) = (%q, %v), want (%q, %v)", tc.body, got, ok, tc.want, tc.ok)
			}
		})
	}
}
