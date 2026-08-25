// internal/jobs/disk_space.go
//
// Free-disk preflight for MODEL_CACHE_PULL (citadel #828). A 2026-08-25
// incident: pulling Lightricks/LTX-Video via the unfiltered HuggingFace
// snapshot path (pullHuggingFace) grabbed ~161GB (13 sibling checkpoints) and
// drove a node's disk to 97%. This file holds the disk-space half of the fix
// (see model_cache_pull_patterns.go and hf_repo_size.go for the other half:
// selecting only the files actually needed).
//
// Design: the decision (planDiskPreflight) is pure and takes already-resolved
// numbers, so it needs no disk/network access to unit-test. The only I/O is
// availableDiskBytesFn (platform statfs/GetDiskFreeSpaceEx, disk_space_unix.go
// / disk_space_windows.go) and the HF metadata fetch in hf_repo_size.go — both
// are package-var funcs so callers (and tests) can inject fakes.
package jobs

import (
	"fmt"
	"os"
	"path/filepath"
)

// availableDiskBytesFn resolves free space at a directory. Overridable for
// tests; production wiring is the platform-specific defaultAvailableDiskBytes.
var availableDiskBytesFn = defaultAvailableDiskBytes

// diskSafetyMarginBytes is the default headroom required ABOVE the estimated
// download size before a pull is allowed to proceed. A download writes
// partial/resume files during the pull and the on-disk layout can exceed the
// summed repo file sizes slightly, so this is not just rounding slop.
// Overridable per-job via the payload's `min_free_bytes` field.
const diskSafetyMarginBytes int64 = 2 << 30 // 2 GiB

// nearestExistingDir walks up from path until it finds a directory that
// exists, so a free-space check still works before the destination cache dir
// has been created by a first-ever download (statfs/GetDiskFreeSpaceEx both
// require an existing path).
func nearestExistingDir(path string) string {
	for {
		if fi, err := os.Stat(path); err == nil && fi.IsDir() {
			return path
		}
		parent := filepath.Dir(path)
		if parent == path {
			return path
		}
		path = parent
	}
}

// planDiskPreflight is the pure decision at the heart of #828's fix: does
// availableBytes cover requiredBytes plus marginBytes? Deliberately separated
// from all I/O (no disk/network access) so it is trivially unit-tested against
// both outcomes -- proceeds when it fits, fails closed (downloading NOTHING)
// with a clear message when it doesn't.
func planDiskPreflight(dir string, requiredBytes, availableBytes, marginBytes int64) error {
	if requiredBytes < 0 {
		requiredBytes = 0
	}
	if marginBytes < 0 {
		marginBytes = 0
	}
	needed := requiredBytes + marginBytes
	if needed < requiredBytes {
		// int64 overflow guard; requiredBytes alone is already enormous, so drop
		// the margin rather than wrapping to a small/negative "needed".
		needed = requiredBytes
	}
	if availableBytes < needed {
		return fmt.Errorf(
			"insufficient disk space at %s: need %s (%s estimated download + %s safety margin) but only %s free — downloading nothing",
			dir, humanBytes(needed), humanBytes(requiredBytes), humanBytes(marginBytes), humanBytes(availableBytes),
		)
	}
	return nil
}

// humanBytes renders a byte count as a short human-readable string (e.g.
// "18.7 GiB"), used only in preflight log/error messages.
func humanBytes(n int64) string {
	if n < 0 {
		return "-" + humanBytes(-n)
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for n2 := n / unit; n2 >= unit; n2 /= unit {
		div *= unit
		exp++
	}
	units := "KMGTPE"
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), units[exp])
}
