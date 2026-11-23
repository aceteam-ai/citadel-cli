package platform

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/browserurl"
)

// EgressPublicOnly is the status "egress" value of a session whose browser
// connects through an egress guard that refuses every non-public address:
// loopback, private and home networks, link-local, and the mesh
// (citadel-cli#1867 C4b).
const EgressPublicOnly = "public_only"

// nodeEgressRequired is the node's public-only setting for remote sessions,
// read live at every session start. cmd installs it (SetNodeCobrowseEgressSource)
// from the node permissions file; nil keeps today's behavior (no guard).
var (
	nodeEgressMu       sync.Mutex
	nodeEgressRequired func() bool
)

// SetNodeCobrowseEgressSource installs the public-only source the node-wide
// manager reads at every session start. Only the node-wide manager consults it;
// local mode and isolated managers keep their own launch options.
func SetNodeCobrowseEgressSource(f func() bool) {
	nodeEgressMu.Lock()
	nodeEgressRequired = f
	nodeEgressMu.Unlock()
}

// SetEgressRequired installs a per-manager public-only source, which wins over
// the node source. A nil source falls back to it (node-wide manager only).
func (m *CobrowseSessionManager) SetEgressRequired(f func() bool) {
	m.mu.Lock()
	m.egressRequired = f
	m.mu.Unlock()
	m.startEgressSweep()
}

// egressRequiredNow reports whether the next session must run behind the
// egress guard.
func (m *CobrowseSessionManager) egressRequiredNow() bool {
	m.mu.Lock()
	f, nodeWide := m.egressRequired, m.useResolver
	m.mu.Unlock()
	if f == nil && nodeWide {
		nodeEgressMu.Lock()
		f = nodeEgressRequired
		nodeEgressMu.Unlock()
	}
	return f != nil && f()
}

// ErrLegacyCobrowsePublicOnly refuses the legacy single-browser COBROWSE job
// on a public-only node: that browser has no egress guard.
var ErrLegacyCobrowsePublicOnly = errors.New("this node allows public-only co-browsing, which the legacy single-browser co-browse does not support; use a browser session (COBROWSE_SESSION) instead")

// refuseWhenPublicOnly is the legacy CobrowseManager's gate, checked on every
// action. The setting may turn on while a legacy browser runs; that browser is
// then stopped, not just refused, since its pages keep their network access.
func (m *CobrowseManager) refuseWhenPublicOnly() error {
	err := refuseLegacyWhenPublicOnly()
	if err != nil && m.IsRunning() {
		log.Printf("[cobrowse] stopping the legacy co-browse browser: this node is now public-only")
		_ = m.Stop()
	}
	return err
}

func refuseLegacyWhenPublicOnly() error {
	nodeEgressMu.Lock()
	f := nodeEgressRequired
	nodeEgressMu.Unlock()
	if f != nil && f() {
		return ErrLegacyCobrowsePublicOnly
	}
	return nil
}

// startSessionEgressGuard starts one session's guard on loopback. A package
// var so tests substitute a resolver and dialer.
var startSessionEgressGuard = func(onRefused func(scheme, host string)) (*browserurl.EgressGuard, error) {
	return browserurl.StartEgressGuardWithOptions(browserurl.EgressGuardOptions{OnRefused: onRefused})
}

// egressAuditMaxHosts bounds the distinct destinations one session logs, so a
// page cannot flood the log by requesting many hosts.
const egressAuditMaxHosts = 32

// egressAudit logs a session's refused destinations as scheme plus host only,
// once per destination, never a path, query or full URL.
type egressAudit struct {
	id     string
	mu     sync.Mutex
	seen   map[string]struct{}
	capped bool
}

func newEgressAudit(id string) *egressAudit {
	return &egressAudit{id: id, seen: map[string]struct{}{}}
}

// refused is the guard's OnRefused callback. host has already been reduced to
// a bounded name or address by the guard.
func (a *egressAudit) refused(scheme, host string) {
	key := scheme + "://" + host
	a.mu.Lock()
	if _, ok := a.seen[key]; ok {
		a.mu.Unlock()
		return
	}
	if len(a.seen) >= egressAuditMaxHosts {
		first := !a.capped
		a.capped = true
		a.mu.Unlock()
		if first {
			log.Printf("[cobrowse] audit egress_refused session=%s: more than %d destinations refused; further ones are not logged", a.id, egressAuditMaxHosts)
		}
		return
	}
	a.seen[key] = struct{}{}
	a.mu.Unlock()
	log.Printf("[cobrowse] audit egress_refused session=%s dest=%s (public-only)", a.id, key)
}

