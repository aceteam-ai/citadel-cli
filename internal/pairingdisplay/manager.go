// Package pairingdisplay renders a platform-pushed node:exec pairing code on
// this node's active console (citadel #659 P0; see docs/design-pairing-display.md
// Part II §8-14), and, on POSIX platforms only, separately serves it back to
// a local, authenticated `citadel pairing-code` pull command over a Unix
// socket for the headless fleet (P1, design doc §9.2/§14 -- see socket.go's
// package doc for why Windows does not get the pull-command socket at all).
// It is a leaf package: it imports neither internal/worker nor
// internal/network, so it is fully testable with an injected fake Renderer
// and no real console/VT access.
//
// # Security invariant
//
// The code passes through Manager.Show, is retained in memory only (never
// disk), and leaves this package through at most two channels: a Renderer
// call, and, on POSIX only, the pull-command socket (socket_posix.go) --
// which is itself gated to 0600 (same UID / root only), a REAL
// kernel-enforced boundary there. On Windows that socket is never opened at
// all (socket_windows.go) precisely because os.Chmod provides no equivalent
// guarantee on that platform -- see socket.go's package doc for the full
// reasoning. The code is never logged, never written to disk (the crash
// marker carries a grant_request_id and a target device name, never the
// code), and no exported method other than the socket's own request handler
// ever returns it. See internal/worker/pairing_display.go's package doc for
// the full invariant this package is one leg of.
package pairingdisplay

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// stateMarkerFile is the crash marker's filename, written under the
// machine-convergent state dir passed to Configure. Never contains the code.
const stateMarkerFile = "pairing-display-state.json"

// ShowRequest is what a Renderer needs to render a pending code. Code is the
// only sensitive field; everything else is safe to log.
type ShowRequest struct {
	Code        string
	RequestedBy string
	ExpiresAt   time.Time
	TTL         time.Duration
}

// RenderResult is what a Renderer.Show call reports back.
type RenderResult struct {
	// Delivered is true only on a confirmed render onto a real text console.
	// Governing rule (design doc §8, restated because every caller depends on
	// it): a false positive here means a human never receives the code at
	// all, since it suppresses the backend's linked-device fallback. Every
	// ambiguous case must resolve to false.
	Delivered bool
	// Surface names what was used, e.g. "console". Empty when !Delivered.
	Surface string
	// Reason explains a !Delivered outcome: "unsupported_os", "no_console",
	// "graphical_session", "permission_denied", or "render_error".
	Reason string
}

// Renderer is the injected, per-OS rendering surface (see render_linux.go /
// render_other.go). Split into ResolveTarget + Show so Manager can write the
// crash marker BEFORE attempting to render (see Show's doc comment for why
// that ordering matters).
type Renderer interface {
	// ResolveTarget identifies a render target (e.g. a VT device path) right
	// now, without writing anything or requiring privilege. ok=false means no
	// candidate exists at all; reason explains why ("unsupported_os" on a
	// non-Linux build, "no_console" on Linux with no VT subsystem/container).
	ResolveTarget() (target string, reason string, ok bool)
	// Show renders req onto the previously-resolved target. May still fail
	// for reasons ResolveTarget cannot see (a graphical session now owns the
	// console, no write permission, a render error).
	Show(target string, req ShowRequest) RenderResult
	// Clear clears whatever is currently shown on target and leaves a short
	// human-readable note ("pairing code cleared", "pairing code expired",
	// ...). Best-effort: the caller logs a failure, never surfaces it as a
	// job failure.
	Clear(target string, note string) error
	// DetectSurfaces reports the capability surfaces usable right now,
	// without writing anything (the §11 heartbeat probe). Nil/empty means no
	// capability.
	DetectSurfaces() []string
}

// ShowOutcome is what Manager.Show reports back to its caller (the
// SHOW_PAIRING_CODE handler). It becomes the job result verbatim — see
// internal/worker/pairing_display.go — and never carries the code.
type ShowOutcome struct {
	Delivered bool
	Surface   string
	Reason    string
}

// ClearOutcome is what Manager.Clear reports back (the CLEAR_PAIRING_CODE
// handler, and internally on TTL expiry / shutdown).
type ClearOutcome struct {
	Cleared bool
	Reason  string
}

