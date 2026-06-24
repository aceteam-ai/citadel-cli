// cmd/shutdown_test.go
package cmd

import (
	"sync/atomic"
	"testing"
	"time"
)

// TestRunShutdownSequenceAllComplete verifies that when every step finishes
// well within the grace period, the sequence reports no stragglers and all
// steps actually ran.
func TestRunShutdownSequenceAllComplete(t *testing.T) {
	var ran int32
	steps := []shutdownStep{
		{name: "a", fn: func() { atomic.AddInt32(&ran, 1) }},
		{name: "b", fn: func() { atomic.AddInt32(&ran, 1) }},
		{name: "c", fn: func() { atomic.AddInt32(&ran, 1) }},
	}

	stuck := runShutdownSequence(time.Second, steps)
	if len(stuck) != 0 {
		t.Fatalf("expected no stuck steps, got %v", stuck)
	}
	if got := atomic.LoadInt32(&ran); got != 3 {
		t.Fatalf("expected 3 steps to run, got %d", got)
	}
}

// TestRunShutdownSequenceStuckStep is the core safety-net test: a deliberately
// blocking step must NOT prevent the sequence from returning after the grace
// period, and the stuck step must be reported by name so the caller can log it
// and force-exit (issue #312).
func TestRunShutdownSequenceStuckStep(t *testing.T) {
	release := make(chan struct{})
	defer close(release) // unblock the stuck goroutine so the test doesn't leak it

	var fastRan int32
	steps := []shutdownStep{
		{name: "fast", fn: func() { atomic.AddInt32(&fastRan, 1) }},
		{name: "stuck", fn: func() { <-release }},
	}

	start := time.Now()
	stuck := runShutdownSequence(50*time.Millisecond, steps)
	elapsed := time.Since(start)

	if elapsed >= time.Second {
		t.Fatalf("sequence did not return promptly after grace period: %s", elapsed)
	}
	if len(stuck) != 1 || stuck[0] != "stuck" {
		t.Fatalf("expected [stuck], got %v", stuck)
	}
	if got := atomic.LoadInt32(&fastRan); got != 1 {
		t.Fatalf("expected fast step to have run, got %d", got)
	}
}

// TestRunShutdownSequenceEmpty verifies the empty-input fast path.
func TestRunShutdownSequenceEmpty(t *testing.T) {
	if stuck := runShutdownSequence(time.Second, nil); stuck != nil {
		t.Fatalf("expected nil for empty steps, got %v", stuck)
	}
}

// TestRunShutdownSequenceNilFn verifies a step with a nil fn is treated as an
// instantly-completing no-op rather than panicking.
func TestRunShutdownSequenceNilFn(t *testing.T) {
	steps := []shutdownStep{
		{name: "noop", fn: nil},
	}
	if stuck := runShutdownSequence(time.Second, steps); len(stuck) != 0 {
		t.Fatalf("expected no stuck steps, got %v", stuck)
	}
}
