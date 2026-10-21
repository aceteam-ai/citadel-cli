//go:build linux

package mediainput

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func openWorkspaceInput(root *os.Root, path string) (*os.File, error) {
	if !validPath(path) {
		return nil, os.ErrInvalid
	}
	// os.Root resolves safe parents but may resolve a symlink leaf itself,
	// including after an O_NOFOLLOW ELOOP. Anchor the parent with os.Root,
	// then make the leaf's no-follow rule an actual kernel openat operation.
	parent, err := root.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	dir, err := parent.Open(".")
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	fd, err := unix.Openat(int(dir.Fd()), filepath.Base(path), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), "private media input"), nil
}
