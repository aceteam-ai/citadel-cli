//go:build linux

package reconstruct

import (
	"context"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// publishDirectory promotes a private engine bundle without replacing any
// existing destination, including an empty directory or dangling symlink. The
// no-replace condition is part of the rename syscall, not a prior stat check.
// Unsupported kernels/filesystems fail closed; there is no os.Rename fallback.
//
// Parent directories are opened without following their final component and
// the rename uses those descriptors and leaf names. Earlier path components
// still belong to the trusted local operator: this is NOT workspace confinement
// or protection against a hostile owner mutating the private source directory.
// It also does not establish fsync/crash durability or distributed-FS recovery.
func publishDirectory(ctx context.Context, source, destination string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_DIRECTORY | unix.O_NOFOLLOW
	sourceFD, err := unix.Open(filepath.Dir(source), flags, 0)
	if err != nil {
		return &os.PathError{Op: "open publication source parent", Path: source, Err: err}
	}
	defer unix.Close(sourceFD)
	destinationFD, err := unix.Open(filepath.Dir(destination), flags, 0)
	if err != nil {
		return &os.PathError{Op: "open publication destination parent", Path: destination, Err: err}
	}
	defer unix.Close(destinationFD)
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := unix.Renameat2(sourceFD, filepath.Base(source), destinationFD,
		filepath.Base(destination), unix.RENAME_NOREPLACE); err != nil {
		return &os.LinkError{Op: "publish without replacement", Old: source, New: destination, Err: err}
	}
	return nil
}
