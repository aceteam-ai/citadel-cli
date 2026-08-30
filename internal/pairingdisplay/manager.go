// Package pairingdisplay renders a platform-pushed node:exec pairing code on
// this node's active console (citadel #659 P0; see docs/design-pairing-display.md
// Part II §8-14). It is a leaf package: it imports neither internal/worker nor
// internal/network, so it is fully testable with an injected fake Renderer and
// no real console/VT access.
//
// # Security invariant
//
// The code passes through Manager.Show and into a Renderer call, and nowhere
// else. It is never logged, never written to disk (the crash marker carries a
// grant_request_id and a target device name, never the code), and never
// returned from any method here. See internal/worker/pairing_display.go's
// package doc for the full invariant this package is one leg of.
package pairingdisplay

import (
	"encoding/json"
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
	target         string
	expiresAt      time.Time
	timer          *time.Timer
}

// Manager owns the currently-displayed pairing code's lifecycle: render, TTL
// auto-clear, replacement, and crash-safe cleanup. At most one code is shown
// at a time (design doc §8.2's latest-grant-wins replacement rule).
type Manager struct {
	mu       sync.Mutex
	renderer Renderer
	stateDir string
	pending  *pendingCode
	logf     func(format string, args ...any)
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
// resolved target ever leave this call.
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

	if m.renderer == nil {
		return ShowOutcome{Reason: "unsupported_os"}
	}
	target, failReason, ok := m.renderer.ResolveTarget()
	if !ok {
		return ShowOutcome{Reason: failReason}
	}

	expiresAt := time.Now().Add(ttl)
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

	pc := &pendingCode{grantRequestID: grantRequestID, target: target, expiresAt: expiresAt}
	pc.timer = time.AfterFunc(ttl, func() { m.onExpire(grantRequestID) })
	m.pending = pc

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

// clearPendingLocked stops any pending timer, best-effort clears the
// renderer, and deletes the marker. Called under m.mu. No-op when nothing is
// pending.
func (m *Manager) clearPendingLocked(note string) {
	if m.pending == nil {
		return
	}
	if m.pending.timer != nil {
		m.pending.timer.Stop()
	}
	target := m.pending.target
	m.pending = nil
	if m.renderer != nil {
		if err := m.renderer.Clear(target, "pairing code "+note); err != nil {
			m.logf("pairing-display: clear failed: %v", err)
		}
	}
	m.deleteMarkerLocked()
}

// onExpire is the TTL timer callback (design doc §12, path 1). It re-checks
// identity under the lock so a replacement that raced the timer (Show called
// for a different grant just before this fires) does not clear the NEW
// code.
func (m *Manager) onExpire(grantRequestID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pending == nil || m.pending.grantRequestID != grantRequestID {
		return // already replaced or cleared
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
