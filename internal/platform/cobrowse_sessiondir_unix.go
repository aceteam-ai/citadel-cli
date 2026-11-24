//go:build !windows

// internal/platform/cobrowse_sessiondir_unix.go
//
// Owner-only check for one co-browse session's working dir (the browser
// profile), used by the macOS headless runner before it launches into it.
package platform

import (
	"fmt"
	"os"
	"syscall"
)

// validatePrivateSessionDir refuses a per-session working dir that is a
// symlink, is not a directory, is not owned by the current user, or is
// accessible to group or other. Unlike the shared base dir (which only must not
// be group/other-writable), a session dir holds a live browser profile, so any
// group or other access is refused. It never repairs a dir.
func validatePrivateSessionDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("co-browse session dir: %w", err)
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return fmt.Errorf("co-browse session dir %s is not a plain directory (symlinks are refused)", dir)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("co-browse session dir %s is accessible to other users (mode %o); refusing to launch a browser in it", dir, fi.Mode().Perm())
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("co-browse session dir %s: could not determine owner", dir)
	}
	if int(st.Uid) != os.Getuid() {
		return fmt.Errorf("co-browse session dir %s is owned by uid %d, not the current uid %d; refusing to launch a browser in it", dir, st.Uid, os.Getuid())
	}
	return nil
}
