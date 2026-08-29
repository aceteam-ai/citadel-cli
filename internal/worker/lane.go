package worker

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// lane is the bounded admission + execution primitive that decouples job CLAIM
// from job EXECUTION (citadel-cli#908, aceteam#8254). It generalizes the pattern
// #489 (long-session) and #903 (GPU-bound) already used ad hoc into one named
// object with two independent bounds:
//
//   - admit: a buffered channel sized to the admission depth. A claimed job may
//     only be dispatched onto the lane if a non-blocking send into admit
//     succeeds; the spawned goroutine holds that slot for its WHOLE life
//     (queued + executing), releasing it on return. So admit bounds how many
//     jobs can be claimed-but-not-done on this lane at once, and the fetch loop
//     NEVER blocks: it either admits (and loops back to source.Next immediately)
//     or Nacks now (transparent, non-terminal retry -- the same shape as the
//     #825 GPU-slot-full Nack).
//   - exec: a buffered channel sized to the execution concurrency. A goroutine
//     acquires an exec slot before running the handler; all waiting happens
//     INSIDE that goroutine, off the fetch-loop's critical path.
//
// The exec-concurrency of the general/unbounded lane is deliberately 1 (see
// runner.go's lane construction): that reproduces EXACTLY today's implicit
// single-writer guarantee over the unlocked citadel.yaml / modules.lock
// read-modify-write paths (only one such job ran at a time process-wide because
// the sequential fetch loop was itself the lock), just relocated off the fetch
// loop. No manifest/lockfile locking is required at v1 as a result.
//
// hasExecWait selects the exec-acquire discipline (owned by runner.go's
// runLaneJob, not this type): false = unbounded wait (general lane); true =
// bounded wait that, on expiry, returns the existing model_warming backpressure
// signal instead of blocking (the inference admission queue, aceteam#8254).
type lane struct {
	name        string
	admit       chan struct{}
	exec        chan struct{}
	execCap     int
	hasExecWait bool

	// admitted counts jobs currently holding an admit slot (queued + executing);
	// executing counts jobs currently past the exec-acquire. queued for display
	// is admitted-executing. Both are plain atomics; busySince is the only field
	// needing the mutex (it is a compound stamp on the executing==execCap edge).
	admitted  int64
	executing int64

	mu        sync.Mutex
	busySince *time.Time
}

// newLane builds a lane with the given admission depth and execution
// concurrency. Both are floored at 1 so a mis-sized caller can never produce an
// unbuffered channel (which would make tryAdmit always fail, or the exec-acquire
// never succeed).
func newLane(name string, admitDepth, execConcurrency int, hasExecWait bool) *lane {
	if admitDepth < 1 {
		admitDepth = 1
	}
	if execConcurrency < 1 {
		execConcurrency = 1
	}
	return &lane{
		name:        name,
		admit:       make(chan struct{}, admitDepth),
		exec:        make(chan struct{}, execConcurrency),
		execCap:     execConcurrency,
		hasExecWait: hasExecWait,
	}
}

// tryAdmit non-blockingly reserves an admission slot. On success the caller MUST
// eventually call releaseAdmit exactly once. Returns false when the lane is at
// its admission bound (the fetch loop Nacks the job in that case).
func (l *lane) tryAdmit() bool {
	select {
	case l.admit <- struct{}{}:
		atomic.AddInt64(&l.admitted, 1)
		return true
	default:
		return false
	}
}

// releaseAdmit frees the admission slot reserved by a successful tryAdmit.
func (l *lane) releaseAdmit() {
	atomic.AddInt64(&l.admitted, -1)
	<-l.admit
}

// beginExec records that a job has acquired an exec slot and is now executing.
// Stamps busySince on the executing==execCap (fully saturated) edge.
func (l *lane) beginExec() {
	n := atomic.AddInt64(&l.executing, 1)
	if int(n) >= l.execCap {
		l.mu.Lock()
		if l.busySince == nil {
			t := time.Now()
			l.busySince = &t
		}
		l.mu.Unlock()
	}
}

