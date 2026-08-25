package worker

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

// TestInferenceQueueReconciler_NotServingYet_NoOp asserts that a node with no
// serving engine gets no AddQueue calls -- this is the pre-fix state (#612)
// that a fresh node sits in until an engine starts.
func TestInferenceQueueReconciler_NotServingYet_NoOp(t *testing.T) {
	var addCalls int32
	r := NewInferenceQueueReconciler(
		[]string{"jobs:v1:gpu-general"},
		func(ctx context.Context) bool { return false },
		func(ctx context.Context, queue string) error {
			atomic.AddInt32(&addCalls, 1)
			return nil
		},
	)

	r.Reconcile(context.Background())
	r.Reconcile(context.Background())

	if got := atomic.LoadInt32(&addCalls); got != 0 {
		t.Fatalf("AddQueue called %d times while not serving; want 0", got)
	}
}

// TestInferenceQueueReconciler_TransitionSubscribes is the core #612 case: a
// node boots with no engine (not serving), then an engine starts later (e.g. a
// platform SERVICE_START). The next Reconcile tick must subscribe without a
// restart.
func TestInferenceQueueReconciler_TransitionSubscribes(t *testing.T) {
	var serving atomic.Bool
	var added []string
	var mu sync.Mutex

	r := NewInferenceQueueReconciler(
		[]string{"jobs:v1:gpu-general"},
		func(ctx context.Context) bool { return serving.Load() },
		func(ctx context.Context, queue string) error {
			mu.Lock()
			added = append(added, queue)
			mu.Unlock()
			return nil
		},
	)

	// Boot: no engine yet.
	r.Reconcile(context.Background())
	mu.Lock()
	if len(added) != 0 {
		mu.Unlock()
		t.Fatalf("subscribed before engine started: %v", added)
	}
	mu.Unlock()

	// Engine starts (e.g. SERVICE_START from a console model deploy).
	serving.Store(true)
	r.Reconcile(context.Background())

	mu.Lock()
	defer mu.Unlock()
	if len(added) != 1 || added[0] != "jobs:v1:gpu-general" {
		t.Fatalf("want subscribe to [jobs:v1:gpu-general] after transition, got %v", added)
	}
}

// TestInferenceQueueReconciler_IdempotentAfterSubscribe asserts no repeated
// AddQueue calls (and no probing) once subscribed -- steady-state should be a
// true no-op, not a growing sweep.
func TestInferenceQueueReconciler_IdempotentAfterSubscribe(t *testing.T) {
	var addCalls, servingCalls int32
	r := NewInferenceQueueReconciler(
		[]string{"jobs:v1:gpu-general"},
		func(ctx context.Context) bool {
			atomic.AddInt32(&servingCalls, 1)
			return true
		},
		func(ctx context.Context, queue string) error {
			atomic.AddInt32(&addCalls, 1)
			return nil
		},
	)

	for i := 0; i < 5; i++ {
		r.Reconcile(context.Background())
	}

	if got := atomic.LoadInt32(&addCalls); got != 1 {
		t.Fatalf("AddQueue called %d times across 5 ticks; want exactly 1", got)
	}
	if got := atomic.LoadInt32(&servingCalls); got != 1 {
		t.Fatalf("IsServing probed %d times across 5 ticks; want exactly 1 (steady-state should skip the probe once subscribed)", got)
	}
}

// TestInferenceQueueReconciler_MultipleQueues verifies every configured queue
// is added on the transition (e.g. a node whose static InferenceQueues(caps,
// true) resolves to more than one tag queue).
func TestInferenceQueueReconciler_MultipleQueues(t *testing.T) {
	var mu sync.Mutex
	var added []string
	r := NewInferenceQueueReconciler(
		[]string{"jobs:v1:gpu-general", "jobs:v1:tag:gpu:rtx3090"},
		func(ctx context.Context) bool { return true },
		func(ctx context.Context, queue string) error {
			mu.Lock()
			added = append(added, queue)
			mu.Unlock()
			return nil
		},
	)

	r.Reconcile(context.Background())

	mu.Lock()
	defer mu.Unlock()
	if len(added) != 2 {
		t.Fatalf("want both queues added, got %v", added)
	}
}

