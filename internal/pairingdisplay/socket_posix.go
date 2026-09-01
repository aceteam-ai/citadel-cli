//go:build !windows

// The pull-command socket's real implementation (citadel #659 P1). Built
// on every non-Windows platform (Linux, macOS, and other Unix targets this
// module might someday build for) -- see socket.go's package doc for why
// Windows is excluded and gets socket_windows.go's honest stub instead.
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
	// The 0600 permission bit is the ACTUAL access-control boundary on
	// POSIX (kernel-enforced: only this UID or root can connect) -- see
	// socket.go's package doc. This is deliberately POSIX-only code; do not
	// assume this Chmod call provides the same guarantee if ever ported to
	// another platform without re-reading that doc.
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
// expiry, but this re-checks time.Now().Before(expiresAt) defensively so a
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
