//go:build windows

package worklock

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func nativeContentionProbeOps() contentionProbeOps {
	return contentionProbeOps{
		lstat: func(path string) (os.FileInfo, error) {
			info, err := os.Lstat(path)
			if err != nil {
				return nil, err
			}
			// Windows Lstat defers its file-ID lookup until SameFile. Resolve
			// it now, not after opening the probe, to retain this observation's
			// identity rather than resolving both snapshots at a later path.
			if !info.Mode().IsRegular() || !os.SameFile(info, info) {
				return nil, errContentionProbeUnavailable
			}
			return info, nil
		},
		open: func(path string) (contentionProbeFile, error) {
			name, err := windows.UTF16PtrFromString(path)
			if err != nil {
				return nil, err
			}
			// Existing ACLs remain authoritative. No SHARE_DELETE: a cooperating
			// pathname replacement cannot race the observation while open.
			handle, err := windows.CreateFile(name, windows.GENERIC_READ,
				windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING,
				windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
			if err != nil {
				return nil, err
			}
			file := os.NewFile(uintptr(handle), path)
			var info windows.ByHandleFileInformation
			kind, kindErr := windows.GetFileType(handle)
			if kindErr != nil || kind != windows.FILE_TYPE_DISK || windows.GetFileInformationByHandle(handle, &info) != nil ||
				info.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 {
				_ = file.Close()
				return nil, errContentionProbeUnavailable
			}
			return file, nil
		},
		sameFile: os.SameFile,
		lock: func(file contentionProbeFile) error {
			var overlapped windows.Overlapped
			return windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlapped)
		},
		unlock: func(file contentionProbeFile) error {
			var overlapped windows.Overlapped
			return windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &overlapped)
		},
		isContended: func(err error) bool { return errors.Is(err, windows.ERROR_LOCK_VIOLATION) },
	}
}
