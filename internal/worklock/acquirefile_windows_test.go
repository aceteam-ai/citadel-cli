//go:build windows

package worklock

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestAcquireFileRecordReadableWhileHeld pins why AcquireFile locks a byte past
// the record: Windows byte-range locks are mandatory, so a probe on another
// handle must still read the holder's PID while the lock is held.
func TestAcquireFileRecordReadableWhileHeld(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test-supervisor.lock")
	l, err := AcquireFile(path, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()

	if held, pid := IsHeldFile(path); !held || pid != os.Getpid() {
		t.Fatalf("IsHeldFile = (%v, %d), want (true, %d)", held, pid, os.Getpid())
	}
	_, err = AcquireFile(path, "test", nil)
	var running *ErrAlreadyRunning
	if !errors.As(err, &running) || running.PID != os.Getpid() {
		t.Fatalf("second AcquireFile err = %v, want ErrAlreadyRunning naming PID %d", err, os.Getpid())
	}

	l.Release()
	if held, _ := IsHeldFile(path); held {
		t.Fatal("IsHeldFile reports held after Release")
	}
}
