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

// atomicReplaceWindows replaces a binary on Windows
// Windows locks running executables, so we use a rename workaround
func atomicReplaceWindows(src, dst string) error {
	// 1. Rename current to .old
	oldPath := dst + ".old"
	os.Remove(oldPath) // Remove any existing .old

	// Try to rename the current binary
	if err := os.Rename(dst, oldPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to rename current binary: %w", err)
	}

	// 2. Copy new to dst
	if err := copyFile(src, dst); err != nil {
		// Rollback: restore old binary
		os.Rename(oldPath, dst)
		return fmt.Errorf("failed to copy new binary: %w", err)
	}

	// 3. Schedule cleanup of .old (it will be cleaned up on next update or restart)
	// Windows may still have a lock on the .old file, so we don't remove it here

	return nil
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

	// Clean up .tmp files
	tmpPath := currentPath + ".tmp"
	os.Remove(tmpPath)

	return nil
}
