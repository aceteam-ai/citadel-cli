//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package memory

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// openRegularTranscript opens without following links and with nonblocking
// FIFO/device semantics, then proves the descriptor is the same regular file
// currently named by path. A hostile hook input therefore cannot block us in
// open(2) or race a checked path into a different object.
func openRegularTranscript(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	info, statErr := f.Stat()
	pathInfo, lstatErr := os.Lstat(path)
	if statErr != nil || lstatErr != nil || !info.Mode().IsRegular() ||
		pathInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, pathInfo) {
		_ = f.Close()
		return nil, fmt.Errorf("transcript is not a stable regular file")
	}
	return f, nil
}
