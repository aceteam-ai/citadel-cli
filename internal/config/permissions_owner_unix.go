//go:build !windows

package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// keepOwner gives the replacement file the existing file's owner, so an atomic
// save (for example by root) does not leave the permissions file unreadable to
// the user the worker runs as. If the owner cannot be kept, the save is
// refused: before saves were atomic, a writer without access to the existing
// file could not replace it either.
func keepOwner(tmp *os.File, existing string) error {
	info, err := os.Stat(existing)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat permissions: %w", err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	tmpInfo, err := tmp.Stat()
	if err != nil {
		return fmt.Errorf("stat permissions temp file: %w", err)
	}
	if tst, ok := tmpInfo.Sys().(*syscall.Stat_t); ok && tst.Uid == st.Uid && tst.Gid == st.Gid {
		return nil
	}
	if err := tmp.Chown(int(st.Uid), int(st.Gid)); err != nil {
		return fmt.Errorf("keep permissions file owner: %w", err)
	}
	return nil
}
