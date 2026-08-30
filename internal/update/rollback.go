// internal/update/rollback.go
// Binary management and rollback for auto-update
package update

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

// GetCurrentBinaryPath returns the path to the currently running binary
func GetCurrentBinaryPath() (string, error) {
	execPath, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("failed to get executable path: %w", err)
	}

	// Resolve symlinks
	resolvedPath, err := filepath.EvalSymlinks(execPath)
	if err != nil {
		return execPath, nil
	}

	return resolvedPath, nil
}

// BackupCurrent copies the current binary to citadel.previous
func BackupCurrent() error {
	currentPath, err := GetCurrentBinaryPath()
	if err != nil {
		return err
	}

	previousPath := GetPreviousBinaryPath()

	if err := EnsureUpdateDir(); err != nil {
		return err
	}

	return copyFile(currentPath, previousPath)
}

// ApplyUpdate replaces the current binary with the new one
// Automatically rolls back if the new binary fails validation
func ApplyUpdate(newBinaryPath string) error {
	currentPath, err := GetCurrentBinaryPath()
	if err != nil {
		return fmt.Errorf("failed to get current binary path: %w", err)
	}

	// 1. Backup current binary
	if err := BackupCurrent(); err != nil {
		return fmt.Errorf("failed to backup current binary: %w", err)
	}

	// 2. Replace binary (platform-specific)
	if runtime.GOOS == "windows" {
		if err := atomicReplaceWindows(newBinaryPath, currentPath); err != nil {
			if rollbackErr := Rollback(); rollbackErr != nil {
				return fmt.Errorf("replace failed (%w) and rollback failed (%w)", err, rollbackErr)
			}
			return fmt.Errorf("replace failed, rolled back: %w", err)
		}
	} else {
		if err := atomicReplaceUnix(newBinaryPath, currentPath); err != nil {
			if rollbackErr := Rollback(); rollbackErr != nil {
				return fmt.Errorf("replace failed (%w) and rollback failed (%w)", err, rollbackErr)
			}
			return fmt.Errorf("replace failed, rolled back: %w", err)
		}
	}

	// 3. Validate new binary
	if err := ValidateBinary(currentPath); err != nil {
		if rollbackErr := Rollback(); rollbackErr != nil {
			return fmt.Errorf("validation failed (%w) and rollback failed (%w)", err, rollbackErr)
		}
		return fmt.Errorf("new binary failed validation, rolled back: %w", err)
	}

	// 4. Clean up pending binary
	os.Remove(newBinaryPath)

	return nil
}

// Rollback restores the previous binary
func Rollback() error {
	previousPath := GetPreviousBinaryPath()

	// Check if previous binary exists
	if _, err := os.Stat(previousPath); os.IsNotExist(err) {
		return fmt.Errorf("no previous binary found at %s", previousPath)
	}

	currentPath, err := GetCurrentBinaryPath()
	if err != nil {
		return fmt.Errorf("failed to get current binary path: %w", err)
	}

	// Restore previous binary
	if runtime.GOOS == "windows" {
		return atomicReplaceWindows(previousPath, currentPath)
	}
	return atomicReplaceUnix(previousPath, currentPath)
}

// ValidateBinary runs the binary with --version to check if it's working
func ValidateBinary(path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, path, "version")
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("binary validation failed: %w", err)
	}

	return nil
}

// HasPreviousVersion returns true if a previous binary exists for rollback
func HasPreviousVersion() bool {
	previousPath := GetPreviousBinaryPath()
	_, err := os.Stat(previousPath)
	return err == nil
}

// GetPreviousVersionInfo returns information about the previous binary if available
func GetPreviousVersionInfo() (string, error) {
	previousPath := GetPreviousBinaryPath()

	if _, err := os.Stat(previousPath); os.IsNotExist(err) {
		return "", fmt.Errorf("no previous version available")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, previousPath, "version")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to get previous version: %w", err)
	}

	return string(output), nil
}

// atomicReplaceUnix replaces a binary atomically on Unix systems
func atomicReplaceUnix(src, dst string) error {
	// On Unix, rename is atomic within same filesystem
	// First copy to temp in same dir, then rename
	tmpPath := dst + ".tmp"

	if err := copyFile(src, tmpPath); err != nil {
		return err
	}

	if err := os.Chmod(tmpPath, 0755); err != nil {
		os.Remove(tmpPath)
		return err
	}

	if err := os.Rename(tmpPath, dst); err != nil {
		os.Remove(tmpPath)
		return err
	}

	return nil
}

// windowsSwapFailAt lets tests simulate a process kill at a specific point in
// atomicReplaceWindows's sequence (without a real subprocess kill): the
// function returns early with a synthetic error, leaving on disk exactly
// what a real kill at that point would have left, since every step up to
// that point already fully completed. Empty in production; never set outside
// tests in this package.
//
// Valid values: "before_copy", "after_copy", "after_rename1".
var windowsSwapFailAt string

