//go:build !windows

// internal/platform/cobrowse_basedir_unix.go
//
// Unix-only half of the session base-dir trust check (issue #793 security review).
// Split out (mirrors internal/nvr/storage_unix.go + storage_windows.go) because the
// owner check needs syscall.Stat_t, which does not exist in this shape on Windows.
package platform

import (
	"fmt"
	"os"
	"syscall"
)

// validateCobrowseBaseDirPerms confirms dir is owned by the current UID and is not
// group/other-writable, so sweep() can trust every pidfile it finds beneath it. The
// base dir lives under a shared, world-writable os.TempDir(); without this check a
// local unprivileged user could pre-create the dir (or leave it group/other-writable)
// and plant a pidfile naming an arbitrary PID for a future sweep -- frequently running
// as root under systemd (see CLAUDE.md) -- to kill.
func validateCobrowseBaseDirPerms(dir string, info os.FileInfo) error {
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("cobrowse base dir %s is group/other-writable (mode %o); refusing to trust its pidfiles", dir, info.Mode().Perm())
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("cobrowse base dir %s: could not determine owner", dir)
	}
	if int(st.Uid) != os.Getuid() {
		return fmt.Errorf("cobrowse base dir %s is owned by uid %d, not the current uid %d; refusing to trust its pidfiles", dir, st.Uid, os.Getuid())
	}
	return nil
}
