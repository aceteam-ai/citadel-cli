//go:build linux || darwin

package update

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

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
	resolveInstalledPath = func() (string, error) { return installed, nil }
	beforeFinalInstalledCheck = func(stagePath string) {
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
}

func buildCitadelFixture(t *testing.T, output, version string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-buildvcs=false", "-o", output, "-ldflags", "-s -w -X github.com/aceteam-ai/citadel-cli/cmd.version="+version, "./cmd/citadel")
	cmd.Dir = filepath.Join("..", "..")
	data, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("compile-only Citadel fixture %s: %v\n%s", version, err, data)
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
