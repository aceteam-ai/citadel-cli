package reconcile

import (
	"errors"
	"sync"
	"time"
)

// ErrFullWipeRefused is the sentinel a full-wipe-guard refusal wraps (see
// ReconcileOnce's guardErr), so callers can identify "this pass was refused
// by the guard" via errors.Is without string-matching the message. It survives
// errors.Join (Join's result supports Unwrap() []error, which errors.Is
// traverses), so it is detectable on the joined guardErr+reportErr that
// ReconcileOnce actually returns.
var ErrFullWipeRefused = errors.New("reconcile: refused full wipe")

// DefaultSummaryInterval is how often a PERSISTING refusal is re-logged, once
// the initial loud false->true transition has already fired. One hour: loud
// enough to keep the signal in the journal, far short of the 4574-in-5-days
// noise floor an identical 5-minute WARN produced (citadel-cli#742).
const DefaultSummaryInterval = time.Hour

// HealthState is the queryable, additive snapshot of the reconcile loop's
// full-wipe-guard status (citadel-cli#742). The zero value (Refused: false)
// is the healthy/default state -- a caller mirroring this onto an omitempty
// heartbeat field should omit the whole block in that case, exactly like
// WorkerLiveness/SwapActivity omit theirs when their subsystem has nothing to
// report.
type HealthState struct {
	// Refused is true while the most recent pass (and every consecutive pass
	// since) was refused by the full-wipe guard.
	Refused bool `json:"refused"`
	// Reason is the full-wipe guard's error text for the CURRENT (most
	// recent) refused pass.
	Reason string `json:"reason,omitempty"`
	// Since is when the refused streak began (the false->true transition).
	// Zero when Refused is false.
	Since time.Time `json:"since,omitempty"`
	// Count is the number of consecutive refused passes since Since.
	Count int `json:"count,omitempty"`
}

// HealthTracker turns a stream of per-pass ReconcileOnce outcomes into (a) a
// state snapshot suitable for the heartbeat (State) and (b) a throttled log:
// loud once on the false->true (becomes refused) transition, a periodic
// summary while the refusal persists, loud once again on the true->false
// (clears) transition -- instead of an identical WARN on every single pass.
// Node 1297 logged 4574 of the old identical warning over 5+ days, which at
// that volume is indistinguishable from background noise (citadel-cli#742).
//
// Safe for concurrent use: Observe is called from the single reconcile-pass
// goroutine (ReconcileOnce is the one call site); State is read from any
// heartbeat-collection goroutine.
type HealthTracker struct {
	mu    sync.Mutex
	state HealthState

	// sinceLastLog counts refused passes since the log line last fired
	// (either the loud transition or a periodic summary), distinct from the
	// streak-total Count on HealthState -- it is what the summary line's "N
	// passes since last summary" reports.
	sinceLastLog int
	lastLoggedAt time.Time

	// Injectable seams for deterministic tests.
	now             func() time.Time
	logf            func(format string, args ...any)
	summaryInterval time.Duration
}

// NewHealthTracker builds a tracker. logf receives the throttled log lines;
// a nil logf is a valid no-op sink (state tracking still works, nothing is
// printed) so a caller that only wants the queryable State() need not stub a
// logger.
func NewHealthTracker(logf func(format string, args ...any)) *HealthTracker {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &HealthTracker{
		now:             time.Now,
		logf:            logf,
		summaryInterval: DefaultSummaryInterval,
	}
}

// Observe records the outcome of one ReconcileOnce pass. passErr is exactly
// what ReconcileOnce returned (nil on a healthy/converged/no-op pass). Nil
// receiver is a safe no-op, so a Reconciler built without NewReconciler (a
// zero-value struct literal, e.g. in a test) never panics on this call.
func (t *HealthTracker) Observe(passErr error) {
	if t == nil {
		return
	}
	refused := errors.Is(passErr, ErrFullWipeRefused)

	t.mu.Lock()
	defer t.mu.Unlock()

	now := t.now()
	wasRefused := t.state.Refused

	switch {
	case refused && !wasRefused:
		// false -> true: loud, unconditional -- this is the alarm.
		t.state = HealthState{Refused: true, Reason: passErr.Error(), Since: now, Count: 1}
		t.sinceLastLog = 0
		t.lastLoggedAt = now
		t.logf("⚠️ reconcile refused by full-wipe guard: %v", passErr)

	case refused && wasRefused:
		// Still refused: keep the reason/count current, but only log on the
		// decaying cadence -- an identical WARN every pass is exactly the
		// noise this tracker replaces.
		t.state.Reason = passErr.Error()
		t.state.Count++
		t.sinceLastLog++
		if now.Sub(t.lastLoggedAt) >= t.summaryInterval {
			t.logf("⚠️ reconcile still refused by full-wipe guard (%d pass(es) since last summary, refused since %s): %v",
				t.sinceLastLog, t.state.Since.Format(time.RFC3339), passErr)
			t.sinceLastLog = 0
			t.lastLoggedAt = now
		}

	case !refused && wasRefused:
		// true -> false: loud, unconditional -- the clear is as important a
		// signal as the alarm (a node whose reconcile silently recovers is
		// fine; one where the operator never learns it recovered looks stuck).
		t.logf("✅ reconcile refusal cleared (was refused since %s, %d pass(es) refused)",
			t.state.Since.Format(time.RFC3339), t.state.Count)
		t.state = HealthState{}
		t.sinceLastLog = 0
		t.lastLoggedAt = time.Time{}

	default:
		// !refused && !wasRefused: steady healthy state. Nothing to log,
		// nothing to update -- this is the common case for every node that
		// has never hit the guard.
	}
}

// State returns a snapshot of the current health state. Nil receiver returns
// the zero value (healthy/never-refused), matching Observe's nil-safety.
func (t *HealthTracker) State() HealthState {
	if t == nil {
		return HealthState{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state
}
