//go:build linux || darwin

package update

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	updateLockWait     = 25 * time.Millisecond
	updateLockDeadline = 10 * time.Second
)

type installedIdentity struct {
	uid uint32
	gid uint32
}

type fileIdentity struct {
	dev uint64
	ino uint64
}

type mutationLock struct {
	file     *os.File
	path     string
	identity installedIdentity
	released bool
}

var mutationLockWait = func(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func acquireMutationLock(ctx context.Context, destination string, allowCreate bool) (*mutationLock, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, updateLockDeadline)
	defer cancel()

	identity, err := statInstalledIdentity(destination)
	if err != nil {
		return nil, fmt.Errorf("update lock unavailable: installed executable identity: %w", err)
	}
	path := destination + ".citadel-update.lock"
	file, err := openMutationLock(path, identity, allowCreate)
	if err != nil {
		return nil, fmt.Errorf("update lock unavailable/not safely provisioned: %w", err)
	}
	lock := &mutationLock{file: file, path: path, identity: identity}
	for {
		err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			if err := validateLockIdentity(file, path, identity); err != nil {
				_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
				_ = file.Close()
				return nil, fmt.Errorf("update lock unavailable/not safely provisioned: %w", err)
			}
			return lock, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EINTR) {
			_ = file.Close()
			return nil, fmt.Errorf("update lock unavailable: %w", err)
		}
		if err := mutationLockWait(ctx, updateLockWait); err != nil {
			_ = file.Close()
			return nil, fmt.Errorf("update lock unavailable: timed out waiting for updater transaction: %w", err)
		}
	}
}

func acquireRecoveryMutationLock(context.Context, string) (*mutationLock, error) {
	return nil, nil
}

func statInstalledIdentity(path string) (installedIdentity, error) {
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		return installedIdentity{}, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 {
		return installedIdentity{}, fmt.Errorf("installed executable is not a single-link regular file")
	}
	return installedIdentity{uid: st.Uid, gid: st.Gid}, nil
}

func openMutationLock(path string, identity installedIdentity, allowCreate bool) (*os.File, error) {
	flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW
	fd, err := unix.Open(path, flags, 0)
	if err == nil {
		file := os.NewFile(uintptr(fd), path)
		if validateErr := validateLockIdentity(file, path, identity); validateErr != nil {
			_ = file.Close()
			return nil, validateErr
		}
		return file, nil
	}
	if !errors.Is(err, unix.ENOENT) || !allowCreate {
		return nil, err
	}
	if !lockCreationOwnerAllowed(os.Geteuid(), identity.uid) {
		return nil, fmt.Errorf("first lock creation requires installed executable owner")
	}

	fd, err = unix.Open(path, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if errors.Is(err, unix.EEXIST) {
		return openMutationLock(path, identity, false)
	}
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if err := file.Chmod(0o644); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := validateLockIdentity(file, path, identity); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func lockCreationOwnerAllowed(euid int, installedUID uint32) bool {
	return euid >= 0 && uint32(euid) == installedUID
}

func validateLockIdentity(file *os.File, path string, identity installedIdentity) error {
	fdInfo, err := file.Stat()
	if err != nil {
		return err
	}
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !fdInfo.Mode().IsRegular() || pathInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(fdInfo, pathInfo) {
		return fmt.Errorf("lock is not a stable regular file")
	}
	if fdInfo.Mode().Perm() != 0o644 {
		return fmt.Errorf("lock has unsafe mode %04o", fdInfo.Mode().Perm())
	}
	st, ok := fdInfo.Sys().(*syscall.Stat_t)
	if !ok || st.Nlink != 1 || st.Uid != identity.uid {
		return fmt.Errorf("lock has unsafe ownership or link count")
	}
	return nil
}

func (l *mutationLock) Release() error {
	if l == nil || l.released {
		return nil
	}
	l.released = true
	unlockErr := unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
	closeErr := l.file.Close()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}

func setStageOwner(file *os.File, identity installedIdentity) error {
	if err := file.Chown(int(identity.uid), int(identity.gid)); err != nil {
		return fmt.Errorf("preserve installed executable ownership: %w", err)
	}
	return nil
}

func createPrivateAttemptDir(parent string) (string, os.FileInfo, error) {
	dir, err := os.MkdirTemp(parent, ".attempt-")
	if err != nil {
		return "", nil, err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(dir)
		}
	}()
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", nil, err
	}
	fd, err := unix.Open(filepath.Clean(dir), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY, 0)
	if err != nil {
		return "", nil, err
	}
	file := os.NewFile(uintptr(fd), dir)
	info, err := file.Stat()
	_ = file.Close()
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return "", nil, fmt.Errorf("attempt directory is not private")
	}
	cleanup = false
	return dir, info, nil
}

func openRegularNoFollow(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("not a regular file")
	}
	named, err := os.Lstat(path)
	if err != nil || named.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, named) {
		_ = file.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("file identity changed while opening")
	}
	return file, nil
}

func captureFileIdentity(file *os.File) (fileIdentity, error) {
	info, err := file.Stat()
	if err != nil {
		return fileIdentity{}, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || st.Nlink != 1 {
		return fileIdentity{}, fmt.Errorf("file is not a single-link regular file")
	}
	return fileIdentity{dev: uint64(st.Dev), ino: uint64(st.Ino)}, nil
}

func validateCapturedFileIdentity(file *os.File, expected fileIdentity) error {
	actual, err := captureFileIdentity(file)
	if err != nil {
		return err
	}
	if actual != expected {
		return fmt.Errorf("file identity changed")
	}
	return nil
}