// crashMarker is the durable, on-disk record of a currently-displayed code —
// deliberately never the code itself (design doc §12). It exists so a crash
// between Show and the next Clear does not strand a code on screen forever:
// the next process to start calls ReconcileStale, which reads this file and
// clears the named target.
type crashMarker struct {
	Target         string    `json:"target"`
	ExpiresAt      time.Time `json:"expires_at"`
	GrantRequestID string    `json:"grant_request_id"`
}

type pendingCode struct {
	grantRequestID string
	// code and requestedBy are retained ONLY so the local pull-command
	// socket (socket_posix.go, design doc §9.2/P1 -- POSIX only, see
	// socket.go's package doc) can serve them back to `citadel pairing-code`
	// -- a different process than the one that called Show. This is new as
	// of P1: P0 never stored the code at all (it flowed straight into the
	// renderer and was discarded). Neither field is ever logged, and
	// neither is written to the crash marker (crashMarker below stays
	// code-less) -- on POSIX, only the 0600, same-UID-only socket ever
	// exposes them; on Windows startSocketLocked/stopSocketLocked
	// (socket_windows.go) are no-ops, so these fields are tracked but never
	// exposed through any channel there at all. See socket.go's
	// package-level doc for the full boundary.
	code        string
	requestedBy string
	// target is the resolved console device, e.g. "/dev/tty1". Empty when
	// no console render ever succeeded for this pending code (no_console,
	// graphical_session, unsupported_os, ...) -- the code is still tracked
	// (for the pull-command socket, which is precisely the headless-fleet
	// answer for exactly these cases) but there is nothing on a screen to
	// clear.
	target    string
	expiresAt time.Time
	timer     *time.Timer
	// gen is a monotonically-increasing generation stamp, distinct from
	// grantRequestID. It exists because §8.2 allows a re-Show for the SAME
	// grantRequestID (a delivery retry) to reset the TTL: if the OLD timer
	// has already fired and is blocked on m.mu when the retry's Show
	// completes, onExpire would otherwise compare grantRequestID (equal,
	// since it's a same-grant retry) and clear the freshly-rendered code.
	// Comparing gen instead makes onExpire identify ITS OWN Show call, not
	// just its grant.
	gen uint64
}

// Manager owns the currently-displayed pairing code's lifecycle: render, TTL
// auto-clear, replacement, and crash-safe cleanup. At most one code is shown
// at a time (design doc §8.2's latest-grant-wins replacement rule).
type Manager struct {
	mu       sync.Mutex
	renderer Renderer
	stateDir string
	pending  *pendingCode
	nextGen  uint64
	logf     func(format string, args ...any)
	// listener serves the pull-command socket (socket.go) for the current
	// pending code, when one exists and stateDir is configured. nil when
	// nothing is pending or no state dir is set (e.g. most tests).
	listener net.Listener
}

// NewManager constructs a Manager around an injected Renderer, so it is fully
// testable without a real console or /dev access. Production code should use
// Get()/Configure() instead of calling this directly, except in tests.
func NewManager(r Renderer) *Manager {
	return &Manager{renderer: r, logf: func(string, ...any) {}}
}

var (
	singleton     *Manager
	singletonOnce sync.Once
)

// Get returns the process-wide pairing-display manager singleton. A
// singleton is required because the SHOW_PAIRING_CODE/CLEAR_PAIRING_CODE job
// handler (cmd/nodejobs.go construction) and the heartbeat capability probe
// (cmd/work.go construction) must observe the same pending-code state,
// mirroring the internal/platform.GetCobrowseManager precedent.
func Get() *Manager {
	singletonOnce.Do(func() {
		singleton = NewManager(newPlatformRenderer())
	})
	return singleton
}

// Configure sets the machine-convergent state directory the crash marker is
// written under. Callers must pass network.GetNodeConfigDir() — this package
// deliberately does not import internal/network itself (it stays a leaf), so
// the resolved path is threaded in, mirroring internal/worker/swap_persist.go's
// WithPersistence wiring. Safe to call more than once (e.g. from both
// runWork and runTUIWorker in the same process); takes effect on the next
// Show/ReconcileStale.
func Configure(stateDir string) {
	m := Get()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stateDir = stateDir
}

