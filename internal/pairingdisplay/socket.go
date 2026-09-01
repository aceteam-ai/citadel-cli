// The pull-command socket (citadel #659 P1, design doc §9.2/§14).
//
// The console renderer (render_linux.go) only reaches a genuinely-headed
// node -- the realistic majority of the fleet is headless GPU servers with
// no VT to write to at all. For those, `citadel pairing-code` is the
// answer: a short-lived CLI invocation, run by a human with their OWN
// independent shell access (SSH key, physical login), that reads back
// whatever code the long-running `citadel work`/control-center process is
// currently holding.
//
// # Why a socket, and not the crash marker or the status HTTP server
//
// The pending code lives in the MEMORY of the long-running worker process
// (Manager.pending); `citadel pairing-code` is a SEPARATE, short-lived
// process, so a cross-process transport is required. The design doc (§9.2)
// checked the two existing candidates and rejected both: the worker's
// status HTTP server is unauthenticated locally and, under --gateway, is
// additionally served over the mesh VPN listener -- the code must never
// ride it in any field; and internal/instance's socket is a raw PTY attach
// relay for the TUI, wrong protocol and wrong directory. This is therefore
// a dedicated, purpose-built Unix socket.
//
// # POSIX only -- this is NOT a cross-platform mechanism
//
// The real implementation (socket_posix.go, `//go:build !windows`) relies
// on the socket file's `0600` permission bit as its ONLY access control:
// only the same UID (or root) can connect. That is a real, kernel-enforced
// boundary on Linux and macOS. It is NOT one on Windows: Go's `os.Chmod` on
// Windows only toggles the read-only ATTRIBUTE, not a DACL, so a `0600`
// unix socket there is reachable by any local account. Rather than serve a
// socket with a false security guarantee, Windows gets a stub
// (socket_windows.go) that never opens a socket at all and whose
// `RequestPendingCode` fails honestly with ErrUnsupportedPlatform --
// mirroring the existing posture for the P0 console renderer
// (render_other.go, `unsupported_os`; Windows pairing-display end-to-end is
// P3 in the design doc). A real Windows implementation needs a proper DACL
// (see socket_windows.go's doc comment) and is tracked as a follow-up, not
// built here.
//
// # Shape of the mechanism (POSIX)
//
//   - Path: <stateDir>/pairing.sock, where stateDir is the SAME
//     network.GetNodeConfigDir() Configure() is pointed at -- the
//     machine-convergent node config dir (the #383/#845 rule this whole
//     package already follows for the crash marker), so a root `citadel
//     work` and a non-root operator's `citadel pairing-code` invocation
//     resolve the identical path.
//   - Mode 0600, enforced by the kernel on POSIX -- see above.
//   - Listener lifetime is tied to a pending code, not the process: it
//     starts on a successful Show (regardless of console-render outcome --
//     see manager.go's Show) and stops the moment nothing is pending
//     (Clear, TTL expiry, replacement, or graceful Shutdown), all via
//     startSocketLocked/stopSocketLocked from within clearPendingLocked.
//     There is nothing to serve outside that window, so there is no
//     listening socket outside it either.
//   - Protocol: one-shot, unauthenticated-at-the-application-layer
//     request/response. A client connects and reads until EOF; the server
//     writes one JSON PendingCodeInfo and closes. There is no request to
//     parse -- the socket's own file mode is the only access control this
//     needs on POSIX (mirrors the design doc's explicit call-out that the
//     TTY check on the CLI side is "cosmetic", not load-bearing).
package pairingdisplay

import (
	"errors"
	"time"
)

// socketFileName is the pull-command socket's filename, written under the
// same machine-convergent state dir as stateMarkerFile. Never contains the
// code on disk -- on POSIX it is a Unix socket special file, not a regular
// file with content; it is never created at all on Windows.
const socketFileName = "pairing.sock"

// ErrUnsupportedPlatform is returned by RequestPendingCode on a platform
// where the pull-command socket provides no real access control (Windows --
// see socket_windows.go). It is a distinct, non-nil error rather than a
// silent Pending:false, because those two outcomes mean very different
// things to an operator: "the platform pushed a code and you can't get it
// this way yet" versus "nothing is pending." Conflating them would be
// actively misleading, not just imprecise.
var ErrUnsupportedPlatform = errors.New("pairing-code pull is not supported on this platform yet (tracked as a follow-up; use the console renderer or the operator's linked device instead)")

// PendingCodeInfo is what the pairing.sock listener serves to
// `citadel pairing-code` (cmd/pairing_code.go). Code is the one sensitive
// field -- see the package doc's no-leak discipline: it must never be
// logged or written to the crash marker, only served over this 0600,
// same-UID-only socket (POSIX only -- see this file's package doc).
type PendingCodeInfo struct {
	// Pending is false when nothing is currently pending (including "no
	// worker process is running at all", or "this platform doesn't support
	// the pull command" -- see RequestPendingCode).
	Pending        bool      `json:"pending"`
	Code           string    `json:"code,omitempty"`
	GrantRequestID string    `json:"grant_request_id,omitempty"`
	RequestedBy    string    `json:"requested_by,omitempty"`
	ExpiresAt      time.Time `json:"expires_at,omitempty"`
	TTLSeconds     int       `json:"ttl_seconds,omitempty"`
}
