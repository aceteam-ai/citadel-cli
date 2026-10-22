//go:build linux || darwin

package nodevault

import (
	"os"

	"golang.org/x/sys/unix"
)

func prepareLockoutStateFile(file *os.File, dir string) error {
	return prepareLockoutStateOwner(file, dir, os.Geteuid(), unix.Fchown)
}

// Read ownership from a pinned, no-follow parent descriptor, then change only
// the new temporary inode. No existing state or supplied symlink is chowned.
// The existing node directory and its ancestors must remain trusted; this is
// not a defense against a malicious directory owner renaming parent entries.
func prepareLockoutStateOwner(file *os.File, dir string, euid int, chown func(int, int, int) error) error {
	parentFD, err := unix.Open(dir, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer unix.Close(parentFD)
	var parent, state unix.Stat_t
	if err := unix.Fstat(parentFD, &parent); err != nil {
		return err
	}
	if err := unix.Fstat(int(file.Fd()), &state); err != nil {
		return err
	}
	if state.Mode&unix.S_IFMT != unix.S_IFREG || state.Nlink != 1 || state.Mode&0o777 != 0o600 {
		return ErrLockoutUnavailable
	}
	if euid == 0 {
		if err := chown(int(file.Fd()), int(parent.Uid), int(parent.Gid)); err != nil {
			return err
		}
		if err := unix.Fstat(int(file.Fd()), &state); err != nil {
			return err
		}
	}
	if state.Uid != parent.Uid || euid == 0 && state.Gid != parent.Gid {
		return ErrLockoutUnavailable
	}
	return nil
}
