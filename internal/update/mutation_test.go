//go:build linux || darwin

package update

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestMutationLockRefusesCanceledContextWithoutSideEffect(t *testing.T) {
	dir := t.TempDir()
	destination := filepath.Join(dir, "citadel")
	if err := os.WriteFile(destination, []byte("fixture"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := acquireMutationLock(ctx, destination, true); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled lock acquisition error = %v", err)
	}
	if _, err := os.Lstat(destination + ".citadel-update.lock"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled acquisition created sidecar: %v", err)
	}
}

func TestMutationLockRefusesFIFOWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	destination := filepath.Join(dir, "citadel")
	if err := os.WriteFile(destination, []byte("fixture"), 0o755); err != nil {
		t.Fatal(err)
	}
	lockPath := destination + ".citadel-update.lock"
	if err := unix.Mkfifo(lockPath, 0o600); err != nil {
		t.Fatal(err)
	}

	requireFIFORefusalPromptly(t, lockPath, func() error {
		lock, err := acquireMutationLock(context.Background(), destination, true)
		if lock != nil {
			_ = lock.Release()
		}
		return err
	})
}

func TestOpenRegularNoFollowRefusesFIFOWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "candidate")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}

	requireFIFORefusalPromptly(t, path, func() error {
		file, err := openRegularNoFollow(path)
		if file != nil {
			_ = file.Close()
		}
		return err
	})
}

