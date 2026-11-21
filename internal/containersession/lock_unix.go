//go:build !windows

package containersession

import (
	"errors"
	"os"
	"syscall"
)

// acquireLock takes the per-session advisory lock on a file in the host run
// dir, which is never mounted into the container. It never blocks: a second
// wake, sleep or re-seal for the same session is refused or skipped.
func acquireLock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, ErrRunDir
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrBusy
		}
		return nil, ErrRunDir
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
}
