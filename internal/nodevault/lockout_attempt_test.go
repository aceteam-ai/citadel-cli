package nodevault

import (
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func always(ok bool) func() bool { return func() bool { return ok } }

// TestLockoutAttemptAppliesVaultPolicy pins #1398: the exported limiter used by
// legacy passcode checks enforces the same thresholds and semantics as the vault.
func TestLockoutAttemptAppliesVaultPolicy(t *testing.T) {
	l := OpenLockout(t.TempDir())
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

// TestLockoutSharedWithVault pins the single node-wide counter: legacy checks
// and the vault read and write the same state file under the same base dir.
func TestLockoutSharedWithVault(t *testing.T) {
	base := t.TempDir()
	v := Open(base)
	v.paramsOverride = &fastParams
	mustSet(t, v, "654321")

	l := OpenLockout(base)
	if got, want := l.path, v.lockoutPath(); got != want {
		t.Fatalf("lockout path %q, vault lockout path %q", got, want)
	}
	if got, want := l.path, filepath.Join(base, vaultDirName, lockoutStateFile); got != want {
		t.Fatalf("lockout path %q, want %q", got, want)
	}
	for i := 0; i < maxFreeAttempts; i++ {
		_ = l.Attempt(always(false))
	}
	if err := v.VerifyPIN("654321"); !IsLockedOut(err) {
		t.Fatalf("legacy failures must lock the shared counter: %v", err)
	}
	if err := v.ResetLockout(); err != nil {
		t.Fatal(err)
	}
	if st := l.Status(); st.LockedOut || st.FailedAttempts != 0 {
		t.Fatalf("vault reset must clear the shared state: %+v", st)
	}
}

// TestLockoutAttemptNoConfigWritesNothingUntilAttempt confirms opening the
// limiter and reading status does not create state or the vault header.
func TestLockoutAttemptNoConfigWritesNothingUntilAttempt(t *testing.T) {
	base := t.TempDir()
	l := OpenLockout(base)
	_ = l.Status()
	if Open(base).IsConfigured() {
		t.Fatal("status must not configure a vault")
	}
	_ = l.Attempt(always(false))
	if Open(base).IsConfigured() {
		t.Fatal("recording a legacy failure must not make the vault look enrolled")
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
			_ = OpenLockout(base).Attempt(func() bool { checks.Add(1); return false })
		}()
	}
	wg.Wait()
	if got := checks.Load(); got != maxFreeAttempts {
		t.Fatalf("%d checks ran, want exactly %d before the lockout refused the rest", got, maxFreeAttempts)
	}
	if st := OpenLockout(base).Status(); st.FailedAttempts != maxFreeAttempts || !st.LockedOut {
		t.Fatalf("status after concurrent guesses: %+v", st)
	}
}
