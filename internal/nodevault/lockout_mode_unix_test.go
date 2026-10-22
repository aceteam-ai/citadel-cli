//go:build linux || darwin

package nodevault

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestLockoutNewSidecarModeAcrossUmasks(t *testing.T) {
	if path := os.Getenv("CITADEL_LOCKOUT_UMASK_TEST_PATH"); path != "" {
		mask, err := strconv.ParseUint(os.Getenv("CITADEL_LOCKOUT_UMASK_TEST_MASK"), 8, 32)
		if err != nil {
			t.Fatal(err)
		}
		// This is a disposable re-executed test process, never the parent test
		// runner: umask is process-global and must not race other tests.
		unix.Umask(int(mask))
		file, err := openLockoutFile(path)
		if err != nil {
			t.Fatal(err)
		}
		before, statErr := file.Stat()
		_ = file.Close()
		if statErr != nil || before.Mode().Perm() != 0o600 {
			t.Fatalf("new lock mode differs under umask %03o: %v", mask, statErr)
		}
		file, err = openLockoutFile(path)
		if err != nil {
			t.Fatalf("new lock refused its next open: %v", err)
		}
		after, statErr := file.Stat()
		_ = file.Close()
		if statErr != nil || !os.SameFile(before, after) || after.Mode().Perm() != 0o600 {
			t.Fatal("subsequent open changed the persistent lock inode/mode")
		}
		return
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mask := range []string{"000", "077", "277"} {
		t.Run(mask, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, executable, "-test.run=^TestLockoutNewSidecarModeAcrossUmasks$")
			cmd.Env = append(os.Environ(),
				"CITADEL_LOCKOUT_UMASK_TEST_PATH="+filepath.Join(t.TempDir(), "owned.lock"),
				"CITADEL_LOCKOUT_UMASK_TEST_MASK="+mask)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("owned umask helper failed: %v %s", err, output)
			}
		})
	}
}

func TestLockoutExistingWrongModeIsNotRepaired(t *testing.T) {
	path := filepath.Join(t.TempDir(), "existing.lock")
	raw := []byte("existing owned inode")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	file, err := openLockoutFile(path)
	if file != nil {
		_ = file.Close()
		t.Fatal("existing wrong-mode lock was accepted")
	}
	if err == nil {
		t.Fatal("existing wrong-mode lock was not refused")
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(before, after) || after.Mode().Perm() != 0o400 {
		t.Fatal("refusal removed/replaced or chmodded the existing lock")
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, raw) {
		t.Fatal("refusal changed existing lock bytes")
	}
}

func TestLockoutOwnerPreparationFailureAndForeignOwnerStayRefused(t *testing.T) {
	dir := t.TempDir()
	file, err := os.CreateTemp(dir, "owned.lock-")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := file.Chmod(0o600); err != nil {
		t.Fatal(err)
	}
	var state, parent unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &state); err != nil {
		t.Fatal(err)
	}
	if err := unix.Stat(dir, &parent); err != nil {
		t.Fatal(err)
	}
	before, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	called := false
	err = checkLockoutFileOwner(int(file.Fd()), state, parent, true, 0, func(fd, uid, gid int) error {
		called = true
		if fd != int(file.Fd()) || uid != int(parent.Uid) || gid != int(parent.Gid) {
			t.Fatal("new-owner preparation used the wrong inode/identity")
		}
		return unix.EPERM
	})
	if !called || !errors.Is(err, unix.EPERM) {
		t.Fatalf("failed new-owner preparation was not refused: called=%v err=%v", called, err)
	}
	after, err := os.Lstat(file.Name())
	if err != nil || !os.SameFile(before, after) || after.Mode().Perm() != 0o600 {
		t.Fatal("owner refusal removed/replaced or changed the created inode")
	}
	// The owner policy is always the parent UID, never a creator exception.
	// Synthetic stat input tests this branch without changing any real UID.
	foreignParent := parent
	foreignParent.Uid = state.Uid ^ 1
	for _, created := range []bool{false, true} {
		called = false
		err := checkLockoutFileOwner(int(file.Fd()), state, foreignParent, created, 0, func(int, int, int) error {
			called = true
			return nil // a reported success is still checked against actual FD metadata
		})
		if !errors.Is(err, ErrLockoutUnavailable) || called != created {
			t.Fatalf("foreign owner accepted or existing owner mutated: created=%v called=%v err=%v", created, called, err)
		}
	}
	final, err := os.Lstat(file.Name())
	if err != nil || !os.SameFile(before, final) || final.Mode().Perm() != 0o600 {
		t.Fatal("foreign-owner refusal removed/replaced or changed the inode")
	}
}
