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
//     never evicted (filtered out of the candidate set before planning).
//   - LRU: candidates are pre-sorted least-recently-used first so PlanPreemption's
//     stable idle-then-largest-VRAM ordering breaks ties by LRU.
package worker

import (
	"context"
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
}

// SwapManager serializes and observes model swaps on a node.
type SwapManager struct {
	ctrl SwapController

	// requiredVRAM returns the VRAM budget (bytes) a swap-in of `backend` needs.
	requiredVRAM func(backend string) uint64
	// loadEstimate returns the expected cold-start time for `backend`, used for
	// the warming ETA.
	loadEstimate func(backend string) time.Duration
	now          func() time.Time

	waitBudget    time.Duration
	minResidency  time.Duration
	backgroundMax time.Duration // ceiling on a background swap (releases the lock)
	readyPoll     time.Duration // readiness poll interval

	mu       sync.Mutex
	inflight *swapOp              // the single in-flight swap, or nil
	lastUsed map[string]time.Time // per-engine last request time (LRU)
	readyAt  map[string]time.Time // per-engine last became-ready time (min-residency)
	// startedAt records when this node last ISSUED a start for an engine, which
	// is the only trustworthy evidence that an unbound port is a cold start
	// rather than an engine that is simply not running (citadel-cli#705). The
	// readiness gate reads it through EngineStartedAt; it is cleared when a start
	// fails and when an engine is evicted, so a stale entry can never keep an
	// absent engine reporting "warming".
	startedAt map[string]time.Time
}

// NewSwapManager builds a swap manager with default timing and VRAM/load
// estimates sourced from the shared status tables. ctrl supplies the node
// side-effects.
func NewSwapManager(ctrl SwapController) *SwapManager {
	return &SwapManager{
		ctrl:          ctrl,
		requiredVRAM:  func(b string) uint64 { return uint64(status.EngineVRAMEstimateMB(b)) * 1024 * 1024 },
		loadEstimate:  defaultLoadEstimate,
		now:           time.Now,
		waitBudget:    swapWaitBudget,
		minResidency:  swapMinResidency,
		backgroundMax: swapBackgroundMaxDur,
		readyPoll:     swapReadyPollEvery,
		lastUsed:      map[string]time.Time{},
		readyAt:       map[string]time.Time{},
		startedAt:     map[string]time.Time{},
	}
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
		return SwapOutcome{Ready: true}, nil
	}

	op := m.startOrJoin(backend, model)
	if op == nil {
		// A different model is being swapped in and single-flight refuses to start
		// a second one, so THIS model's load has not begun. Reporting a bare
		// cold-start estimate here would be a fabricated number: the real wait is
		// the in-flight swap finishing PLUS this engine's own load. Quote that, and
		// pace the retry hint to it, so a caller does not busy-retry a node that is
		// not working on its request (citadel-cli#680). Telling the two cases apart
		// on the wire ("loading yours" vs "busy with another") is citadel-cli#681.
		eta := m.blockedETASeconds(backend)
		return SwapOutcome{
			Ready:             false,
			ETASeconds:        eta,
			RetryAfterSeconds: retryAfterFor(eta),
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
			return SwapOutcome{Ready: true}, nil
		}
		// Completed but not ready (transient block, e.g. min-residency): warm.
		return SwapOutcome{Ready: false, ETASeconds: m.etaSeconds(op)}, nil
	case <-timer.C:
		return SwapOutcome{Ready: false, ETASeconds: m.etaSeconds(op)}, nil
	case <-ctx.Done():
		// The job context was cancelled; the background swap keeps running so a
		// retry can pick up the now-resident model. Report warming.
		return SwapOutcome{Ready: false, ETASeconds: m.etaSeconds(op)}, nil
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
	}()

	ctx, cancel := context.WithTimeout(context.Background(), m.backgroundMax)
	defer cancel()

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
	if err := m.ctrl.Start(ctx, op.backend, op.model); err != nil {
		m.clearStartAttempt(op.backend)
		op.err = fmt.Errorf("failed to start %s for swap: %w", op.backend, err)
		return
	}

	// Wait for readiness up to the background ceiling.
	for {
		if m.ctrl.Ready(ctx, op.backend) {
			op.ready = true
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
	required := m.requiredVRAM(op.backend)
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

	// Now apply the min-residency floor: an engine that became ready within the
	// floor is not a candidate right now.
	eligible := m.filterMinResidency(candidates)
	plan := status.PlanPreemption(eligible, required, freeVRAM)
	if !plan.Fits {
		op.transient = true // blocked only by min-residency; retry soon
		return nil
	}

	for _, name := range plan.Stop {
		if err := m.ctrl.StopNonDurable(name); err != nil {
			return fmt.Errorf("cannot swap in %s: failed to evict %s: %w", op.backend, name, err)
		}
		m.forget(name)
	}
	return nil
}

// filterMinResidency drops candidates that became ready within the min-residency
// floor (they must not be evicted yet).
func (m *SwapManager) filterMinResidency(candidates []status.PreemptCandidate) []status.PreemptCandidate {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]status.PreemptCandidate, 0, len(candidates))
	for _, c := range candidates {
		if t, ok := m.readyAt[c.Name]; ok && now.Sub(t) < m.minResidency {
			continue
		}
		out = append(out, c)
	}
	return out
}

// sortByLRU orders candidates least-recently-used first (unknown = oldest).
func (m *SwapManager) sortByLRU(candidates []status.PreemptCandidate) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sort.SliceStable(candidates, func(i, j int) bool {
		return m.lastUsed[candidates[i].Name].Before(m.lastUsed[candidates[j].Name])
	})
}

// touch records that `backend` was just requested (LRU freshness).
func (m *SwapManager) touch(backend string) {
	m.mu.Lock()
	m.lastUsed[backend] = m.now()
	m.mu.Unlock()
}

// markReady records that `backend` just became ready (starts its min-residency
// window and refreshes its LRU stamp).
func (m *SwapManager) markReady(backend string) {
	now := m.now()
	m.mu.Lock()
	m.readyAt[backend] = now
	m.lastUsed[backend] = now
	m.mu.Unlock()
}

// forget clears the residency window of an evicted engine, and with it the
// record that a start was ever issued: an engine we just stopped is not booting.
func (m *SwapManager) forget(name string) {
	m.mu.Lock()
	delete(m.readyAt, name)
	delete(m.startedAt, name)
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

// blockedETASeconds estimates the wait for a backend whose swap cannot start
// because a DIFFERENT swap holds the single-flight slot: the in-flight swap's
// remaining time plus this backend's own cold start. Without the first term the
// node quotes a number that assumes work already underway that has not begun.
func (m *SwapManager) blockedETASeconds(backend string) int {
	own := int(m.loadEstimate(backend).Seconds())

	m.mu.Lock()
	op := m.inflight
	m.mu.Unlock()
	if op == nil {
		// The blocking swap finished between startOrJoin and here. This backend
		// still is not resident, so its own cold start is the honest estimate.
		return own
	}

	remaining := int((op.loadEst - m.now().Sub(op.startedAt)).Seconds())
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
