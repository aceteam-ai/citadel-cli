//go:build windows

// Windows stub for the pull-command socket (citadel #659 P1).
//
// Go's os.Chmod on Windows only toggles the read-only ATTRIBUTE -- it does
// not set an ACL/DACL, so the 0600 permission socket_posix.go relies on as
// its ONLY access-control boundary (see socket.go's package doc) is not a
// real guarantee on Windows: any local account could potentially connect to
// a unix socket "chmod"'d 0600 there and read a live node:exec pairing
// code. Rather than serve a socket with a false security guarantee, this
// stub disables the pull-command mechanism ENTIRELY on Windows -- mirroring
// the existing posture for the P0 console renderer (render_other.go),
// which is already a Windows/macOS stub returning "unsupported_os"
// (Windows pairing-display support end-to-end is P3 in the design doc;
// macOS gets the real socket_posix.go implementation, since POSIX file
// permissions there ARE a real kernel-enforced boundary).
//
// A real Windows implementation needs a proper DACL -- via
// golang.org/x/sys/windows's SetNamedSecurityInfo, restricting access to
// the owning SID + Administrators -- reusing the ACL primitives already
// built for Windows adapter/service permissions in
// internal/network/acl_windows.go (citadel #789/#884). Tracked as a
// follow-up, not built here.
package pairingdisplay

import "time"

// startSocketLocked/stopSocketLocked are no-ops on Windows: the worker
// never opens a pull-command socket here, so nothing regresses (there was
// none before this package existed either) and no code is ever exposed
// through a permission model that doesn't actually restrict access.
func (m *Manager) startSocketLocked() {}
func (m *Manager) stopSocketLocked()  {}

// RequestPendingCode always fails honestly on Windows with
// ErrUnsupportedPlatform. Returning Pending:false with a nil error here
// would be indistinguishable from "no code is currently pending," which is
// actively misleading: the platform may well have pushed a pending code,
// but this CLI has no supported way to retrieve it on this platform. See
// ErrUnsupportedPlatform's doc comment (socket.go) and this file's package
// doc for why.
func RequestPendingCode(stateDir string, timeout time.Duration) (PendingCodeInfo, error) {
	return PendingCodeInfo{}, ErrUnsupportedPlatform
}