func requireFIFORefusalPromptly(t *testing.T, path string, open func() error) {
	t.Helper()
	result := make(chan error, 1)
	go func() { result <- open() }()

	select {
	case err := <-result:
		if err == nil {
			t.Fatal("FIFO was accepted")
		}
	case <-time.After(time.Second):
		// Unblock a mutant that omitted O_NONBLOCK so the test leaves no goroutine.
		writer, unblockErr := unix.Open(path, unix.O_WRONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if unblockErr == nil {
			_ = unix.Close(writer)
		}
		select {
		case <-result:
		case <-time.After(time.Second):
			t.Fatalf("FIFO open remained blocked and could not be cleaned up: %v", unblockErr)
		}
		t.Fatal("FIFO open blocked instead of refusing promptly")
	}
}

func TestMutationLockSerializesPersistsAndRejectsUnsafeMode(t *testing.T) {
	dir := t.TempDir()
	destination := filepath.Join(dir, "citadel")
	if err := os.WriteFile(destination, []byte("fixture"), 0o755); err != nil {
		t.Fatal(err)
	}
	first, err := acquireMutationLock(context.Background(), destination, true)
	if err != nil {
		t.Fatal(err)
	}
	originalWait := mutationLockWait
	mutationLockWait = func(context.Context, time.Duration) error { return context.Canceled }
	t.Cleanup(func() { mutationLockWait = originalWait })
	if _, err := acquireMutationLock(context.Background(), destination, true); err == nil {
		t.Fatal("second same-destination transaction acquired held lock")
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	if err := first.Release(); err != nil {
		t.Fatalf("second release: %v", err)
	}
	lockPath := destination + ".citadel-update.lock"
	info, err := os.Stat(lockPath)
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("persistent lock mode = %v, %v", info.Mode().Perm(), err)
	}
	if err := os.Chmod(lockPath, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := acquireMutationLock(context.Background(), destination, true); err == nil {
		t.Fatal("unsafe existing lock mode accepted")
	}
	if err := os.Chmod(lockPath, 0o644|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(lockPath); err != nil {
		t.Fatal(err)
	} else if info.Mode()&os.ModeSticky == 0 {
		t.Fatal("filesystem did not retain the sticky mode bit")
	}
	if _, err := acquireMutationLock(context.Background(), destination, true); err == nil {
		t.Fatal("lock with special mode bits accepted")
	}
}

func TestApplyReleaseExactBindsInstalledFetchedAndCandidate(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	installed := filepath.Join(dir, "citadel")
	candidate := filepath.Join(dir, "candidate")
	buildCitadelFixture(t, installed, "v2.0.0")
	buildCitadelFixture(t, candidate, "v2.1.0")
	originalResolver := resolveInstalledPath
	resolveInstalledPath = func() (string, error) { return installed, nil }
	t.Cleanup(func() { resolveInstalledPath = originalResolver })

	before, relation, err := CheckExactTarget(context.Background(), "2.1.0")
	if err != nil || before != "v2.0.0" || relation != TargetNewer {
		t.Fatalf("preflight = %q, %v, %v", before, relation, err)
	}
	result, err := ApplyRelease(context.Background(), candidate, "v2.1.0", "v2.1.0", ExactRemote)
	if err != nil {
		t.Fatal(err)
	}
	if result.Disposition != Applied || result.InstalledBefore != "v2.0.0" || result.InstalledAfter != "v2.1.0" {
		t.Fatalf("result = %#v", result)
	}
	current, err := ReadExecutableVersion(installed)
	if err != nil || current.Version != "v2.1.0" {
		t.Fatalf("installed metadata = %#v, %v", current, err)
	}
	previous, err := ReadExecutableVersion(GetPreviousBinaryPath())
	if err != nil || previous.Version != "v2.0.0" {
		t.Fatalf("previous metadata = %#v, %v", previous, err)
	}
}

func TestApplyReleaseExactSupersededAndMismatchDoNotBackupOrSwap(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	installed := filepath.Join(dir, "citadel")
	candidate := filepath.Join(dir, "candidate")
	buildCitadelFixture(t, installed, "v2.2.0")
	buildCitadelFixture(t, candidate, "v2.1.0")
	originalResolver := resolveInstalledPath
	resolveInstalledPath = func() (string, error) { return installed, nil }
	t.Cleanup(func() { resolveInstalledPath = originalResolver })

	if _, err := ApplyRelease(context.Background(), candidate, "v2.1.0", "v2.1.0", ExactRemote); !IsDeterministicUpdateError(err) {
		t.Fatalf("superseded exact error = %v", err)
	}
	if _, err := os.Stat(GetPreviousBinaryPath()); !os.IsNotExist(err) {
		t.Fatalf("superseded exact created backup: %v", err)
	}
	if _, err := ApplyRelease(context.Background(), candidate, "v2.3.0", "v2.3.0", ExactRemote); !IsDeterministicUpdateError(err) {
		t.Fatalf("candidate mismatch error = %v", err)
	}
	meta, err := ReadExecutableVersion(installed)
	if err != nil || meta.Version != "v2.2.0" {
		t.Fatalf("installed changed after refusal: %#v, %v", meta, err)
	}
}

func TestApplyReleaseFinalInstalledRecheckRefusesSupersession(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	installed := filepath.Join(dir, "citadel")
	candidate := filepath.Join(dir, "candidate")
	replacement := filepath.Join(dir, "replacement")
	buildCitadelFixture(t, installed, "v2.0.0")
	buildCitadelFixture(t, candidate, "v2.1.0")
	buildCitadelFixture(t, replacement, "v2.2.0")
	originalResolver := resolveInstalledPath
	originalHook := beforeFinalInstalledCheck
	resolveInstalledPath = func() (string, error) { return installed, nil }
	beforeFinalInstalledCheck = func(string) {
		if err := os.Rename(replacement, installed); err != nil {
			t.Fatalf("simulate noncooperating supersession: %v", err)
		}
	}
	t.Cleanup(func() {
		resolveInstalledPath = originalResolver
		beforeFinalInstalledCheck = originalHook
	})

	if _, err := ApplyRelease(context.Background(), candidate, "v2.1.0", "v2.1.0", ExactRemote); !IsDeterministicUpdateError(err) {
		t.Fatalf("final supersession error = %v", err)
	}
	if _, err := os.Stat(GetPreviousBinaryPath()); !os.IsNotExist(err) {
		t.Fatalf("final supersession created backup: %v", err)
	}
	meta, err := ReadExecutableVersion(installed)
	if err != nil || meta.Version != "v2.2.0" {
		t.Fatalf("superseding installation changed: %#v, %v", meta, err)
	}
}

func TestApplyReleaseFinalEqualIsNoOpWithoutBackup(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	installed := filepath.Join(dir, "citadel")
	candidate := filepath.Join(dir, "candidate")
	supersedingEqual := filepath.Join(dir, "equal")
	buildCitadelFixture(t, installed, "v2.0.0")
	buildCitadelFixture(t, candidate, "v2.1.0")
	buildCitadelFixture(t, supersedingEqual, "v2.1.0")
	originalResolver := resolveInstalledPath
	originalHook := beforeFinalInstalledCheck
	resolveInstalledPath = func() (string, error) { return installed, nil }
	beforeFinalInstalledCheck = func(string) {
		if err := os.Rename(supersedingEqual, installed); err != nil {
			t.Fatalf("simulate equal concurrent install: %v", err)
		}
	}
	t.Cleanup(func() {
		resolveInstalledPath = originalResolver
		beforeFinalInstalledCheck = originalHook
	})
	result, err := ApplyRelease(context.Background(), candidate, "v2.1.0", "v2.1.0", ExactRemote)
	if err != nil || result.Disposition != AlreadyCurrent || result.InstalledBefore != "v2.1.0" {
		t.Fatalf("result = %#v, %v", result, err)
	}
	if _, err := os.Stat(GetPreviousBinaryPath()); !os.IsNotExist(err) {
		t.Fatalf("final equal created backup: %v", err)
	}
}

func TestApplyReleaseRefusesReplacedCompletedStageBeforeBackup(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	installed := filepath.Join(dir, "citadel")
	candidate := filepath.Join(dir, "candidate")
	replacement := filepath.Join(dir, "replacement")
	buildCitadelFixture(t, installed, "v2.0.0")
	buildCitadelFixture(t, candidate, "v2.1.0")
	buildCitadelFixture(t, replacement, "v2.1.0")
	originalResolver := resolveInstalledPath
	originalHook := beforeFinalInstalledCheck
	var foreignStagePath string
	resolveInstalledPath = func() (string, error) { return installed, nil }
	beforeFinalInstalledCheck = func(stagePath string) {
		foreignStagePath = stagePath
		if err := os.Remove(stagePath); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(replacement, stagePath); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		resolveInstalledPath = originalResolver
		beforeFinalInstalledCheck = originalHook
	})
	if _, err := ApplyRelease(context.Background(), candidate, "v2.1.0", "v2.1.0", ExactRemote); err == nil {
		t.Fatal("replaced completed stage was installed")
	}
	if _, err := os.Stat(GetPreviousBinaryPath()); !os.IsNotExist(err) {
		t.Fatalf("replaced stage created backup: %v", err)
	}
	meta, err := ReadExecutableVersion(foreignStagePath)
	if err != nil || meta.Version != "v2.1.0" {
		t.Fatalf("refused foreign stage was removed or changed: %#v, %v", meta, err)
	}
}

func TestPublicRollbackAcquiresMutationLockOnce(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	installed := filepath.Join(dir, "citadel")
	buildCitadelFixture(t, installed, "v2.1.0")
	if err := EnsureUpdateDir(); err != nil {
		t.Fatal(err)
	}
	buildCitadelFixture(t, GetPreviousBinaryPath(), "v2.0.0")
	originalResolver := resolveInstalledPath
	resolveInstalledPath = func() (string, error) { return installed, nil }
	t.Cleanup(func() { resolveInstalledPath = originalResolver })
	if err := Rollback(); err != nil {
		t.Fatalf("rollback (possible recursive lock): %v", err)
	}
	meta, err := ReadExecutableVersion(installed)
	if err != nil || meta.Version != "v2.0.0" {
		t.Fatalf("rolled-back metadata = %#v, %v", meta, err)
	}
	if stages, err := filepath.Glob(filepath.Join(dir, ".citadel-update-stage-*")); err != nil || len(stages) != 0 {
		t.Fatalf("rollback left swap stages = %v, %v", stages, err)
	}
	if descriptors, err := os.ReadDir("/proc/self/fd"); err == nil {
		for _, descriptor := range descriptors {
			target, readErr := os.Readlink(filepath.Join("/proc/self/fd", descriptor.Name()))
			if readErr == nil && strings.Contains(target, dir) && strings.Contains(target, ".citadel-update-stage-") {
				t.Fatalf("rollback left swap-stage descriptor open: %s -> %s", descriptor.Name(), target)
			}
		}
	}
}

func TestMutationLocksForDifferentDestinationsDoNotContend(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "citadel-a")
	b := filepath.Join(dir, "citadel-b")
	for _, path := range []string{a, b} {
		if err := os.WriteFile(path, []byte("fixture"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	la, err := acquireMutationLock(context.Background(), a, true)
	if err != nil {
		t.Fatal(err)
	}
	defer la.Release()
	lb, err := acquireMutationLock(context.Background(), b, true)
	if err != nil {
		t.Fatal(err)
	}
	defer lb.Release()
}

func TestMutationLockProvisioningPolicyAndUnsafeLink(t *testing.T) {
	if !lockCreationOwnerAllowed(1001, 1001) || lockCreationOwnerAllowed(0, 1001) || lockCreationOwnerAllowed(1001, 0) {
		t.Fatal("first-creation policy must require effective owner identity")
	}
	dir := t.TempDir()
	destination := filepath.Join(dir, "citadel")
	if err := os.WriteFile(destination, []byte("fixture"), 0o755); err != nil {
		t.Fatal(err)
	}
	lock, err := acquireMutationLock(context.Background(), destination, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(destination+".citadel-update.lock", filepath.Join(dir, "lock-alias")); err != nil {
		t.Fatal(err)
	}
	if _, err := acquireMutationLock(context.Background(), destination, true); err == nil {
		t.Fatal("multi-link lock file accepted")
	}
}

func TestMutationLockFirstCreationFailsInReadOnlyParent(t *testing.T) {
	dir := t.TempDir()
	destination := filepath.Join(dir, "citadel")
	if err := os.WriteFile(destination, []byte("fixture"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if _, err := acquireMutationLock(context.Background(), destination, true); err == nil {
		t.Fatal("first lock creation unexpectedly succeeded in read-only parent")
	}
}
