//go:build linux || darwin || windows

package nodevault

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLockoutAbsentStateResetRefusesBrokenSidecarWithoutMutation(t *testing.T) {
	l := OpenLegacyPasscodeLockout(t.TempDir())
	lockPath := l.path + ".lock"
	if err := os.Mkdir(lockPath, 0o700); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.ResetIfPresent(); !errors.Is(err, ErrLockoutUnavailable) {
		t.Fatal("absent-state reset reported success with an unusable sidecar")
	}
	if status := l.Status(); !status.Unavailable || status.UnavailableReason != LockoutSidecarUnavailable {
		t.Fatalf("reset failure and sidecar status disagree: %+v", status)
	}
	after, err := os.Lstat(lockPath)
	if err != nil || !os.SameFile(before, after) || after.Mode() != before.Mode() {
		t.Fatal("refused reset replaced or changed the sidecar")
	}
	if _, err := os.Lstat(l.path); !os.IsNotExist(err) {
		t.Fatal("refused reset created state")
	}
}

func TestLockoutAbsentStateResetDoesNotCreateParentOrSidecar(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "absent-node")
	l := OpenLegacyPasscodeLockout(dir)
	if err := l.ResetIfPresent(); err != nil {
		t.Fatalf("healthy absent-state no-op refused: %v", err)
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatal("absent-state reset created its parent or sidecar")
	}
	if status := l.Status(); status.Unavailable || status.UnavailableReason != "" {
		t.Fatalf("healthy absent snapshot retained an unavailable reason: %+v", status)
	}
}

func TestLockoutStatusDistinguishesStateAndSidecarFailures(t *testing.T) {
	l := OpenLegacyPasscodeLockout(t.TempDir())
	raw := []byte("failed_attempts: [\n")
	if err := os.WriteFile(l.path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(l.path)
	if err != nil {
		t.Fatal(err)
	}
	if status := l.Status(); !status.Unavailable || status.UnavailableReason != LockoutStateUnavailable {
		t.Fatalf("corrupt state lost its static attribution: %+v", status)
	}
	if _, err := os.Lstat(l.path + ".lock"); !os.IsNotExist(err) {
		t.Fatal("state inspection created a sidecar")
	}
	if err := os.Mkdir(l.path+".lock", 0o700); err != nil {
		t.Fatal(err)
	}
	if status := l.Status(); !status.Unavailable || status.UnavailableReason != LockoutSidecarUnavailable {
		t.Fatalf("unusable sidecar was hidden behind corrupt state: %+v", status)
	}
	after, err := os.Stat(l.path)
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("status replaced corrupt state")
	}
	if got, err := os.ReadFile(l.path); err != nil || !bytes.Equal(got, raw) {
		t.Fatal("status changed corrupt-state bytes")
	}
}