// SetLogFunc overrides the manager's log function. Log lines carry
// grant_request_id and target only — never the code (see the package doc).
// Optional; defaults to a no-op.
func (m *Manager) SetLogFunc(f func(format string, args ...any)) {
	if f == nil {
		f = func(string, ...any) {}
	}
	m.mu.Lock()
	m.logf = f
	m.mu.Unlock()
}

// Show renders code on the node's console for ttl, replacing whatever is
// currently displayed (latest-grant-wins; design doc §8.2). It never
// returns, logs, or persists the code — only grantRequestID and the
// resolved target ever leave this call via ShowOutcome/logs/the crash
// marker.
//
// The code and requestedBy ARE retained in m.pending itself (P1, design doc
// §9.2) so the pull-command socket can serve them back to a separate
// `citadel pairing-code` process -- see pendingCode's doc comment. That
// tracking happens UNCONDITIONALLY, before any console-rendering attempt
// below, and regardless of whether one succeeds: the pull command is the
// headless-fleet answer, so it must work precisely when console delivery is
// impossible (no_console, graphical_session, unsupported_os). The
// ShowOutcome/job-result semantics this method returns are unchanged by
// that -- Delivered still means "confirmed console render", never "a pull
// command could retrieve it".
//
// Ordering is load-bearing (design doc §12): the crash marker is written
// BEFORE the renderer is asked to draw anything. A process kill between the
// two leaves a marker naming a target that was never actually written to —
// ReconcileStale's clear on that target is harmless (one clear-screen on a
// getty). The reverse order (render succeeds, then crash before the marker
// exists) is the failure this ordering avoids: a real code would be
// stranded on screen with nothing to ever clean it up.
func (m *Manager) Show(code string, ttl time.Duration, grantRequestID, requestedBy string) ShowOutcome {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Replace whatever is currently pending (a different or the same grant).
	m.clearPendingLocked("replaced")

	expiresAt := time.Now().Add(ttl)
	m.nextGen++
	gen := m.nextGen
	pc := &pendingCode{
		grantRequestID: grantRequestID,
		code:           code,
		requestedBy:    requestedBy,
		expiresAt:      expiresAt,
		gen:            gen,
	}
	pc.timer = time.AfterFunc(ttl, func() { m.onExpire(gen) })
	m.pending = pc
	// Best-effort: a listener failure (no state dir configured, permission
	// error) must never block or fail the console-delivery attempt below --
	// the pull command is an additional retrieval path, not the primary one.
	m.startSocketLocked()

	if m.renderer == nil {
		m.logf("pairing-display: no renderer configured (grant_request_id=%s)", grantRequestID)
		return ShowOutcome{Reason: "unsupported_os"}
	}
	target, failReason, ok := m.renderer.ResolveTarget()
	if !ok {
		m.logf("pairing-display: console target unavailable (grant_request_id=%s, reason=%s)", grantRequestID, failReason)
		return ShowOutcome{Reason: failReason}
	}

	marker := crashMarker{Target: target, ExpiresAt: expiresAt, GrantRequestID: grantRequestID}
	if err := m.writeMarkerLocked(marker); err != nil {
		m.logf("pairing-display: failed to write crash marker (grant_request_id=%s): %v", grantRequestID, err)
	}

	result := m.renderer.Show(target, ShowRequest{
		Code:        code,
		RequestedBy: requestedBy,
		ExpiresAt:   expiresAt,
		TTL:         ttl,
	})
	if !result.Delivered {
		m.deleteMarkerLocked()
		m.logf("pairing-display: not delivered (grant_request_id=%s, reason=%s)", grantRequestID, result.Reason)
		return ShowOutcome{Reason: result.Reason}
	}

	// Record the target on the SAME pendingCode we already created above (do
	// not replace m.pending -- that would hand it a new timer/gen for no
	// reason and could race the pull-command socket's read of m.pending).
	pc.target = target

	m.logf("pairing-display: showing code (grant_request_id=%s, surface=%s, target=%s, expires_at=%s)",
		grantRequestID, result.Surface, target, expiresAt.UTC().Format(time.RFC3339))
	return ShowOutcome{Delivered: true, Surface: result.Surface}
}

