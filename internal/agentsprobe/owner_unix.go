//go:build !windows

// internal/agentsprobe/owner_unix.go
//
// POSIX half of the node-dir-owner tier of the layered resolver (aceteam #8993
// S2). Split behind a build tag because syscall.Stat_t (and its Uid field) do
// not exist in this shape on Windows -- the same pattern as
// internal/platform/cobrowse_basedir_unix.go.
package agentsprobe

import (
	"os"
	"syscall"
)

// statOwnerUID returns the owning uid of path, or ok=false when it cannot be
// determined (path missing/unstat-able, or the platform does not expose a uid).
func statOwnerUID(path string) (int, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}
