package power

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestShouldInhibit(t *testing.T) {
	cases := []struct {
		name    string
		enabled bool
		src     Source
		want    bool
	}{
		{"enabled+AC inhibits", true, SourceAC, true},
		{"enabled+battery does not", true, SourceBattery, false},
		{"enabled+unknown does not", true, SourceUnknown, false},
		{"disabled+AC does not", false, SourceAC, false},
		{"disabled+battery does not", false, SourceBattery, false},
		{"disabled+unknown does not", false, SourceUnknown, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ShouldInhibit(c.enabled, c.src); got != c.want {
				t.Errorf("ShouldInhibit(%v, %v) = %v, want %v", c.enabled, c.src, got, c.want)
			}
		})
	}
}

func TestSourceString(t *testing.T) {
	cases := map[Source]string{
		SourceAC:      "AC",
		SourceBattery: "battery",
		SourceUnknown: "unknown",
	}
	for src, want := range cases {
		if got := src.String(); got != want {
			t.Errorf("Source(%d).String() = %q, want %q", src, got, want)
		}
	}
}

// stubInhibitor records Start/Stop calls without spawning any process.
type stubInhibitor struct {
	mu       sync.Mutex
	active   bool
	starts   int
	stops    int
	startErr error
}

func (s *stubInhibitor) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.starts++
	if s.startErr != nil {
		return s.startErr
	}
	s.active = true
	return nil
}

func (s *stubInhibitor) Stop() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stops++
	s.active = false
	return nil
}

func (s *stubInhibitor) Active() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active
}

func (s *stubInhibitor) counts() (starts, stops int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.starts, s.stops
}

func TestMonitor_DisabledDoesNothing(t *testing.T) {
	stub := &stubInhibitor{}
	m := NewMonitor(false,
		WithInhibitor(stub),
		WithDetector(func() Source { return SourceAC }),
	)
	m.Run(context.Background()) // returns immediately when disabled
	if starts, stops := stub.counts(); starts != 0 || stops != 0 {
		t.Errorf("disabled monitor touched inhibitor: starts=%d stops=%d", starts, stops)
	}
}

func TestMonitor_AssertsOnAC(t *testing.T) {
	stub := &stubInhibitor{}
	m := NewMonitor(true,
		WithInhibitor(stub),
		WithDetector(func() Source { return SourceAC }),
	)
	m.reconcile()
	if !stub.Active() {
		t.Error("expected inhibitor active on AC + enabled")
	}
	if starts, _ := stub.counts(); starts != 1 {
		t.Errorf("expected 1 start, got %d", starts)
	}
	// Idempotent: a second reconcile on AC should not re-start.
	m.reconcile()
	if starts, _ := stub.counts(); starts != 1 {
		t.Errorf("expected start to remain 1 (idempotent), got %d", starts)
	}
}

func TestMonitor_ReleasesOnBatteryTransition(t *testing.T) {
	src := SourceAC
	stub := &stubInhibitor{}
	m := NewMonitor(true,
		WithInhibitor(stub),
		WithDetector(func() Source { return src }),
	)
	m.reconcile()
	if !stub.Active() {
		t.Fatal("expected active on AC")
	}
	src = SourceBattery
	m.reconcile()
	if stub.Active() {
		t.Error("expected released after switching to battery")
	}
	if _, stops := stub.counts(); stops != 1 {
		t.Errorf("expected 1 stop on battery transition, got %d", stops)
	}
}

func TestMonitor_StartErrorLeavesInactive(t *testing.T) {
	stub := &stubInhibitor{startErr: errors.New("boom")}
	transitions := 0
	m := NewMonitor(true,
		WithInhibitor(stub),
		WithDetector(func() Source { return SourceAC }),
		WithTransitionHook(func(bool, Source) { transitions++ }),
	)
	m.reconcile()
	if stub.Active() {
		t.Error("inhibitor should not be active when Start errors")
	}
	if transitions != 0 {
		t.Errorf("no transition hook should fire when Start errors, got %d", transitions)
	}
}

func TestMonitor_ReleasesOnContextCancel(t *testing.T) {
	stub := &stubInhibitor{}
	m := NewMonitor(true,
		WithInhibitor(stub),
		WithDetector(func() Source { return SourceAC }),
		WithInterval(time.Hour),
	)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.Run(ctx)
		close(done)
	}()

	// Wait for the initial reconcile to assert.
	deadline := time.After(time.Second)
	for !stub.Active() {
		select {
		case <-deadline:
			t.Fatal("inhibitor never became active")
		case <-time.After(time.Millisecond):
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if stub.Active() {
		t.Error("inhibitor should be released after Run returns")
	}
}

func TestMonitor_Status(t *testing.T) {
	stub := &stubInhibitor{}
	m := NewMonitor(true,
		WithInhibitor(stub),
		WithDetector(func() Source { return SourceAC }),
	)
	enabled, active, src := m.Status()
	if !enabled || active || src != SourceAC {
		t.Errorf("Status before reconcile = (%v,%v,%v), want (true,false,AC)", enabled, active, src)
	}
	m.reconcile()
	_, active, _ = m.Status()
	if !active {
		t.Error("Status should report active after reconcile on AC")
	}
}
