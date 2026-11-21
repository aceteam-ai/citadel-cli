//go:build unix

package composerefresh

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Preserve an existing file's owner; new files inherit the services directory's
// owner so a root worker does not strand a user's compose or first stamp.
func keepComposeOwner(tmp *os.File, path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		info, err = os.Stat(filepath.Dir(path))
	} else if err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("refusing nonregular compose metadata")
	}
	if err != nil {
		return err
	}
	want, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("unknown compose owner")
	}
	current, err := tmp.Stat()
	if err != nil {
		return err
	}
	have, ok := current.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("unknown temporary compose owner")
	}
	return composeOwnerDecision(have.Uid, have.Gid, want.Uid, want.Gid, func() error { return tmp.Chown(int(want.Uid), int(want.Gid)) })
}

func composeOwnerDecision(uid, gid, wantUID, wantGID uint32, chown func() error) error {
	if uid == wantUID && gid == wantGID {
		return nil
	}
	if err := chown(); err != nil && uid != wantUID {
		return fmt.Errorf("keep compose owner: %w", err)
	}
	return nil
}
