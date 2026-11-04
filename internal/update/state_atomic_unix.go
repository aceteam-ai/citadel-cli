//go:build linux || darwin

package update

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func openStateLock(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

func tryStateLock(file *os.File) (bool, error) {
	err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
		return false, nil
	}
	return err == nil, err
}

func unlockStateFile(file *os.File) { _ = unix.Flock(int(file.Fd()), unix.LOCK_UN) }

func replaceStateFile(from, to string) error { return os.Rename(from, to) }

func syncStateDir(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}

func keepStateOwner(file *os.File, path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("update state is not a regular file")
	}
	want, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("cannot read update state owner")
	}
	current, err := file.Stat()
	if err != nil {
		return err
	}
	have, ok := current.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("cannot read replacement state owner")
	}
	if have.Uid == want.Uid && have.Gid == want.Gid {
		return nil
	}
	if err := file.Chown(int(want.Uid), int(want.Gid)); err != nil {
		return fmt.Errorf("preserve update state owner: %w", err)
	}
	return nil
}
