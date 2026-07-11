package status

import (
	"errors"
	"testing"
	"time"
)

func idlePtr(idle bool, seconds int64) *IdleState {
	return &IdleState{Idle: idle, IdleSeconds: seconds}
}

func TestAutoStopEnabled(t *testing.T) {
	cases := map[string]bool{
		"":      false,
		"0":     false,
		"false": false,
		"no":    false,
		"off":   false,
		"1":     true,
		"true":  true,
		"TRUE":  true,
		"yes":   true,
		"on":    true,
		" on ":  true,
	}
	for val, want := range cases {
		t.Setenv(AutoStopEnvVar, val)
		if got := AutoStopEnabled(); got != want {
			t.Errorf("AutoStopEnabled with %q = %v, want %v", val, got, want)
		}
	}
}

func TestAutoStopThresholdSeconds(t *testing.T) {
	t.Setenv(AutoStopThresholdEnvVar, "120")
	if got := AutoStopThresholdSeconds(); got != 120 {
		t.Errorf("threshold = %d, want 120", got)
	}
	// Invalid/non-positive falls back to the shared idle default.
	t.Setenv(AutoStopThresholdEnvVar, "0")
	t.Setenv("SERVICE_IDLE_THRESHOLD_SECONDS", "")
	if got := AutoStopThresholdSeconds(); got != DefaultIdleThresholdSeconds {
		t.Errorf("threshold with 0 = %d, want default %d", got, DefaultIdleThresholdSeconds)
	}
	t.Setenv(AutoStopThresholdEnvVar, "garbage")
	if got := AutoStopThresholdSeconds(); got != DefaultIdleThresholdSeconds {
		t.Errorf("threshold with garbage = %d, want default", got)
	}
}

func TestReconcileDisabledIsNoOp(t *testing.T) {
	called := false
	r := NewAutoStopReconciler(false, 60, func(IdleCandidate) error { called = true; return nil }, nil)
	st := &NodeStatus{Services: []ServiceInfo{{Name: "vllm", Status: ServiceStatusRunning, IdleState: idlePtr(true, 999)}}}
	if n := r.Reconcile(st); n != 0 {
		t.Errorf("disabled Reconcile stopped %d, want 0", n)
	}
	if called {
		t.Error("disabled reconciler must never invoke the stop func")
	}
}

func TestReconcileNilStopFuncIsNoOp(t *testing.T) {
	r := NewAutoStopReconciler(true, 60, nil, nil)
	if r.Enabled() {
		t.Error("reconciler with nil stop func must not be Enabled")
	}
	st := &NodeStatus{Services: []ServiceInfo{{Name: "vllm", Status: ServiceStatusRunning, IdleState: idlePtr(true, 999)}}}
	if n := r.Reconcile(st); n != 0 {
		t.Errorf("nil-stop Reconcile stopped %d, want 0", n)
	}
}

func TestReconcileNeverActsOnAbsentSignal(t *testing.T) {
	var stopped []IdleCandidate
	r := NewAutoStopReconciler(true, 60, func(c IdleCandidate) error { stopped = append(stopped, c); return nil }, nil)
	st := &NodeStatus{
		Services: []ServiceInfo{
			{Name: "unknown-idle", Status: ServiceStatusRunning, IdleState: nil},             // absent signal -> never
			{Name: "not-idle", Status: ServiceStatusRunning, IdleState: idlePtr(false, 0)},   // busy -> never
			{Name: "idle-below", Status: ServiceStatusRunning, IdleState: idlePtr(true, 30)}, // below threshold -> never
		},
	}
	if n := r.Reconcile(st); n != 0 {
		t.Fatalf("stopped %d, want 0 (got %+v)", n, stopped)
	}
}

func TestReconcileStopsConfirmedIdlePastThreshold(t *testing.T) {
	var stopped []IdleCandidate
	r := NewAutoStopReconciler(true, 60, func(c IdleCandidate) error { stopped = append(stopped, c); return nil }, nil)
	st := &NodeStatus{
		Services: []ServiceInfo{
			{Name: "vllm", Status: ServiceStatusRunning, IdleState: idlePtr(true, 300)}, // idle past -> stop
			{Name: "vllm-busy", Status: ServiceStatusRunning, IdleState: idlePtr(false, 0)},
			{Name: "vllm-stopped", Status: ServiceStatusStopped, IdleState: idlePtr(true, 999)}, // not running -> skip
		},
		Apps: []AppInfo{
			{Name: "diffusers", Status: ServiceStatusRunning, IdleState: idlePtr(true, 3600)}, // app path -> stop
		},
	}
	n := r.Reconcile(st)
	if n != 2 {
		t.Fatalf("stopped %d, want 2 (got %+v)", n, stopped)
	}
	want := map[string]EntityKind{"vllm": EntityService, "diffusers": EntityApp}
	for _, c := range stopped {
		if k, ok := want[c.Name]; !ok || k != c.Kind {
			t.Errorf("unexpected stop %+v", c)
		}
	}
}

func TestReconcileThresholdBoundaryInclusive(t *testing.T) {
	var stopped int
	r := NewAutoStopReconciler(true, 60, func(IdleCandidate) error { stopped++; return nil }, nil)
	st := &NodeStatus{Services: []ServiceInfo{{Name: "edge", Status: ServiceStatusRunning, IdleState: idlePtr(true, 60)}}}
	if n := r.Reconcile(st); n != 1 || stopped != 1 {
		t.Errorf("idle_seconds==threshold should stop: got n=%d stopped=%d", n, stopped)
	}
}

func TestReconcileContinuesAfterStopError(t *testing.T) {
	var attempts int
	r := NewAutoStopReconciler(true, 60, func(c IdleCandidate) error {
		attempts++
		if c.Name == "boom" {
			return errors.New("compose down failed")
		}
		return nil
	}, nil)
	st := &NodeStatus{Services: []ServiceInfo{
		{Name: "boom", Status: ServiceStatusRunning, IdleState: idlePtr(true, 999)},
		{Name: "ok", Status: ServiceStatusRunning, IdleState: idlePtr(true, 999)},
	}}
	n := r.Reconcile(st)
	if attempts != 2 {
		t.Errorf("expected 2 stop attempts, got %d", attempts)
	}
	if n != 1 {
		t.Errorf("expected 1 successful stop (ok), got %d", n)
	}
}

func TestZeroThresholdClampedToDefault(t *testing.T) {
	t.Setenv("SERVICE_IDLE_THRESHOLD_SECONDS", "")
	r := NewAutoStopReconciler(true, 0, func(IdleCandidate) error { return nil }, nil)
	if r.thresholdSeconds != int64(DefaultIdleThresholdSeconds) {
		t.Errorf("zero threshold clamped to %d, want default %d", r.thresholdSeconds, DefaultIdleThresholdSeconds)
	}
}

func TestIdleAge(t *testing.T) {
	if idleAge(nil) != 0 {
		t.Error("nil idle age should be 0")
	}
	if got := idleAge(idlePtr(true, 90)); got != 90*time.Second {
		t.Errorf("idleAge = %v, want 90s", got)
	}
}
