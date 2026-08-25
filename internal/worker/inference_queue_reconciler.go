package worker

import (
	"context"
	"sync"
)

// InferenceQueueReconciler watches for a node's "has a serving engine"
// transition and adds the node's inference queue(s) to a live JobSource the
// moment that becomes true, without restarting the worker.
//
// Why this exists (issue #612): the inference-queue subscription is normally
// resolved once at `citadel work` startup from a live DiscoverLocalEngines
// probe. A node with no discrete GPU (so no static GPU-derived queue) and no
// serving engine yet at boot gets no inference queue at all. If an engine
// starts later -- e.g. a platform SERVICE_START from a console model deploy,
// on a fresh node that had nothing running when the worker came up -- the
// node never joins jobs:v1:gpu-general and inference silently stops routing
// to it until an operator restarts the worker. This type closes that gap by
// re-checking "am I serving" on the heartbeat's existing ~30s tick (via
// Reconcile) and subscribing once serving flips false->true.
//
// Deliberately subscribe-only: it never removes a queue when serving flips
// back to false. Redis Streams has no clean "unsubscribe" here (JobSource has
// no RemoveQueue, and dropping a consumer mid-flight raises a pending-entries
// question this issue does not need to answer), and a brief period where a
// now-idle node stays subscribed to gpu-general is the same tradeoff
// direct-Redis mode already accepts unconditionally. Once subscribed, a
// reconciler instance is done: it does not re-probe or re-subscribe again
// (AddQueue is idempotent regardless, but skipping the repeat probe avoids
// steady-state work on an already-healthy node).
type InferenceQueueReconciler struct {
	// IsServing reports whether this node currently has a serving engine. In
	// production this wraps status.DiscoverLocalEngines under a short timeout
	// (mirrors nodeIsServingModels in cmd/work.go); tests inject a stub.
	IsServing func(ctx context.Context) bool

	// AddQueue subscribes the live source to an additional queue. In
	// production this closes over the concrete *APISource/*RedisSource
	// AddQueue method (their signatures differ, so cmd/work.go adapts).
	AddQueue func(ctx context.Context, queue string) error

	// Log, if set, receives one line per state transition. Optional.
	Log func(format string, args ...any)

	mu         sync.Mutex
	queues     []string // the inference queue(s) to add once serving is true
	subscribed bool     // true once every queue has been added successfully
	probing    bool     // true while an IsServing/AddQueue probe is in flight
}

// NewInferenceQueueReconciler builds a reconciler for the given queue set. An
// empty/nil queues is valid and makes every Reconcile call a no-op (nothing to
// resolve into, e.g. a node whose static capabilities already cover the
// inference queue -- there is no gap for this reconciler to fill).
func NewInferenceQueueReconciler(queues []string, isServing func(ctx context.Context) bool, addQueue func(ctx context.Context, queue string) error) *InferenceQueueReconciler {
	q := make([]string, 0, len(queues))
	for _, name := range queues {
		if name != "" {
			q = append(q, name)
		}
	}
	return &InferenceQueueReconciler{
		queues:    q,
		IsServing: isServing,
		AddQueue:  addQueue,
	}
}

// Reconcile checks the current serving state and, on a false->true
// transition, subscribes to every configured queue. It is safe to call
// concurrently and repeatedly (e.g. once per heartbeat collection) --
// subsequent calls after a successful subscribe return immediately without
// re-probing.
//
// At most one probe (IsServing + AddQueue) runs at a time, guarded by
// `probing`. This matters because callers may run Reconcile in its own
// goroutine specifically because IsServing can block for an unbounded time
// (e.g. nodeIsServingModels's underlying `docker ps` has no context deadline
// on a wedged container runtime -- see cmd/work.go). Without this guard, a
// wedged runtime would leave the previous tick's probe goroutine permanently
// blocked while every subsequent heartbeat tick launches another one -- an
// unbounded goroutine leak instead of the bounded stall the caller moved to a
// goroutine to avoid.
func (r *InferenceQueueReconciler) Reconcile(ctx context.Context) {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.subscribed || r.probing || len(r.queues) == 0 || r.IsServing == nil || r.AddQueue == nil {
		r.mu.Unlock()
		return
	}
	r.probing = true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.probing = false
		r.mu.Unlock()
	}()

	// IsServing may block for an unbounded time (see the doc comment above),
	// so deliberately do this outside the lock -- it must never block a
	// concurrent Reconcile call (or the run loop) waiting on it. `probing`
	// above is what actually prevents pile-up; the lock here is only ever
	// held briefly.
	if !r.IsServing(ctx) {
		return
	}

	r.mu.Lock()
	if r.subscribed { // re-check: another goroutine may have won the race above
		r.mu.Unlock()
		return
	}
	queues := append([]string(nil), r.queues...)
	r.mu.Unlock()

	for _, q := range queues {
		if err := r.AddQueue(ctx, q); err != nil {
			if r.Log != nil {
				r.Log("inference-queue reconcile: failed to subscribe to %s: %v (will retry)", q, err)
			}
			return // leave subscribed=false so the next tick retries everything
		}
		if r.Log != nil {
			r.Log("inference-queue reconcile: engine now serving; subscribed to %s (issue #612)", q)
		}
	}

	r.mu.Lock()
	r.subscribed = true
	r.mu.Unlock()
}
