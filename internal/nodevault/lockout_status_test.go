//go:build linux || darwin || windows

package nodevault

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLockoutStatusMissingSidecarDoesNotCreateFilesOrParent(t *testing.T) {
	base := t.TempDir()
	for _, statePresent := range []bool{false, true} {
		name := "absent-state"
		if statePresent {
			name = "existing-state"
		}
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(base, name)
			l := OpenLegacyPasscodeLockout(dir)
			if statePresent {
				if err := writeLockoutAt(l.path, lockoutState{}); err != nil {
					t.Fatal(err)
				}
			}
			if status := l.Status(); status.Unavailable || status.FailedAttempts != 0 {
				t.Fatalf("missing sidecar reported unhealthy: %+v", status)
			}
			if _, err := os.Lstat(l.path + ".lock"); !os.IsNotExist(err) {
				t.Fatalf("status created a sidecar: %v", err)
			}
			if !statePresent {
				if _, err := os.Stat(dir); !os.IsNotExist(err) {
					t.Fatalf("status created a missing parent: %v", err)
				}
			}
		})
	}
}

func TestLockoutStatusBrokenSidecarDoesNotPresentCleanState(t *testing.T) {
	for _, statePresent := range []bool{false, true} {
		l := OpenLegacyPasscodeLockout(t.TempDir())
		var raw []byte
		var stateBefore os.FileInfo
		if statePresent {
			if err := writeLockoutAt(l.path, lockoutState{}); err != nil {
				t.Fatal(err)
			}
			var err error
			raw, err = os.ReadFile(l.path)
			if err != nil {
				t.Fatal(err)
			}
			stateBefore, err = os.Stat(l.path)
			if err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Mkdir(l.path+".lock", 0o700); err != nil {
			t.Fatal(err)
		}
		before, err := os.Lstat(l.path + ".lock")
		if err != nil {
			t.Fatal(err)
		}
		if status := l.Status(); !status.Unavailable || status.FailedAttempts != 0 || !status.LastVerifiedAt.IsZero() {
			t.Fatalf("broken sidecar presented a healthy history: %+v", status)
		}
		after, err := os.Lstat(l.path + ".lock")
		if err != nil || !os.SameFile(before, after) || after.Mode() != before.Mode() {
			t.Fatal("status repaired or replaced the broken sidecar")
		}
		if statePresent {
			stateAfter, err := os.Stat(l.path)
			if err != nil || !os.SameFile(stateBefore, stateAfter) || stateAfter.Mode() != stateBefore.Mode() {
				t.Fatal("unavailable status replaced or changed state")
			}
			if got, err := os.ReadFile(l.path); err != nil || !bytes.Equal(got, raw) {
				t.Fatal("unavailable status changed state bytes")
			}
		} else {
			if _, err := os.Stat(l.path); !os.IsNotExist(err) {
				t.Fatal("unavailable status created state")
			}
		}
	}
}

func TestLockoutStatusHealthyHeldSidecarIsReadOnlyAndDoesNotWait(t *testing.T) {
	l := OpenLegacyPasscodeLockout(t.TempDir())
	if err := writeLockoutAt(l.path, lockoutState{}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(l.path)
	if err != nil {
		t.Fatal(err)
	}
	stateBefore, err := os.Stat(l.path)
	if err != nil {
		t.Fatal(err)
	}
	file, err := openLockoutFile(l.path + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.Write([]byte("owned-sidecar-marker")); err != nil {
		t.Fatal(err)
	}
	lockBefore, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if err := lockLockoutFile(file, time.Now()); err != nil {
		t.Fatal(err)
	}
	released := false
	defer func() {
		if !released {
			unlockLockoutFile(file)
		}
	}()
	result := make(chan LockoutStatus, 1)
	go func() { result <- l.Status() }()
	select {
	case status := <-result:
		if status.Unavailable {
			t.Fatal("a held healthy lock is not an unavailable snapshot")
		}
	case <-time.After(time.Second):
		t.Fatal("status waited for an advisory lock")
	}
	stateAfter, err := os.Stat(l.path)
	if err != nil || !os.SameFile(stateBefore, stateAfter) || stateBefore.Mode() != stateAfter.Mode() {
		t.Fatal("status changed the state inode or mode")
	}
	if got, err := os.ReadFile(l.path); err != nil || !bytes.Equal(got, raw) {
		t.Fatal("status changed state bytes")
	}
	lockAfter, err := os.Stat(l.path + ".lock")
	if err != nil || !os.SameFile(lockBefore, lockAfter) || lockAfter.Mode() != lockBefore.Mode() {
		t.Fatal("status changed the sidecar inode or mode")
	}
	// Windows byte-range locking intentionally remains held above; do not read
	// its locked bytes through another handle until the transaction releases it.
	unlockLockoutFile(file)
	released = true
	if got, err := os.ReadFile(l.path + ".lock"); err != nil || string(got) != "owned-sidecar-marker" {
		t.Fatal("status changed sidecar bytes")
	}
}
