package worker

import (
	"sync"
	"sync/atomic"
	"time"
)

// WorkerState is a concurrency-safe snapshot of the running worker's live
// introspection state. It exists so an out-of-band control path (the status
// HTTP server, reachable over the tsnet mesh) can answer "is the worker
// consuming, what is it subscribed to, and why are per-node jobs not arriving"
// WITHOUT dispatching a job through the very Redis queue being debugged
// (issue #236). A single *WorkerState is created in cmd/work.go and threaded by
// pointer into the JobSource, the Runner, and the status server.
//
// All fields are read/written from multiple goroutines: the run loop (counts,
// poll time), the source (consume status, queues), and the status handler
// (reads). Counters use atomics; the queue slice and string fields are guarded
// by mu.
type WorkerState struct {
	mu sync.RWMutex

	workerID      string
	consumerGroup string
	source        string   // source name (e.g. "redis-api", "redis")
	queues        []string // streams currently subscribed/consumed
	perNodeQueue  string   // the per-node shell stream, if subscribed
	headscaleID   string
	orgID         string

	startedAt time.Time

	// lastPollUnixNano is the time of the most recent completed poll cycle
	// (job received OR empty result), stored as UnixNano for lock-free reads.
	lastPollUnixNano int64
	// lastJobUnixNano is the time the most recent job was received.
	lastJobUnixNano int64
	// oldestInFlightUnixNano is the time the CURRENT streak of in-flight work
	// began -- i.e. the last moment inFlight transitioned 0 -> >0 -- reset to 0
	// whenever inFlight drops back to 0. Unlike lastJobUnixNano (which every
	// job start overwrites, including a short job that starts WHILE a longer
	// one is still running), this is not perturbed by concurrent short jobs:
	// as long as at least one job has been continuously in flight, it keeps
	// pointing at when that streak started. Exists so the self-heal STUCK
	// check (issue #489 review) measures "how long has something been
	// running", not "when did the most recent job start" -- on a
	// maxConcurrency=1 node with the #489 long-session async lane, a wedged
	// MEETING_JOIN sits in_flight for hours while a stream of ordinary
	// SHELL_COMMAND jobs keeps completing beside it; each one used to reset
	// lastJobUnixNano, so a STUCK ceiling measured against it would never trip
	// for the wedged meeting as long as short jobs kept arriving.
	oldestInFlightUnixNano int64
	// inFlightMu serializes the inFlight increment/decrement against the
	// conditional oldestInFlightUnixNano store that accompanies a 0<->non-zero
	// transition. inFlight itself stays a plain atomic (readers like Snapshot
	// use atomic.LoadInt64 with no lock), so this mutex exists ONLY to make
	// "bump inFlight, then maybe touch oldestInFlightUnixNano" one atomic
	// transaction with respect to OTHER concurrent transitions -- without it,
	// a decrement-to-zero and a separate increment-to-one racing on two
	// different goroutines could apply their oldestInFlightUnixNano stores out
	// of order (the decrement's zero-out landing AFTER the new streak's
	// stamp), silently erasing a legitimate in-flight start time.
	inFlightMu sync.Mutex
	// lastConsumeStatus is the HTTP status of the most recent consume call
	// (API mode). 0 means "never polled / unknown". This is THE signal that
	// would have surfaced the pre-fix 400s in #3924.
	lastConsumeStatus int32
	// lastConsumeErr is the most recent consume error string ("" if the last
	// poll succeeded).
	lastConsumeErr atomic.Pointer[string]

	inFlight  int64
	processed int64
	failed    int64

	// executing counts jobs currently INSIDE a handler (past lane admission and
	// the exec-slot acquire), a strict subset of inFlight (citadel-cli#908). It
	// exists so the self-heal STUCK check can distinguish a job that has been
	// legitimately QUEUED for hours behind a long model pull (the general lane's
	// exec-concurrency is 1) from one whose handler is actually wedged -- only
	// the latter should trip STUCK. inFlight = queued + executing, so
	// queued (the wire field) is inFlight - executing.
	executing int64
	// oldestExecutingUnixNano is the executing analogue of
	// oldestInFlightUnixNano: the time the current EXECUTING streak began (last
	// executing 0 -> >0 transition), cleared when executing drains to 0. The
	// self-heal STUCK check reads this (surfaced as OldestExecutingAt), NOT
	// oldestInFlightUnixNano, so a long queue wait cannot look like a wedged
	// handler.
	oldestExecutingUnixNano int64
	// executingMu serializes the executing increment/decrement against the
	// conditional oldestExecutingUnixNano stamp, for the identical reason
	// inFlightMu exists for inFlight/oldestInFlightUnixNano above -- kept
	// separate so the two brackets (received-at-claim, executing-at-dispatch)
	// never contend with each other.
	executingMu sync.Mutex
}

// NewWorkerState creates an empty WorkerState stamped with the start time.
func NewWorkerState() *WorkerState {
	s := &WorkerState{startedAt: time.Now()}
	return s
}

