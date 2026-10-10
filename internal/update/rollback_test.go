// internal/update/rollback_test.go
package update

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func provisionTestMutationLock(t *testing.T, destination string) {
	t.Helper()
	lock, err := acquireMutationLock(context.Background(), destination, true)
	if err != nil {
		t.Fatalf("provision validated mutation lock: %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatalf("release provisioned mutation lock: %v", err)
	}
}

func executableVersionIfExists(t *testing.T, path string) (string, bool) {
	t.Helper()
	_, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false
		}
		t.Fatalf("stat %s: %v", path, err)
	}
	meta, err := ReadExecutableVersion(path)
	if err != nil {
		t.Fatalf("read executable metadata %s: %v", path, err)
	}
	return meta.Version, true
}

// assertCompleteBinarySomewhere is the core citadel#926 invariant: after any
// interruption of the swap sequence, a complete binary (either the old or the
// new content -- both are valid, complete binaries) must be recoverable at
// either dst or dst+".old". It must never be the case that neither holds a
// complete binary.
func assertCompleteBinarySomewhere(t *testing.T, dst, oldVersion, newVersion string) {
	t.Helper()
	dstVersion, dstOK := executableVersionIfExists(t, dst)
	oldPathVersion, oldOK := executableVersionIfExists(t, dst+".old")

	if dstOK && (dstVersion == oldVersion || dstVersion == newVersion) {
		return
	}
	if oldOK && (oldPathVersion == oldVersion || oldPathVersion == newVersion) {
		return
	}
	t.Fatalf("no complete binary recoverable at %s or %s.old (dst=%q[present=%v], old=%q[present=%v])",
		dst, dst, dstVersion, dstOK, oldPathVersion, oldOK)
}

// TestAtomicReplaceWindows_InjectedFailures tables the four points at which a
// kill can land in atomicReplaceWindows's sequence (citadel#926) and asserts
// that in every case a complete binary remains recoverable at dst or
// dst+".old" -- never neither -- and that recoverInterruptedSwap restores dst
// when it's missing.
func TestAtomicReplaceWindows_InjectedFailures(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows swap/recovery semantics require native Windows handles and ACLs")
	}
	const oldVersion = "v2.0.0"
	const newVersion = "v2.1.0"

	cases := []struct {
		name           string
		failAt         string // windowsSwapFailAt value; "" = no injected failure (success path)
		expectErr      bool
		wantDstMissing bool // whether dst is expected to be missing immediately after the (failed) call
	}{
		{
			name:      "before_copy: killed before touching anything",
			failAt:    "before_copy",
			expectErr: true,
			// dst is completely untouched.
			wantDstMissing: false,
		},
		{
			name:      "after_copy: killed after staging .new, before first rename",
			failAt:    "after_copy",
			expectErr: true,
			// dst is completely untouched; .new is fully staged.
			wantDstMissing: false,
		},
		{
			name:      "after_rename1: killed between the two renames",
			failAt:    "after_rename1",
			expectErr: true,
			// dst was renamed to .old and not yet replaced -- the one real gap.
			wantDstMissing: true,
		},
		{
			name:           "after both renames: successful swap",
			failAt:         "",
			expectErr:      false,
			wantDstMissing: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, "citadel.new.download")
			dst := filepath.Join(dir, "citadel.exe")

			buildCitadelFixture(t, src, newVersion)
			buildCitadelFixture(t, dst, oldVersion)
			provisionTestMutationLock(t, dst)

			windowsSwapFailAt = tc.failAt
			defer func() { windowsSwapFailAt = "" }()

			err := atomicReplaceWindows(src, dst)

			if tc.expectErr && err == nil {
				t.Fatalf("expected an error for failAt=%q, got nil", tc.failAt)
			}
			if !tc.expectErr && err != nil {
				t.Fatalf("expected no error for the success path, got: %v", err)
			}

			_, statErr := os.Stat(dst)
			dstPresent := statErr == nil
			if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("stat destination: %v", statErr)
			}
			if dstPresent == tc.wantDstMissing {
				t.Fatalf("dst presence mismatch: present=%v, wantMissing=%v", dstPresent, tc.wantDstMissing)
			}

			// Core invariant: a complete binary must be recoverable at dst or
			// dst+".old" in every single case, including the interrupted ones.
			assertCompleteBinarySomewhere(t, dst, oldVersion, newVersion)

			// Now drive the auto-recovery helper and confirm it restores dst
			// whenever dst was left missing.
			recovered, recErr := recoverInterruptedSwap(dst)
			if recErr != nil {
				t.Fatalf("recoverInterruptedSwap returned an error: %v", recErr)
			}
			if tc.wantDstMissing && !recovered {
				t.Fatalf("expected recoverInterruptedSwap to report recovery, got recovered=false")
			}
			if !tc.wantDstMissing && recovered {
				t.Fatalf("expected recoverInterruptedSwap to be a no-op when dst was present, got recovered=true")
			}

			finalVersion, finalPresent := executableVersionIfExists(t, dst)
			if !finalPresent {
				t.Fatalf("dst missing after recovery attempt")
			}
			if finalVersion != oldVersion && finalVersion != newVersion {
				t.Fatalf("dst version after recovery is neither old nor new binary: %q", finalVersion)
			}

			// Specifically for the after_rename1 case, recovery should prefer
			// the fully-staged new binary (completing the intended update)
			// over silently rolling back to the old one.
			if tc.failAt == "after_rename1" && finalVersion != newVersion {
				t.Fatalf("expected recovery to prefer the staged new binary; got version %q", finalVersion)
			}
		})
	}
}

