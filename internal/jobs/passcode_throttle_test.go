package jobs

import (
	"strings"
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestThrottle() (*PasscodeThrottle, *fakeClock) {
	c := &fakeClock{t: time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)}
	th := NewPasscodeThrottle()
	th.now = c.now
	return th, c
}

const throttlePin = "4821"

func pinVerify(p string) bool { return p == throttlePin }

func setState(verify func(string) bool) func() (PasscodeState, error) {
	return func() (PasscodeState, error) { return PasscodeState{Set: true, Check: BoolCheck(verify)}, nil }
}

func TestPasscodeThrottleCooldownAfterMaxFailures(t *testing.T) {
	th, clock := newTestThrottle()
	shell := ShellGate{Enabled: func() bool { return true }, HasPasscode: func() bool { return true }, VerifyPasscode: pinVerify, Throttle: th}
	for i := 0; i < DefaultPasscodeMaxFailures; i++ {
		if got := ShellGateReason(shell, "wrong"); got != ReasonPasscodeInvalid {
			t.Fatalf("failure %d: reason %q, want passcode_invalid", i+1, got)
		}
	}
	verifyCalled := false
	shell.VerifyPasscode = func(p string) bool { verifyCalled = true; return pinVerify(p) }
	shell.Check = nil
	if got := ShellGateReason(shell, throttlePin); got != ReasonPasscodeThrottled {
		t.Fatalf("correct passcode during cooldown: reason %q, want passcode_throttled", got)
	}
	if verifyCalled {
		t.Fatal("a throttled attempt must not verify the passcode")
	}
	clock.advance(DefaultPasscodeCooldown + time.Second)
	if got := ShellGateReason(shell, throttlePin); got != "" {
		t.Fatalf("after the cooldown the correct passcode must pass, got %q", got)
	}
}

func TestPasscodeThrottleIsSharedAcrossSurfaces(t *testing.T) {
	th, _ := newTestThrottle()
	shell := ShellGate{Enabled: func() bool { return true }, HasPasscode: func() bool { return true }, VerifyPasscode: pinVerify, Throttle: th}
	desktop := DesktopGate{Load: setState(pinVerify), Throttle: th}
	for i := 0; i < DefaultPasscodeMaxFailures; i++ {
		if i%2 == 0 {
			ShellGateReason(shell, "wrong")
		} else {
			DesktopGateReason(desktop, "wrong")
		}
	}
	if got := DesktopGateReason(desktop, throttlePin); got != ReasonPasscodeThrottled {
		t.Fatalf("desktop after mixed failures: %q, want passcode_throttled", got)
	}
	if got := ShellGateReason(shell, throttlePin); got != ReasonPasscodeThrottled {
		t.Fatalf("shell after mixed failures: %q, want passcode_throttled", got)
	}
	if DesktopRefusal(ReasonPasscodeThrottled).Reason != ReasonPasscodeThrottled || WorkflowShellRefusal(ReasonPasscodeThrottled).Reason != ReasonPasscodeThrottled {
		t.Fatal("throttled refusals must carry the distinct reason on every surface")
	}
}

func TestPasscodeThrottleWindowAndSuccessReset(t *testing.T) {
	th, clock := newTestThrottle()
	g := DesktopGate{Load: setState(pinVerify), Throttle: th}
	// Failures older than the window do not count.
	for i := 0; i < DefaultPasscodeMaxFailures-1; i++ {
		DesktopGateReason(g, "wrong")
	}
	clock.advance(DefaultPasscodeFailureWindow + time.Second)
	DesktopGateReason(g, "wrong")
	if th.Throttled() {
		t.Fatal("failures outside the window must not start a cooldown")
	}
	// A correct passcode clears the count.
	th, _ = newTestThrottle()
	g.Throttle = th
	for i := 0; i < DefaultPasscodeMaxFailures-1; i++ {
		DesktopGateReason(g, "wrong")
	}
	if got := DesktopGateReason(g, throttlePin); got != "" {
		t.Fatalf("correct passcode before the limit: %q", got)
	}
	DesktopGateReason(g, "wrong")
	if th.Throttled() {
		t.Fatal("a correct passcode must reset the failure count")
	}
}

func TestPasscodeThrottleNilAndNoPasscodeNode(t *testing.T) {
	var nilThrottle *PasscodeThrottle
	g := DesktopGate{Load: setState(pinVerify), Throttle: nilThrottle}
	for i := 0; i < 2*DefaultPasscodeMaxFailures; i++ {
		DesktopGateReason(g, "wrong")
	}
	if got := DesktopGateReason(g, throttlePin); got != "" {
		t.Fatalf("a nil throttle must never throttle, got %q", got)
	}
	th, _ := newTestThrottle()
	open := DesktopGate{Load: func() (PasscodeState, error) { return PasscodeState{Set: false, Check: BoolCheck(pinVerify)}, nil }, Throttle: th}
	for i := 0; i < 2*DefaultPasscodeMaxFailures; i++ {
		if got := DesktopGateReason(open, "wrong"); got != "" {
			t.Fatalf("a node without a passcode must stay unchanged, got %q", got)
		}
	}
	if th.Throttled() {
		t.Fatal("a node without a passcode must never count failures")
	}
}

func TestPasscodeLockoutIsDistinctAndNotCounted(t *testing.T) {
	th, _ := newTestThrottle()
	locked := func(string) PasscodeVerdict { return PasscodeLockedOut }
	shell := ShellGate{Enabled: func() bool { return true }, HasPasscode: func() bool { return true }, Check: locked, Throttle: th}
	desktop := DesktopGate{Load: func() (PasscodeState, error) { return PasscodeState{Set: true, Check: locked}, nil }, Throttle: th}
	for i := 0; i < 2*DefaultPasscodeMaxFailures; i++ {
		if got := ShellGateReason(shell, throttlePin); got != ReasonPasscodeLocked {
			t.Fatalf("shell during vault lockout: %q, want passcode_locked", got)
		}
		if got := DesktopGateReason(desktop, throttlePin); got != ReasonPasscodeLocked {
			t.Fatalf("desktop during vault lockout: %q, want passcode_locked", got)
		}
	}
	if th.Throttled() {
		t.Fatal("lockout refusals are not wrong passcodes and must not start a throttle cooldown")
	}
	for _, r := range []*ShellRefusal{DesktopRefusal(ReasonPasscodeLocked), WorkflowShellRefusal(ReasonPasscodeLocked)} {
		if r.Reason != ReasonPasscodeLocked || !strings.Contains(r.Message, "locked") {
			t.Fatalf("lockout refusal = %+v", r)
		}
	}
}
