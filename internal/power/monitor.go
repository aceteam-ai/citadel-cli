package power

import (
	"context"
	"time"
)

// DefaultPollInterval is how often the monitor re-evaluates the power source.
const DefaultPollInterval = 30 * time.Second

// Monitor drives an Inhibitor based on the opt-in flag and the live power
// source. It asserts sleep inhibition only while ShouldInhibit is true and
// releases it on transitions to battery and on context cancellation, giving
// "release on switch to battery" and "release on exit" for free.
type Monitor struct {
	enabled   bool
	interval  time.Duration
	detect    func() Source
	inhibitor Inhibitor

	// onTransition is an optional hook fired whenever the asserted state flips,
	// used by callers (the TUI / work loop) to log "Keep-awake: on (AC)".
	onTransition func(active bool, src Source)
}

// MonitorOption configures a Monitor.
type MonitorOption func(*Monitor)

// WithInterval overrides the poll interval (mainly for tests).
func WithInterval(d time.Duration) MonitorOption {
	return func(m *Monitor) {
		if d > 0 {
			m.interval = d
		}
	}
}

// WithDetector overrides the power-source detector (mainly for tests).
func WithDetector(f func() Source) MonitorOption {
	return func(m *Monitor) {
		if f != nil {
			m.detect = f
		}
	}
}

// WithInhibitor overrides the inhibitor (mainly for tests, to stub subprocess
// spawning).
func WithInhibitor(i Inhibitor) MonitorOption {
	return func(m *Monitor) {
		if i != nil {
			m.inhibitor = i
		}
	}
}

// WithTransitionHook registers a callback invoked on each asserted-state flip.
func WithTransitionHook(f func(active bool, src Source)) MonitorOption {
	return func(m *Monitor) { m.onTransition = f }
}

// NewMonitor builds a Monitor. When enabled is false the monitor does nothing
// (and Run returns immediately), so callers can construct it unconditionally.
func NewMonitor(enabled bool, opts ...MonitorOption) *Monitor {
	m := &Monitor{
		enabled:   enabled,
		interval:  DefaultPollInterval,
		detect:    DetectPowerSource,
		inhibitor: NewInhibitor(),
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Run blocks until ctx is cancelled, re-evaluating the power source every
// interval and asserting/releasing inhibition accordingly. It always releases
// the assertion before returning. When the monitor is disabled, it returns
// immediately without touching the inhibitor.
func (m *Monitor) Run(ctx context.Context) {
	if !m.enabled {
		return
	}

	// Ensure we never leave an assertion held when Run exits.
	defer func() { _ = m.inhibitor.Stop() }()

	m.reconcile()

	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.reconcile()
		}
	}
}

// reconcile evaluates the gating decision once and drives the inhibitor to
// match. Exposed package-internally so tests can step it deterministically.
func (m *Monitor) reconcile() {
	src := m.detect()
	want := ShouldInhibit(m.enabled, src)
	have := m.inhibitor.Active()

	switch {
	case want && !have:
		if err := m.inhibitor.Start(); err == nil && m.onTransition != nil {
			m.onTransition(true, src)
		}
	case !want && have:
		_ = m.inhibitor.Stop()
		if m.onTransition != nil {
			m.onTransition(false, src)
		}
	}
}

// Stop releases any held assertion immediately. It is safe to call when the
// monitor is disabled, never started, or already stopped (the underlying
// inhibitor is idempotent). Callers should `defer monitor.Stop()` in the work
// loop so the assertion is released synchronously before the process exits,
// rather than relying on the Run goroutine's deferred cleanup (which races
// process exit).
func (m *Monitor) Stop() {
	_ = m.inhibitor.Stop()
}

// Status reports the current asserted state and last-detected source for the
// TUI ("Keep-awake: on (AC)"). It is a live read and does not change state.
func (m *Monitor) Status() (enabled, active bool, src Source) {
	return m.enabled, m.inhibitor.Active(), m.detect()
}