// endExec records that a job has finished executing and released its exec slot.
// Clears busySince the moment executing drops below execCap.
func (l *lane) endExec() {
	n := atomic.AddInt64(&l.executing, -1)
	if int(n) < l.execCap {
		l.mu.Lock()
		l.busySince = nil
		l.mu.Unlock()
	}
}

// snapshot returns the lane's current activity for the heartbeat (LaneActivity).
func (l *lane) snapshot() LaneSnapshot {
	admitted := atomic.LoadInt64(&l.admitted)
	executing := atomic.LoadInt64(&l.executing)
	queued := admitted - executing
	if queued < 0 {
		// admitted and executing are loaded independently, so a momentary skew
		// (a job between beginExec and its admitted-- ... it never happens in
		// that order, but atomics are loaded separately) must never surface a
		// negative count.
		queued = 0
	}
	var busy *time.Time
	l.mu.Lock()
	if l.busySince != nil {
		t := *l.busySince
		busy = &t
	}
	l.mu.Unlock()
	return LaneSnapshot{
		Lane:         l.name,
		Queued:       int(queued),
		Executing:    int(executing),
		ExecCapacity: l.execCap,
		BusySince:    busy,
	}
}

// LaneSnapshot is the worker-side view of one lane's activity, projected onto
// status.LaneActivity by cmd/work.go's laneActivityFrom (internal/status cannot
// import internal/worker). The JSON tags MUST match status.LaneActivity;
// TestLaneShapeParity pins that they stay in sync.
type LaneSnapshot struct {
	Lane         string     `json:"lane"`
	Queued       int        `json:"queued"`
	Executing    int        `json:"executing"`
	ExecCapacity int        `json:"exec_capacity"`
	BusySince    *time.Time `json:"busy_since,omitempty"`
}

// Lane tuning. Following the WORKER_* env convention already used for the
// per-job watchdog (deadline.go) and self-heal (selfheal.go). Package vars, not
// consts, so tests can adjust them without sleeping a real queue-wait --
// mirroring the swap-accounting knobs (swap.go).
const (
	inferenceQueueWaitEnvVar = "WORKER_INFERENCE_QUEUE_WAIT_SECONDS"
	unboundedLaneQueueEnvVar = "WORKER_UNBOUNDED_LANE_QUEUE"
)

var (
	// inferenceQueueWaitSecondsDefault bounds how long an admitted inference job
	// waits for a free execution slot before the node answers with the existing
	// model_warming backpressure signal (a SUCCESS the platform already retries)
	// instead of blocking. A conservative starting default; tune from telemetry.
	inferenceQueueWaitSecondsDefault = 120
	// unboundedLaneQueueDefault bounds how many SERVICE_START / model-pull /
	// build / MODULE_SET jobs can be claimed-but-not-yet-running at once (the
	// general lane's admission depth). Small on purpose: hitting it Nacks (a
	// transparent retry), and it should only be reached under a real backlog.
	unboundedLaneQueueDefault = 8
)

// resolveInferenceQueueWait reads the inference queue-wait budget from the env,
// falling back to the default. A zero/negative/garbage value is treated as the
// default rather than "unbounded": an unbounded inference queue-wait would
// silently defeat the backpressure signal this lane exists to emit.
func resolveInferenceQueueWait() time.Duration {
	if d, ok := envTimeoutSeconds(inferenceQueueWaitEnvVar, inferenceQueueWaitSecondsDefault); ok {
		return d
	}
	return time.Duration(inferenceQueueWaitSecondsDefault) * time.Second
}

// resolveUnboundedLaneQueue reads the general lane's admission depth from the
// env, falling back to the default. Floored at 1 by newLane regardless.
func resolveUnboundedLaneQueue() int {
	if v := strings.TrimSpace(os.Getenv(unboundedLaneQueueEnvVar)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return unboundedLaneQueueDefault
}
