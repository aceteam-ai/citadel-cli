//go:build windows

package nodevault

import (
	"errors"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

const lockoutStatusSupported = true

func openLockoutFile(path string) (*os.File, error) {
	return openWindowsLockoutFile(path, windows.OPEN_ALWAYS)
}

func openExistingLockoutFile(path string) (*os.File, error) {
	return openWindowsLockoutFile(path, windows.OPEN_EXISTING)
}

func openWindowsLockoutFile(path string, disposition uint32) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	// The node directory's existing ACL remains the authority. Do not follow a
	// final reparse point; omitting SHARE_DELETE prevents pathname replacement
	// while any cooperating process owns an open lock handle.
	handle, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, disposition,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, err
	}
	var info windows.ByHandleFileInformation
	if windows.GetFileInformationByHandle(handle, &info) != nil || info.NumberOfLinks != 1 ||
		info.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 {
		_ = windows.CloseHandle(handle)
		return nil, ErrLockoutUnavailable
	}
	return os.NewFile(uintptr(handle), path), nil
}

func lockLockoutFile(file *os.File, deadline time.Time) error {
	for {
		var overlapped windows.Overlapped
		err := windows.LockFileEx(windows.Handle(file.Fd()),
			windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlapped)
		if err == nil {
			return nil
		}
		if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) && !errors.Is(err, windows.ERROR_IO_PENDING) {
			return err
		}
		if !time.Now().Before(deadline) {
			return ErrLockoutUnavailable
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func unlockLockoutFile(file *os.File) {
	var overlapped windows.Overlapped
	_ = windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &overlapped)
}
