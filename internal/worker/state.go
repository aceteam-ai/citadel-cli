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
	return snap
}
