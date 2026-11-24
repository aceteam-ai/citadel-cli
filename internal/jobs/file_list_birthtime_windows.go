//go:build windows

package jobs

import (
	"os"
	"syscall"
	"time"
)

func fileBirthTime(path string) (time.Time, bool) {
	info, err := os.Lstat(path)
	if err != nil {
		return time.Time{}, false
	}
	stat, ok := info.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		return time.Time{}, false
	}
	if stat.CreationTime.HighDateTime == 0 && stat.CreationTime.LowDateTime == 0 {
		return time.Time{}, false
	}
	return time.Unix(0, stat.CreationTime.Nanoseconds()), true
}
