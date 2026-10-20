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
// the user the worker runs as. If the owning user cannot be kept, the save is
// refused: before saves were atomic, a writer without access to the existing
// file could not replace it either. A group that cannot be kept is tolerated
// when the owning user is unchanged, since the file is 0600.
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
	tst, ok := tmpInfo.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	return ownerDecision(tst.Uid, tst.Gid, st.Uid, st.Gid, func() error {
		return tmp.Chown(int(st.Uid), int(st.Gid))
	})
}

// ownerDecision is keepOwner's pure core: chown when the owner or group
// differs; refuse when the owning user cannot be kept; tolerate a group that
// cannot be kept when the owning user is unchanged.
func ownerDecision(tmpUID, tmpGID, wantUID, wantGID uint32, chown func() error) error {
	if tmpUID == wantUID && tmpGID == wantGID {
		return nil
	}
	if err := chown(); err != nil {
		if tmpUID == wantUID {
			return nil
		}
		return fmt.Errorf("keep permissions file owner: %w", err)
	}
	return nil
}
