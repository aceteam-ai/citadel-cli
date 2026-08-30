// internal/update/rollback_test.go
package update

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// writeTestBinary writes content to path, creating parent dirs as needed.
func writeTestBinary(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("failed to create dir for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0755); err != nil {
		t.Fatalf("failed to write %s: %v", path, err)
	}
}

// readIfExists returns (content, true) if path exists, else ("", false).
func readIfExists(t *testing.T, path string) (string, bool) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false
		}
		t.Fatalf("failed to stat/read %s: %v", path, err)
	}
	return string(data), true
}

// assertCompleteBinarySomewhere is the core citadel#926 invariant: after any
// interruption of the swap sequence, a complete binary (either the old or the
// new content -- both are valid, complete binaries) must be recoverable at
// either dst or dst+".old". It must never be the case that neither holds a
// complete binary.
func assertCompleteBinarySomewhere(t *testing.T, dst, oldContent, newContent string) {
	t.Helper()
	dstContent, dstOK := readIfExists(t, dst)
	oldPathContent, oldOK := readIfExists(t, dst+".old")

	if dstOK && (dstContent == oldContent || dstContent == newContent) {
		return
	}
	if oldOK && (oldPathContent == oldContent || oldPathContent == newContent) {
		return
	}
	t.Fatalf("no complete binary recoverable at %s or %s.old (dst=%q[present=%v], old=%q[present=%v])",
		dst, dst, dstContent, dstOK, oldPathContent, oldOK)
}

// TestAtomicReplaceWindows_InjectedFailures tables the four points at which a
// kill can land in atomicReplaceWindows's sequence (citadel#926) and asserts
// that in every case a complete binary remains recoverable at dst or
// dst+".old" -- never neither -- and that recoverInterruptedSwap restores dst
// when it's missing.
func TestAtomicReplaceWindows_InjectedFailures(t *testing.T) {
	const oldContent = "old-binary-content-v1"
	const newContent = "new-binary-content-v2-longer-than-v1"

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

			writeTestBinary(t, src, newContent)
			writeTestBinary(t, dst, oldContent)

			windowsSwapFailAt = tc.failAt
			defer func() { windowsSwapFailAt = "" }()

			err := atomicReplaceWindows(src, dst)

			if tc.expectErr && err == nil {
				t.Fatalf("expected an error for failAt=%q, got nil", tc.failAt)
			}
			if !tc.expectErr && err != nil {
				t.Fatalf("expected no error for the success path, got: %v", err)
			}

			_, dstPresent := readIfExists(t, dst)
			if dstPresent == tc.wantDstMissing {
				t.Fatalf("dst presence mismatch: present=%v, wantMissing=%v", dstPresent, tc.wantDstMissing)
			}

			// Core invariant: a complete binary must be recoverable at dst or
			// dst+".old" in every single case, including the interrupted ones.
			assertCompleteBinarySomewhere(t, dst, oldContent, newContent)

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

			finalContent, finalPresent := readIfExists(t, dst)
			if !finalPresent {
				t.Fatalf("dst missing after recovery attempt")
			}
			if finalContent != oldContent && finalContent != newContent {
				t.Fatalf("dst content after recovery is neither old nor new binary: %q", finalContent)
			}

			// Specifically for the after_rename1 case, recovery should prefer
			// the fully-staged new binary (completing the intended update)
			// over silently rolling back to the old one.
			if tc.failAt == "after_rename1" && finalContent != newContent {
				t.Fatalf("expected recovery to prefer the staged new binary; got content %q", finalContent)
			}
		})
	}
}

// TestRecoverInterruptedSwap_OnlyOldPresent pins the exact scenario named in
// citadel#926: a temp dir with only "citadel.old" present (dst itself
// missing, and no ".new" staging file left over -- e.g. it was already
// cleaned up by a previous successful run) must recover to a valid dst.
func TestRecoverInterruptedSwap_OnlyOldPresent(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "citadel.exe")
	oldPath := dst + ".old"

	writeTestBinary(t, oldPath, "previous-version-binary")

	recovered, err := recoverInterruptedSwap(dst)
	if err != nil {
		t.Fatalf("recoverInterruptedSwap returned an error: %v", err)
	}
	if !recovered {
		t.Fatalf("expected recovery to report true")
	}

	content, present := readIfExists(t, dst)
	if !present {
		t.Fatalf("dst was not created by recovery")
	}
	if content != "previous-version-binary" {
		t.Fatalf("unexpected recovered content: %q", content)
	}

	// .old should have been consumed by the rename (moved, not copied).
	if _, stillPresent := readIfExists(t, oldPath); stillPresent {
		t.Fatalf(".old should have been renamed away, but still exists")
	}
}

// TestRecoverInterruptedSwap_NoBackupsAvailable asserts the honest failure
// mode when dst is missing and there is truly nothing to recover from.
func TestRecoverInterruptedSwap_NoBackupsAvailable(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "citadel.exe")

	recovered, err := recoverInterruptedSwap(dst)
	if err == nil {
		t.Fatalf("expected an error when no backup binaries are available")
	}
	if recovered {
		t.Fatalf("expected recovered=false alongside the error")
	}
}

// TestRecoverInterruptedSwap_NoOpWhenDstPresent asserts recovery never
// touches a healthy installation.
func TestRecoverInterruptedSwap_NoOpWhenDstPresent(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "citadel.exe")
	oldPath := dst + ".old"
	newPath := dst + ".new"

	writeTestBinary(t, dst, "current-binary")
	writeTestBinary(t, oldPath, "old-binary")
	writeTestBinary(t, newPath, "new-binary")

	recovered, err := recoverInterruptedSwap(dst)
	if err != nil {
		t.Fatalf("recoverInterruptedSwap returned an error: %v", err)
	}
	if recovered {
		t.Fatalf("expected no-op (recovered=false) when dst already exists")
	}

	// Nothing should have moved.
	if content, ok := readIfExists(t, dst); !ok || content != "current-binary" {
		t.Fatalf("dst was modified unexpectedly: content=%q present=%v", content, ok)
	}
	if content, ok := readIfExists(t, oldPath); !ok || content != "old-binary" {
		t.Fatalf(".old was modified unexpectedly: content=%q present=%v", content, ok)
	}
	if content, ok := readIfExists(t, newPath); !ok || content != "new-binary" {
		t.Fatalf(".new was modified unexpectedly: content=%q present=%v", content, ok)
	}
}

// TestRecoverInterruptedSwap_PrefersNewOverOld asserts the documented
// preference: when both dst+".new" (fully staged, complete) and dst+".old"
// (previous version) are present alongside a missing dst, recovery completes
// the interrupted update rather than silently rolling it back.
func TestRecoverInterruptedSwap_PrefersNewOverOld(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "citadel.exe")
	oldPath := dst + ".old"
	newPath := dst + ".new"

	writeTestBinary(t, oldPath, "old-binary")
	writeTestBinary(t, newPath, "new-binary")

	recovered, err := recoverInterruptedSwap(dst)
	if err != nil {
		t.Fatalf("recoverInterruptedSwap returned an error: %v", err)
	}
	if !recovered {
		t.Fatalf("expected recovery to report true")
	}

	content, present := readIfExists(t, dst)
	if !present {
		t.Fatalf("dst was not created by recovery")
	}
	if content != "new-binary" {
		t.Fatalf("expected recovery to prefer the new binary, got %q", content)
	}

	// .old is left untouched (not consumed) since .new was used instead.
	if content, ok := readIfExists(t, oldPath); !ok || content != "old-binary" {
		t.Fatalf(".old should have been left alone: content=%q present=%v", content, ok)
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
