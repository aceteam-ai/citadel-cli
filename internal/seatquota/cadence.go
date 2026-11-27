package seatquota

import (
	"sync"
	"time"
)

// ReadCadence is an in-memory, report-only single-flight model for one supplied
// seat/provider binding. It performs no reads or timers. A future collector must
// own one instance per trusted seat, persist attempts before I/O, prove idle
// state and retain identity/isolation checks. This is not execution admission.
// Do not copy a ReadCadence after use.
type ReadCadence struct {
	mu                           sync.Mutex
	seat, provider               string
	lastAttempt, lastObservation time.Time
	lastClock                    time.Time
	active                       *ReadAttempt
}

// ReadAttempt identifies one local model transition, not a credential or vendor
// request. Only its issuing ReadCadence can complete it, exactly once.
type ReadAttempt struct {
	owner   *ReadCadence
	started time.Time
}

type ReadSnapshot struct {
	SeatRef         string
	Provider        string
	LastAttempt     time.Time
	LastObservation time.Time
	InFlight        bool
}

func NewReadCadence(seat, provider string) (*ReadCadence, error) {
	if !seatPattern.MatchString(seat) {
		return nil, ErrInput
	}
	switch provider {
	case "codex", "muse", "claude", "copilot":
	default:
		return nil, ErrInput
	}
	return &ReadCadence{seat: seat, provider: provider}, nil
}

// Begin records a due attempt before returning it. Concurrent calls cannot
// reserve overlapping reads, and a failed/canceled read never erases its floor.
// Turn-only providers never reserve idle or on-demand reads. Jitter and clocks
// are caller inputs, not evidence that a vendor read is safe or authorized.
func (c *ReadCadence) Begin(now time.Time, onDemand bool, jitter time.Duration) (*ReadAttempt, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.validClock(now) || jitter < -time.Minute || jitter > time.Minute {
		return nil, false
	}
	c.lastClock = now
	if (c.provider != "codex" && c.provider != "muse") || c.active != nil || !Due(c.lastAttempt, c.lastObservation, now, onDemand, jitter) {
		return nil, false
	}
	c.lastAttempt = now.UTC()
	c.active = &ReadAttempt{owner: c, started: now.UTC()}
	return c.active, true
}

// Complete closes only the matching attempt. A zero observed time represents
// failure/cancellation, preserving the previous observation and attempt floor.
// A successful supplied receipt must fall within the attempt; late completion
// cannot replace a newer push. Invalid completions leave the attempt held.
func (c *ReadCadence) Complete(attempt *ReadAttempt, observed, now time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if attempt == nil || attempt.owner != c || c.active != attempt || !c.validClock(now) {
		return ErrInput
	}
	if !observed.IsZero() && (!validReceiptTime(observed) || observed.Before(attempt.started) || observed.After(now)) {
		return ErrInput
	}
	c.lastClock = now
	if observed.After(c.lastObservation) {
		c.lastObservation = observed.UTC()
	}
	c.active = nil
	return nil
}

// Observe records only the newest supplied receipt time, for a push or a real
// turn side effect. It neither clears an active read nor restores allowance.
// The caller remains responsible for validating the actual observation.
func (c *ReadCadence) Observe(observed, now time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.validClock(now) || !validReceiptTime(observed) || observed.After(now) {
		return ErrInput
	}
	c.lastClock = now
	if observed.After(c.lastObservation) {
		c.lastObservation = observed.UTC()
	}
	return nil
}

func (c *ReadCadence) Snapshot() ReadSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return ReadSnapshot{SeatRef: c.seat, Provider: c.provider, LastAttempt: c.lastAttempt, LastObservation: c.lastObservation, InFlight: c.active != nil}
}

func (c *ReadCadence) validClock(now time.Time) bool {
	return seatPattern.MatchString(c.seat) && validReceiptTime(now) && !now.Before(c.lastClock)
}

func validReceiptTime(at time.Time) bool {
	return !at.IsZero() && at.Year() >= 1 && at.Year() <= 9999
}
