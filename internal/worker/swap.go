// internal/worker/swap.go
//
// VRAM-aware on-demand model hotswap — swap manager (citadel-cli#632, Phase 1).
//
// When CITADEL_MODEL_HOTSWAP is on, an llm_inference job for an installed-but-
// not-resident engine triggers a swap: preempt idle/LRU non-pinned resident
// engines to free VRAM, start the target engine, and wait ≤15s for it to become
// ready. If it becomes ready in time the request is served normally; otherwise
// the handler returns a structured `model_warming` result and the platform
// retries.
//
// Design constraints (all enforced here):
//   - Cold-start is minutes (bonsai's first start even builds an image). So the
//     actual compose-up + load runs in a BACKGROUND goroutine with its own
//     context, NOT on the job's context (which the runner cancels). EnsureResident
//     starts-or-joins that background swap and only OBSERVES it for ≤15s.
//   - Single-flight: at most ONE swap runs per node. A miss for the SAME model
//     while a swap is in flight attaches to it; a miss for a DIFFERENT model
//     returns warming immediately and starts NO second swap (no GPU thrash).
//   - Non-durable eviction: peers are stopped WITHOUT desired_status:stopped
//     (StopNonDurable), so an evicted model stays eligible to swap back in — the
//     opposite of #577's sticky operator stop. The internal SERVICE_START also
//     carries only {service, model} (no vram_mb), so #577's DURABLE preemptForVRAM
//     stays inert; the swap does its own non-durable PlanPreemption here.
//   - Min-residency floor: an engine that became ready within minResidency is
//     never evicted (filtered out of the candidate set before planning). On top
//     of that floor, an engine that has not yet had a request dispatched to it
//     since becoming ready is protected until it has (citadel-cli#687) — a load
//     that never served anything was pure waste, and a 60s floor under a 78s
//     load meant a model could become evictable before it finished loading.
//   - LRU: candidates are pre-sorted least-recently-used first so PlanPreemption's
//     stable idle-then-largest-VRAM ordering breaks ties by LRU.
package worker

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/status"
)

// Default swap timing knobs. The 15s wait budget and 60s min-residency floor are
// fixed by the #632 design; the background load ceiling bounds a wedged compose-up
// so the single-flight lock is eventually released.
const (
	swapWaitBudget       = 15 * time.Second
	swapMinResidency     = 60 * time.Second
	swapBackgroundMaxDur = 15 * time.Minute
	swapReadyPollEvery   = 1 * time.Second
	warmingRetryAfter    = 10 // seconds; the platform's retry_after hint
	warmingRetryAfterMax = 60 // seconds; ceiling on a paced retry hint
)

// SwapController abstracts the node side-effects a swap performs, so the swap
// manager's decision logic (single-flight, LRU, min-residency, planning) is
// unit-testable against a mock. The real implementation (cmd/hotswap.go) wraps a
// jobs.ServiceHandler + a status collector.
type SwapController interface {
	// Resident reports whether the engine `backend` is currently running (its
	// container is up). Residency, not readiness: a running-but-loading engine is
	// resident (the normal inference path then waits for readiness).
	Resident(ctx context.Context, backend string) bool

	// PreemptInputs returns the live preemption inputs: the running managed
	// serving engines other than `exclude` as PlanPreemption candidates (with
	// VRAM footprint + instantaneous idle), and the node's currently-free VRAM in
	// bytes. haveVRAM is false when free VRAM cannot be determined (no GPU /
	// nvidia-smi absent), in which case the swap skips preemption (fail-safe).
	PreemptInputs(ctx context.Context, exclude string) (candidates []status.PreemptCandidate, freeVRAM uint64, haveVRAM bool)

	// StopNonDurable stops an engine WITHOUT marking it durably stopped, so it
	// stays eligible to swap back in (the #632 non-durable eviction path).
	StopNonDurable(name string) error

	// Start starts the target engine serving `model` (an internal SERVICE_START
	// with {service, model} only — no vram_mb, so #577 preemption stays inert).
	Start(ctx context.Context, backend, model string) error

	// Ready reports whether the engine's HTTP API answers (loaded and serving).
	Ready(ctx context.Context, backend string) bool

	// MeasuredVRAM returns the LIVE measured VRAM footprint (bytes) attributed
	// to the now-resident engine `backend`, and whether a measurement was
	// available. Called once, right after Ready reports true, so the engine's
	// process actually exists to attribute VRAM to. ok is false when no
	// footprint signal exists (no GPU, footprint collection failed) — callers
	// must never treat that as "measured zero" (citadel-cli#689).
	MeasuredVRAM(ctx context.Context, backend string) (bytes uint64, ok bool)
}