// atomicReplaceWindows replaces a binary on Windows. Windows locks running
// executables, so dst cannot be overwritten directly the way atomicReplaceUnix
// overwrites via a same-directory rename -- the running process holds dst open.
//
// citadel#926: the copy of src happens ENTIRELY into a same-directory staging
// file (dst+".new") before either rename touches dst or dst+".old" at all, so
// a kill during (or before) the copy leaves dst completely untouched -- no
// window in which dst is missing or partially written. Only two renames
// remain, both metadata-only operations on the same directory/filesystem and
// therefore atomic (they either fully apply or not at all -- no partial-file
// state). The only interrupted state possible is a kill BETWEEN the two
// renames: dst momentarily doesn't exist, but both dst+".old" (the previous,
// complete binary) and dst+".new" (the new, complete binary -- copy already
// finished before this window opened) are present and each independently a
// complete, valid binary. recoverInterruptedSwap (below) detects and repairs
// exactly that state.
func atomicReplaceWindows(src, dst string) error {
	oldPath := dst + ".old"
	newPath := dst + ".new"

	// Clean up any leftover staging file from a previous failed attempt.
	os.Remove(newPath)

	if windowsSwapFailAt == "before_copy" {
		return fmt.Errorf("simulated failure before copy")
	}

	// 1. Copy the new binary into a temp file in the SAME directory as dst,
	//    fully, before dst or dst+".old" are touched at all.
	if err := copyFile(src, newPath); err != nil {
		os.Remove(newPath)
		return fmt.Errorf("failed to stage new binary: %w", err)
	}
	if err := os.Chmod(newPath, 0755); err != nil {
		os.Remove(newPath)
		return fmt.Errorf("failed to set permissions on staged binary: %w", err)
	}

	if windowsSwapFailAt == "after_copy" {
		return fmt.Errorf("simulated failure after copy")
	}

	// 2. Move the current binary out of the way.
	os.Remove(oldPath) // Remove any existing .old
	if err := os.Rename(dst, oldPath); err != nil && !os.IsNotExist(err) {
		// dst is untouched; the staged copy is no longer needed.
		os.Remove(newPath)
		return fmt.Errorf("failed to rename current binary: %w", err)
	}

	if windowsSwapFailAt == "after_rename1" {
		return fmt.Errorf("simulated failure between renames")
	}

	// 3. Move the staged new binary into place.
	if err := os.Rename(newPath, dst); err != nil {
		// Restore the previous binary so dst is never left empty.
		os.Rename(oldPath, dst)
		return fmt.Errorf("failed to install new binary: %w", err)
	}

	// Windows may still have a lock on the .old file (e.g. the running
	// process was renamed out from under itself), so we don't remove it
	// here; it is cleaned up on next update or restart (CleanupOldBinaries).

	return nil
}

// recoverInterruptedSwap detects and repairs the on-disk state left behind by
// an atomicReplaceWindows call that was interrupted between renaming the
// running binary to dst+".old" and renaming the staged dst+".new" into place
// at dst -- the only window in which dst can be completely missing (see
// atomicReplaceWindows's doc comment).
//
// It is a no-op when dst already exists (nothing to recover) or an error when
// neither backup exists (nothing to recover FROM). It prefers dst+".new" --
// the fully-staged, intended update -- over dst+".old" -- the previous
// version -- because a ".new" file only ever exists once it has been
// completely copied and chmod'd, so recovering from it COMPLETES the
// interrupted update rather than silently rolling it back to the old version.
// Returns (false, nil) when dst was already present and nothing needed to be
// done.
func recoverInterruptedSwap(dst string) (bool, error) {
	if _, err := os.Stat(dst); err == nil {
		return false, nil // dst present; nothing to recover
	} else if !os.IsNotExist(err) {
		return false, fmt.Errorf("failed to stat %s: %w", dst, err)
	}

	newPath := dst + ".new"
	oldPath := dst + ".old"

	if _, err := os.Stat(newPath); err == nil {
		if err := os.Rename(newPath, dst); err != nil {
			return false, fmt.Errorf("failed to recover %s from %s: %w", dst, newPath, err)
		}
		return true, nil
	}

	if _, err := os.Stat(oldPath); err == nil {
		if err := os.Rename(oldPath, dst); err != nil {
			return false, fmt.Errorf("failed to recover %s from %s: %w", dst, oldPath, err)
		}
		return true, nil
	}

	return false, fmt.Errorf("no backup binary found to recover %s (checked %s and %s)", dst, newPath, oldPath)
}

// RecoverInterruptedSwap checks for and repairs an interrupted Windows binary
// swap (citadel#926): a process kill between atomicReplaceWindows's two
// renames can leave no binary at all at the current executable's path, with
// the complete previous or staged-new binary sitting at ".old"/".new"
// instead. Windows-scoped: always a no-op (false, nil) on every other OS,
// since atomicReplaceUnix's single-rename swap has no equivalent interrupted
// state to recover from. Safe to call unconditionally at process startup --
// it only acts when the current binary's path is missing on disk.
func RecoverInterruptedSwap() (bool, error) {
	if runtime.GOOS != "windows" {
		return false, nil
	}
	dst, err := GetCurrentBinaryPath()
	if err != nil {
		return false, fmt.Errorf("failed to resolve current binary path: %w", err)
	}
	return recoverInterruptedSwap(dst)
}

// copyFile copies a file from src to dst
func copyFile(src, dst string) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	srcInfo, err := srcFile.Stat()
	if err != nil {
		return err
	}

	dstFile, err := os.OpenFile(dst, os.O_RDWR|os.O_CREATE|os.O_TRUNC, srcInfo.Mode())
	if err != nil {
		return err
	}
	defer dstFile.Close()

	if _, err := io.Copy(dstFile, srcFile); err != nil {
		return err
	}

	return dstFile.Sync()
}

// CleanupOldBinaries removes old/temporary binary files
func CleanupOldBinaries() error {
	currentPath, err := GetCurrentBinaryPath()
	if err != nil {
		return nil // Ignore errors in cleanup
	}

	// Clean up .old files (Windows leftovers)
	oldPath := currentPath + ".old"
	os.Remove(oldPath)

	// Clean up .new files (Windows staging leftovers, citadel#926)
	newPath := currentPath + ".new"
	os.Remove(newPath)

	// Clean up .tmp files
	tmpPath := currentPath + ".tmp"
	os.Remove(tmpPath)

	return nil
}