// guardProc ties a session's guard to its browser: the guard closes once the
// browser has stopped, on every path that stops it (teardown, a stop during
// launch). It also logs how many connections the guard refused.
// The returned channel is closed once the guard has been closed that way.
func guardProc(proc *cobrowseProc, g *browserurl.EgressGuard, id string) <-chan struct{} {
	inner := proc.stop
	var once sync.Once
	stopped := make(chan struct{})
	proc.stop = func() error {
		var err error
		if inner != nil {
			err = inner()
		}
		once.Do(func() {
			close(stopped)
			g.Close()
			if n := g.Refused(); n > 0 {
				log.Printf("[cobrowse] session %s: egress guard refused %d connection(s)", id, n)
			}
		})
		return err
	}
	return stopped
}

// watchGuard stops session id if its guard stops serving on its own. A
// browser behind a dead guard has no route, but the session must not stay up
// looking healthy.
func (m *CobrowseSessionManager) watchGuard(id string, failed, stopped <-chan struct{}) {
	select {
	case <-stopped:
	case <-failed:
		log.Printf("[cobrowse] session %s: egress guard stopped unexpectedly; stopping the session", id)
		_ = m.Stop(id)
	}
}

// egressCanaryWait bounds the post-launch route check.
const egressCanaryWait = 15 * time.Second

// errEgressNotRouted refuses a session whose browser did not send the canary
// through its guard: a managed policy or similar override routes it elsewhere.
var errEgressNotRouted = errors.New("the browser did not route its traffic through the egress guard (a browser policy may override the proxy), so the public-only session was refused")

// egressRouteCheck verifies, after launch, that the browser really uses its
// guard. A package var so unit tests without a browser substitute it.
var egressRouteCheck = verifyEgressRoute

// verifyEgressRoute opens the guard's canary URL in a background tab through
// the browser's own CDP endpoint and waits for the guard to see it.
func verifyEgressRoute(proc *cobrowseProc, g *browserurl.EgressGuard) error {
	if proc == nil || proc.debugPort <= 0 {
		return errEgressNotRouted
	}
	ctx, cancel := context.WithTimeout(context.Background(), egressCanaryWait)
	defer cancel()
	ws, err := fetchCDPBrowserWSContext(ctx, proc.debugPort)
	if err != nil {
		return fmt.Errorf("public-only route check: %w", err)
	}
	prefix := fmt.Sprintf("ws://127.0.0.1:%d/devtools/browser/", proc.debugPort)
	if !strings.HasPrefix(ws, prefix) || (proc.browserID != "" && ws != prefix+proc.browserID) {
		return fmt.Errorf("public-only route check: %w", errCDPImpostor)
	}
	res, err := cdpDialAndSendContext(ctx, ws, "Target.createTarget", map[string]any{"url": g.CanaryURL(), "background": true})
	if err != nil {
		return fmt.Errorf("public-only route check: %w", err)
	}
	if tid, _ := res["targetId"].(string); tid != "" {
		defer func() {
			cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer ccancel()
			_, _ = cdpDialAndSendContext(cctx, ws, "Target.closeTarget", map[string]any{"targetId": tid})
		}()
	}
	for !g.CanarySeen() {
		select {
		case <-ctx.Done():
			return errEgressNotRouted
		case <-time.After(50 * time.Millisecond):
		}
	}
	return nil
}

var egressContainerNote sync.Once

// startEgressForLaunch starts the guard for one session and fills lo. A
// guarded session runs on the host browser (the guard listens on host
// loopback, which a container's network cannot reach) and directly, never in
// a detached scope: its guard does not survive a worker restart, so neither
// may the browser.
func startEgressForLaunch(id string, lo *LaunchOptions) (*browserurl.EgressGuard, error) {
	g, err := startSessionEgressGuard(newEgressAudit(id).refused)
	if err != nil {
		return nil, fmt.Errorf("this node allows public-only co-browsing, and the egress guard could not start, so the session was refused: %w", err)
	}
	lo.ProxyServer = g.ProxyURL()
	lo.Detach = false
	lo.UseContainer = false
	return g, nil
}

// EndedReasonPublicOnly ends an unguarded session that was running when the
// node switched to public-only browsing.
const EndedReasonPublicOnly = "public_only_enabled"

// egressSweepInterval is how often a manager that can be switched to
// public-only checks for unguarded sessions to stop.
var egressSweepInterval = 30 * time.Second

// egressSwitchable reports whether this manager reads a public-only source
// (the node-wide manager, or one given SetEgressRequired). Local mode is not.
func (m *CobrowseSessionManager) egressSwitchable() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.useResolver || m.egressRequired != nil
}

