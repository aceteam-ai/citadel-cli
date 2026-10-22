//go:build !linux && !darwin && !windows

package nodevault

import (
	"os"
	"testing"
)

func TestLockoutStatusUnsupportedBackendIsUnavailableWithoutCreation(t *testing.T) {
	l := OpenLegacyPasscodeLockout(t.TempDir())
	if status := l.Status(); !status.Unavailable {
		t.Fatal("unsupported mutation backend reported available")
	}
	if _, err := os.Lstat(l.path + ".lock"); !os.IsNotExist(err) {
		t.Fatal("unsupported status created a sidecar")
	}
}
