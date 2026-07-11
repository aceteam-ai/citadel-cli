package cmd

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/redisapi"
)

// mockConnector implements apiConnector, returning a scripted sequence of
// errors before succeeding. It records how many times Connect was called.
type mockConnector struct {
	errs  []error // returned in order; nil means success
	calls int32
}

func (m *mockConnector) Connect(ctx context.Context) error {
	n := atomic.AddInt32(&m.calls, 1)
	idx := int(n - 1)
	if idx < len(m.errs) {
		return m.errs[idx]
	}
	return nil // success once the script is exhausted
}

// withFastBackoff shrinks the backoff bounds for the duration of a test so the
// retry loop does not sleep for real seconds.
func withFastBackoff(t *testing.T) {
	t.Helper()
	origInitial, origMax, origChunk := connectBackoffInitial, connectBackoffMax, connectRateLimitChunk
	connectBackoffInitial = time.Millisecond
	connectBackoffMax = 5 * time.Millisecond
	connectRateLimitChunk = time.Millisecond
	t.Cleanup(func() {
		connectBackoffInitial, connectBackoffMax, connectRateLimitChunk = origInitial, origMax, origChunk
	})
}

func TestConnectWithBackoff_RetriesGenericErrorThenSucceeds(t *testing.T) {
	withFastBackoff(t)

	m := &mockConnector{errs: []error{
		errors.New("dial tcp: connection refused"),
		errors.New("dial tcp: connection refused"),
		nil,
	}}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := connectWithBackoff(ctx, m)
	if err != nil {
		t.Fatalf("connectWithBackoff returned error, want nil (should retry, not exit): %v", err)
	}
	if got := atomic.LoadInt32(&m.calls); got != 3 {
		t.Errorf("Connect called %d times, want 3 (2 failures + 1 success)", got)
	}
}

func TestConnectWithBackoff_Retries429ThenSucceeds(t *testing.T) {
	withFastBackoff(t)

	// A short retry_after: the loop must honor the hint (sleep, then retry
	// in-process) rather than exiting fatally.
	rle := &redisapi.RateLimitError{
		StatusCode: 429,
		Message:    "Rate limit exceeded",
		Limit:      50000,
		Window:     "day",
		RetryAfter: 5 * time.Millisecond,
	}
	m := &mockConnector{errs: []error{rle, nil}}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := connectWithBackoff(ctx, m)
	if err != nil {
		t.Fatalf("connectWithBackoff returned error on 429, want nil (should back off, not exit): %v", err)
	}
	if got := atomic.LoadInt32(&m.calls); got != 2 {
		t.Errorf("Connect called %d times, want 2 (1 rate-limited + 1 success)", got)
	}
}

// TestConnectWithBackoff_429LongResetYieldsToShutdown proves the key #443
// property: a 429 with a very long retry_after (the 86400s that caused the
// 24h lockout) does NOT block shutdown. The chunked, ctx-aware sleep must
// yield to cancellation quickly, and the process must never retry tighter than
// the server hint in the meantime.
func TestConnectWithBackoff_429LongResetYieldsToShutdown(t *testing.T) {
	// Use a real (long) chunk so the test would hang if the sleep were not
	// ctx-aware; cancellation must still be near-instant.
	origChunk := connectRateLimitChunk
	connectRateLimitChunk = 90 * time.Second
	t.Cleanup(func() { connectRateLimitChunk = origChunk })

	rle := &redisapi.RateLimitError{
		StatusCode: 429,
		Message:    "Rate limit exceeded",
		Limit:      50000,
		Window:     "day",
		RetryAfter: 86400 * time.Second, // the value that caused the 24h lockout
	}
	m := &mockConnector{errs: []error{rle, rle, rle}}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- connectWithBackoff(ctx, m) }()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("returned %v, want context.Canceled", err)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Errorf("took %s to honor cancellation on an 86400s retry_after; sleep not ctx-aware", elapsed)
		}
		// Must NOT have hammered: only the first Connect (which got the 429).
		if got := atomic.LoadInt32(&m.calls); got != 1 {
			t.Errorf("Connect called %d times during the honored backoff, want 1 (no hammering)", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("connectWithBackoff blocked on an 86400s retry_after instead of yielding to shutdown")
	}
}

func TestConnectWithBackoff_RespectsContextCancellation(t *testing.T) {
	// Backoff sleeps must respect ctx so systemctl stop / SIGTERM is immediate.
	// Use the real (long) backoff so the test would hang if cancellation were
	// not honored during the sleep.
	origInitial := connectBackoffInitial
	connectBackoffInitial = 10 * time.Second
	t.Cleanup(func() { connectBackoffInitial = origInitial })

	m := &mockConnector{errs: []error{
		errors.New("connection refused"), // forces entry into the backoff sleep
		errors.New("connection refused"),
		errors.New("connection refused"),
	}}

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel shortly after the loop enters its first (10s) backoff sleep.
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- connectWithBackoff(ctx, m) }()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("connectWithBackoff returned %v, want context.Canceled", err)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Errorf("connectWithBackoff took %s to honor cancellation; sleep not ctx-aware", elapsed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("connectWithBackoff did not return after context cancellation (backoff sleep ignored ctx)")
	}
}

func TestSleepCtx_ReturnsFalseOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	if sleepCtx(ctx, time.Hour) {
		t.Error("sleepCtx returned true despite cancellation")
	}
}

func TestSleepCtx_CompletesFullSleep(t *testing.T) {
	ctx := context.Background()
	if !sleepCtx(ctx, 5*time.Millisecond) {
		t.Error("sleepCtx returned false for a completed sleep")
	}
}
