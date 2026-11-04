//go:build linux || darwin

package nodevault

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const lockoutStatusSupported = true

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
	st, parent, err := lockoutFileMetadata(fd, path)
	if err != nil {
		_ = file.Close()
		return nil, ErrLockoutUnavailable
	}
	fallback, err := checkLockoutFileOwner(fd, st, parent, created, os.Geteuid(), unix.Fchown, unix.Fstat)
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if fallback {
		lockoutRootOwnerWarning.Do(func() {
			slog.Warn("nodevault: lockout sidecar remains root-owned; non-root access requires offline ownership reconciliation after all participating processes stop")
		})
	}
	return file, nil
}

// Existing-only inspection shares pure validation, never creator assignment,
// mode repair, warnings or flock acquisition. Parent aliases remain trusted.
func openExistingLockoutFile(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	state, parent, err := lockoutFileMetadata(fd, path)
	if err == nil {
		_, err = existingLockoutFileOwner(state, parent, os.Geteuid())
	}
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func lockoutFileMetadata(fd int, path string) (unix.Stat_t, unix.Stat_t, error) {
	var state, parent unix.Stat_t
	if unix.Fstat(fd, &state) != nil || unix.Stat(filepath.Dir(path), &parent) != nil ||
		state.Mode&unix.S_IFMT != unix.S_IFREG || state.Nlink != 1 || state.Mode&0o777 != 0o600 {
		return state, parent, ErrLockoutUnavailable
	}
	return state, parent, nil
}

var lockoutRootOwnerWarning sync.Once

// A root worker assigns only a new inode to its parent owner. If assignment is
// unavailable (for example in a user namespace), root may keep using its own
// inode, including on later opens. No non-root owner exception or existing-inode
// repair is allowed. The bool reports this mixed-UID handover limitation.
// Syscall dependencies let tests cover ownership without changing any real UID.
func checkLockoutFileOwner(fd int, state, parent unix.Stat_t, created bool, euid int, chown func(int, int, int) error, stat func(int, *unix.Stat_t) error) (bool, error) {
	if created && euid == 0 {
		ownerErr := chown(fd, int(parent.Uid), int(parent.Gid))
		if err := stat(fd, &state); err != nil {
			return false, err
		}
		if ownerErr != nil && state.Uid != 0 {
			return false, ownerErr
		}
	}
	return existingLockoutFileOwner(state, parent, euid)
}

// Pure owner policy shared with status, which must never try assignment.
func existingLockoutFileOwner(state, parent unix.Stat_t, euid int) (bool, error) {
	if state.Uid == parent.Uid {
		return false, nil
	}
	if euid == 0 && state.Uid == 0 {
		return true, nil
	}
	return false, ErrLockoutUnavailable
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