// SetIdentity records the static identity/config of the worker. Safe to call
// during startup before the run loop begins.
func (s *WorkerState) SetIdentity(workerID, source, consumerGroup, headscaleID, orgID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.workerID = workerID
	s.source = source
	s.consumerGroup = consumerGroup
	s.headscaleID = headscaleID
	s.orgID = orgID
	s.mu.Unlock()
}

// SetQueues records the full list of streams the worker consumes from.
func (s *WorkerState) SetQueues(queues []string) {
	if s == nil {
		return
	}
	cp := make([]string, len(queues))
	copy(cp, queues)
	s.mu.Lock()
	s.queues = cp
	s.mu.Unlock()
}

// SetPerNodeQueue records the per-node shell stream name once subscribed.
func (s *WorkerState) SetPerNodeQueue(queue string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.perNodeQueue = queue
	s.mu.Unlock()
}

// RecordPoll stamps the time of a completed poll cycle.
func (s *WorkerState) RecordPoll() {
	if s == nil {
		return
	}
	atomic.StoreInt64(&s.lastPollUnixNano, time.Now().UnixNano())
}

// RecordConsumeStatus records the HTTP status and error of the most recent
// consume call. status<=0 is ignored (no HTTP status available, e.g. direct
// Redis mode). err may be empty to clear a prior error.
func (s *WorkerState) RecordConsumeStatus(status int, err string) {
	if s == nil {
		return
	}
	if status > 0 {
		atomic.StoreInt32(&s.lastConsumeStatus, int32(status))
	}
	e := err
	s.lastConsumeErr.Store(&e)
}

// RecordJobReceived stamps the time the worker received a job and increments
// the in-flight counter. Pair with RecordJobDone.
func (s *WorkerState) RecordJobReceived() {
	if s == nil {
		return
	}
	now := time.Now().UnixNano()
	atomic.StoreInt64(&s.lastJobUnixNano, now)

	s.inFlightMu.Lock()
	newInFlight := atomic.AddInt64(&s.inFlight, 1)
	if newInFlight == 1 {
		// 0 -> 1 transition: a fresh in-flight streak begins now. A job that
		// starts while another is ALREADY in flight (concurrency > 1, or the
		// #489 long-session async lane) must not move this -- see the field
		// comment on oldestInFlightUnixNano.
		atomic.StoreInt64(&s.oldestInFlightUnixNano, now)
	}
	s.inFlightMu.Unlock()
}

// RecordJobDone decrements in-flight and increments processed or failed.
func (s *WorkerState) RecordJobDone(ok bool) {
	if s == nil {
		return
	}
	s.inFlightMu.Lock()
	newInFlight := atomic.AddInt64(&s.inFlight, -1)
	if newInFlight == 0 {
		// >0 -> 0 transition: the in-flight streak has fully drained.
		atomic.StoreInt64(&s.oldestInFlightUnixNano, 0)
	}
	s.inFlightMu.Unlock()

	if ok {
		atomic.AddInt64(&s.processed, 1)
	} else {
		atomic.AddInt64(&s.failed, 1)
	}
}

// RecordJobExecuting stamps the transition of a job from queued (or directly
// claimed, for non-laned jobs) into actual handler execution. Increments the
// executing counter and, on the 0 -> 1 transition, stamps the executing streak
// start. Pair with RecordJobExecuteDone. Distinct from RecordJobReceived, which
// brackets the WIDER claimed-to-done span (inFlight): a job admitted onto a lane
// is RecordJobReceived immediately but only RecordJobExecuting once it acquires
// an exec slot, so the interval between them is genuinely "queued, not running".
func (s *WorkerState) RecordJobExecuting() {
	if s == nil {
		return
	}
	now := time.Now().UnixNano()
	s.executingMu.Lock()
	if atomic.AddInt64(&s.executing, 1) == 1 {
		atomic.StoreInt64(&s.oldestExecutingUnixNano, now)
	}
	s.executingMu.Unlock()
}

// RecordJobExecuteDone decrements the executing counter and clears the executing
// streak start when it drains to 0. Pair with RecordJobExecuting. It does NOT
// touch processed/failed (RecordJobDone owns outcome classification, on the
// wider claimed-to-done bracket).
func (s *WorkerState) RecordJobExecuteDone() {
	if s == nil {
		return
	}
	s.executingMu.Lock()
	if atomic.AddInt64(&s.executing, -1) == 0 {
		atomic.StoreInt64(&s.oldestExecutingUnixNano, 0)
	}
	s.executingMu.Unlock()
}

