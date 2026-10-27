//go:build !linux && !darwin && !windows

package nodevault

import (
	"errors"
	"os"
	"testing"
)

func TestLockoutStatusUnsupportedBackendIsUnavailableWithoutCreation(t *testing.T) {
	l := OpenLegacyPasscodeLockout(t.TempDir())
	if status := l.Status(); !status.Unavailable || status.UnavailableReason != LockoutBackendUnavailable {
		t.Fatal("unsupported mutation backend reported available")
	}
	if err := l.ResetIfPresent(); !errors.Is(err, ErrLockoutUnavailable) {
		t.Fatal("unsupported absent-state reset reported success")
	}
	if _, err := os.Lstat(l.path + ".lock"); !os.IsNotExist(err) {
		t.Fatal("unsupported status created a sidecar")
	}
}
