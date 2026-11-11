//go:build linux

package jobs

import (
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strconv"

	"golang.org/x/sys/unix"
)

// fileFetchOpenStaging creates an unnamed inode on the workspace filesystem.
// O_EXCL must not be set: it would prevent linking the verified inode later.
// No named-temp or rename fallback is permitted when this primitive fails.
func fileFetchOpenStaging(root *os.Root) (*os.File, error) {
	dir, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	fd, err := unix.Openat(int(dir.Fd()), ".", unix.O_TMPFILE|unix.O_RDWR|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), "FILE_FETCH anonymous staging"), nil
}

// fileFetchPublish links the verified open inode directly into a parent opened
// beneath the original workspace root. linkat never replaces an existing leaf,
// including a symlink. Destination parents are resolved afresh after transfer.
func fileFetchPublish(root *os.Root, staging *os.File, dest string) error {
	parent, err := root.OpenRoot(filepath.Dir(dest))
	if err != nil {
		return err
	}
	defer parent.Close()
	dir, err := parent.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	return fileFetchLinkFD(staging, dir, filepath.Base(dest))
}

func fileFetchLinkFD(staging, dir *os.File, leaf string) error {
	fd := int(staging.Fd())
	err := unix.Linkat(fd, "", int(dir.Fd()), leaf, unix.AT_EMPTY_PATH)
	if !errors.Is(err, unix.EPERM) && !errors.Is(err, unix.ENOENT) {
		return err
	}
	return fileFetchLinkProcFD(staging, dir, leaf)
}

func fileFetchLinkProcFD(staging, dir *os.File, leaf string) error {
	// AT_EMPTY_PATH requires CAP_DAC_READ_SEARCH on ordinary worker accounts.
	// Linux also supports linking O_TMPFILE through its open /proc/self/fd
	// reference. Pin and validate that procfs directory, and follow only this
	// kernel-owned FD entry. The original staging descriptor stays open.
	procFD, err := unix.Open("/proc/self/fd", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(procFD)
	var fs unix.Statfs_t
	if err := unix.Fstatfs(procFD, &fs); err != nil {
		return err
	}
	if fs.Type != unix.PROC_SUPER_MAGIC {
		return unix.EOPNOTSUPP
	}
	return unix.Linkat(procFD, strconv.FormatUint(uint64(staging.Fd()), 10), int(dir.Fd()), leaf, unix.AT_SYMLINK_FOLLOW)
}

// Probe the complete primitive, not just O_TMPFILE: some kernels, filesystems
// or sandboxes allow anonymous creation but refuse descriptor-based linking.
// The probe's empty inode is briefly visible under a random name; fetched data
// never receives a temporary name. A failed probe suppresses advertisement.
func fileFetchStorageSupported(workspace string) bool {
	if workspace == "" {
		return false
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return false
	}
	defer root.Close()
	staging, err := fileFetchOpenStaging(root)
	if err != nil {
		return false
	}
	defer staging.Close()
	name := ".file-fetch-probe-" + rand.Text()
	if err := fileFetchPublish(root, staging, name); err != nil {
		return false
	}
	// Exercise the create-only refusal as part of the filesystem check.
	createOnly := errors.Is(fileFetchPublish(root, staging, name), os.ErrExist)
	return root.Remove(name) == nil && createOnly
}
