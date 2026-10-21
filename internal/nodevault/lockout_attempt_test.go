package nodevault

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func always(ok bool) func() bool { return func() bool { return ok } }

// TestLockoutAttemptAppliesVaultPolicy pins #1398: the exported limiter used by
// legacy passcode checks enforces the same thresholds and semantics as the vault.
func TestLockoutAttemptAppliesVaultPolicy(t *testing.T) {
	l := OpenLegacyPasscodeLockout(t.TempDir())
	for i := 1; i <= maxFreeAttempts; i++ {
		if err := l.Attempt(always(false)); !errors.Is(err, ErrWrongPIN) {
			t.Fatalf("wrong attempt %d: want ErrWrongPIN, got %v", i, err)
		}
		if i < maxFreeAttempts {
			// A correct secret before the threshold succeeds but does not clear
			// the recent failures.
			if err := l.Attempt(always(true)); err != nil {
				t.Fatalf("correct attempt before lockout: %v", err)
			}
			if st := l.Status(); st.FailedAttempts != i || st.LastVerifiedAt.IsZero() {
				t.Fatalf("success erased failures or skipped the anchor: %+v", st)
			}
		}
	}
	ran := false
	err := l.Attempt(func() bool { ran = true; return true })
	if !IsLockedOut(err) {
		t.Fatalf("correct secret during lockout: want ErrLockedOut, got %v", err)
	}
	if ran {
		t.Fatal("the check must not run during an active lockout")
	}
	if st := l.Status(); !st.LockedOut || st.FailedAttempts != maxFreeAttempts {
		t.Fatalf("a refused attempt must not record a failure: %+v", st)
	}
	if err := l.Reset(); err != nil {
		t.Fatal(err)
	}
	if err := l.Attempt(always(true)); err != nil {
		t.Fatalf("correct secret after Reset: %v", err)
	}
}

// TestLegacyLockoutUsesSiblingFile pins the legacy limiter's location: a file
// directly in the node config directory, never the vault's directory, so a
// legacy node gains no vault/ directory (whose ownership could block a later
// enroll) and never looks enrolled. Vault and legacy state do not mix.
func TestLegacyLockoutUsesSiblingFile(t *testing.T) {
	base := t.TempDir()
	l := OpenLegacyPasscodeLockout(base)
	if got, want := l.path, filepath.Join(base, legacyLockoutStateFile); got != want {
		t.Fatalf("legacy lockout path %q, want %q", got, want)
	}
	v := Open(base)
	if got := v.LegacyPasscodeLockout().path; got != l.path {
		t.Fatalf("vault-derived legacy lockout path %q, want %q", got, l.path)
	}
	_ = l.Status()
	for i := 0; i < maxFreeAttempts; i++ {
		_ = l.Attempt(always(false))
	}
	if _, err := os.Stat(filepath.Join(base, vaultDirName)); !os.IsNotExist(err) {
		t.Fatalf("legacy failures created the vault directory: %v", err)
	}
	if v.IsConfigured() {
		t.Fatal("legacy lockout state made a vault look enrolled")
	}
	if !l.Status().LockedOut {
		t.Fatal("legacy limiter did not lock")
	}
	if st := v.LockoutStatus(); st.FailedAttempts != 0 || st.LockedOut {
		t.Fatalf("legacy failures leaked into the vault's state: %+v", st)
	}
}

// TestLockoutResetIfPresent: an absent state file stays absent; an existing
// one is cleared.
func TestLockoutResetIfPresent(t *testing.T) {
	base := t.TempDir()
	l := OpenLegacyPasscodeLockout(base)
	if err := l.ResetIfPresent(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(l.path); !os.IsNotExist(err) {
		t.Fatalf("reset created a state file: %v", err)
	}
	for i := 0; i < maxFreeAttempts; i++ {
		_ = l.Attempt(always(false))
	}
	if err := l.ResetIfPresent(); err != nil {
		t.Fatal(err)
	}
	if st := l.Status(); st.LockedOut || st.FailedAttempts != 0 {
		t.Fatalf("reset left state: %+v", st)
	}
}

// TestLockoutAttemptSerializesInProcess proves concurrent guesses through
// separate handles cannot all pass the admission check before failures are
// recorded: at most maxFreeAttempts checks run.
func TestLockoutAttemptSerializesInProcess(t *testing.T) {
	base := t.TempDir()
	var checks atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 4*maxFreeAttempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = OpenLegacyPasscodeLockout(base).Attempt(func() bool { checks.Add(1); return false })
		}()
	}
	wg.Wait()
	if got := checks.Load(); got != maxFreeAttempts {
		t.Fatalf("%d checks ran, want exactly %d before the lockout refused the rest", got, maxFreeAttempts)
	}
	if st := OpenLegacyPasscodeLockout(base).Status(); st.FailedAttempts != maxFreeAttempts || !st.LockedOut {
		t.Fatalf("status after concurrent guesses: %+v", st)
	}
}