// WorkerSnapshot is a point-in-time, JSON-serializable view of WorkerState.
type WorkerSnapshot struct {
	WorkerID        string   `json:"worker_id"`
	Source          string   `json:"source"`
	ConsumerGroup   string   `json:"consumer_group"`
	Queues          []string `json:"queues"`
	PerNodeQueue    string   `json:"per_node_queue,omitempty"`
	HeadscaleNodeID string   `json:"headscale_node_id,omitempty"`
	// IdentityUnresolved reports that this worker never resolved its Headscale
	// node ID. It is stated POSITIVELY rather than left to the absence of
	// HeadscaleNodeID (which is omitempty, so a degraded node was previously
	// indistinguishable from an older payload) because it changes what the node
	// will do: with no identity it declines every target_node-addressed job
	// (citadel-cli#654). Untargeted work still runs, so the node looks healthy
	// on every other signal -- this is the field that explains the timeouts.
	IdentityUnresolved bool       `json:"identity_unresolved,omitempty"`
	OrgID              string     `json:"org_id,omitempty"`
	Consuming          bool       `json:"consuming"`
	StartedAt          time.Time  `json:"started_at"`
	UptimeSeconds      int64      `json:"uptime_seconds"`
	LastPollAt         *time.Time `json:"last_poll_at,omitempty"`
	LastJobAt          *time.Time `json:"last_job_at,omitempty"`
	// OldestInFlightAt is when the CURRENT in-flight streak began (nil when
	// InFlight==0) -- see WorkerState.oldestInFlightUnixNano for why this is
	// distinct from LastJobAt once more than one job can be in flight (#489's
	// long-session async lane, or MaxConcurrency > 1). The self-heal STUCK
	// check reads this, not LastJobAt.
	OldestInFlightAt  *time.Time `json:"oldest_in_flight_at,omitempty"`
	LastConsumeStatus int        `json:"last_consume_status"`
	LastConsumeError  string     `json:"last_consume_error,omitempty"`
	InFlight          int64      `json:"in_flight"`
	// Queued and Executing partition InFlight (Queued = InFlight - Executing):
	// Queued jobs are claimed and admitted onto a lane but waiting for a free
	// execution slot; Executing jobs are inside a handler (citadel-cli#908).
	// Additive/back-compatible -- InFlight keeps its exact prior meaning.
	Queued    int64 `json:"queued"`
	Executing int64 `json:"executing"`
	// OldestExecutingAt is when the current EXECUTING streak began (nil when
	// Executing==0). The self-heal STUCK check reads this rather than
	// OldestInFlightAt, so a job merely queued behind a long one for hours does
	// not read as a wedged handler (citadel-cli#908 §2d).
	OldestExecutingAt *time.Time `json:"oldest_executing_at,omitempty"`
	Processed         int64      `json:"processed"`
	Failed            int64      `json:"failed"`
}

// Snapshot returns a consistent copy of the current state. Safe for concurrent
// use. "Consuming" is true if a poll completed within the last 30s, which is a
// generous bound given the default 5s block timeout.
func (s *WorkerState) Snapshot() WorkerSnapshot {
	if s == nil {
		return WorkerSnapshot{}
	}
	s.mu.RLock()
	snap := WorkerSnapshot{
		WorkerID:        s.workerID,
		Source:          s.source,
		ConsumerGroup:   s.consumerGroup,
		Queues:          append([]string(nil), s.queues...),
		PerNodeQueue:    s.perNodeQueue,
		HeadscaleNodeID: s.headscaleID,
		OrgID:           s.orgID,
		StartedAt:       s.startedAt,
	}
	snap.IdentityUnresolved = s.headscaleID == ""
	s.mu.RUnlock()

	snap.UptimeSeconds = int64(time.Since(snap.StartedAt).Seconds())
	snap.LastConsumeStatus = int(atomic.LoadInt32(&s.lastConsumeStatus))
	if p := s.lastConsumeErr.Load(); p != nil {
		snap.LastConsumeError = *p
	}
	snap.InFlight = atomic.LoadInt64(&s.inFlight)
	snap.Executing = atomic.LoadInt64(&s.executing)
	// Queued is derived from two independently-loaded atomics, so clamp at 0: a
	// momentary skew between the loads must never surface a negative count.
	snap.Queued = snap.InFlight - snap.Executing
	if snap.Queued < 0 {
		snap.Queued = 0
	}
	snap.Processed = atomic.LoadInt64(&s.processed)
	snap.Failed = atomic.LoadInt64(&s.failed)

	if ns := atomic.LoadInt64(&s.lastPollUnixNano); ns > 0 {
		t := time.Unix(0, ns)
		snap.LastPollAt = &t
		snap.Consuming = time.Since(t) < 30*time.Second
	}
	if ns := atomic.LoadInt64(&s.lastJobUnixNano); ns > 0 {
		t := time.Unix(0, ns)
		snap.LastJobAt = &t
	}
	if ns := atomic.LoadInt64(&s.oldestInFlightUnixNano); ns > 0 {
		t := time.Unix(0, ns)
		snap.OldestInFlightAt = &t
	}
	if ns := atomic.LoadInt64(&s.oldestExecutingUnixNano); ns > 0 {
		t := time.Unix(0, ns)
		snap.OldestExecutingAt = &t
	}
	return snap
}
