package reconcile

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// TestHealthTrackerThrottlesRepeatedRefusal is the core citadel-cli#742
// assertion: a refused loop running N times must log loudly once (the
// false->true transition) plus periodic summaries, not N identical warnings.
// Node 1297 logged 4574 identical WARNs over 5+ days before this fix.
func TestHealthTrackerThrottlesRepeatedRefusal(t *testing.T) {
	var logs []string
	tracker := NewHealthTracker(func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	})
	now := time.Unix(1_700_000_000, 0)
	tracker.now = func() time.Time { return now }
	tracker.summaryInterval = time.Hour

	refusalErr := fmt.Errorf("%w: empty desired state with 3 module(s) installed", ErrFullWipeRefused)

	// 10 consecutive refused passes, 5 minutes apart (the loop's default
	// interval) -- well short of the hour-long summary cadence.
	for i := 0; i < 10; i++ {
		tracker.Observe(refusalErr)
		now = now.Add(5 * time.Minute)
	}

	if len(logs) != 1 {
		t.Fatalf("expected exactly 1 log line (the loud transition) after 10 refused passes within the summary window, got %d: %v", len(logs), logs)
	}
	state := tracker.State()
	if !state.Refused {
		t.Fatal("expected State().Refused == true")
	}
	if state.Count != 10 {
		t.Errorf("Count = %d, want 10 (one per observed refused pass)", state.Count)
	}
	if state.Reason == "" {
		t.Error("Reason should be populated on a refused state")
	}
	if state.Since.IsZero() {
		t.Error("Since should be set on a refused state")
	}

	// Cross the summary interval: the next refused pass should log a second
	// (summary) line, still not one per pass.
	now = now.Add(time.Hour)
	tracker.Observe(refusalErr)
	if len(logs) != 2 {
		t.Fatalf("expected a second (summary) log line after crossing the summary interval, got %d: %v", len(logs), logs)
	}
	if state2 := tracker.State(); state2.Count != 11 {
		t.Errorf("Count after summary pass = %d, want 11", state2.Count)
	}
}

// TestHealthTrackerLogsLoudlyOnClear asserts the true->false transition is
// always logged (never throttled away), and that State() resets to the
// zero/healthy value.
func TestHealthTrackerLogsLoudlyOnClear(t *testing.T) {
	var logs []string
	tracker := NewHealthTracker(func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	})
	now := time.Unix(1_700_000_000, 0)
	tracker.now = func() time.Time { return now }

	refusalErr := fmt.Errorf("%w: empty desired state", ErrFullWipeRefused)
	tracker.Observe(refusalErr)
	tracker.Observe(refusalErr)
	if len(logs) != 1 {
		t.Fatalf("expected 1 log line before the clear, got %d: %v", len(logs), logs)
	}

	tracker.Observe(nil) // a converged pass: refused -> ok
	if len(logs) != 2 {
		t.Fatalf("expected the clear transition to log, got %d lines: %v", len(logs), logs)
	}

	state := tracker.State()
	if state.Refused {
		t.Error("State().Refused should be false after a clear")
	}
	if !state.Since.IsZero() || state.Count != 0 || state.Reason != "" {
		t.Errorf("state should fully reset on clear, got %+v", state)
	}

	// A further healthy pass must not log again -- clear already happened.
	tracker.Observe(nil)
	if len(logs) != 2 {
		t.Errorf("a subsequent healthy pass should not log, got %d lines: %v", len(logs), logs)
	}
}

// TestHealthTrackerHealthyPathNeverLogs asserts a node that has never hit the
// guard produces zero log lines and a permanently zero-value State() -- the
// "no change to the non-refused path" requirement.
func TestHealthTrackerHealthyPathNeverLogs(t *testing.T) {
	var logs []string
	tracker := NewHealthTracker(func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	})

	for i := 0; i < 5; i++ {
		tracker.Observe(nil)
	}
	if len(logs) != 0 {
		t.Errorf("healthy passes should never log, got %v", logs)
	}
	if state := tracker.State(); state.Refused {
		t.Errorf("State() should stay zero-value on an all-healthy stream, got %+v", state)
	}

	// A non-guard error (e.g. a transient fetch failure) is not a full-wipe
	// refusal and must not be reported as one.
	tracker.Observe(errors.New("control plane unreachable"))
	if len(logs) != 0 {
		t.Errorf("a non-guard pass error should not trip the refusal alarm, got %v", logs)
	}
	if state := tracker.State(); state.Refused {
		t.Error("a non-guard error must not be reported as a full-wipe refusal")
	}
}

// TestHealthTrackerNilSafe asserts Observe/State on a nil *HealthTracker are
// no-ops rather than panics, matching the doc contract that lets Reconciler
// callers skip a nil-check.
func TestHealthTrackerNilSafe(t *testing.T) {
	var tracker *HealthTracker
	tracker.Observe(fmt.Errorf("%w: x", ErrFullWipeRefused)) // must not panic
	if got := tracker.State(); got.Refused {
		t.Errorf("nil tracker State() = %+v, want zero value", got)
	}
}

// TestReconcileOnceFeedsHealthTracker is the integration half: the ONE call
// site (ReconcileOnce) must report both the refusal and its clearing to
// Health, exercised through the real Reconciler rather than HealthTracker in
// isolation, and Health must default to a working (non-nil) tracker via
// NewReconciler so callers that never replace it still get correct State().
func TestReconcileOnceFeedsHealthTracker(t *testing.T) {
	ops := newFakeOps(
		InstalledModule{Name: "a", Source: "a", Health: HealthRunning},
	)
	provider := &FakeProvider{Desired: DesiredState{Revision: "rev-empty"}} // zero modules, MANAGED
	rec := NewReconciler(provider, ops, "node-742")
	rec.RefuseFullWipe = true

	if rec.Health == nil {
		t.Fatal("NewReconciler must default Health to a non-nil tracker")
	}

	if _, _, err := rec.ReconcileOnce(context.Background()); err == nil {
		t.Fatal("expected the guard to refuse")
	}
	state := rec.Health.State()
	if !state.Refused {
		t.Fatalf("Health.State() after a refused pass = %+v, want Refused=true", state)
	}
	if state.Count != 1 {
		t.Errorf("Count after first refused pass = %d, want 1", state.Count)
	}

	// Now the control plane assigns something real: the next pass converges,
	// and the refusal must clear.
	provider.Desired = ds(ModuleAssignment{Name: "a", Source: "a", DesiredStatus: StatusRunning})
	if _, _, err := rec.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("expected the converged pass to succeed, got %v", err)
	}
	if state := rec.Health.State(); state.Refused {
		t.Errorf("Health.State() after a converged pass = %+v, want Refused=false", state)
	}
}