// TestRecoverInterruptedSwap_OnlyOldPresent pins the exact scenario named in
// citadel#926: a temp dir with only "citadel.old" present (dst itself
// missing, and no ".new" staging file left over -- e.g. it was already
// cleaned up by a previous successful run) must recover to a valid dst.
func TestRecoverInterruptedSwap_OnlyOldPresent(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows recovery requires native Windows handles and ACLs")
	}
	dir := t.TempDir()
	dst := filepath.Join(dir, "citadel.exe")
	oldPath := dst + ".old"

	buildCitadelFixture(t, dst, "v2.0.0")
	provisionTestMutationLock(t, dst)
	if err := os.Rename(dst, oldPath); err != nil {
		t.Fatal(err)
	}

	recovered, err := recoverInterruptedSwap(dst)
	if err != nil {
		t.Fatalf("recoverInterruptedSwap returned an error: %v", err)
	}
	if !recovered {
		t.Fatalf("expected recovery to report true")
	}

	version, present := executableVersionIfExists(t, dst)
	if !present {
		t.Fatalf("dst was not created by recovery")
	}
	if version != "v2.0.0" {
		t.Fatalf("unexpected recovered version: %q", version)
	}

	// .old should have been consumed by the rename (moved, not copied).
	if _, err := os.Stat(oldPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf(".old should have been renamed away, but still exists")
	}
}

func TestRecoverInterruptedSwap_OnlyNewPresent(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows recovery requires native Windows handles and ACLs")
	}
	dir := t.TempDir()
	dst := filepath.Join(dir, "citadel.exe")
	newPath := dst + ".new"
	buildCitadelFixture(t, dst, "v2.1.0")
	provisionTestMutationLock(t, dst)
	if err := os.Rename(dst, newPath); err != nil {
		t.Fatal(err)
	}

	recovered, err := recoverInterruptedSwap(dst)
	if err != nil || !recovered {
		t.Fatalf("new-only recovery = %v, %v", recovered, err)
	}
	if version, present := executableVersionIfExists(t, dst); !present || version != "v2.1.0" {
		t.Fatalf("new-only recovered version = %q, present=%v", version, present)
	}
	if _, err := os.Stat(newPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf(".new should have been renamed away: %v", err)
	}
}

// TestRecoverInterruptedSwap_NoBackupsAvailable asserts the honest failure
// mode when dst is missing and there is truly nothing to recover from.
func TestRecoverInterruptedSwap_NoBackupsAvailable(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows recovery requires native Windows handles and ACLs")
	}
	dir := t.TempDir()
	dst := filepath.Join(dir, "citadel.exe")
	buildCitadelFixture(t, dst, "v2.0.0")
	provisionTestMutationLock(t, dst)
	if err := os.Remove(dst); err != nil {
		t.Fatal(err)
	}

	recovered, err := recoverInterruptedSwap(dst)
	if err == nil {
		t.Fatalf("expected an error when no backup binaries are available")
	}
	if recovered {
		t.Fatalf("expected recovered=false alongside the error")
	}
}

func TestRecoverInterruptedSwap_RefusesMissingValidatedLock(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows recovery requires native Windows handles and ACLs")
	}
	dir := t.TempDir()
	dst := filepath.Join(dir, "citadel.exe")
	oldPath := dst + ".old"
	buildCitadelFixture(t, oldPath, "v2.0.0")

	recovered, err := recoverInterruptedSwap(dst)
	if err == nil || recovered {
		t.Fatalf("recovery without validated persistent lock = %v, %v", recovered, err)
	}
	if _, present := executableVersionIfExists(t, oldPath); !present {
		t.Fatal("refused recovery removed original backup")
	}
}

