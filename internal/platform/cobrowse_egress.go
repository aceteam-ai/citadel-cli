package platform

import (
	"fmt"
	"log"
	"sync"

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
func guardProc(proc *cobrowseProc, g *browserurl.EgressGuard, id string) {
	inner := proc.stop
	var once sync.Once
	proc.stop = func() error {
		var err error
		if inner != nil {
			err = inner()
		}
		once.Do(func() {
			g.Close()
			if n := g.Refused(); n > 0 {
				log.Printf("[cobrowse] session %s: egress guard refused %d connection(s)", id, n)
			}
		})
		return err
	}
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
