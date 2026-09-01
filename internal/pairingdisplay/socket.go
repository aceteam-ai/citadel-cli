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
// a dedicated, purpose-built Unix socket:
//
//   - Path: <stateDir>/pairing.sock, where stateDir is the SAME
//     network.GetNodeConfigDir() Configure() is pointed at -- the
//     machine-convergent node config dir (the #383/#845 rule this whole
//     package already follows for the crash marker), so a root `citadel
//     work` and a non-root operator's `citadel pairing-code` invocation
//     resolve the identical path.
//   - Mode 0600: the file permission is the actual access-control boundary
//     (design doc §10.4's actor-equivalence) -- only the same UID (or root)
//     can connect. A root worker means the human runs `sudo citadel
//     pairing-code`, the same privilege bar as every other fleet operation.
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
//     needs (mirrors the design doc's explicit call-out that the TTY check
//     on the CLI side is "cosmetic", not load-bearing).
package pairingdisplay

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// socketFileName is the pull-command socket's filename, written under the
// same machine-convergent state dir as stateMarkerFile. Never contains the
// code on disk -- it is a Unix socket special file, not a regular file with
// content.
const socketFileName = "pairing.sock"

// PendingCodeInfo is what the pairing.sock listener serves to
// `citadel pairing-code` (cmd/pairing_code.go). Code is the one sensitive
// field -- see the package doc's no-leak discipline: it must never be
// logged or written to the crash marker, only served over this 0600,
// same-UID-only socket.
type PendingCodeInfo struct {
	// Pending is false when nothing is currently pending (including "no
	// worker process is running at all" -- see RequestPendingCode).
	Pending        bool      `json:"pending"`
	Code           string    `json:"code,omitempty"`
	GrantRequestID string    `json:"grant_request_id,omitempty"`
	RequestedBy    string    `json:"requested_by,omitempty"`
	ExpiresAt      time.Time `json:"expires_at,omitempty"`
	TTLSeconds     int       `json:"ttl_seconds,omitempty"`
}

func (m *Manager) socketPath() string {
	if m.stateDir == "" {
		return ""
	}
	return filepath.Join(m.stateDir, socketFileName)
}

// startSocketLocked begins serving the pull-command socket for the
// currently-pending code. Called under m.mu, from Show, unconditionally
// (before any console-rendering attempt) -- see Show's doc comment. A
// failure here (no state dir configured, e.g. most tests; a permission
// error) is logged and never affects the console-delivery ShowOutcome --
// the pull command is an additional retrieval path, not the primary one.
func (m *Manager) startSocketLocked() {
	path := m.socketPath()
	if path == "" {
		return // no state dir configured; pull-command unavailable
	}
	// Best-effort: clear a stale socket file a crashed process left behind.
	// A live listener never leaves one (stopSocketLocked always removes its
	// own), so this only ever fires after a SIGKILL/crash, mirroring
	// ReconcileStale's crash-marker cleanup for the console leg.
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		m.logf("pairing-display: pull-command socket unavailable: %v", err)
		return
	}
	if err := os.Chmod(path, 0o600); err != nil {
		m.logf("pairing-display: failed to set pull-command socket permissions: %v", err)
		_ = ln.Close()
		_ = os.Remove(path)
		return
	}
	m.listener = ln
	go m.serveSocket(ln)
}

// stopSocketLocked closes the pull-command socket, if one is currently
// listening, and removes its file. Called under m.mu, from
// clearPendingLocked -- so it runs on every path that clears m.pending
// (explicit Clear, TTL expiry, replacement, Shutdown).
func (m *Manager) stopSocketLocked() {
	if m.listener == nil {
		return
	}
	_ = m.listener.Close()
	m.listener = nil
	if path := m.socketPath(); path != "" {
		_ = os.Remove(path)
	}
}

// serveSocket accepts one-shot connections until ln is closed by
// stopSocketLocked (Accept then returns an error and the loop exits --
// there is nothing to distinguish "closed on purpose" from a real listener
// error here, and nothing to retry either way).
func (m *Manager) serveSocket(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		m.handleSocketConn(conn)
	}
}

// socketWriteTimeout bounds how long a single pull-command response write
// may take, so a slow/hung client can never wedge the accept loop's calling
// goroutine indefinitely.
const socketWriteTimeout = 5 * time.Second

func (m *Manager) handleSocketConn(conn net.Conn) {
	defer conn.Close()
	info := m.snapshotPending()
	raw, err := json.Marshal(info)
	if err != nil {
		return
	}
	_ = conn.SetWriteDeadline(time.Now().Add(socketWriteTimeout))
	_, _ = conn.Write(raw)
}

// snapshotPending returns the currently-pending code, if any and
// unexpired. The TTL timer (onExpire) normally clears m.pending exactly at
// expiry, but this re-checks time.Now().After(expiresAt) defensively so a
// request racing the timer callback can never observe a technically-expired
// code (design doc's "never print an EXPIRED code" requirement, restated
// for this new read path).
func (m *Manager) snapshotPending() PendingCodeInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pending == nil || !time.Now().Before(m.pending.expiresAt) {
		return PendingCodeInfo{}
	}
	return PendingCodeInfo{
		Pending:        true,
		Code:           m.pending.code,
		GrantRequestID: m.pending.grantRequestID,
		RequestedBy:    m.pending.requestedBy,
		ExpiresAt:      m.pending.expiresAt,
		TTLSeconds:     int(time.Until(m.pending.expiresAt).Round(time.Second) / time.Second),
	}
}

// RequestPendingCode queries a locally-running citadel work/control-center
// process for its currently-pending pairing code, over the pull-command
// socket. stateDir MUST be network.GetNodeConfigDir() -- the same
// machine-convergent directory Configure() is pointed at (this package
// stays a leaf and does not import internal/network itself; see Configure's
// doc comment for the identical convention).
//
// Returns (Pending: false, nil error) -- not an error -- for every "there is
// nothing to retrieve" case: no code is currently pending, no worker
// process is running at all (ENOENT), or a stale socket file with nothing
// listening on it (ECONNREFUSED). A real dial/read/decode failure, or a
// permission error (the caller is not the socket owner and not root --
// still surfaced distinctly, since that IS actionable information, unlike
// the cases above), is returned as an error.
func RequestPendingCode(stateDir string, timeout time.Duration) (PendingCodeInfo, error) {
	var info PendingCodeInfo
	if stateDir == "" {
		return info, nil
	}
	path := filepath.Join(stateDir, socketFileName)

	conn, err := net.DialTimeout("unix", path, timeout)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
			return info, nil
		}
		if errors.Is(err, os.ErrPermission) {
			return info, fmt.Errorf("permission denied connecting to %s (are you the node's worker user, or root?): %w", path, err)
		}
		return info, fmt.Errorf("connect to pairing-code socket: %w", err)
	}
	defer conn.Close()

	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return info, fmt.Errorf("set read deadline: %w", err)
	}
	raw, err := io.ReadAll(conn)
	if err != nil {
		return info, fmt.Errorf("read pairing-code response: %w", err)
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		return info, fmt.Errorf("decode pairing-code response: %w", err)
	}

	// Defense in depth: never trust an already-expired code even if the
	// server's own snapshot raced its TTL timer (see snapshotPending's
	// identical check on the server side).
	if info.Pending && !info.ExpiresAt.IsZero() && !time.Now().Before(info.ExpiresAt) {
		return PendingCodeInfo{}, nil
	}
	return info, nil
}
