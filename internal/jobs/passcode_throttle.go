package jobs

import (
	"sync"
	"time"
)

// ReasonPasscodeThrottled means too many wrong passcodes were presented to this
// node recently, so passcode-gated jobs are refused until the cooldown ends,
// even with the correct passcode.
const ReasonPasscodeThrottled = "passcode_throttled"

const msgPasscodeThrottled = "too many incorrect node passcode attempts; passcode-gated jobs on this node are paused briefly, retry after the cooldown"

// ReasonPasscodeBusy means checks already in flight have reserved the remaining
// failure budget. The presented passcode has not been checked; retry later.
// This is not a wrong passcode and must not revoke a platform grant window.
const ReasonPasscodeBusy = "passcode_busy"

const msgPasscodeBusy = "node passcode checks are busy; this passcode was not checked, retry shortly"

// ReasonPasscodeLocked means the node's master-PIN vault is in its own lockout
// window, so a passcode could not be checked. It says nothing about whether the
// presented passcode was right.
const ReasonPasscodeLocked = "passcode_locked"

const msgPasscodeLocked = "the node master PIN is locked after too many failed attempts; passcode-gated jobs are refused until the lockout ends"

// PasscodeVerdict is the outcome of checking one presented passcode.
type PasscodeVerdict int

const (
	// PasscodeMismatch: the passcode is absent or wrong.
	PasscodeMismatch PasscodeVerdict = iota
	// PasscodeMatch: the passcode is correct.
	PasscodeMatch
	// PasscodeLockedOut: the check could not run because the node's master-PIN
	// vault is locked out. Not a wrong passcode.
	PasscodeLockedOut
	// PasscodeUnavailable: the check could not run because the node's passcode
	// state could not be read (an I/O or stat error). Not a wrong passcode.
	PasscodeUnavailable
)

// BoolCheck adapts a plain true/false verifier, which has no lockout signal.
func BoolCheck(verify func(pin string) bool) func(pin string) PasscodeVerdict {
	if verify == nil {
		return nil
	}
	return func(pin string) PasscodeVerdict {
		if verify(pin) {
			return PasscodeMatch
		}
		return PasscodeMismatch
	}
}

// Default throttle bounds: DefaultPasscodeMaxFailures wrong passcodes within
// DefaultPasscodeFailureWindow start a DefaultPasscodeCooldown.
const (
	DefaultPasscodeMaxFailures   = 10
	DefaultPasscodeFailureWindow = time.Minute
	DefaultPasscodeCooldown      = 5 * time.Minute
)

// PasscodeThrottle limits wrong node-passcode attempts across every
// passcode-gated job surface on a node (SHELL_COMMAND, the WORKFLOW_RUN Shell
// node, the desktop handlers). One instance is shared by all of them, so
// failures on one surface count toward the others.
//
// After MaxFailures wrong passcodes within Window, every passcode-gated job is
// refused with ReasonPasscodeThrottled for Cooldown, without checking the
// passcode. Only a presented, non-empty passcode that is wrong counts: an
// absent passcode, a vault lockout and an unreadable passcode state do not.
// Failures expire only with time; a correct passcode does not clear them, so
// wrong guesses interleaved with correct ones still reach the limit. Checks
// through one throttle cannot overshoot the limit: a check reserves a slot
// under the lock, runs unlocked, and is recorded under the lock. The window's
// failures plus checks in flight never exceed MaxFailures. Capacity reserved
// by in-flight checks is reported
// as ReasonPasscodeBusy, not as incorrect attempts. A check that panics releases
// its slot without counting a failure. State is in memory only, so a worker
// restart resets it.
//
// A nil *PasscodeThrottle never throttles.
type PasscodeThrottle struct {
	MaxFailures int
	Window      time.Duration
	Cooldown    time.Duration

	mu          sync.Mutex
	failures    []time.Time
	pending     int
	lockedUntil time.Time
	now         func() time.Time
}