// SwapOutcome is what EnsureResident reports to the inference handler.
type SwapOutcome struct {
	// Ready is true when the target engine is resident and serving; the handler
	// then proceeds to the normal inference path.
	Ready bool
	// ETASeconds is the estimated remaining seconds until the model is ready,
	// surfaced in the model_warming result when Ready is false.
	ETASeconds int
	// RetryAfterSeconds is the hint the platform should wait before retrying.
	// Zero means "use the standard hint". It is set above the default only when
	// the node knows the caller's model is not being worked on yet, so a retry
	// loop does not spin against a node doing nothing for it (citadel-cli#680).
	RetryAfterSeconds int
	// WarmingFor names the model actually loading on this node right now, which
	// may NOT be the requested model (citadel-cli#681). Set whenever Ready is
	// false and the node has SOME load in flight:
	//   - equal to the requested model when this node is loading THAT model
	//     (the common case: a freshly started or joined same-backend swap, or
	//     the post-swap readiness probe).
	//   - the DIFFERENT in-flight model when single-flight refused to start a
	//     second swap and this request's load has not begun at all — this is
	//     the case the caller cannot distinguish from the above without this
	//     field, and ETASeconds already reflects the combined wait for it
	//     (citadel-cli#680's blockedETASeconds).
	// Empty only when the node cannot name a load at all (should not happen
	// alongside Ready=false, but callers must not treat empty as "loading
	// mine" — it means "unknown").
	WarmingFor string
}

// swapOp is a single in-flight background swap. done is closed when the swap
// finishes; ready/err record its terminal state. transient marks a swap that
// could not proceed right now (e.g. min-residency blocked every candidate) but
// is not a hard failure — the caller should warm and retry.
type swapOp struct {
	backend   string
	model     string
	startedAt time.Time
	loadEst   time.Duration
	done      chan struct{}
	ready     bool
	transient bool
	err       error

	// evicted names the engines this swap stopped to make room. It is what the
	// ledger's rate bound counts, so it must be appended to as stops succeed,
	// not derived from the plan (a plan whose first stop failed evicted one
	// engine, not all of them).
	evicted []string
	// started records whether a SERVICE_START was actually issued, which
	// separates a swap that spent the box's time from one refused before it
	// began.
	started bool
}

// SwapManager serializes and observes model swaps on a node.
type SwapManager struct {
	ctrl SwapController

	// requiredVRAM is the coarse per-engine PROVISIONING ESTIMATE (bytes) — the
	// table (status.EngineVRAMEstimateMB), unavoidable for a backend this node
	// has never actually measured. requiredVRAMBytes (below) is what callers
	// should use: it prefers vramMeasured over this when a measurement exists
	// (citadel-cli#689).
	requiredVRAM func(backend string) uint64
	// loadEstimate returns the expected cold-start time for `backend`, used for
	// the warming ETA.
	loadEstimate func(backend string) time.Duration
	now          func() time.Time

	// preflight decides whether `backend` is serveable before this swap starts
	// it (citadel-cli#956, swap_preflight.go). Defaults to
	// status.EngineServeablePreflight (reusing #955's image/weights/disk
	// checks unmodified); overridden in tests to avoid depending on a real
	// docker/podman daemon or real disk state.
	preflight func(backend string, sys status.SystemMetrics) (blocked bool, reason string)
	// diskMetrics supplies the disk reading preflight's disk-headroom check
	// needs, WITHOUT running a full status.Collector.Collect() (memory + a
	// 100ms CPU sample + disk) on every swap decision -- EnsureResident calls
	// this on every on-demand swap, not once per ~30s heartbeat tick like the
	// collector. Defaults to status.DiskMetricsOnly (disk.Usage("/") alone).
	diskMetrics func() status.SystemMetrics

	waitBudget    time.Duration
	minResidency  time.Duration
	backgroundMax time.Duration // ceiling on a background swap (releases the lock)
	readyPoll     time.Duration // readiness poll interval

	// Swap rate bound (citadel-cli#687). See swap_ledger.go.
	rateWindow           time.Duration
	maxEvictingPerWindow int

	mu       sync.Mutex
	inflight *swapOp              // the single in-flight swap, or nil
	lastUsed map[string]time.Time // per-engine last request time (LRU)
	readyAt  map[string]time.Time // per-engine last became-ready time (min-residency)
	// servedAt records when a request was last DISPATCHED to an engine — the
	// moment EnsureResident hands it to the inference path. Compared against
	// readyAt, it answers "has this engine done anything since it loaded?",
	// which is the citadel-cli#687 eviction invariant. It is deliberately not
	// lastUsed: lastUsed is touched on a MISS too, so an engine that was only
	// ever asked for while absent would look served.
	servedAt map[string]time.Time
	// loadMeasured is the observed cold-start duration per engine, replacing the
	// coarse table estimate once this node has actually timed one. It survives
	// eviction on purpose: it measures the ENGINE, not the residency, and
	// dropping it would send the next swap-in back to the table (the same defect
	// citadel-cli#688 describes for lastUsed).
	loadMeasured map[string]time.Duration
	// vramMeasured is the observed LIVE VRAM footprint (bytes) per (engine,
	// model) pair, recorded once a swap-in becomes ready (citadel-cli#689).
	// Keyed by vramCacheKey(backend, model), not backend alone: the same engine
	// (vllm in particular) can be swapped in for different models across
	// swaps, and a stale measurement from a smaller model would understate the
	// budget for a larger one. Mirrors loadMeasured's eviction behavior: it
	// SURVIVES eviction, because the footprint of a given (engine, model) does
	// not change because the engine was stopped, and dropping it would send the
	// next swap-in of the same pair back to the padded provisioning budget it
	// exists to replace.
	vramMeasured map[string]uint64
	// swaps is the in-process swap ledger (swap_ledger.go).
	swaps []SwapRecord
	// startedAt records when this node last ISSUED a start for an engine, which
	// is the only trustworthy evidence that an unbound port is a cold start
	// rather than an engine that is simply not running (citadel-cli#705). The
	// readiness gate reads it through EngineStartedAt; it is cleared when a start
	// fails and when an engine is evicted, so a stale entry can never keep an
	// absent engine reporting "warming".
	startedAt map[string]time.Time

	// Durable lastUsed mirror (citadel-cli#688; see swap_persist.go). Empty
	// persistPath means persistence was never enabled (the NewSwapManager(ctrl)
	// default, and every existing test) and every method below becomes a no-op.
	persistPath   string
	persistLogf   func(format string, args ...any)
	persistMinGap time.Duration
	persistMu     sync.Mutex // guards lastPersistAt only; I/O runs outside it
	lastPersistAt time.Time
}

