package worker

import (
	"context"
	"testing"
	"time"
)

// TestWakePumpNudgeTriggersImmediateConsume is the core #7270 guarantee: a wake
// nudge makes next() return via the non-blocking drain WITHOUT waiting out the
// (here effectively infinite) blocking read.
func TestWakePumpNudgeTriggersImmediateConsume(t *testing.T) {
	readBlocking := func(ctx context.Context) (*Job, error) {
		// Simulate a long poll block that only ends on shutdown, so if the wake
		// path were broken the test would hang until the deadline below.
		<-ctx.Done()
		return nil, ctx.Err()
	}
	wakeJob := &Job{ID: "wake-job"}
	readNonBlocking := func(ctx context.Context) (*Job, error) { return wakeJob, nil }

	pump := newWakePump(readBlocking, readNonBlocking)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pump.start(ctx)

	pump.signal() // nudge arrives (coalesced into the trigger)

	done := make(chan *Job, 1)
	go func() {
		j, _ := pump.next(ctx)
		done <- j
	}()

	select {
	case j := <-done:
		if j == nil || j.ID != "wake-job" {
			t.Fatalf("expected wake-job from the non-blocking drain, got %v", j)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("next() did not return promptly on a wake nudge (blocked on the poll)")
	}
}

// TestWakePumpReturnsBlockingReadWhenNoNudge: with no nudge, next() returns the
// ordinary blocking read result and never touches the non-blocking drain.
func TestWakePumpReturnsBlockingReadWhenNoNudge(t *testing.T) {
	blockJob := &Job{ID: "block-job"}
	readBlocking := func(ctx context.Context) (*Job, error) { return blockJob, nil }
	readNonBlocking := func(ctx context.Context) (*Job, error) {
		t.Error("non-blocking drain must not run without a nudge")
		return nil, nil
	}

	pump := newWakePump(readBlocking, readNonBlocking)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pump.start(ctx)

	j, err := pump.next(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if j == nil || j.ID != "block-job" {
		t.Fatalf("expected block-job, got %v", j)
	}
}

// TestWakePumpPrefetchDeliveredOnNextCall pins the prefetch-by-one contract: a
// wake-path return leaves the in-flight blocking read pending; its result is
// delivered to the NEXT next() call, and no message is dropped or double-read.
func TestWakePumpPrefetchDeliveredOnNextCall(t *testing.T) {
	releaseBlock := make(chan struct{})
	blockJob := &Job{ID: "block-job"}
	readCalls := 0
	readBlocking := func(ctx context.Context) (*Job, error) {
		readCalls++
		<-releaseBlock
		return blockJob, nil
	}
	wakeJob := &Job{ID: "wake-job"}
	readNonBlocking := func(ctx context.Context) (*Job, error) { return wakeJob, nil }

	pump := newWakePump(readBlocking, readNonBlocking)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pump.start(ctx)

	pump.signal()
	j1, _ := pump.next(ctx) // wake path -> wake-job; blocking read still pending
	if j1 == nil || j1.ID != "wake-job" {
		t.Fatalf("first next() should return wake-job, got %v", j1)
	}

	// Release the pending blocking read; the second next() must return its result
	// WITHOUT issuing a second blocking read (readInFlight was carried over).
	close(releaseBlock)
	j2, _ := pump.next(ctx)
	if j2 == nil || j2.ID != "block-job" {
		t.Fatalf("second next() should return the prefetched block-job, got %v", j2)
	}
	if readCalls != 1 {
		t.Fatalf("expected exactly one blocking read across a wake-then-drain cycle, got %d", readCalls)
	}
}

// TestWakePumpSignalCoalesces: a burst of nudges must never block the caller
// (subscriber goroutine); the buffered trigger collapses them to one.
func TestWakePumpSignalCoalesces(t *testing.T) {
	pump := newWakePump(
		func(ctx context.Context) (*Job, error) { return nil, nil },
		func(ctx context.Context) (*Job, error) { return nil, nil },
	)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			pump.signal()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("signal() blocked under a burst; the trigger is not coalescing")
	}
}

// TestWakePumpNextRespectsContextCancel: next() unblocks on ctx cancellation
// even while a blocking read is outstanding (clean shutdown).
func TestWakePumpNextRespectsContextCancel(t *testing.T) {
	readBlocking := func(ctx context.Context) (*Job, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	pump := newWakePump(readBlocking, readBlocking)
	ctx, cancel := context.WithCancel(context.Background())
	pump.start(ctx)

	done := make(chan error, 1)
	go func() {
		_, err := pump.next(ctx)
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected a context error on cancellation")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("next() did not unblock on context cancellation")
	}
}
