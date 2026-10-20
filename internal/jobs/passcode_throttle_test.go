package jobs

import (
	"strings"
	"sync"
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

func TestPasscodeThrottleWindowExpiresFailures(t *testing.T) {
	th, clock := newTestThrottle()
	g := DesktopGate{Load: setState(pinVerify), Throttle: th}
	for i := 0; i < DefaultPasscodeMaxFailures-1; i++ {
		DesktopGateReason(g, "wrong")
	}
	clock.advance(DefaultPasscodeFailureWindow + time.Second)
	DesktopGateReason(g, "wrong")
	if th.Throttled() {
		t.Fatal("failures outside the window must not start a cooldown")
	}
}

func TestPasscodeThrottleCorrectPasscodeDoesNotClearFailures(t *testing.T) {
	th, _ := newTestThrottle()
	g := DesktopGate{Load: setState(pinVerify), Throttle: th}
	for i := 0; i < DefaultPasscodeMaxFailures-1; i++ {
		DesktopGateReason(g, "wrong")
		if got := DesktopGateReason(g, throttlePin); got != "" {
			t.Fatalf("correct passcode before the limit: %q", got)
		}
	}
	if got := DesktopGateReason(g, "wrong"); got != ReasonPasscodeInvalid {
		t.Fatalf("limit-reaching guess: %q", got)
	}
	if !th.Throttled() {
		t.Fatal("wrong guesses interleaved with correct passcodes must still reach the limit")
	}
}

func TestPasscodeThrottleDoesNotCountAbsentOrUncheckablePasscodes(t *testing.T) {
	th, _ := newTestThrottle()
	counting := func(string) bool { return false } // rejects everything, like a real verifier
	shell := ShellGate{Enabled: func() bool { return true }, HasPasscode: func() bool { return true }, VerifyPasscode: counting, Throttle: th}
	desktop := DesktopGate{Load: setState(counting), Throttle: th}
	for i := 0; i < 3*DefaultPasscodeMaxFailures; i++ {
		if got := ShellGateReason(shell, ""); got != ReasonPasscodeInvalid {
			t.Fatalf("absent shell passcode: %q", got)
		}
		if got := DesktopGateReason(desktop, ""); got != ReasonPasscodeInvalid {
			t.Fatalf("absent desktop passcode: %q", got)
		}
	}
	unavailable := DesktopGate{Load: func() (PasscodeState, error) {
		return PasscodeState{Set: true, Check: func(string) PasscodeVerdict { return PasscodeUnavailable }}, nil
	}, Throttle: th}
	for i := 0; i < 3*DefaultPasscodeMaxFailures; i++ {
		if got := DesktopGateReason(unavailable, throttlePin); got != ReasonPasscodeUnavailable {
			t.Fatalf("uncheckable passcode: %q", got)
		}
	}
	if th.Throttled() {
		t.Fatal("absent and uncheckable passcodes must not start a cooldown")
	}
}

func TestPasscodeThrottleConcurrentChecksCannotOvershoot(t *testing.T) {
	th, _ := newTestThrottle()
	var mu sync.Mutex
	checked := 0
	slow := func(string) bool {
		mu.Lock()
		checked++
		mu.Unlock()
		time.Sleep(2 * time.Millisecond)
		return false
	}
	g := DesktopGate{Load: setState(slow), Throttle: th}
	var wg sync.WaitGroup
	for i := 0; i < 4*DefaultPasscodeMaxFailures; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); DesktopGateReason(g, "wrong") }()
	}
	wg.Wait()
	if checked != DefaultPasscodeMaxFailures {
		t.Fatalf("concurrent wrong guesses checked = %d, want exactly %d before the cooldown", checked, DefaultPasscodeMaxFailures)
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
