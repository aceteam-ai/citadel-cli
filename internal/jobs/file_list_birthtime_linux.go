//go:build linux

package jobs

import (
	"time"

	"golang.org/x/sys/unix"
)

func fileBirthTime(path string) (time.Time, bool) {
	var stat unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, path, unix.AT_SYMLINK_NOFOLLOW, unix.STATX_BTIME, &stat); err != nil || stat.Mask&unix.STATX_BTIME == 0 {
		return time.Time{}, false
	}
	return time.Unix(stat.Btime.Sec, int64(stat.Btime.Nsec)), true
}
