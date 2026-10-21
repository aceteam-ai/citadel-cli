package nodevault

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
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

func TestSuccessDoesNotResetRecentFailures(t *testing.T) {
	v := newTestVault(t)
	mustSet(t, v, "654321")

	for i := 1; i <= maxFreeAttempts; i++ {
		if err := v.VerifyPIN("000000"); !errors.Is(err, ErrWrongPIN) {
			t.Fatalf("wrong PIN %d: %v", i, err)
		}
		if i < maxFreeAttempts {
			if err := v.VerifyPIN("654321"); err != nil {
				t.Fatalf("correct PIN before lockout: %v", err)
			}
			if lo := v.LockoutStatus(); lo.FailedAttempts != i || lo.LastVerifiedAt.IsZero() {
				t.Fatalf("success erased failures or failed to refresh verification: %+v", lo)
			}
		}
	}
	if err := v.VerifyPIN("654321"); !IsLockedOut(err) {
		t.Fatalf("interleaved correct PINs evaded lockout: %v", err)
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

func TestNormalizeLockoutWindowBoundaryAndClockRollback(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	until := now.Add(lockoutBase)
	verified := now.Add(-time.Minute)
	recent := now.Add(-failureWindow + time.Nanosecond)
	future := now.Add(time.Hour)
	original := []time.Time{future, now.Add(-failureWindow), recent, now.Add(-failureWindow - time.Nanosecond)}
	s := normalizeLockout(lockoutState{
		FailedAttempts: 99, FailureTimes: original,
		LockoutUntil: until, LastVerifiedAt: verified,
	}, now)
	if s.FailedAttempts != 2 || !reflect.DeepEqual(s.FailureTimes, []time.Time{recent, future}) {
		t.Fatalf("bad boundary pruning: %+v", s)
	}
	if !s.LockoutUntil.Equal(until) || !s.LastVerifiedAt.Equal(verified) {
		t.Fatal("pruning altered lockout/verification anchors")
	}
	if !original[0].Equal(future) {
		t.Fatal("pure normalization mutated its caller's history")
	}
}

func TestNormalizeLockoutKeepsNewest64AndMirrorsCount(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	history := make([]time.Time, 0)
	for i := 0; i < maxFailureHistory+20; i++ {
		history = append(history, now.Add(-time.Duration(i)*time.Second))
	}
	s := normalizeLockout(lockoutState{FailureTimes: history, FailedAttempts: 999}, now)
	if len(s.FailureTimes) != maxFailureHistory || s.FailedAttempts != maxFailureHistory {
		t.Fatalf("unbounded/stale count: %+v", s)
	}
	if !s.FailureTimes[0].Equal(now.Add(-time.Duration(maxFailureHistory-1)*time.Second)) || !s.FailureTimes[maxFailureHistory-1].Equal(now) {
		t.Fatal("retained oldest rather than newest failures")
	}
	if backoff(s.FailedAttempts) != lockoutCap {
		t.Fatal("history cap must preserve capped backoff")
	}
}

func TestLegacyLockoutMigrationIsConservativeAndPersistedOnce(t *testing.T) {
	v := newTestVault(t)
	mustSet(t, v, "654321")
	// An old binary persisted only a count, with no trustworthy failure times.
	if err := os.WriteFile(v.lockoutPath(), []byte("failed_attempts: 7\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(v.lockoutPath())
	if err != nil {
		t.Fatal(err)
	}
	if got := v.LockoutStatus().FailedAttempts; got != 7 {
		t.Fatalf("legacy count reset on read: %d", got)
	}
	after, err := os.ReadFile(v.lockoutPath())
	if err != nil || string(before) != string(after) {
		t.Fatal("status read unexpectedly rewrote state")
	}
	v.recordSuccessLocked()
	data, err := os.ReadFile(v.lockoutPath())
	if err != nil {
		t.Fatal(err)
	}
	var persisted lockoutState
	if err := yaml.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.FailedAttempts != 7 || len(persisted.FailureTimes) != 7 || persisted.LastVerifiedAt.IsZero() {
		t.Fatalf("migration erased guesses: %+v", persisted)
	}
	for _, at := range persisted.FailureTimes {
		if !at.Equal(persisted.LastVerifiedAt) {
			t.Fatal("undated legacy failures must be seeded at migration write time")
		}
	}
	// A later independent handle ages the persisted timestamps, not a fresh
	// migration time on every read. Explicit [] must not reseed old counts.
	future := persisted.LastVerifiedAt.Add(failureWindow + time.Second)
	v2 := Open(filepath.Dir(v.dir))
	expired := v2.readLockoutAt(future)
	if expired.FailedAttempts != 0 || expired.FailureTimes == nil {
		t.Fatalf("persisted history did not expire: %+v", expired)
	}
	if err := v2.writeLockout(expired); err != nil {
		t.Fatal(err)
	}
	if got := v.readLockoutAt(future); got.FailedAttempts != 0 || got.FailureTimes == nil {
		t.Fatalf("empty history was treated as legacy again: %+v", got)
	}
}

func TestLegacyCountMigrationIsBounded(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	for _, count := range []int{-1, 0, 1, maxFailureHistory, maxFailureHistory + 1000000} {
		s := normalizeLockout(lockoutState{FailedAttempts: count}, now)
		if want := min(max(count, 0), maxFailureHistory); s.FailedAttempts != want || len(s.FailureTimes) != want {
			t.Fatalf("legacy count %d not bounded: %+v", count, s)
		}
	}
}

func TestExpiredHistoryDoesNotClearActiveLockout(t *testing.T) {
	v := newTestVault(t)
	mustSet(t, v, "654321")
	now := time.Now()
	until := now.Add(time.Hour)
	if err := v.writeLockout(lockoutState{
		FailureTimes: []time.Time{now.Add(-failureWindow - time.Minute)},
		FailedAttempts: 1, LockoutUntil: until,
	}); err != nil {
		t.Fatal(err)
	}
	if status := v.LockoutStatus(); status.FailedAttempts != 0 || !status.LockedOut {
		t.Fatalf("wrong status after pruning: %+v", status)
	}
	if err := v.VerifyPIN("654321"); !IsLockedOut(err) {
		t.Fatalf("correct PIN bypassed active lockout: %v", err)
	}
	v.recordSuccessLocked() // also pin the helper's invariant independently
	if s := v.readLockout(); !s.LockoutUntil.Equal(until) || s.LastVerifiedAt.IsZero() {
		t.Fatalf("success cleared active lockout: %+v", s)
	}
}

func TestExpiredFailuresDoNotCountTowardNextFailure(t *testing.T) {
	v := newTestVault(t)
	mustSet(t, v, "654321")
	old := time.Now().Add(-failureWindow - time.Minute)
	if err := v.writeLockout(lockoutState{FailedAttempts: maxFreeAttempts, FailureTimes: []time.Time{old, old, old, old, old}}); err != nil {
		t.Fatal(err)
	}
	if err := v.VerifyPIN("000000"); !errors.Is(err, ErrWrongPIN) {
		t.Fatal(err)
	}
	if s := v.LockoutStatus(); s.FailedAttempts != 1 || s.LockedOut {
		t.Fatalf("expired guesses counted: %+v", s)
	}
}

func TestHistoryConvergesAcrossSequentialInstances(t *testing.T) {
	base := t.TempDir()
	v1 := Open(base)
	v1.paramsOverride = &fastParams
	mustSet(t, v1, "654321")
	v2 := Open(base)
	v2.paramsOverride = &fastParams
	for i := 1; i <= maxFreeAttempts; i++ {
		v := v1
		if i%2 == 0 {
			v = v2
		}
		if err := v.VerifyPIN("000000"); !errors.Is(err, ErrWrongPIN) {
			t.Fatal(err)
		}
		if i < maxFreeAttempts {
			if err := v2.VerifyPIN("654321"); err != nil {
				t.Fatal(err)
			}
		}
	}
	v3 := Open(base)
	v3.paramsOverride = &fastParams
	if status := v3.LockoutStatus(); status.FailedAttempts != maxFreeAttempts || !status.LockedOut {
		t.Fatalf("reopened handle lost interleaved history: %+v", status)
	}
	if err := v3.VerifyPIN("654321"); !IsLockedOut(err) {
		t.Fatalf("reopened handle bypassed lockout: %v", err)
	}
	verified := v3.LockoutStatus().LastVerifiedAt
	if err := v3.ResetLockout(); err != nil {
		t.Fatal(err)
	}
	if s := v1.LockoutStatus(); s.FailedAttempts != 0 || s.LockedOut || !s.LastVerifiedAt.Equal(verified) {
		t.Fatalf("local-presence reset failed or erased verification anchor: %+v", s)
	}
}
