//go:build linux || darwin

package nodevault

import (
	"errors"
	"os"
	"testing"
	"time"
)

func TestLockoutUnreadableStateRequiresExplicitLocalRecovery(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the Unix read-mode refusal; non-root CI exercises this case")
	}
	l := OpenLegacyPasscodeLockout(t.TempDir())
	if err := writeLockoutAt(l.path, lockoutState{LastVerifiedAt: time.Now(), FailureTimes: []time.Time{time.Now()}}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(l.path, 0); err != nil {
		t.Fatal(err)
	}
	called := false
	if err := l.Attempt(func() bool { called = true; return true }); !errors.Is(err, ErrLockoutUnavailable) || called {
		t.Fatalf("unreadable state admitted a check: called=%v err=%v", called, err)
	}
	if status := l.Status(); !status.Unavailable || !status.LastVerifiedAt.IsZero() {
		t.Fatalf("unreadable state reported available: %+v", status)
	}
	for _, mutation := range []func() error{
		func() error { return recordFailureAt(l.path) },
		func() error { return recordSuccessAt(l.path) },
	} {
		if err := mutation(); !errors.Is(err, ErrLockoutUnavailable) {
			t.Fatalf("ordinary mutation recovered unreadable state: %v", err)
		}
	}
	if info, err := os.Stat(l.path); err != nil || info.Mode().Perm() != 0 {
		t.Fatal("ordinary mutation replaced the unreadable state")
	}
	if err := l.ResetIfPresent(); err != nil {
		t.Fatalf("explicit local recovery refused unreadable state: %v", err)
	}
	state, err := loadLockoutAt(l.path, time.Now())
	if err != nil || state.FailedAttempts != 0 || !state.LastVerifiedAt.IsZero() || !state.LockoutUntil.IsZero() || state.FailureTimes == nil {
		t.Fatalf("recovery retained unreadable history/anchor: %+v %v", state, err)
	}
}
