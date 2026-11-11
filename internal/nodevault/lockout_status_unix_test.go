//go:build linux || darwin

package nodevault

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestLockoutStatusRefusesUnsafeExistingSidecarWithoutRepair(t *testing.T) {
	for _, kind := range []string{"symlink", "fifo", "hardlink", "wrong-mode"} {
		t.Run(kind, func(t *testing.T) {
			l := OpenLegacyPasscodeLockout(t.TempDir())
			path := l.path + ".lock"
			target := filepath.Join(filepath.Dir(path), "unrelated-target")
			raw := []byte("owned-marker")
			if err := os.WriteFile(target, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			var err error
			switch kind {
			case "symlink":
				err = os.Symlink(target, path)
			case "fifo":
				err = unix.Mkfifo(path, 0o600)
			case "hardlink":
				err = os.Link(target, path)
			case "wrong-mode":
				err = os.WriteFile(path, raw, 0o400)
			}
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			if status := l.Status(); !status.Unavailable {
				t.Fatalf("unsafe %s sidecar reported healthy", kind)
			}
			if err := l.ResetIfPresent(); !errors.Is(err, ErrLockoutUnavailable) {
				t.Fatalf("absent-state reset reported success with unsafe %s sidecar", kind)
			}
			after, err := os.Lstat(path)
			if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() {
				t.Fatal("status changed the supplied sidecar")
			}
			if got, err := os.ReadFile(target); err != nil || !bytes.Equal(got, raw) {
				t.Fatal("status changed the unrelated target")
			}
			if _, err := os.Stat(l.path); !os.IsNotExist(err) {
				t.Fatal("status created state during inspection")
			}
		})
	}
}

func TestExistingLockoutStatusOwnerPolicyIsPure(t *testing.T) {
	for _, tc := range []struct {
		uid      uint32
		euid     int
		fallback bool
		allowed  bool
	}{
		{1001, 1001, false, true},
		{0, 0, true, true},
		{0, 1001, false, false},
		{1002, 0, false, false},
		{1002, 1002, false, false},
	} {
		var state, parent unix.Stat_t
		state.Uid, parent.Uid = tc.uid, 1001
		fallback, err := existingLockoutFileOwner(state, parent, tc.euid)
		if (err == nil) != tc.allowed || fallback != tc.fallback {
			t.Fatalf("wrong existing-only owner policy: %+v fallback=%v err=%v", tc, fallback, err)
		}
	}
}
