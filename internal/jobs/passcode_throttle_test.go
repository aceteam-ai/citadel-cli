package jobs

import (
	"encoding/json"
	"errors"
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
	shell := ShellGate{Enabled: func() bool { return true }, State: func() (PasscodeState, error) { return PasscodeState{Set: true, Check: locked}, nil }, Throttle: th}
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

func TestShellGateStateReadErrorIsUnavailableNotUnset(t *testing.T) {
	g := ShellGate{Enabled: func() bool { return true }, State: func() (PasscodeState, error) {
		return PasscodeState{}, errors.New("read permissions: permission denied")
	}}
	if got := ShellGateReason(g, throttlePin); got != ReasonPasscodeUnavailable {
		t.Fatalf("unreadable shell passcode state: %q, want passcode_unavailable", got)
	}
	g.State = func() (PasscodeState, error) { return PasscodeState{Set: false}, nil }
	if got := ShellGateReason(g, throttlePin); got != ReasonPasscodeNotSet {
		t.Fatalf("no passcode set: %q, want passcode_not_set", got)
	}
}

func TestPasscodeThrottlePanicReleasesSlotWithoutFailure(t *testing.T) {
	th, _ := newTestThrottle()
	panicValue := &struct{ message string }{"owned test panic"}
	for i := 0; i < 2*th.MaxFailures; i++ {
		func() {
			defer func() {
				if got := recover(); got != panicValue {
					t.Errorf("panic changed or swallowed: %v", got)
				}
			}()
			checkPasscode(th, func(string) PasscodeVerdict { panic(panicValue) }, throttlePin)
			t.Error("panicking verifier returned")
		}()
		if th.pending != 0 || len(th.failures) != 0 || th.Throttled() {
			t.Fatalf("panic leaked or counted: pending=%d failures=%d", th.pending, len(th.failures))
		}
	}
	if got := checkPasscode(th, BoolCheck(pinVerify), throttlePin); got != "" {
		t.Fatalf("correct check after recovered panics refused: %q", got)
	}
	if got := checkPasscode(th, BoolCheck(pinVerify), "wrong"); got != ReasonPasscodeInvalid || len(th.failures) != 1 || th.pending != 0 {
		t.Fatalf("returned mismatch not counted exactly once: reason=%q failures=%d pending=%d", got, len(th.failures), th.pending)
	}
}

func TestPasscodeThrottleConcurrentCorrectChecksAreBusyNotIncorrect(t *testing.T) {
	th, _ := newTestThrottle()
	started := make(chan struct{}, th.MaxFailures+1)
	release := make(chan struct{})
	results := make(chan string, th.MaxFailures+1)
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	var wg sync.WaitGroup
	defer func() { unblock(); wg.Wait() }()
	check := func(string) PasscodeVerdict {
		started <- struct{}{}
		<-release
		return PasscodeMatch
	}
	startCheck := func() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- checkPasscode(th, check, throttlePin)
		}()
	}
	for i := 0; i < th.MaxFailures; i++ {
		startCheck()
	}
	for i := 0; i < th.MaxFailures; i++ {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("reserved checks did not enter the owned verifier")
		}
	}
	startCheck()
	select {
	case reason := <-results:
		if reason != ReasonPasscodeBusy {
			t.Fatalf("concurrent correct overflow: %q, want retryable busy", reason)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("busy check blocked instead of returning")
	}
	th.mu.Lock()
	pending, failures, cooldown := th.pending, len(th.failures), th.lockedUntil
	th.mu.Unlock()
	if pending != th.MaxFailures || failures != 0 || !cooldown.IsZero() {
		t.Fatalf("busy changed accounting: pending=%d failures=%d cooldown=%v", pending, failures, cooldown)
	}
	unblock()
	wg.Wait()
	for i := 0; i < th.MaxFailures; i++ {
		if reason := <-results; reason != "" {
			t.Errorf("correct admitted check refused: %q", reason)
		}
	}
	if th.pending != 0 || len(th.failures) != 0 || th.Throttled() {
		t.Fatal("correct burst counted as failures or leaked slots")
	}
	if reason := checkPasscode(th, BoolCheck(pinVerify), throttlePin); reason != "" {
		t.Fatalf("retry after busy refused: %q", reason)
	}
}

func TestPasscodeBusyMessagesPreserveReasonAcrossSurfaces(t *testing.T) {
	th, _ := newTestThrottle()
	for i := 0; i < th.MaxFailures; i++ {
		if reason := th.reserve(); reason != "" {
			t.Fatal(reason)
		}
		defer th.release(false)
	}
	load := func() (PasscodeState, error) {
		return PasscodeState{Set: true, Check: func(string) PasscodeVerdict {
			t.Error("busy gate called verifier")
			return PasscodeMatch
		}}, nil
	}
	if reason := DesktopGateReason(DesktopGate{Load: load, Throttle: th}, throttlePin); reason != ReasonPasscodeBusy {
		t.Errorf("desktop gate lost busy: %q", reason)
	}
	if reason := ShellGateReason(ShellGate{Enabled: func() bool { return true }, State: load, Throttle: th}, throttlePin); reason != ReasonPasscodeBusy {
		t.Errorf("shell/workflow gate lost busy: %q", reason)
	}
	for surface, refusal := range map[string]*ShellRefusal{
		"shell":    {Reason: ReasonPasscodeBusy, Message: shellCommandRefusalMessage(ReasonPasscodeBusy)},
		"workflow": WorkflowShellRefusal(ReasonPasscodeBusy),
		"desktop":  DesktopRefusal(ReasonPasscodeBusy),
	} {
		var wire struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal([]byte(refusal.Error()), &wire); err != nil {
			t.Fatal(err)
		}
		if wire.Reason != ReasonPasscodeBusy || wire.Message != msgPasscodeBusy || !strings.Contains(wire.Message, "retry") || strings.Contains(wire.Message, "incorrect") {
			t.Errorf("%s busy became incorrect/invalid refusal: %+v", surface, wire)
		}
	}
}

func TestPasscodeBusyDoesNotMaskActualCooldown(t *testing.T) {
	th, _ := newTestThrottle()
	for i := 0; i < th.MaxFailures; i++ {
		checkPasscode(th, func(string) PasscodeVerdict { return PasscodeMismatch }, "wrong")
	}
	if reason := checkPasscode(th, func(string) PasscodeVerdict {
		t.Error("cooldown reached verifier")
		return PasscodeMatch
	}, throttlePin); reason != ReasonPasscodeThrottled {
		t.Fatalf("real failure cooldown changed to %q", reason)
	}
}