// Clear clears the code for grantRequestID. A mismatch (a different or no
// code is currently displayed) is an idempotent success with
// reason=not_displayed, never an error — the backend fires this on
// confirm/revoke and must not see a spurious job failure for a code that
// already expired (design doc §8.2).
func (m *Manager) Clear(grantRequestID string) ClearOutcome {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pending == nil || m.pending.grantRequestID != grantRequestID {
		return ClearOutcome{Reason: "not_displayed"}
	}
	m.clearPendingLocked("cleared")
	return ClearOutcome{Cleared: true}
}

// Shutdown clears any pending display and its crash marker on graceful
// process exit, so a clean SIGTERM/systemd stop never leaves a stale code on
// screen with a dangling marker (design doc §12).
func (m *Manager) Shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.clearPendingLocked("cleared")
}

// ReconcileStale clears a code left on screen by a previous process that
// crashed or was killed before it could clear normally (SIGKILL, power
// loss; design doc §12). Must be called once at startup, before job
// consumption begins, by every process that can receive SHOW_PAIRING_CODE
// (runWork, runTUIWorker). Returns true if a stale marker was found and
// (best-effort) cleared.
func (m *Manager) ReconcileStale() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	marker, ok := m.readMarkerLocked()
	if !ok {
		return false
	}
	if m.renderer != nil {
		if err := m.renderer.Clear(marker.Target, "pairing code cleared after restart"); err != nil {
			m.logf("pairing-display: reconcile clear failed (grant_request_id=%s): %v", marker.GrantRequestID, err)
		}
	}
	m.deleteMarkerLocked()
	return true
}

// clearPendingLocked stops any pending timer, closes the pull-command
// socket, best-effort clears the renderer, and deletes the marker. Called
// under m.mu. No-op when nothing is pending.
func (m *Manager) clearPendingLocked(note string) {
	if m.pending == nil {
		return
	}
	if m.pending.timer != nil {
		m.pending.timer.Stop()
	}
	target := m.pending.target
	m.pending = nil
	m.stopSocketLocked()
	// target is empty when no console render ever succeeded for this
	// pending code (see pendingCode.target's doc comment) -- nothing was
	// ever drawn, so there is nothing for the renderer to clear.
	if target != "" && m.renderer != nil {
		if err := m.renderer.Clear(target, "pairing code "+note); err != nil {
			m.logf("pairing-display: clear failed: %v", err)
		}
	}
	m.deleteMarkerLocked()
}

// onExpire is the TTL timer callback (design doc §12, path 1). It compares
// by GENERATION, not grantRequestID: §8.2 allows a re-Show for the SAME
// grantRequestID (a delivery retry) to reset the TTL, so a grant-id match
// alone is not enough to prove this timer belongs to the code currently
// displayed -- see pendingCode.gen's doc comment for the exact race this
// closes (the old timer firing after a same-grant retry has already
// re-armed a new one).
func (m *Manager) onExpire(gen uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pending == nil || m.pending.gen != gen {
		return // already replaced, retried, or cleared
	}
	m.clearPendingLocked("expired")
}

func (m *Manager) markerPath() string {
	if m.stateDir == "" {
		return ""
	}
	return filepath.Join(m.stateDir, stateMarkerFile)
}

// writeMarkerLocked is best-effort: a failed write degrades crash-safety
// (§12's residual, accepted gap) but must never block showing the code.
func (m *Manager) writeMarkerLocked(marker crashMarker) error {
	path := m.markerPath()
	if path == "" {
		return nil // no state dir configured (e.g. a test); crash-safety degrades silently
	}
	raw, err := json.Marshal(marker)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(m.stateDir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}

func (m *Manager) readMarkerLocked() (crashMarker, bool) {
	var marker crashMarker
	path := m.markerPath()
	if path == "" {
		return marker, false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return marker, false
	}
	if err := json.Unmarshal(raw, &marker); err != nil {
		return marker, false
	}
	if marker.Target == "" {
		return marker, false
	}
	return marker, true
}

func (m *Manager) deleteMarkerLocked() {
	path := m.markerPath()
	if path == "" {
		return
	}
	_ = os.Remove(path)
}

// DetectSurfaces returns the capability surfaces this node can currently
// render a pairing code on (design doc §11) — resolve-target + text-mode +
// write-access checks, without writing anything. Cheap (two opens, one
// ioctl on Linux); safe to call on every heartbeat collection. Independent
// of Configure/Get's pending-code state — it always probes fresh.
func DetectSurfaces() []string {
	return newPlatformRenderer().DetectSurfaces()
}
