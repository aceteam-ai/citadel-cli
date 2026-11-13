//go:build !windows

package localcobrowse

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

const ownerLockFile = "local-cobrowse.lock"

// acquireOSLock takes an exclusive, non-blocking flock. The kernel drops it when
// the process exits, so a crashed owner never leaves a stale lock.
func acquireOSLock(dir string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(dir, ownerLockFile), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			// Another account's process created the lock file: it owns
			// co-browse in this directory.
			return nil, ownerErrorFrom(dir)
		}
		return nil, fmt.Errorf("open owner lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ownerErrorFrom(dir)
		}
		return nil, fmt.Errorf("lock owner file: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
