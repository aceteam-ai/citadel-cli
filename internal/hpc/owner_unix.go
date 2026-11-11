//go:build !windows

package hpc

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// ownerUID returns the numeric owner of a file from its stat result.
func ownerUID(info os.FileInfo) (int, error) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return -1, fmt.Errorf("hpc profile: cannot read owner of %s", info.Name())
	}
	return int(st.Uid), nil
}

// openNoFollow opens path read-only without following a final symlink and
// without blocking on a FIFO. The caller must fstat the result.
func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}

// isSymlinkOpenError reports whether err is the O_NOFOLLOW refusal.
func isSymlinkOpenError(err error) bool { return errors.Is(err, syscall.ELOOP) }
