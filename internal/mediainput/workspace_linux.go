//go:build linux

package mediainput

import (
	"os"
	"path/filepath"
	"strconv"

	"golang.org/x/sys/unix"
)

func openWorkspaceInput(root *os.Root, path string) (*os.File, error) {
	return openWorkspaceInputWithOps(root, path, workspaceOpenOps{
		open: unix.Open, openat: unix.Openat, fstat: unix.Fstat, fstatfs: unix.Fstatfs,
	})
}

// A per-call syscall seam lets owned fixtures observe acquisition order and
// synthesize device types without creating or opening a real device. There is
// no mutable package-level hook or caller-controlled procfs path.
type workspaceOpenOps struct {
	open    func(string, int, uint32) (int, error)
	openat  func(int, string, int, uint32) (int, error)
	fstat   func(int, *unix.Stat_t) error
	fstatfs func(int, *unix.Statfs_t) error
}

func openWorkspaceInputWithOps(root *os.Root, path string, ops workspaceOpenOps) (*os.File, error) {
	if !ValidPath(path) {
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
	// O_PATH does not invoke the leaf's readable open (and therefore a device
	// driver's open). O_NOFOLLOW pins a symlink itself; fstat must reject it.
	pin, err := ops.openat(int(dir.Fd()), filepath.Base(path), unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(pin)
	var pinned unix.Stat_t
	if err := ops.fstat(pin, &pinned); err != nil {
		return nil, err
	}
	if pinned.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, os.ErrInvalid
	}

	// Reopen only this live pin, never the caller's pathname. The mount
	// namespace is trusted, but an absent or substituted non-procfs directory
	// must fail closed. Keep both anchors alive until the read FD is verified.
	proc, err := ops.open("/proc/self/fd", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(proc)
	var fs unix.Statfs_t
	if err := ops.fstatfs(proc, &fs); err != nil {
		return nil, err
	}
	if fs.Type != unix.PROC_SUPER_MAGIC {
		return nil, os.ErrInvalid
	}
	// Following this kernel FD magic link is intentional; O_NOFOLLOW would
	// refuse it. The untrusted workspace leaf was already pinned no-follow.
	fd, err := ops.openat(proc, strconv.Itoa(pin), unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	var readable unix.Stat_t
	if err := ops.fstat(fd, &readable); err != nil {
		unix.Close(fd)
		return nil, err
	}
	if readable.Mode&unix.S_IFMT != unix.S_IFREG || readable.Dev != pinned.Dev || readable.Ino != pinned.Ino {
		unix.Close(fd)
		return nil, os.ErrInvalid
	}
	return os.NewFile(uintptr(fd), "private media input"), nil
}
