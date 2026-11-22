//go:build linux

package containerjobs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func trustedHostDirectory(uid uint32, mode uint32) bool {
	return uid == 0 && mode&unix.S_IFMT == unix.S_IFDIR && mode&0022 == 0
}

// Walk from the root FD, refusing links and untrusted ancestors before mkdir.
// A non-root owner cannot rename any verified ancestor to redirect later native
// commands or Docker's bind-source lookup. Never repair an unsafe directory.
func openContainerHostRoot(path string) (*os.File, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return nil, ErrUnavailable
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, ErrUnavailable
	}
	current := os.NewFile(uintptr(fd), "/")
	defer func() {
		if current != nil {
			_ = current.Close()
		}
	}()
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for i, part := range parts {
		var parent unix.Stat_t
		if unix.Fstat(int(current.Fd()), &parent) != nil || !trustedHostDirectory(parent.Uid, parent.Mode) {
			return nil, ErrUnavailable
		}
		next, err := unix.Openat(int(current.Fd()), part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(err, unix.ENOENT) {
			if err := unix.Mkdirat(int(current.Fd()), part, 0700); err != nil && !errors.Is(err, unix.EEXIST) {
				return nil, ErrUnavailable
			}
			next, err = unix.Openat(int(current.Fd()), part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		}
		if err != nil {
			return nil, ErrUnavailable
		}
		child := os.NewFile(uintptr(next), part)
		var stat unix.Stat_t
		if unix.Fstat(next, &stat) != nil || !trustedHostDirectory(stat.Uid, stat.Mode) || i == len(parts)-1 && stat.Mode&0077 != 0 {
			child.Close()
			return nil, ErrUnavailable
		}
		current.Close()
		current = child
	}
	result := current
	current = nil
	return result, nil
}
