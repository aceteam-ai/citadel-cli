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

func (l *logCollector) logf(level, format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, level+": "+fmt.Sprintf(format, args...))
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
//
// It asserts on the durations the loop REQUESTS rather than the gaps it
// achieves. Wall-clock gaps are flaky in the dangerous direction: a loaded
// machine overshoots a 20ms sleep to 87ms, which is indistinguishable from a
// correct 80ms backoff, so a hot-loop regression would pass.
func TestEnableWebSocketWithRetry_BacksOffRatherThanHotLooping(t *testing.T) {
	origInitial, origMax := connectBackoffInitial, connectBackoffMax
	connectBackoffInitial = 20 * time.Millisecond
	connectBackoffMax = time.Second
	t.Cleanup(func() { connectBackoffInitial, connectBackoffMax = origInitial, origMax })

	var mu sync.Mutex
	var requested []time.Duration
	origSleep := backoffSleep
	backoffSleep = func(ctx context.Context, d time.Duration) bool {
		mu.Lock()
		requested = append(requested, d)
		mu.Unlock()
		return ctx.Err() == nil
	}
	t.Cleanup(func() { backoffSleep = origSleep })

	requestedCount := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(requested)
	}

	conn := &recordingConnector{failures: 1 << 30} // never succeeds
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := enableWebSocketWithRetry(ctx, conn, func(string, string, ...any) {})

	if !waitFor(t, 3*time.Second, func() bool { return requestedCount() >= 4 }) {
		t.Fatalf("loop requested %d sleeps; it is hot-looping or not running", requestedCount())
	}
	cancel()
	<-done

	mu.Lock()
	got := append([]time.Duration(nil), requested[:4]...)
	mu.Unlock()

	// Nominal 20/40/80/160ms, each jittered by +/-20%.
	nominal := connectBackoffInitial
	for i, d := range got {
		lo := time.Duration(float64(nominal) * 0.8)
		hi := time.Duration(float64(nominal) * 1.2)
		if d < lo || d > hi {
			t.Errorf("sleep %d = %s, want within [%s, %s] of the %s backoff step", i, d, lo, hi, nominal)
		}
		nominal *= 2
	}

	// Jitter is +/-20%, so consecutive doubling steps cannot overlap: growth
	// must be strictly monotonic. A flat backoff (or none) fails here.
	for i := 1; i < len(got); i++ {
		if got[i] <= got[i-1] {
			t.Errorf("backoff is not growing: sleep %d = %s, sleep %d = %s", i-1, got[i-1], i, got[i])
		}
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

	done := enableWebSocketWithRetry(ctx, conn, func(string, string, ...any) {})
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

	done := enableWebSocketWithRetry(ctx, conn, func(string, string, ...any) {})
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
		name  string
		body  string
		want  string
		state pubSubProbeState
	}{
		{"websocket", `{"worker":{"consuming":true,"pubsub_transport":"websocket"}}`, "websocket", pubSubProbeOK},
		{"http fallback", `{"worker":{"consuming":true,"pubsub_transport":"http"}}`, "http", pubSubProbeOK},
		{"no worker section", `{"version":"v1"}`, "", pubSubProbeNotReported},
		{"older worker, field absent", `{"worker":{"consuming":true}}`, "", pubSubProbeNotReported},
		// A reply we cannot decode is NOT an absent server: reporting it as one
		// would tell the operator to enable something already running (#735).
		{"malformed", `not json`, "", pubSubProbeMalformed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, state := parsePubSubTransport([]byte(tc.body))
			if got != tc.want || state != tc.state {
				t.Errorf("parsePubSubTransport(%s) = (%q, %v), want (%q, %v)", tc.body, got, state, tc.want, tc.state)
			}
		})
	}
}

// fakeWakeEnabler records EnableWake calls and fails until allowed to succeed.
type fakeWakeEnabler struct {
	mu       sync.Mutex
	calls    []string
	failWith error
}

func (f *fakeWakeEnabler) EnableWake(_ context.Context, channel string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, channel)
	return f.failWith
}

func (f *fakeWakeEnabler) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// TestArmWakeAfterWebSocket_ArmsOnceTheRetryLands is the partial-fix guard: a
// late WebSocket connect goes through Connect, not reconnect, so OnReconnect
// callbacks never fire and push-wake would stay off forever on a node that
// started during a control-plane blip -- while `citadel status` reported a
// healthy "websocket" transport.
func TestArmWakeAfterWebSocket_ArmsOnceTheRetryLands(t *testing.T) {
	wsRetry := make(chan struct{})
	src := &fakeWakeEnabler{}
	logs := &logCollector{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	armWakeAfterWebSocket(ctx, wsRetry, src, "node:wake:42", logs.logf)

	if got := src.callCount(); got != 0 {
		t.Fatalf("EnableWake called %d time(s) before the WebSocket landed, want 0", got)
	}

	close(wsRetry)

	if !waitFor(t, 2*time.Second, func() bool { return src.callCount() == 1 }) {
		t.Fatalf("push-wake was never re-armed after the WebSocket retry landed (calls=%d)", src.callCount())
	}
	if !waitFor(t, 2*time.Second, func() bool { return logs.contains("armed after WebSocket recovery") }) {
		t.Error("re-arm was not logged")
	}
}

// TestArmWakeAfterWebSocket_NoopWhenRetryAlreadyFinished: the caller already
// invoked EnableWake against the final state, so there is nothing to wait for.
func TestArmWakeAfterWebSocket_NoopWhenRetryAlreadyFinished(t *testing.T) {
	wsRetry := make(chan struct{})
	close(wsRetry)
	src := &fakeWakeEnabler{}

	armWakeAfterWebSocket(context.Background(), wsRetry, src, "node:wake:42", func(string, string, ...any) {})

	time.Sleep(30 * time.Millisecond)
	if got := src.callCount(); got != 0 {
		t.Errorf("EnableWake called %d time(s), want 0 (retry had already finished)", got)
	}
}

// TestArmWakeAfterWebSocket_StopsOnShutdown: a cancelled context must not leave
// a goroutine waiting on a channel that will never close.
func TestArmWakeAfterWebSocket_StopsOnShutdown(t *testing.T) {
	wsRetry := make(chan struct{})
	src := &fakeWakeEnabler{}
	ctx, cancel := context.WithCancel(context.Background())

	armWakeAfterWebSocket(ctx, wsRetry, src, "node:wake:42", func(string, string, ...any) {})
	cancel()

	time.Sleep(50 * time.Millisecond)
	if got := src.callCount(); got != 0 {
		t.Errorf("EnableWake called %d time(s) after shutdown, want 0", got)
	}
}
