package nodevault

import (
	"errors"
	"testing"
	"time"
)

func TestLockoutAfterMaxAttempts(t *testing.T) {
	v := newTestVault(t)
	mustSet(t, v, "654321")

	// Burn through the free attempts with wrong PINs.
	for i := 0; i < maxFreeAttempts; i++ {
		if err := v.VerifyPIN("000000"); !errors.Is(err, ErrWrongPIN) {
			t.Fatalf("attempt %d: want ErrWrongPIN, got %v", i, err)
		}
	}
	// Now locked out: even the CORRECT PIN is refused.
	err := v.VerifyPIN("654321")
	if !IsLockedOut(err) {
		t.Fatalf("want lockout after %d failures, got %v", maxFreeAttempts, err)
	}
	var lo *ErrLockedOut
	if !errors.As(err, &lo) || lo.Until.Before(time.Now()) {
		t.Fatalf("lockout has no future Until: %v", err)
	}
}

func TestSuccessResetsLockoutCounter(t *testing.T) {
	v := newTestVault(t)
	mustSet(t, v, "654321")

	// A few failures, then a success, then failures again should NOT trip the
	// lock early (the counter reset on success).
	for i := 0; i < maxFreeAttempts-1; i++ {
		_ = v.VerifyPIN("000000")
	}
	if err := v.VerifyPIN("654321"); err != nil {
		t.Fatalf("correct PIN before lockout should pass: %v", err)
	}
	if lo := v.LockoutStatus(); lo.FailedAttempts != 0 {
		t.Fatalf("counter not reset after success: %d", lo.FailedAttempts)
	}
	if err := v.VerifyPIN("000000"); !errors.Is(err, ErrWrongPIN) {
		t.Fatalf("post-reset attempt should be a plain wrong-PIN, got %v", err)
	}
}

// TestLockoutIsCrossInstance proves the counter is on disk, node-wide: a second
// Vault handle over the SAME directory (a different process/surface) sees the
// lockout set by the first.
func TestLockoutIsCrossInstance(t *testing.T) {
	base := t.TempDir()
	v1 := Open(base)
	v1.paramsOverride = &fastParams
	if err := v1.SetPIN("654321", DefaultPolicy(), true); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxFreeAttempts; i++ {
		_ = v1.VerifyPIN("000000")
	}

	v2 := Open(base) // independent handle, same dir
	v2.paramsOverride = &fastParams
	if err := v2.VerifyPIN("654321"); !IsLockedOut(err) {
		t.Fatalf("second handle did not observe cross-process lockout: %v", err)
	}
}

func TestResetLockoutLocalPresence(t *testing.T) {
	v := newTestVault(t)
	mustSet(t, v, "654321")
	for i := 0; i < maxFreeAttempts; i++ {
		_ = v.VerifyPIN("000000")
	}
	if !v.LockoutStatus().LockedOut {
		t.Fatal("expected lockout")
	}
	// Local-presence recovery clears the lock without data loss (no auto-wipe).
	if err := v.ResetLockout(); err != nil {
		t.Fatal(err)
	}
	if err := v.VerifyPIN("654321"); err != nil {
		t.Fatalf("PIN should work after ResetLockout: %v", err)
	}
}

func TestBackoffGrowsAndCaps(t *testing.T) {
	if backoff(maxFreeAttempts) != lockoutBase {
		t.Fatalf("first lockout = %v, want %v", backoff(maxFreeAttempts), lockoutBase)
	}
	if got := backoff(maxFreeAttempts + 1); got != lockoutBase*2 {
		t.Fatalf("second lockout = %v, want %v", got, lockoutBase*2)
	}
	if got := backoff(maxFreeAttempts + 100); got != lockoutCap {
		t.Fatalf("far-out lockout = %v, want cap %v", got, lockoutCap)
	}
}
