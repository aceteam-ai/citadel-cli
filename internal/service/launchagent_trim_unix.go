//go:build !windows

package service

import (
	"os"
	"syscall"
)

// truncateIfOversize empties path when it is a regular file over max bytes
// (best effort). It opens with O_NOFOLLOW and O_NONBLOCK and truncates through
// the descriptor, so a symlink or FIFO planted at the log path is never
// followed or waited on.
func truncateIfOversize(path string, max int64) {
	fd, err := syscall.Open(path, syscall.O_WRONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	if fi, err := f.Stat(); err == nil && fi.Mode().IsRegular() && fi.Size() > max {
		_ = f.Truncate(0)
	}
}
