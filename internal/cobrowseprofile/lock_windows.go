//go:build windows

package cobrowseprofile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// lockFile takes an exclusive, fail-immediately LockFileEx lock on path,
// creating it if needed. A held lock in any process, including this one
// through another handle, returns ErrProfileBusy. Windows releases the lock
// when the handle closes or the process exits. A reparse point at the final
// component is opened as itself, never followed, and refused; omitting
// FILE_SHARE_DELETE stops the name from being replaced while the lock is held.
func lockFile(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("cobrowseprofile: create profile dir: %w", err)
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_ALWAYS,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, fmt.Errorf("cobrowseprofile: open profile lock: %w", err)
	}
	var info windows.ByHandleFileInformation
	if windows.GetFileInformationByHandle(h, &info) != nil ||
		info.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 {
		_ = windows.CloseHandle(h)
		return nil, fmt.Errorf("cobrowseprofile: profile lock %s is not a regular file", path)
	}
	var ol windows.Overlapped
	if err := windows.LockFileEx(h, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &ol); err != nil {
		_ = windows.CloseHandle(h)
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) {
			return nil, ErrProfileBusy
		}
		return nil, fmt.Errorf("cobrowseprofile: lock profile: %w", err)
	}
	return func() {
		var ol windows.Overlapped
		_ = windows.UnlockFileEx(h, 0, 1, 0, &ol)
		_ = windows.CloseHandle(h)
	}, nil
}
