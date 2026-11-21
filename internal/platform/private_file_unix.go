//go:build !windows

package platform

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func openPrivateNoFollow(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

func validatePrivateHandle(f *os.File, st os.FileInfo) error {
	if !st.Mode().IsRegular() || st.Mode().Perm()&0o077 != 0 || st.Mode().Perm()&0o400 == 0 {
		return errors.New("file is not private and readable")
	}
	return validateCobrowseBaseDirPerms(f.Name(), st)
}

func validateSettingsParent(path string, st os.FileInfo) error {
	return validateCobrowseBaseDirPerms(path, st)
}
