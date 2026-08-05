//go:build !windows

package nvr

import (
	"path/filepath"
	"syscall"
)

// isMountpoint reports whether path has its own filesystem mounted, by comparing
// its st_dev against its parent's. This is the HOST-side check: a failed NFS
// mount leaves a plain local directory whose st_dev matches its parent.
func isMountpoint(path string) (bool, error) {
	var st, parent syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return false, err
	}
	if err := syscall.Stat(filepath.Dir(path), &parent); err != nil {
		return false, err
	}
	return st.Dev != parent.Dev, nil
}

// fsType returns the filesystem magic number for path (statfs f_type). This is
// what the CONTAINER-side check must use: inside the frigate container /media is
// always a bind mount, so st_dev always differs from its parent and an
// isMountpoint check there is a false pass. Widened to int64 because f_type is
// int64 on Linux and uint32 on darwin.
func fsType(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Type), nil
}
