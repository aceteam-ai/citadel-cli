//go:build !windows

package jobs

import "syscall"

// defaultAvailableDiskBytes returns free space available to an unprivileged
// user on path's filesystem (statfs Bavail*Bsize), mirroring the pattern
// already used in internal/nvr/storage_unix.go (syscall.Statfs, no external
// dependency). Works on both Linux and Darwin: Bavail/Bsize field widths differ
// per platform (int64 vs uint32) but both cast cleanly to uint64.
func defaultAvailableDiskBytes(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}
