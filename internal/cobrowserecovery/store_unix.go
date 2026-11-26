//go:build !windows

package cobrowserecovery

import (
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

func Supported() bool { return true }
func privateDir(path string) (bool, error) {
	created := false
	if err := os.Mkdir(path, 0700); err == nil {
		created = true
	} else if !os.IsExist(err) {
		return false, err
	}
	if err := checkPrivateDir(path); err != nil {
		return false, err
	}
	// Persist the directory entry too, before any reservation can authorize work.
	parent, err := os.Open(filepath.Dir(path))
	if err != nil {
		return false, err
	}
	defer parent.Close()
	return created, parent.Sync()
}
func checkPrivateDir(path string) error {
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !st.IsDir() || st.Mode().Perm()&0077 != 0 {
		return ErrUnavailable
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); !ok || sys.Uid != uint32(os.Geteuid()) {
		return ErrUnavailable
	}
	return nil
}
func privateOpen(path string, create bool) (*os.File, error) {
	flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_CLOEXEC
	if create {
		flags = unix.O_RDWR | unix.O_CREAT | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_CLOEXEC
	}
	fd, err := unix.Open(path, flags, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 || !ok || sys.Uid != uint32(os.Geteuid()) || sys.Nlink != 1 {
		_ = f.Close()
		return nil, ErrUnavailable
	}
	return f, nil
}
func lockExclusive(f *os.File) error { return unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB) }