func TestRecoverInterruptedSwap_RefusesInvalidArtifactIdentity(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows recovery requires native Windows handles and ACLs")
	}
	dir := t.TempDir()
	dst := filepath.Join(dir, "citadel.exe")
	oldPath := dst + ".old"
	newPath := dst + ".new"
	buildCitadelFixture(t, dst, "v2.0.0")
	provisionTestMutationLock(t, dst)
	if err := os.Rename(dst, oldPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(newPath, 0o700); err != nil {
		t.Fatal(err)
	}

	recovered, err := recoverInterruptedSwap(dst)
	if err == nil || recovered {
		t.Fatalf("recovery with directory artifact = %v, %v", recovered, err)
	}
	if _, statErr := os.Stat(dst); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("refused recovery created destination: %v", statErr)
	}
	if _, present := executableVersionIfExists(t, oldPath); !present {
		t.Fatal("refused recovery removed original backup")
	}
}

// TestRecoverInterruptedSwap_NoOpWhenDstPresent asserts recovery never
// touches a healthy installation.
func TestRecoverInterruptedSwap_NoOpWhenDstPresent(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows recovery requires native Windows handles and ACLs")
	}
	dir := t.TempDir()
	dst := filepath.Join(dir, "citadel.exe")
	oldPath := dst + ".old"
	newPath := dst + ".new"

	buildCitadelFixture(t, dst, "v2.1.0")
	provisionTestMutationLock(t, dst)
	buildCitadelFixture(t, oldPath, "v2.0.0")
	buildCitadelFixture(t, newPath, "v2.2.0")

	recovered, err := recoverInterruptedSwap(dst)
	if err != nil {
		t.Fatalf("recoverInterruptedSwap returned an error: %v", err)
	}
	if recovered {
		t.Fatalf("expected no-op (recovered=false) when dst already exists")
	}

	// Nothing should have moved.
	if version, ok := executableVersionIfExists(t, dst); !ok || version != "v2.1.0" {
		t.Fatalf("dst was modified unexpectedly: version=%q present=%v", version, ok)
	}
	if version, ok := executableVersionIfExists(t, oldPath); !ok || version != "v2.0.0" {
		t.Fatalf(".old was modified unexpectedly: version=%q present=%v", version, ok)
	}
	if version, ok := executableVersionIfExists(t, newPath); !ok || version != "v2.2.0" {
		t.Fatalf(".new was modified unexpectedly: version=%q present=%v", version, ok)
	}
}

// TestRecoverInterruptedSwap_PrefersNewOverOld asserts the documented
// preference: when both dst+".new" (fully staged, complete) and dst+".old"
// (previous version) are present alongside a missing dst, recovery completes
// the interrupted update rather than silently rolling it back.
func TestRecoverInterruptedSwap_PrefersNewOverOld(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows recovery requires native Windows handles and ACLs")
	}
	dir := t.TempDir()
	dst := filepath.Join(dir, "citadel.exe")
	oldPath := dst + ".old"
	newPath := dst + ".new"

	buildCitadelFixture(t, dst, "v2.0.0")
	provisionTestMutationLock(t, dst)
	if err := os.Rename(dst, oldPath); err != nil {
		t.Fatal(err)
	}
	buildCitadelFixture(t, newPath, "v2.1.0")

	recovered, err := recoverInterruptedSwap(dst)
	if err != nil {
		t.Fatalf("recoverInterruptedSwap returned an error: %v", err)
	}
	if !recovered {
		t.Fatalf("expected recovery to report true")
	}

	version, present := executableVersionIfExists(t, dst)
	if !present {
		t.Fatalf("dst was not created by recovery")
	}
	if version != "v2.1.0" {
		t.Fatalf("expected recovery to prefer the new binary, got %q", version)
	}

	// .old is left untouched (not consumed) since .new was used instead.
	if version, ok := executableVersionIfExists(t, oldPath); !ok || version != "v2.0.0" {
		t.Fatalf(".old should have been left alone: version=%q present=%v", version, ok)
	}
}

// TestRecoverInterruptedSwap_ExposedWrapperIsWindowsScoped pins that the
// exported RecoverInterruptedSwap entry point (the one actually wired into
// startup / citadel update install / citadel update rollback) is a strict
// no-op on non-Windows platforms, regardless of on-disk state -- Unix's
// atomicReplaceUnix has no equivalent interrupted-swap window to recover
// from, and this must never touch files on Unix.
func TestRecoverInterruptedSwap_ExposedWrapperIsWindowsScoped(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("this test asserts non-Windows no-op behavior")
	}

	recovered, err := RecoverInterruptedSwap()
	if err != nil {
		t.Fatalf("expected no error on non-Windows, got: %v", err)
	}
	if recovered {
		t.Fatalf("expected no-op (recovered=false) on non-Windows")
	}
}
