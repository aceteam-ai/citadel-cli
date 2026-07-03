package worklock

import (
	"path/filepath"
	"testing"
)

func TestDecideStaleLock(t *testing.T) {
	const self = 4242
	tests := []struct {
		name       string
		holderPID  int
		selfPID    int
		holderLive bool
		want       staleLockAction
	}{
		{"empty lock file (pid 0) reclaims", 0, self, false, actionReclaim},
		{"negative pid reclaims", -1, self, false, actionReclaim},
		{"own pid recorded reclaims", self, self, true, actionReclaim},
		{"dead holder reclaims", 1234, self, false, actionReclaim},
		{"live holder refuses", 1234, self, true, actionRefuse},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := decideStaleLock(tt.holderPID, tt.selfPID, tt.holderLive)
			if got != tt.want {
				t.Fatalf("decideStaleLock(%d,%d,%v) = %v, want %v",
					tt.holderPID, tt.selfPID, tt.holderLive, got, tt.want)
			}
		})
	}
}

func TestLockPathForStateDir(t *testing.T) {
	// The lock must live in the PARENT of the network state dir so ClearState
	// (which RemoveAll's the network dir) never deletes it.
	got := LockPathForStateDir("/home/u/citadel-node/network")
	want := filepath.FromSlash("/home/u/citadel-node/" + LockFileName)
	if got != want {
		t.Fatalf("LockPathForStateDir = %q, want %q", got, want)
	}

	// Trailing slash must not change the result.
	got2 := LockPathForStateDir("/home/u/citadel-node/network/")
	if got2 != want {
		t.Fatalf("LockPathForStateDir(trailing slash) = %q, want %q", got2, want)
	}
}

// TestAcquireSecondIsRejected verifies the core guarantee: while one holder has
// the lock, a second Acquire on the same node is refused with *ErrAlreadyRunning
// carrying the holder PID. A third attempt succeeds once the first releases.
func TestAcquireSecondIsRejected(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "network")

	first, err := Acquire(stateDir, nil)
	if err != nil {
		t.Fatalf("first Acquire failed: %v", err)
	}
	if first == nil {
		t.Fatal("first Acquire returned nil lock")
	}

	// Second acquire while first is held: must be refused.
	second, err := Acquire(stateDir, nil)
	if err == nil {
		second.Release()
		t.Fatal("second Acquire succeeded while first held the lock; expected refusal")
	}
	are, ok := err.(*ErrAlreadyRunning)
	if !ok {
		t.Fatalf("second Acquire error = %T (%v), want *ErrAlreadyRunning", err, err)
	}
	if are.PID <= 0 {
		t.Errorf("ErrAlreadyRunning.PID = %d, want the holder PID (>0)", are.PID)
	}

	// Release the first and confirm a new Acquire now succeeds (lock reusable).
	first.Release()
	third, err := Acquire(stateDir, nil)
	if err != nil {
		t.Fatalf("Acquire after Release failed: %v", err)
	}
	third.Release()
}

// TestReleaseIdempotent verifies Release can be called multiple times safely.
func TestReleaseIdempotent(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "network")
	l, err := Acquire(stateDir, nil)
	if err != nil {
		t.Fatalf("Acquire failed: %v", err)
	}
	l.Release()
	l.Release() // must not panic
}

// TestStaleLockReclaimed simulates a dead prior holder: a lock file exists with a
// PID that is not alive, and no live process holds the OS lock. Acquire must
// reclaim it (the flock/LockFileEx is free once the prior process exited).
func TestStaleLockReclaimed(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "network")

	// Hold and release to leave a lock file behind with a now-defunct PID record.
	first, err := Acquire(stateDir, nil)
	if err != nil {
		t.Fatalf("first Acquire failed: %v", err)
	}
	// Simulate a crash where the file is NOT removed but the lock is freed on exit
	// by closing the fd without Remove. We cannot easily fork here, so we rely on
	// Release freeing the OS lock; the reclaim path is exercised by decideStaleLock
	// unit tests. This test asserts re-acquire works after the OS lock is free.
	first.Release()

	got, err := Acquire(stateDir, func(string, ...any) {})
	if err != nil {
		t.Fatalf("re-Acquire after prior holder exited failed: %v", err)
	}
	got.Release()
}
