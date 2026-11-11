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

func TestLockoutStateRootPreparationUsesTemporaryFDAndParentOwner(t *testing.T) {
	dir := t.TempDir()
	file, err := os.CreateTemp(dir, "owned-temp-")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var parent unix.Stat_t
	if err := unix.Stat(dir, &parent); err != nil {
		t.Fatal(err)
	}
	called := false
	err = prepareLockoutStateOwner(file, dir, 0, func(fd, uid, gid int) error {
		called = true
		if fd != int(file.Fd()) || uid != int(parent.Uid) || gid != int(parent.Gid) {
			t.Fatal("ownership preparation did not use the new inode and parent identity")
		}
		// Same-owner fchown works without privileged ownership changes, so CI
		// can exercise the root-policy branch as an ordinary user.
		return unix.Fchown(fd, uid, gid)
	})
	if err != nil || !called {
		t.Fatalf("root-policy preparation failed: called=%v err=%v", called, err)
	}
	if err := prepareLockoutStateOwner(file, dir, 0, func(int, int, int) error { return unix.EPERM }); !errors.Is(err, unix.EPERM) {
		t.Fatalf("fchown refusal was ignored: %v", err)
	}
}

func TestLockoutStatePreparationResolvesTrustedParentButDoesNotFollowTempName(t *testing.T) {
	dir := t.TempDir()
	owned := filepath.Join(dir, "owned")
	if err := os.Mkdir(owned, 0o700); err != nil {
		t.Fatal(err)
	}
	file, err := os.CreateTemp(owned, "owned-temp-")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	alias := filepath.Join(dir, "alias")
	if err := os.Symlink(owned, alias); err != nil {
		t.Fatal(err)
	}
	called := false
	if err := prepareLockoutStateOwner(file, alias, 0, func(fd, uid, gid int) error {
		called = true
		if fd != int(file.Fd()) {
			t.Fatal("preparation did not use the captured temporary descriptor")
		}
		return unix.Fchown(fd, uid, gid)
	}); err != nil || !called {
		t.Fatalf("trusted symlink parent was refused: called=%v err=%v", called, err)
	}
	victim := filepath.Join(owned, "not-the-temp")
	raw := []byte("unrelated owned bytes")
	if err := os.WriteFile(victim, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	originalName := file.Name()
	if err := os.Rename(originalName, originalName+"-moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, originalName); err != nil {
		t.Fatal(err)
	}
	if err := prepareLockoutStateOwner(file, alias, 0, unix.Fchown); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(victim); err != nil || !bytes.Equal(got, raw) {
		t.Fatal("descriptor preparation touched the replacement target")
	}
	if info, err := os.Stat(victim); err != nil || info.Mode().Perm() != 0o644 {
		t.Fatal("descriptor preparation changed the replacement target mode")
	}
}

func TestLegacyLockoutTransactionsThroughTrustedSymlinkNodeDirectory(t *testing.T) {
	for _, existing := range []bool{false, true} {
		name := "fresh"
		if existing {
			name = "existing-state"
		}
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			realDir, alias := filepath.Join(base, "real-node"), filepath.Join(base, "node-alias")
			if err := os.Mkdir(realDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(realDir, alias); err != nil {
				t.Fatal(err)
			}
			limiter := OpenLegacyPasscodeLockout(alias)
			realState := filepath.Join(realDir, legacyLockoutStateFile)
			wantFailures := 0
			if existing {
				if err := recordFailureAt(realState); err != nil {
					t.Fatal(err)
				}
				wantFailures = 1
			}
			if err := limiter.Attempt(always(true)); err != nil {
				t.Fatalf("trusted alias denied correct legacy check: %v", err)
			}
			if err := limiter.Attempt(always(false)); !errors.Is(err, ErrWrongPIN) {
				t.Fatalf("trusted alias failed to record wrong legacy check: %v", err)
			}
			wantFailures++
			if err := limiter.Attempt(always(true)); err != nil {
				t.Fatalf("trusted alias denied subsequent correct check: %v", err)
			}
			before := limiter.Status()
			if before.Unavailable || before.FailedAttempts != wantFailures || before.LastVerifiedAt.IsZero() {
				t.Fatalf("alias history/verification differs: %+v", before)
			}
			for _, suffix := range []string{"", ".lock"} {
				viaAlias, err := os.Stat(limiter.path + suffix)
				if err != nil {
					t.Fatal(err)
				}
				viaReal, err := os.Stat(realState + suffix)
				if err != nil || !os.SameFile(viaAlias, viaReal) {
					t.Fatal("alias and real node directory do not share accounting inodes")
				}
			}
			if err := limiter.ResetIfPresent(); err != nil {
				t.Fatalf("local-presence reset through alias failed: %v", err)
			}
			after := limiter.Status()
			if after.Unavailable || after.FailedAttempts != 0 || after.LockedOut || !after.LastVerifiedAt.Equal(before.LastVerifiedAt) {
				t.Fatalf("alias reset did not clear only history: %+v", after)
			}
			if info, err := os.Lstat(alias); err != nil || info.Mode()&os.ModeSymlink == 0 {
				t.Fatal("accounting replaced the trusted directory alias")
			}
		})
	}
}

func TestLockoutOwnershipPreparationFailureCannotPublishState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.yaml")
	raw := []byte("existing state remains")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	err := writeFileAtomicPrepared(path, []byte("must not publish"), 0o600, func(file *os.File, parent string) error {
		if parent != dir {
			t.Fatal("unexpected state parent")
		}
		return prepareLockoutStateOwner(file, parent, 0, func(int, int, int) error { return unix.EPERM })
	})
	if !errors.Is(err, unix.EPERM) {
		t.Fatalf("ownership preparation failure was ignored: %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, raw) {
		t.Fatal("failed ownership preparation replaced existing state")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "state.yaml" {
		t.Fatalf("failed state preparation left an owned temp: %v %v", entries, err)
	}
}

func TestLockoutStateRootWriterInheritsParentOwnership(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("actual cross-UID fchown needs root; descriptor policy is tested without it")
	}
	dir := filepath.Join(t.TempDir(), "user-owned")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(dir, 12345, 12345); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "state.yaml")
	if err := writeLockoutAt(path, lockoutState{}); err != nil {
		t.Fatal(err)
	}
	var state unix.Stat_t
	if err := unix.Lstat(path, &state); err != nil || state.Uid != 12345 || state.Gid != 12345 || state.Mode&0o777 != 0o600 {
		t.Fatalf("root-created state did not inherit parent ownership: %+v %v", state, err)
	}
}
