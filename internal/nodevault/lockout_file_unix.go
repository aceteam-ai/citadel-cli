//go:build linux || darwin

package nodevault

import (
	"errors"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

func openLockoutFile(path string) (*os.File, error) {
	flags := unix.O_RDWR | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
	fd, err := unix.Open(path, flags|unix.O_CREAT|unix.O_EXCL, 0o600)
	created := err == nil
	if errors.Is(err, unix.EEXIST) {
		fd, err = unix.Open(path, flags, 0)
	}
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	// O_CREAT's mode is filtered by umask. Set only our new inode explicitly;
	// never repair or broaden permissions on an existing supplied lock file.
	if created {
		if err := unix.Fchmod(fd, 0o600); err != nil {
			_ = file.Close()
			return nil, err
		}
	}
	var st, parent unix.Stat_t
	if unix.Fstat(fd, &st) != nil || unix.Stat(filepath.Dir(path), &parent) != nil ||
		st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 || st.Mode&0o777 != 0o600 {
		_ = file.Close()
		return nil, ErrLockoutUnavailable
	}
	if err := checkLockoutFileOwner(fd, st, parent, created, os.Geteuid(), unix.Fchown); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

// A root worker assigns only a new inode to its parent owner. Even the creator
// receives no exception on failed preparation, and existing owners never change.
// The explicit syscall dependency supports failure tests without privileged or
// live-node operations; production passes the real effective UID and Fchown.
func checkLockoutFileOwner(fd int, state, parent unix.Stat_t, created bool, euid int, chown func(int, int, int) error) error {
	if created && euid == 0 {
		if err := chown(fd, int(parent.Uid), int(parent.Gid)); err != nil {
			return err
		}
		if err := unix.Fstat(fd, &state); err != nil {
			return err
		}
	}
	if state.Uid != parent.Uid {
		return ErrLockoutUnavailable
	}
	return nil
}

func lockLockoutFile(file *os.File, deadline time.Time) error {
	for {
		err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EINTR) {
			return err
		}
		if !time.Now().Before(deadline) {
			return ErrLockoutUnavailable
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func unlockLockoutFile(file *os.File) {
	_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
}
