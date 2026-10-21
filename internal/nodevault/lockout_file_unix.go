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
	var st, parent unix.Stat_t
	if unix.Fstat(fd, &st) != nil || unix.Stat(filepath.Dir(path), &parent) != nil ||
		st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 || st.Mode&0o777 != 0o600 {
		_ = file.Close()
		return nil, ErrLockoutUnavailable
	}
	// A root worker must not create a 0600 root-owned lock that prevents the
	// owning user's CLI from checking its own node's PIN. Change only our new
	// inode, never chmod/chown an existing supplied file.
	if created && os.Geteuid() == 0 {
		if err := unix.Fchown(fd, int(parent.Uid), int(parent.Gid)); err != nil {
			_ = file.Close()
			return nil, err
		}
		st.Uid = parent.Uid
	}
	if st.Uid != parent.Uid {
		_ = file.Close()
		return nil, ErrLockoutUnavailable
	}
	return file, nil
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
