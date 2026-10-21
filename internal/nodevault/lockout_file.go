package nodevault

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ErrLockoutUnavailable is operational, not a wrong secret or a lockout
// window. Callers must not grant access when a transaction cannot be recorded.
var ErrLockoutUnavailable = errors.New("nodevault: lockout transaction unavailable")

const lockoutLockWait = 3 * time.Second

// The persistent sidecar is never unlinked: state writes replace the YAML
// inode, and unlinking a lock would allow different processes to lock different
// inodes. Only the state transaction belongs here, never a secret check/KDF.
func withLockoutFile(path string, transaction func() error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("%w: %w", ErrLockoutUnavailable, err)
	}
	lockPath := path + ".lock"
	file, err := openLockoutFile(lockPath)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrLockoutUnavailable, err)
	}
	defer file.Close()
	if err := lockLockoutFile(file, time.Now().Add(lockoutLockWait)); err != nil {
		return fmt.Errorf("%w: %w", ErrLockoutUnavailable, err)
	}
	defer unlockLockoutFile(file)
	info, statErr := file.Stat()
	pathInfo, pathErr := os.Lstat(lockPath)
	if statErr != nil || pathErr != nil || !info.Mode().IsRegular() ||
		pathInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, pathInfo) {
		return ErrLockoutUnavailable
	}
	if err := transaction(); err != nil {
		return fmt.Errorf("%w: %w", ErrLockoutUnavailable, err)
	}
	return nil
}
