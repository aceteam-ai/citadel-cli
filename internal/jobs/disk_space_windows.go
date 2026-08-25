//go:build windows

package jobs

import "golang.org/x/sys/windows"

// defaultAvailableDiskBytes returns free space available to the calling user
// on path's volume via GetDiskFreeSpaceEx (the Windows equivalent of statfs
// Bavail*Bsize used on Unix in disk_space_unix.go).
func defaultAvailableDiskBytes(path string) (uint64, error) {
	dir, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var freeBytesAvailable, totalBytes, totalFreeBytes uint64
	if err := windows.GetDiskFreeSpaceEx(dir, &freeBytesAvailable, &totalBytes, &totalFreeBytes); err != nil {
		return 0, err
	}
	return freeBytesAvailable, nil
}