// stopIfUnguarded stops s, and refuses the action, when public-only is now on
// and s runs without a guard: its pages still reach the local network.
func (m *CobrowseSessionManager) stopIfUnguarded(s *cobrowseSession) error {
	if s.egressGuarded() || !m.egressSwitchable() || !m.egressRequiredNow() {
		return nil
	}
	if m.stopUnguarded(s.id) {
		return &SessionEndedError{ID: s.id, Reason: EndedReasonPublicOnly, Detail: publicOnlyEndedDetail}
	}
	// Someone else ended it first; report their reason, not ours.
	if e, ok := m.EndedSession(s.id); ok {
		return &SessionEndedError{ID: s.id, Reason: e.Reason, Detail: e.Detail}
	}
	return fmt.Errorf("no such browser session: %q", s.id)
}

const publicOnlyEndedDetail = "this node switched to public-only browsing; start a new session"

// stopUnguarded ends id with the public-only reason. It reports false, and
// records nothing, when the session was already gone: whatever stopped it
// keeps its own end reason.
func (m *CobrowseSessionManager) stopUnguarded(id string) bool {
	m.mu.Lock()
	s, ok := m.sessions[id]
	delete(m.sessions, id)
	m.mu.Unlock()
	if !ok {
		return false
	}
	log.Printf("[cobrowse] stopping session %s: it has no egress guard and this node is now public-only", id)
	_ = s.teardown()
	m.recordEnded(id, EndedReasonPublicOnly, publicOnlyEndedDetail)
	return true
}

// sweepUnguarded stops every unguarded session when public-only is on.
func (m *CobrowseSessionManager) sweepUnguarded() {
	if !m.egressSwitchable() || !m.egressRequiredNow() {
		return
	}
	m.mu.Lock()
	var ids []string
	for id, s := range m.sessions {
		s.mu.Lock()
		unguarded := s.egress == "" && s.proc != nil
		s.mu.Unlock()
		if unguarded {
			ids = append(ids, id)
		}
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.stopUnguarded(id)
	}
}

// startEgressSweep runs sweepUnguarded periodically so an idle page does not
// keep its network access until the next action. It starts at most once per
// manager at a time: after an unguarded launch, after a re-adoption (adopted
// sessions are never guarded) and when SetEgressRequired installs a source.
// stopEgressSweep (StopAll, ShutdownSessions) ends it.
func (m *CobrowseSessionManager) startEgressSweep() {
	if !m.egressSwitchable() {
		return
	}
	interval := egressSweepInterval
	m.mu.Lock()
	if m.egressSweepStop != nil {
		m.mu.Unlock()
		return
	}
	stop := make(chan struct{})
	m.egressSweepStop = stop
	m.mu.Unlock()
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				m.sweepUnguarded()
			}
		}
	}()
}

func (m *CobrowseSessionManager) stopEgressSweep() {
	m.mu.Lock()
	if m.egressSweepStop != nil {
		close(m.egressSweepStop)
		m.egressSweepStop = nil
	}
	m.mu.Unlock()
}

func (s *cobrowseSession) egressGuarded() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.egress != ""
}

// egressFirstNavigateWait bounds pinning the page and sending the deferred
// start-URL navigation (not the page load; startSession settles that).
const egressFirstNavigateWait = 10 * time.Second

// egressFirstNavigate sends a guarded session's start URL once, after the
// route check. A package var so unit tests without a browser record it.
var egressFirstNavigate = func(ctx context.Context, s *cobrowseSession, url string) error {
	p, err := s.pinActionCtx(ctx)
	if err != nil {
		return err
	}
	_, err = p.cdp("Page.navigate", map[string]any{"url": url})
	return err
}

// clearSessionRestore removes a profile's saved session state (open tabs and
// windows), so a restored profile cannot reopen its last pages at startup.
// Cookies and saved logins are untouched. Every profile directory under the
// user-data dir is covered; a removal error refuses the launch.
func clearSessionRestore(userDataDir string) error {
	entries, err := os.ReadDir(userDataDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("public-only: read profile: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() || e.Type()&os.ModeSymlink != 0 {
			continue
		}
		dir := filepath.Join(userDataDir, e.Name())
		for _, name := range sessionRestoreEntries {
			if err := os.RemoveAll(filepath.Join(dir, name)); err != nil {
				return fmt.Errorf("public-only: clear saved tabs in the profile: %w", err)
			}
		}
	}
	return nil
}

// sessionRestoreEntries are Chromium's saved-session files: the Sessions
// directory (current releases) and the older top-level files.
var sessionRestoreEntries = []string{"Sessions", "Current Session", "Current Tabs", "Last Session", "Last Tabs"}
