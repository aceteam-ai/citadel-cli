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
	DefaultPasscodeMaxFailures   = 5
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
// through one throttle are serialized, so concurrent jobs cannot overshoot
// the limit. State is in memory only, so a worker restart resets it.
//
// A nil *PasscodeThrottle never throttles.
type PasscodeThrottle struct {
	MaxFailures int
	Window      time.Duration
	Cooldown    time.Duration

	mu          sync.Mutex
	failures    []time.Time
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

// recordFailureLocked counts one wrong passcode and starts a cooldown once the
// window holds MaxFailures of them.
func (t *PasscodeThrottle) recordFailureLocked() {
	now := t.clock()
	cutoff := now.Add(-t.Window)
	kept := t.failures[:0]
	for _, f := range t.failures {
		if f.After(cutoff) {
			kept = append(kept, f)
		}
	}
	t.failures = append(kept, now)
	if t.MaxFailures > 0 && len(t.failures) >= t.MaxFailures {
		t.lockedUntil = now.Add(t.Cooldown)
		t.failures = t.failures[:0]
	}
}

// checkPasscode applies the throttle around one passcode check and returns the
// refusal reason, or "" on a match:
//   - ReasonPasscodeThrottled during a cooldown, without calling check;
//   - ReasonPasscodeLocked or ReasonPasscodeUnavailable when the check could
//     not run, not counted;
//   - ReasonPasscodeInvalid for a wrong passcode, counted only when a
//     non-empty passcode was presented.
//
// A nil check refuses. With a non-nil throttle the whole decision, including
// check, runs under the throttle's lock.
func checkPasscode(t *PasscodeThrottle, check func(string) PasscodeVerdict, presented string) string {
	if t != nil {
		t.mu.Lock()
		defer t.mu.Unlock()
		if t.throttledLocked() {
			return ReasonPasscodeThrottled
		}
	}
	if check == nil {
		return ReasonPasscodeInvalid
	}
	switch check(presented) {
	case PasscodeMatch:
		return ""
	case PasscodeLockedOut:
		return ReasonPasscodeLocked
	case PasscodeUnavailable:
		return ReasonPasscodeUnavailable
	default:
		// An absent passcode is a caller without a credential, not a guess.
		if t != nil && presented != "" {
			t.recordFailureLocked()
		}
		return ReasonPasscodeInvalid
	}
}