// TestInferenceQueueReconciler_AddQueueFailureRetries asserts a failed
// AddQueue does not permanently mark the reconciler subscribed -- the next
// tick must retry, since a transient Redis/API error should not strand the
// node unsubscribed forever.
func TestInferenceQueueReconciler_AddQueueFailureRetries(t *testing.T) {
	var attempt int32
	r := NewInferenceQueueReconciler(
		[]string{"jobs:v1:gpu-general"},
		func(ctx context.Context) bool { return true },
		func(ctx context.Context, queue string) error {
			n := atomic.AddInt32(&attempt, 1)
			if n == 1 {
				return errors.New("transient redis error")
			}
			return nil
		},
	)

	r.Reconcile(context.Background()) // fails
	r.Reconcile(context.Background()) // retries and succeeds

	if got := atomic.LoadInt32(&attempt); got != 2 {
		t.Fatalf("want 2 AddQueue attempts (fail then retry-success), got %d", got)
	}

	// A further tick must not call AddQueue again now that it succeeded.
	r.Reconcile(context.Background())
	if got := atomic.LoadInt32(&attempt); got != 2 {
		t.Fatalf("AddQueue called again after success, got %d attempts", got)
	}
}

// TestInferenceQueueReconciler_EmptyQueuesNeverProbes covers a node whose
// static capabilities already fully resolve the inference queue set (e.g. a
// GPU node) -- there is no gap for the reconciler to fill, so it must never
// probe IsServing at all.
func TestInferenceQueueReconciler_EmptyQueuesNeverProbes(t *testing.T) {
	var servingCalls int32
	r := NewInferenceQueueReconciler(
		nil,
		func(ctx context.Context) bool {
			atomic.AddInt32(&servingCalls, 1)
			return true
		},
		func(ctx context.Context, queue string) error {
			t.Fatalf("AddQueue should never be called with an empty queue set")
			return nil
		},
	)

	r.Reconcile(context.Background())

	if got := atomic.LoadInt32(&servingCalls); got != 0 {
		t.Fatalf("IsServing probed %d times with an empty queue set; want 0", got)
	}
}

// TestInferenceQueueReconciler_ConcurrentReconcile exercises concurrent
// Reconcile calls (e.g. a manual /agent/resubscribe racing the heartbeat
// tick) under -race to confirm the mutex-guarded transition is safe.
func TestInferenceQueueReconciler_ConcurrentReconcile(t *testing.T) {
	var addCalls int32
	r := NewInferenceQueueReconciler(
		[]string{"jobs:v1:gpu-general"},
		func(ctx context.Context) bool { return true },
		func(ctx context.Context, queue string) error {
			atomic.AddInt32(&addCalls, 1)
			return nil
		},
	)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.Reconcile(context.Background())
		}()
	}
	wg.Wait()

	// AddQueue itself is idempotent on the real sources, so >=1 is the safety
	// property under race; the strict-single-call case is covered serially
	// above without concurrency ambiguity.
	if got := atomic.LoadInt32(&addCalls); got < 1 {
		t.Fatalf("want at least 1 AddQueue call across concurrent Reconcile calls, got %d", got)
	}
}

// TestInferenceQueueReconciler_NilReceiverSafe asserts a nil reconciler
// (e.g. a mode that never wires one, like direct-Redis today) is a safe no-op
// call site rather than a required nil-check at every callsite.
func TestInferenceQueueReconciler_NilReceiverSafe(t *testing.T) {
	var r *InferenceQueueReconciler
	r.Reconcile(context.Background()) // must not panic
}