// NewPasscodeThrottle returns a throttle with the default bounds.
func NewPasscodeThrottle() *PasscodeThrottle {
	return &PasscodeThrottle{
		MaxFailures: DefaultPasscodeMaxFailures,
		Window:      DefaultPasscodeFailureWindow,
		Cooldown:    DefaultPasscodeCooldown,
	}
}

func (t *PasscodeThrottle) clock() time.Time {
	if t.now != nil {
		return t.now()
	}
	return time.Now()
}

// Throttled reports whether a cooldown is in effect.
func (t *PasscodeThrottle) Throttled() bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.throttledLocked()
}

func (t *PasscodeThrottle) throttledLocked() bool {
	return t.clock().Before(t.lockedUntil)
}

// pruneLocked drops failures older than the window.
func (t *PasscodeThrottle) pruneLocked(now time.Time) {
	cutoff := now.Add(-t.Window)
	kept := t.failures[:0]
	for _, f := range t.failures {
		if f.After(cutoff) {
			kept = append(kept, f)
		}
	}
	t.failures = kept
}

// reserve claims one check slot and returns "", or a distinct refusal for an
// actual failure cooldown versus capacity reserved by checks still in flight.
func (t *PasscodeThrottle) reserve() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.clock()
	if now.Before(t.lockedUntil) {
		return ReasonPasscodeThrottled
	}
	t.pruneLocked(now)
	if t.MaxFailures > 0 && len(t.failures)+t.pending >= t.MaxFailures {
		if t.pending > 0 {
			return ReasonPasscodeBusy
		}
		return ReasonPasscodeThrottled
	}
	t.pending++
	return ""
}

// release ends a reserved check, counting it when it was a wrong passcode and
// starting a cooldown once the window holds MaxFailures of them.
func (t *PasscodeThrottle) release(failed bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pending--
	if !failed {
		return
	}
	now := t.clock()
	t.pruneLocked(now)
	t.failures = append(t.failures, now)
	if t.MaxFailures > 0 && len(t.failures) >= t.MaxFailures {
		t.lockedUntil = now.Add(t.Cooldown)
		t.failures = t.failures[:0]
	}
}

// checkPasscode applies the throttle around one passcode check and returns the
// refusal reason, or "" on a match:
//   - ReasonPasscodeThrottled during a cooldown or when recorded failures have
//     used the budget, without calling check;
//   - ReasonPasscodeBusy when checks in flight have reserved the remaining
//     budget, without calling check (not a wrong passcode);
//   - ReasonPasscodeLocked or ReasonPasscodeUnavailable when the check could
//     not run, not counted;
//   - ReasonPasscodeInvalid for a wrong passcode, counted only when a
//     non-empty passcode was presented.
//
// A nil check refuses. An absent passcode is checked without the throttle (it
// is never counted, so it needs no slot). The check itself, which may be a KDF
// or read the disk, runs outside the throttle's lock.
func checkPasscode(t *PasscodeThrottle, check func(string) PasscodeVerdict, presented string) string {
	if check == nil {
		return ReasonPasscodeInvalid
	}
	failed := false
	if t != nil && presented != "" {
		if reason := t.reserve(); reason != "" {
			return reason
		}
		// A panic is propagated, but its reservation is not retained and it is
		// not recorded as a mismatch. Only a returned mismatch counts.
		defer func() { t.release(failed) }()
	} else if t.Throttled() {
		return ReasonPasscodeThrottled
	}
	verdict := check(presented)
	failed = verdict == PasscodeMismatch
	return verdictReason(verdict)
}

func verdictReason(v PasscodeVerdict) string {
	switch v {
	case PasscodeMatch:
		return ""
	case PasscodeLockedOut:
		return ReasonPasscodeLocked
	case PasscodeUnavailable:
		return ReasonPasscodeUnavailable
	default:
		return ReasonPasscodeInvalid
	}
}
