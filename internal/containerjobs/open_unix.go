//go:build unix

package containerjobs

import (
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"path"
	"syscall"
)

func openRead(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, os.O_RDONLY|unix.O_NONBLOCK, 0)
}

// Distinguish exclusive creation from reopening an existing inode. Never follow
// a final symlink, truncate before validation, or silently recreate a raced path.
func openWrite(root *os.Root, name string) (*os.File, bool, error) {
	flags := os.O_WRONLY | unix.O_NONBLOCK
	file, err := openFinalNoFollow(root, name, flags|os.O_CREATE|os.O_EXCL, 0600)
	if err == nil {
		return file, true, nil
	}
	if !os.IsExist(err) {
		return nil, false, err
	}
	file, err = openFinalNoFollow(root, name, flags, 0)
	return file, false, err
}

func singleFileLink(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && st.Nlink == 1
}

func openImportDirectory(root *os.Root, name string) (*os.File, error) {
	return openFinalNoFollow(root, name, os.O_RDONLY|unix.O_NONBLOCK|unix.O_DIRECTORY, 0)
}

// Root.OpenFile resolves the final symlink itself, including with O_NOFOLLOW.
// Resolve only the confined parent with Root, then let the kernel refuse the
// final symlink relative to that anchored directory descriptor.
func openFinalNoFollow(root *os.Root, name string, flags int, mode uint32) (*os.File, error) {
	parent, err := root.OpenFile(path.Dir(name), os.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	info, err := parent.Stat()
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, ErrInvalid
	}
	fd, err := unix.Openat(int(parent.Fd()), path.Base(name), flags|unix.O_NOFOLLOW|unix.O_CLOEXEC, mode)
	if err != nil {
		return nil, &os.PathError{Op: "openat", Path: name, Err: err}
	}
	return os.NewFile(uintptr(fd), name), nil
}

func archiveInode(info os.FileInfo) string {
	if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Nlink > 1 {
		return fmt.Sprintf("%x:%x", uint64(st.Dev), uint64(st.Ino))
	}
	return ""
}
