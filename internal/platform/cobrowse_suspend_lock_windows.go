//go:build windows

package platform

import (
	"golang.org/x/sys/windows"
	"os"
)

// Match the existing work/profile locks: fail immediately rather than waiting
// behind an old owner. Closing the handle releases this stable inode's lock.
func lockSuspendManifestStore(f *os.File) error {
	var overlapped windows.Overlapped
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlapped)
}
