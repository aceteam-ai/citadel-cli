package reconcile

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Config gates and tunes the reconcile loop. The zero value is DISABLED — a
// node only reconciles if it explicitly opts in (Enabled == true). This keeps
// existing nodes completely unaffected until they choose remote management.
type Config struct {
	// Enabled turns the reconcile loop on. DISABLED BY DEFAULT. The loop must
	// never be started unconditionally from the worker/serve path; wiring it is
	// gated behind this flag (see the package doc / PR notes).
	Enabled bool

	// Interval is the pull-reconcile period. Zero defaults to DefaultInterval.
	Interval time.Duration

	// MinInterval is the debounce floor: two reconcile passes (whether from the
	// ticker or a push nudge) are never run closer together than this. Zero
	// defaults to DefaultMinInterval.
	MinInterval time.Duration

	// Node is the reporting node identifier (device identity) stamped into the
	// actual-state report.
	Node string
}

const (
	// DefaultInterval is the default pull-reconcile period.
	DefaultInterval = 5 * time.Minute
	// DefaultMinInterval is the default debounce floor between passes.
	DefaultMinInterval = 10 * time.Second
)

func (c Config) interval() time.Duration {
	if c.Interval <= 0 {
		return DefaultInterval
	}
	return c.Interval
}

func (c Config) minInterval() time.Duration {
	if c.MinInterval <= 0 {
		return DefaultMinInterval
	}
	return c.MinInterval
}

// Reconciler runs one full pass: Fetch desired -> Reconcile -> Apply -> report
// ActualState. It is the unit the Loop drives, and is independently callable
// for a one-shot reconcile (e.g. from a push-nudge job handler).
type Reconciler struct {
	Provider DesiredStateProvider
	Ops      ModuleOps
	Node     string

	// RefuseFullWipe is a safety belt against an empty/misconfigured control
	// plane. When true, a SUCCESSFUL fetch that yields ZERO desired modules while
	// the node still has installed modules is REFUSED (the pass errors and applies
	// nothing) instead of letting the authoritative engine uninstall every module.
	// A fetch FAILURE is already safe (it aborts before the diff); this guards the
	// distinct "empty backend storage returns 200 with no modules" foot-gun. The
	// live wiring sets it true; it is off in the zero value so existing engine
	// tests keep the raw authoritative semantics.
	RefuseFullWipe bool
}

// NewReconciler builds a Reconciler.
func NewReconciler(p DesiredStateProvider, ops ModuleOps, node string) *Reconciler {
	return &Reconciler{Provider: p, Ops: ops, Node: node}
}

// ReconcileOnce performs a single end-to-end pass. It returns the plan that was
// applied and the apply result. Per-module failures are inside ApplyResult (and
// reported back as Health == HealthError); a returned error is reserved for
// pass-level failures (fetch failed, list failed, context cancelled).
func (r *Reconciler) ReconcileOnce(ctx context.Context) (Plan, ApplyResult, error) {
	desired, err := r.Provider.Fetch(ctx)
	if err != nil {
		return Plan{}, ApplyResult{}, err
	}

	actual, err := r.Ops.ListInstalled(ctx)
	if err != nil {
		return Plan{}, ApplyResult{}, err
	}

	// Full-wipe guard: refuse to converge to an empty desired set while modules
	// are installed (likely an empty/misconfigured control plane), rather than
	// uninstalling everything the node runs.
	if r.RefuseFullWipe && len(desired.Modules) == 0 && len(actual) > 0 {
		return Plan{}, ApplyResult{}, fmt.Errorf(
			"reconcile: refusing empty desired state with %d module(s) installed (possible control-plane misconfiguration)", len(actual))
	}

	plan, err := Reconcile(ctx, desired, actual)
	if err != nil {
		return Plan{}, ApplyResult{}, err
	}

	applyRes, err := Apply(ctx, r.Ops, plan)
	if err != nil {
		return plan, applyRes, err
	}

	// Report actual state back (best-effort: a report failure does not undo a
	// successful converge, but it IS surfaced to the caller).
	report, err := BuildActualState(ctx, r.Ops, applyRes.Errors, r.Node)
	if err != nil {
		return plan, applyRes, err
	}
	// Revision handshake: echo the desired revision the node just converged to.
	// Set unconditionally (even on partial per-module failure) — the node HAS
	// processed this revision; failures surface per-module as Health == HealthError.
	report.AppliedRevision = desired.Revision
	if err := r.Provider.Report(ctx, report); err != nil {
		return plan, applyRes, err
	}

	return plan, applyRes, nil
}

// Loop drives a Reconciler on a configurable pull interval and accepts push
// nudges for an immediate pass. It is DISABLED BY DEFAULT via Config.Enabled
// and is NOT started from the live worker path in this increment (wiring is a
// documented TODO). Build/test it in isolation.
type Loop struct {
	cfg  Config
	rec  *Reconciler
	nudg chan struct{}

	mu       sync.Mutex
	lastPass time.Time

	// now is injectable for deterministic debounce tests; defaults to time.Now.
	now func() time.Time
}

// NewLoop builds a reconcile loop. The returned loop does nothing until Run is
// called, and Run is a no-op when cfg.Enabled is false.
func NewLoop(cfg Config, rec *Reconciler) *Loop {
	return &Loop{
		cfg:  cfg,
		rec:  rec,
		nudg: make(chan struct{}, 1),
		now:  time.Now,
	}
}

// Nudge requests an immediate reconcile pass. It is non-blocking and coalescing:
// multiple nudges before the next pass collapse into one. This is the push path
// (a `reconcile` job on the node queue triggers Nudge).
func (l *Loop) Nudge() {
	select {
	case l.nudg <- struct{}{}:
	default:
		// A pass is already pending; coalesce.
	}
}

// Run drives the loop until ctx is cancelled. It returns immediately (nil) when
// the loop is disabled, so callers can wire it unconditionally and rely on the
// flag for the off switch. Each pass — ticker or nudge — is debounced to
// Config.MinInterval. Per-pass errors are returned via the onPass callback if
// set; the loop itself only stops on ctx cancellation.
func (l *Loop) Run(ctx context.Context, onPass func(Plan, ApplyResult, error)) error {
	if !l.cfg.Enabled {
		return nil
	}

	ticker := time.NewTicker(l.cfg.interval())
	defer ticker.Stop()

	runPass := func() {
		if !l.allow() {
			return
		}
		plan, res, err := l.rec.ReconcileOnce(ctx)
		if onPass != nil {
			onPass(plan, res, err)
		}
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			runPass()
		case <-l.nudg:
			runPass()
		}
	}
}

// allow enforces the debounce floor: it returns true (and stamps the pass time)
// only if at least Config.MinInterval has elapsed since the last allowed pass.
func (l *Loop) allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if !l.lastPass.IsZero() && now.Sub(l.lastPass) < l.cfg.minInterval() {
		return false
	}
	l.lastPass = now
	return true
}
