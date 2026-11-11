package config

import (
	"errors"
	"testing"
)

// fakeLimiter records how often the legacy gate consulted it and what the check
// returned. locked makes every attempt refuse without running the check.
type fakeLimiter struct {
	attempts, failures, successes int
	locked                        bool
}

var errFakeLocked = errors.New("fake: locked out")
var errFakeWrong = errors.New("fake: wrong")

func (f *fakeLimiter) Attempt(check func() bool) error {
	f.attempts++
	if f.locked {
		return errFakeLocked
	}
	if !check() {
		f.failures++
		return errFakeWrong
	}
	f.successes++
	return nil
}

func useLegacyLimiter(t *testing.T, lim PasscodeAttemptLimiter) {
	t.Helper()
	resetVaultHooks(t)
	orig := LegacyPasscodeLockout
	t.Cleanup(func() { LegacyPasscodeLockout = orig })
	VaultConfigured = func() bool { return false }
	VaultVerify = func(string) (bool, bool) { return false, false }
	LegacyPasscodeLockout = func() PasscodeAttemptLimiter { return lim }
}

// TestVerifyPasscodeLegacyPathUsesLockout pins #1398: with no master PIN, the
// shared gate entry point consults the node-wide limiter exactly once per
// presented passcode, and an active lockout denies even the correct passcode.
func TestVerifyPasscodeLegacyPathUsesLockout(t *testing.T) {
	lim := &fakeLimiter{}
	useLegacyLimiter(t, lim)
	p := &Permissions{}
	if err := p.SetPasscode("1234"); err != nil {
		t.Fatal(err)
	}
	if p.VerifyPasscode("wrong") {
		t.Fatal("wrong passcode accepted")
	}
	if !p.VerifyPasscode("1234") {
		t.Fatal("correct passcode rejected")
	}
	if lim.attempts != 2 || lim.failures != 1 || lim.successes != 1 {
		t.Fatalf("one limiter attempt per presented passcode: %+v", lim)
	}
	lim.locked = true
	if p.VerifyPasscode("1234") {
		t.Fatal("correct passcode accepted during lockout")
	}
}

// TestLegacyPasscodeEmptyOrUnsetNeverReachesLimiter: an empty pin or an unset
// hash is rejected for free, so unauthenticated probes cannot lock the node.
func TestLegacyPasscodeEmptyOrUnsetNeverReachesLimiter(t *testing.T) {
	lim := &fakeLimiter{}
	useLegacyLimiter(t, lim)
	unset := &Permissions{}
	if unset.VerifyPasscode("1234") || unset.VerifyLegacyPasscode("1234") {
		t.Fatal("unset hash verified")
	}
	set := &Permissions{}
	if err := set.SetPasscode("1234"); err != nil {
		t.Fatal(err)
	}
	if set.VerifyPasscode("") || set.VerifyLegacyPasscode("") {
		t.Fatal("empty passcode verified")
	}
	if err := set.CheckLegacyPasscode("", lim); !errors.Is(err, ErrPasscodeMismatch) {
		t.Fatalf("empty passcode: %v", err)
	}
	if lim.attempts != 0 {
		t.Fatalf("empty/unset checks consulted the limiter %d times", lim.attempts)
	}
}

// TestCheckLegacyPasscodePreservesLimiterReason: the limiter's own error (for
// example a lockout) is returned unchanged, so callers can tell it from a
// mismatch.
func TestCheckLegacyPasscodePreservesLimiterReason(t *testing.T) {
	p := &Permissions{}
	if err := p.SetPasscode("1234"); err != nil {
		t.Fatal(err)
	}
	if err := p.CheckLegacyPasscode("1234", &fakeLimiter{locked: true}); !errors.Is(err, errFakeLocked) {
		t.Fatalf("lockout reason lost: %v", err)
	}
	if err := p.CheckLegacyPasscode("nope", &fakeLimiter{}); !errors.Is(err, errFakeWrong) {
		t.Fatalf("mismatch reason lost: %v", err)
	}
	if err := p.CheckLegacyPasscode("nope", nil); !errors.Is(err, ErrPasscodeMismatch) {
		t.Fatalf("unlimited mismatch: %v", err)
	}
	if err := p.CheckLegacyPasscode("1234", nil); err != nil {
		t.Fatalf("unlimited match: %v", err)
	}
}

// TestVerifyPasscodeVaultHandledSkipsLegacyLimiter: when the vault handles the
// check, the legacy limiter is not consulted, so one presented passcode is
// counted once (by the vault).
func TestVerifyPasscodeVaultHandledSkipsLegacyLimiter(t *testing.T) {
	lim := &fakeLimiter{}
	useLegacyLimiter(t, lim)
	VaultConfigured = func() bool { return true }
	VaultVerify = func(pin string) (bool, bool) { return pin == "masterpin", true }
	p := &Permissions{PasscodeHash: "$2a$fake"}
	_ = p.VerifyPasscode("wrong")
	_ = p.VerifyPasscode("masterpin")
	if lim.attempts != 0 {
		t.Fatalf("legacy limiter consulted %d times on a vault-handled check", lim.attempts)
	}
}