// NewSwapManager builds a swap manager with default timing and VRAM/load
// estimates sourced from the shared status tables. ctrl supplies the node
// side-effects. Without WithPersistence, lastUsed is purely in-process (matches
// every pre-#688 caller/test); pass WithPersistence to seed it from disk and
// keep it durable across restarts.
func NewSwapManager(ctrl SwapController, opts ...SwapManagerOption) *SwapManager {
	m := &SwapManager{
		ctrl:                 ctrl,
		requiredVRAM:         func(b string) uint64 { return uint64(status.EngineVRAMEstimateMB(b)) * 1024 * 1024 },
		loadEstimate:         defaultLoadEstimate,
		now:                  time.Now,
		preflight:            defaultSwapPreflight,
		diskMetrics:          status.DiskMetricsOnly,
		waitBudget:           swapWaitBudget,
		minResidency:         swapMinResidency,
		backgroundMax:        swapBackgroundMaxDur,
		readyPoll:            swapReadyPollEvery,
		rateWindow:           swapRateWindow,
		maxEvictingPerWindow: swapMaxEvictingPerWindow,
		lastUsed:             map[string]time.Time{},
		readyAt:              map[string]time.Time{},
		startedAt:            map[string]time.Time{},
		servedAt:             map[string]time.Time{},
		loadMeasured:         map[string]time.Duration{},
		vramMeasured:         map[string]uint64{},
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// defaultLoadEstimate is a coarse per-engine cold-start estimate for the warming
// ETA. Bonsai can build its image on first start (~7min per CLAUDE.md); the
// others are cached-image loads of tens of seconds to a couple minutes.
func defaultLoadEstimate(backend string) time.Duration {
	switch backend {
	case "bonsai":
		return 3 * time.Minute
	case "vllm", "sglang", "unlimited-ocr":
		return 90 * time.Second
	default:
		return 60 * time.Second
	}
}

// EnsureResident makes the engine for `backend` (serving `model`) resident, or
// reports that a swap is in progress. It never blocks longer than the wait
// budget. The returned error is a hard failure (e.g. the swap cannot fit without
// evicting a min-residency/pinned holder) that the handler should surface as a
// job failure; a nil error with Ready=false means "warming, retry".
func (m *SwapManager) EnsureResident(ctx context.Context, backend, model string) (SwapOutcome, error) {
	m.touch(backend)

	// Already resident: nothing to do (fast path, no lock contention with swaps).
	if m.ctrl.Resident(ctx, backend) {
		m.markServed(backend)
		return SwapOutcome{Ready: true}, nil
	}

	op := m.startOrJoin(backend, model)
	if op == nil {
		// A different model is being swapped in and single-flight refuses to start
		// a second one, so THIS model's load has not begun. Reporting a bare
		// cold-start estimate here would be a fabricated number: the real wait is
		// the in-flight swap finishing PLUS this engine's own load. Quote that, and
		// pace the retry hint to it, so a caller does not busy-retry a node that is
		// not working on its request (citadel-cli#680).
		//
		// Snapshot the blocking op ONCE so the ETA and the discriminator
		// (citadel-cli#681) agree with each other rather than each re-reading
		// m.inflight and possibly observing two different states.
		blocking := m.inflightSnapshot()
		eta := m.blockedETASeconds(backend, blocking)
		warmingFor := model // blocking swap finished between startOrJoin and here
		if blocking != nil {
			warmingFor = blocking.model
		}
		return SwapOutcome{
			Ready:             false,
			ETASeconds:        eta,
			RetryAfterSeconds: retryAfterFor(eta),
			WarmingFor:        warmingFor,
		}, nil
	}

	// Observe the (possibly background) swap for the remaining wait budget.
	elapsed := m.now().Sub(op.startedAt)
	remaining := m.waitBudget - elapsed
	if remaining < 0 {
		remaining = 0
	}

	timer := time.NewTimer(remaining)
	defer timer.Stop()

	select {
	case <-op.done:
		if op.err != nil {
			return SwapOutcome{}, op.err
		}
		if op.ready {
			m.markServed(backend)
			return SwapOutcome{Ready: true}, nil
		}
		// Completed but not ready (transient block, e.g. min-residency): warm.
		// op.model (not the caller's `model` param) is the honest answer even
		// here — a join onto an in-flight same-backend swap can be for a
		// different model than this call requested (citadel-cli#681).
		return SwapOutcome{Ready: false, ETASeconds: m.etaSeconds(op), WarmingFor: op.model}, nil
	case <-timer.C:
		return SwapOutcome{Ready: false, ETASeconds: m.etaSeconds(op), WarmingFor: op.model}, nil
	case <-ctx.Done():
		// The job context was cancelled; the background swap keeps running so a
		// retry can pick up the now-resident model. Report warming.
		return SwapOutcome{Ready: false, ETASeconds: m.etaSeconds(op), WarmingFor: op.model}, nil
	}
}

// startOrJoin returns the in-flight op to observe: a freshly started swap, the
// existing swap for the SAME backend (joined), or nil when a swap for a DIFFERENT
// backend is already in flight (the caller must not start a second one — the
// single-flight guarantee that stops concurrent misses thrashing the GPU).
func (m *SwapManager) startOrJoin(backend, model string) *swapOp {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.inflight != nil {
		if m.inflight.backend == backend {
			return m.inflight // join the same-model swap
		}
		return nil // different model in flight: refuse
	}

	op := &swapOp{
		backend:   backend,
		model:     model,
		startedAt: m.now(),
		loadEst:   m.loadEstimate(backend),
		done:      make(chan struct{}),
	}
	m.inflight = op
	go m.runSwap(op)
	return op
}

// runSwap performs the actual eviction + start + wait-ready in the background,
// with its own bounded context (independent of any job context). It closes
// op.done when finished and clears the single-flight slot.
func (m *SwapManager) runSwap(op *swapOp) {
	defer func() {
		close(op.done)
		m.mu.Lock()
		if m.inflight == op {
			m.inflight = nil
		}
		m.mu.Unlock()
		m.recordSwap(m.swapRecord(op))
	}()

	ctx, cancel := context.WithTimeout(context.Background(), m.backgroundMax)
	defer cancel()

	// Serveability preflight (citadel-cli#956): runSwap only ever runs for a
	// target that EnsureResident already confirmed is NOT resident (the
	// resident-hit fast path returns before startOrJoin/runSwap are reached at
	// all), so this is exactly "about to start a NEW engine". Checked BEFORE
	// preemption so a genuinely unserveable target never evicts another
	// resident engine to make room for a start that was always going to fail.
	if blocked, reason := m.preflight(op.backend, m.diskMetrics()); blocked {
		op.err = &SwapPreflightBlockedError{Backend: op.backend, Reason: reason}
		return
	}

	// Plan and execute non-durable preemption to free VRAM.
	if err := m.preempt(ctx, op); err != nil {
		op.err = err
		return
	}
	if op.transient {
		return // could not proceed now; caller warms and retries
	}

	// Start the target engine (SERVICE_START {service, model}; no vram_mb).
	// Record the attempt BEFORE issuing it: an inline compose build can take
	// minutes, and the readiness gate must already see the start as in flight
	// while it runs (citadel-cli#705). A failed start clears the record so it
	// cannot pass as evidence that the engine is on its way up.
	m.markStartAttempted(op.backend)
	op.started = true
	if err := m.ctrl.Start(ctx, op.backend, op.model); err != nil {
		m.clearStartAttempt(op.backend)
		op.err = fmt.Errorf("failed to start %s for swap: %w", op.backend, err)
		return
	}

	// Wait for readiness up to the background ceiling.
	for {
		if m.ctrl.Ready(ctx, op.backend) {
			op.ready = true
			// Time the load before marking ready: this is the only place the node
			// learns how long THIS engine actually takes to come up, and that
			// measurement is what raises the residency ceiling above the load
			// (citadel-cli#687) instead of trusting the coarse table.
			m.recordLoadDuration(op.backend, m.now().Sub(op.startedAt))
			// Kick off the LIVE VRAM footprint measurement (citadel-cli#689) now
			// that the engine is actually resident and serving -- this is the
			// only point in the swap lifecycle where a real measurement is even
			// possible (the engine did not exist to attribute VRAM to before
			// Start, and this goroutine's own view of "resident" is what just
			// went true). Deliberately FIRE-AND-FORGET via its own goroutine,
			// NOT awaited here: MeasuredVRAM runs a full status.Collector.Collect
			// (docker stats + nvidia-smi across every service), documented as
			// multi-second on a busy node, and this loop is on the path to
			// close(op.done) via the deferred cleanup below -- EnsureResident's
			// <=15s wait blocks on <-op.done, so a swap that genuinely loaded in
			// time could spuriously report model_warming while waiting on a
			// measurement nobody asked to wait for. measureAndRecordVRAM uses
			// its own bounded context rather than this function's ctx, because
			// this function's ctx is cancelled by the `defer cancel()` above the
			// instant runSwap returns -- which happens right after this line.
			//
			// Gated on vramMeasurableOnReady: for ollama, Ready==true does NOT
			// mean weights are resident (see SwapController.Ready's doc comment)
			// — it means a model is merely LISTED, and it lazy-loads on first
			// request. Measuring here for ollama would attribute a near-zero
			// cold VRAM reading to the engine, and because vramMeasured
			// deliberately survives eviction (see forget below), that bad number
			// would stay cached for the rest of the process's life on exactly
			// the path that decides how much VRAM to free.
			if vramMeasurableOnReady(op.backend) {
				go m.measureAndRecordVRAM(op.backend, op.model)
			}
			m.markReady(op.backend)
			return
		}
		select {
		case <-ctx.Done():
			return // ceiling hit; op.ready stays false (warming)
		case <-time.After(m.readyPoll):
		}
	}
}

// preempt runs PlanPreemption over the min-residency-filtered, LRU-ordered
// candidate set and executes the resulting non-durable stops. It sets
// op.transient (not op.err) when the deploy cannot fit ONLY because min-residency
// is currently protecting an otherwise-evictable engine — a retryable condition.
// A genuine inability to fit (pinned / not enough VRAM even ignoring
// min-residency) is a hard error.
func (m *SwapManager) preempt(ctx context.Context, op *swapOp) error {
	required := m.requiredVRAMBytes(op.backend, op.model)
	if required == 0 {
		return nil // no budget known: skip preemption (fail-safe)
	}
	candidates, freeVRAM, haveVRAM := m.ctrl.PreemptInputs(ctx, op.backend)
	if !haveVRAM {
		return nil // free VRAM unknown: never evict on an absent signal
	}

	// LRU order: least-recently-used first so PlanPreemption's stable
	// idle-then-largest-VRAM sort breaks ties toward the coldest engine.
	m.sortByLRU(candidates)

	// Would it fit if min-residency did not protect anyone? Distinguishes a hard
	// "can never fit" from a transient "can't fit yet".
	fullPlan := status.PlanPreemption(candidates, required, freeVRAM)
	if !fullPlan.Fits {
		return fmt.Errorf("cannot swap in %s: %s", op.backend, fullPlan.Reason)
	}

	// Now apply the residency protections: an engine inside its min-residency
	// floor, or one that has not served anything since it loaded, is not a
	// candidate right now.
	eligible := m.filterResidencyProtected(candidates)
	plan := status.PlanPreemption(eligible, required, freeVRAM)
	if !plan.Fits {
		op.transient = true // blocked only by residency protection; retry soon
		return nil
	}

	// This swap needs to take VRAM away from a resident engine, so it is the kind
	// the rate bound governs (citadel-cli#687). Checked HERE rather than before
	// the swap starts, so a node with free VRAM is never refused for swaps it
	// made earlier: only an eviction counts against the ceiling, and only an
	// eviction is refused by it.
	if len(plan.Stop) > 0 {
		if err := m.checkSwapRate(op.backend); err != nil {
			return err
		}
	}

	for _, name := range plan.Stop {
		if err := m.ctrl.StopNonDurable(name); err != nil {
			return fmt.Errorf("cannot swap in %s: failed to evict %s: %w", op.backend, name, err)
		}
		op.evicted = append(op.evicted, name)
		m.forget(name)
	}
	return nil
}

// checkSwapRate refuses an evicting swap once the node has spent its allowance
// for the window. The error is hard on purpose — see SwapRateLimitedError.
func (m *SwapManager) checkSwapRate(backend string) error {
	m.mu.Lock()
	m.pruneSwapsLocked(m.now())
	n := m.evictingSwapsInWindowLocked(m.now())
	max := m.maxEvictingPerWindow
	window := m.rateWindow
	m.mu.Unlock()

	if max > 0 && n >= max {
		return &SwapRateLimitedError{Backend: backend, Swaps: n, Max: max, Window: window}
	}
	return nil
}

// filterResidencyProtected drops candidates that must not be evicted yet. Two
// protections, in increasing strength:
//
//   - The min-residency floor: an engine that became ready within minResidency
//     is off limits (pre-existing behaviour).
//   - The served-once invariant (citadel-cli#687): an engine that has had NO
//     request dispatched to it since it became ready is off limits until it
//     does. Without this, a model whose load takes 78s under a 60s floor could
//     be evicted before it ever served anything, making the whole load waste.
//     It is bounded by unservedResidencyCeiling so an engine nobody ends up
//     asking for cannot pin VRAM indefinitely.
//
// An engine with no readyAt entry — one this node never swapped in, e.g. started
// by an operator or resident since boot — is protected by NEITHER. That is
// deliberate: it has been up for an unknown-but-long time, and treating "we have
// no record" as "recently loaded" would make long-resident engines unevictable
// after a worker restart.
func (m *SwapManager) filterResidencyProtected(candidates []status.PreemptCandidate) []status.PreemptCandidate {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]status.PreemptCandidate, 0, len(candidates))
	for _, c := range candidates {
		ready, known := m.readyAt[c.Name]
		if !known {
			out = append(out, c)
			continue
		}
		age := now.Sub(ready)
		if age < m.minResidency {
			continue
		}
		served, everServed := m.servedAt[c.Name]
		if (!everServed || served.Before(ready)) && age < m.unservedResidencyCeilingLocked(c.Name) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// unservedResidencyCeilingLocked is how long an engine that has served nothing
// since loading stays protected. It is the engine's own load time — measured if
// this node has timed one, else the table estimate — floored at minResidency, so
// the protection can never be shorter than the load it exists to justify. That
// is the "raise the floor above the measured load time, per engine" half of
// citadel-cli#687. Callers hold m.mu.
func (m *SwapManager) unservedResidencyCeilingLocked(backend string) time.Duration {
	load, ok := m.loadMeasured[backend]
	if !ok {
		load = m.loadEstimate(backend)
	}
	if load < m.minResidency {
		return m.minResidency
	}
	return load
}

// recordLoadDuration remembers how long an engine actually took to become ready.
func (m *SwapManager) recordLoadDuration(backend string, d time.Duration) {
	if d <= 0 {
		return
	}
	m.mu.Lock()
	m.loadMeasured[backend] = d
	m.mu.Unlock()
}

// MeasuredLoad reports the last observed cold-start duration for backend, and
// whether one has been observed at all.
func (m *SwapManager) MeasuredLoad(backend string) (time.Duration, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.loadMeasured[backend]
	return d, ok
}

// vramCacheKey is the vramMeasured map key for a (backend, model) pair. Model
// is part of the key because the same engine can be swapped in for different
// models across swaps (vllm in particular), and their footprints differ.
func vramCacheKey(backend, model string) string {
	return backend + "\x00" + model
}

// vramMeasurableOnReady reports whether Ready()==true for `backend` actually
// means its weights are resident in VRAM -- the precondition for trusting a
// MeasuredVRAM reading taken at that moment as the engine's real footprint.
// False only for ollama: SwapController.Ready's doc comment states ollama
// lists a pulled model before loading it and only loads on the first request,
// so Ready==true there means "listed", not "resident". Mirrors that exception
// exactly so the two cannot silently drift apart.
func vramMeasurableOnReady(backend string) bool {
	return backend != "ollama"
}

// vramMeasureTimeout bounds the background MeasuredVRAM call
// (measureAndRecordVRAM). It is generous relative to a single status
// collection (documented as multi-second on a busy node) because nothing
// downstream is waiting on it -- the only cost of a slow measurement is a
// slightly later cache fill, never a delayed ready signal (that's the whole
// point of running it off the critical path).
const vramMeasureTimeout = 10 * time.Second

// measureAndRecordVRAM runs MeasuredVRAM and caches the result, entirely off
// the swap's readiness critical path (citadel-cli#689 review finding: calling
// this synchronously before close(op.done) let a slow measurement delay the
// ready signal EnsureResident's wait budget blocks on). Called via `go` from
// runSwap, so it uses its OWN bounded context rather than runSwap's: that
// context is cancelled by runSwap's `defer cancel()` the moment runSwap
// returns, which happens immediately after this goroutine is launched --
// reusing it here would race the measurement against its own cancellation.
// Best-effort: an error or a false `ok` just means no measurement is cached
// this round, same as it always has (requiredVRAMBytes falls back to the
// table either way).
func (m *SwapManager) measureAndRecordVRAM(backend, model string) {
	ctx, cancel := context.WithTimeout(context.Background(), vramMeasureTimeout)
	defer cancel()
	bytes, ok := m.ctrl.MeasuredVRAM(ctx, backend)
	if !ok {
		return
	}
	m.recordMeasuredVRAM(backend, model, bytes)
}

// recordMeasuredVRAM remembers the live VRAM footprint (bytes) observed for a
// (backend, model) pair right after it became ready. A zero reading is
// dropped rather than cached: it almost always means the footprint signal was
// unavailable (no GPU, attribution miss), and caching it would make a later
// fit check see "measured: needs nothing", the wrong direction of error for a
// check that exists to prevent an OOM (citadel-cli#689).
func (m *SwapManager) recordMeasuredVRAM(backend, model string, bytes uint64) {
	if bytes == 0 {
		return
	}
	m.mu.Lock()
	m.vramMeasured[vramCacheKey(backend, model)] = bytes
	m.mu.Unlock()
}

// MeasuredVRAMBytes reports the last observed live VRAM footprint (bytes) for
// a (backend, model) pair, and whether one has been observed at all.
func (m *SwapManager) MeasuredVRAMBytes(backend, model string) (uint64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.vramMeasured[vramCacheKey(backend, model)]
	return b, ok
}

// vramFitMarginFactor inflates a MEASURED footprint before it sizes an
// incoming (about-to-load) engine's fit check (citadel-cli#689 review
// finding). The table estimate's padding exists specifically so two big
// engines can't both end up resident at once -- see the engineVRAMEstimateMB
// doc comment in internal/status/hotswap.go: sizing at raw steady-state lets
// the planner see "fits, no evict" and admit a second model into a card that
// cannot actually hold both, "a no-op swap followed by an OOM". A zero-margin
// MEASURED value reintroduces exactly that: on node 1297's 3090,
// unlimited-ocr (~14GB measured) + bonsai (~6GB measured) sum to ~20GB
// resident against a 24GB card -- only ~4GB free, no headroom for load-time
// transients (KV-cache warm-up, allocator fragmentation, CUDA context
// overhead) that a STEADY-STATE reading doesn't capture. 15% is well below
// the table's own padding -- unlimited-ocr's fit value is still ~16GB against
// a 20GB table budget, a real improvement -- but never a bare zero-margin bet
// on the raw number.
const vramFitMarginFactor = 1.15

// vramFitBytes applies the fit-check safety margin to a measured footprint.
// Deliberately NOT applied inside MeasuredVRAMBytes: that accessor (and the
// value cmd/hotswap.go logs) reports the raw measurement for observability,
// so the margin exists in exactly one place -- the fit decision -- rather
// than leaking into every reader as a silently-inflated "measurement".
func vramFitBytes(measured uint64) uint64 {
	return uint64(float64(measured) * vramFitMarginFactor)
}

// requiredVRAMBytes is the VRAM budget (bytes) preempt() sizes a swap-in
// against: a MEASURED footprint from a prior residency of this exact
// (backend, model) pair, margined by vramFitBytes, when one exists, else the
// coarse table estimate (citadel-cli#689). The estimate is unavoidable the
// first time a given pair is swapped in — nothing can measure a model that
// has never been loaded — but every swap after the first replaces the padded
// provisioning number with a margined version of what the engine actually
// used.
func (m *SwapManager) requiredVRAMBytes(backend, model string) uint64 {
	if measured, ok := m.MeasuredVRAMBytes(backend, model); ok {
		return vramFitBytes(measured)
	}
	return m.requiredVRAM(backend)
}

// swapRecord builds the ledger entry for a finished swap. The outcome ordering
// matters: a rate-limited or otherwise failed swap is reported as such even
// though it also did not become ready.
func (m *SwapManager) swapRecord(op *swapOp) SwapRecord {
	outcome := swapOutcomeWarming
	switch {
	case op.err != nil:
		outcome = swapOutcomeFailed
		var rateErr *SwapRateLimitedError
		var preflightErr *SwapPreflightBlockedError
		switch {
		case errors.As(op.err, &rateErr):
			outcome = swapOutcomeRateLimited
		case errors.As(op.err, &preflightErr):
			outcome = swapOutcomePreflightBlocked
		}
	case op.ready:
		outcome = swapOutcomeReady
	case op.transient || !op.started:
		outcome = swapOutcomeBlocked
	}
	return SwapRecord{
		Backend:   op.backend,
		Model:     op.model,
		Evicted:   append([]string(nil), op.evicted...),
		StartedAt: op.startedAt,
		Wait:      m.now().Sub(op.startedAt),
		Outcome:   outcome,
	}
}

// sortByLRU orders candidates least-recently-used first (unknown = oldest).
func (m *SwapManager) sortByLRU(candidates []status.PreemptCandidate) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sort.SliceStable(candidates, func(i, j int) bool {
		return m.lastUsed[candidates[i].Name].Before(m.lastUsed[candidates[j].Name])
	})
}

// touch records that `backend` was just requested (LRU freshness). Persistence
// (if enabled) is debounced, not synchronous: touch fires on every
// EnsureResident call, including the already-resident fast path, so writing on
// every call would be one disk write per inference request.
func (m *SwapManager) touch(backend string) {
	m.mu.Lock()
	m.lastUsed[backend] = m.now()
	m.mu.Unlock()
	m.persistIfDue()
}

// markReady records that `backend` just became ready (starts its min-residency
// window and refreshes its LRU stamp).
func (m *SwapManager) markReady(backend string) {
	now := m.now()
	m.mu.Lock()
	m.readyAt[backend] = now
	m.lastUsed[backend] = now
	m.mu.Unlock()
	m.persistIfDue()
}

// markServed records that a request was just dispatched to backend — the moment
// EnsureResident hands it to the inference path. Compared against readyAt it
// answers "has this engine done anything since it loaded?" (citadel-cli#687).
//
// It means "a request was routed here", not "the engine produced a completion":
// the swap manager hands off before the readiness gate runs and cannot observe
// the result. That is the strongest honest signal available at this seam, and it
// is the one the eviction invariant needs — a load followed by a dispatched
// request was not wasted.
func (m *SwapManager) markServed(backend string) {
	now := m.now()
	m.mu.Lock()
	m.servedAt[backend] = now
	m.mu.Unlock()
}

// forget clears the residency window of an evicted engine, and with it the
// record that a start was ever issued: an engine we just stopped is not booting.
// The served stamp goes too — it belongs to a residency that has ended.
//
// loadMeasured deliberately SURVIVES: it measures how long the engine takes to
// load, which does not change because we stopped it, and dropping it would send
// the next swap-in of this engine back to the coarse table estimate for its
// residency ceiling.
//
// lastUsed ALSO deliberately survives — do not add delete(m.lastUsed, name)
// here. citadel-cli#688's suggested fix is explicit: "On eviction, preserve the
// engine's last-use time rather than dropping it. An evicted engine should
// re-enter as 'used recently', not 'never used'." Clearing it here would make
// an evicted engine look like it was never used at all (sortByLRU treats a
// missing entry as the oldest possible timestamp — first victim), which under
// an LRU-ordered candidate set is precisely the alternating-eviction thrash
// #688 exists to prevent: evict A, forget A, A now looks coldest, evict A
// again before it ever gets to serve. Verified against this function's history
// before writing this comment: forget() has never deleted lastUsed, so this is
// pinning existing (correct) behavior against a plausible-looking future
// regression, not documenting a fix made here.
//
// vramMeasured SURVIVES for the identical reason as loadMeasured (citadel-cli
// #689): the VRAM a (backend, model) pair uses does not change because we
// stopped it.
func (m *SwapManager) forget(name string) {
	m.mu.Lock()
	delete(m.readyAt, name)
	delete(m.startedAt, name)
	delete(m.servedAt, name)
	m.mu.Unlock()
}

// markStartAttempted records that a start of backend was just issued.
func (m *SwapManager) markStartAttempted(backend string) {
	now := m.now()
	m.mu.Lock()
	m.startedAt[backend] = now
	m.mu.Unlock()
}

// clearStartAttempt drops the start record for backend.
func (m *SwapManager) clearStartAttempt(backend string) {
	m.mu.Lock()
	delete(m.startedAt, backend)
	m.mu.Unlock()
}

// EngineStartedAt reports when this node last issued a start for backend, and
// whether one is on record at all. It is the supervisor's answer to "was this
// engine ever actually started", which the readiness gate needs to tell a cold
// start apart from an engine that is not running (citadel-cli#705). Satisfies
// engineStartTracker.
func (m *SwapManager) EngineStartedAt(backend string) (time.Time, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.startedAt[backend]
	return t, ok
}

// etaSeconds estimates remaining seconds until the in-flight swap is ready,
// floored at the retry_after hint so the platform never busy-retries.
func (m *SwapManager) etaSeconds(op *swapOp) int {
	remaining := op.loadEst - m.now().Sub(op.startedAt)
	secs := int(remaining.Seconds())
	if secs < warmingRetryAfter {
		secs = warmingRetryAfter
	}
	return secs
}

// inflightSnapshot returns the single in-flight swap op, or nil, under lock.
// Callers that need to derive more than one value from the same in-flight
// state (e.g. ETA and the citadel-cli#681 WarmingFor discriminator) should
// snapshot once via this and pass it along, rather than each re-reading
// m.inflight and risking two different observations of it.
func (m *SwapManager) inflightSnapshot() *swapOp {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.inflight
}

// blockedETASeconds estimates the wait for a backend whose swap cannot start
// because a DIFFERENT swap holds the single-flight slot: the in-flight swap's
// remaining time plus this backend's own cold start. Without the first term the
// node quotes a number that assumes work already underway that has not begun.
// blocking is the caller's own inflightSnapshot() (nil when the blocking swap
// finished between startOrJoin and here).
func (m *SwapManager) blockedETASeconds(backend string, blocking *swapOp) int {
	own := int(m.loadEstimate(backend).Seconds())

	if blocking == nil {
		// The blocking swap finished between startOrJoin and here. This backend
		// still is not resident, so its own cold start is the honest estimate.
		return own
	}

	remaining := int((blocking.loadEst - m.now().Sub(blocking.startedAt)).Seconds())
	if remaining < 0 {
		remaining = 0
	}
	return remaining + own
}

// retryAfterFor paces the platform's retry to the quoted wait, so a long queue
// behind another model is not polled every 10 seconds. Bounded so a caller is
// never told to disappear for an unbounded stretch.
func retryAfterFor(etaSeconds int) int {
	if etaSeconds <= warmingRetryAfter {
		return warmingRetryAfter
	}
	if etaSeconds > warmingRetryAfterMax {
		return warmingRetryAfterMax
	}
	return etaSeconds
}
