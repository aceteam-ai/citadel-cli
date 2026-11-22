package platform

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/cdpdiscovery"
)

// Per-call dependencies, not mutable package hooks. Only the default source
// connects to a browser; tests supply fake discovery and CDP reads.
type suspendPageSource struct {
	verify func(context.Context, int, string) error
	list   func(context.Context, int) ([]cdpdiscovery.Target, error)
	read   func(context.Context, pin, func() error) (suspendTab, error)
}

func browserSuspendPageSource() suspendPageSource {
	return suspendPageSource{
		verify: func(ctx context.Context, port int, id string) error {
			ws, err := fetchCDPBrowserWSContext(ctx, port)
			u, parseErr := url.Parse(ws)
			if err != nil || parseErr != nil || u == nil || u.Path != "/devtools/browser/"+id || u.RawQuery != "" || u.Fragment != "" {
				return errors.New("browser suspend identity unavailable")
			}
			return nil
		},
		list: func(ctx context.Context, port int) ([]cdpdiscovery.Target, error) {
			// ListComplete, not ListBounded: never acknowledge a partial capture.
			return cdpdiscovery.ListComplete(ctx, port, cdpHTTPClient)
		},
		read: func(ctx context.Context, p pin, fence func() error) (suspendTab, error) {
			c, err := openSuspendCaptureConn(ctx, p)
			if err != nil {
				return suspendTab{}, err
			}
			defer c.Close()
			return readSuspendTabOn(c, fence)
		},
	}
}

// captureSuspendPages is the live-session capture phase, not a suspension.
// No registry or payload is written and no browser is stopped. The eventual
// transaction must retain its own lifecycle exclusion and revalidate this
// session before publishing the capture. No job action calls this yet.
func (s *cobrowseSession) captureSuspendPages(ctx context.Context, owner string, ownerInitiated bool) (suspendManifest, error) {
	return s.captureSuspendPagesWith(ctx, owner, ownerInitiated, browserSuspendPageSource())
}

func (s *cobrowseSession) captureSuspendPagesWith(ctx context.Context, owner string, ownerInitiated bool, source suspendPageSource) (suspendManifest, error) {
	var captured suspendManifest
	err := s.withSuspendPageCapture(ctx, owner, ownerInitiated, source, func(manifest suspendManifest) error {
		captured = manifest
		return nil
	})
	if err != nil {
		return suspendManifest{}, err
	}
	return captured, nil
}

// withSuspendPageCapture retains action exclusion through a trusted continuation,
// so intent staging need not release and reacquire the action lock after capture.
// The continuation must not re-enter an action or ordinary teardown. It owns
// lifecycle/registry exclusion and revalidation around its own awaits; the action
// lock alone neither stops external navigation nor grants authority. No manager
// transaction uses this boundary yet. A continuation may have durable effects
// even when cancellation or failure prevents acknowledgement; do not roll those
// effects back by deleting a possibly referenced capture.
func (s *cobrowseSession) withSuspendPageCapture(ctx context.Context, owner string, ownerInitiated bool,
	source suspendPageSource, use func(suspendManifest) error) error {
	if ctx == nil || use == nil || ctx.Err() != nil {
		return errors.New("could not capture this owner-bound browser session")
	}
	if _, finite := ctx.Deadline(); !finite || !validSuspendOwner(owner) ||
		source.verify == nil || source.list == nil || source.read == nil {
		return errors.New("could not capture this owner-bound browser session")
	}
	if !s.actionMu.TryLock() {
		return errors.New("browser session is busy; retry later")
	}
	defer s.actionMu.Unlock()
	manifest, err := s.captureSuspendPagesLockedWith(ctx, owner, ownerInitiated, source)
	if err != nil {
		return err
	}
	if ctx.Err() != nil || use(manifest) != nil || ctx.Err() != nil {
		return errors.New("browser capture continuation incomplete")
	}
	return nil
}

// Only withSuspendPageCapture calls this implementation, while holding actionMu.
func (s *cobrowseSession) captureSuspendPagesLockedWith(ctx context.Context, owner string, ownerInitiated bool, source suspendPageSource) (suspendManifest, error) {
	refuse := func() (suspendManifest, error) {
		return suspendManifest{}, errors.New("could not capture this owner-bound browser session")
	}
	s.mu.Lock()
	proc, generation, id := s.proc, s.generation, s.id
	tracking, policy := s.tracking, s.policy
	port, browserID := 0, ""
	if proc != nil {
		port, browserID = proc.debugPort, proc.browserID
	}
	valid := proc != nil && proc.debugPort > 0 && proc.debugPort <= 65535 && proc.browserID != "" &&
		sessionIDPattern.MatchString(id) && len(id) <= 64 && len(browserID) <= 128 && !strings.ContainsAny(browserID, "/?#\\") &&
		s.regDir != "" && s.ownerUserID == owner && generation != 0 &&
		(!s.persistent || s.encProfile != nil) &&
		checkSuspendState(string(s.state), s.humanDrivingLocked(), ownerInitiated) == nil
	s.mu.Unlock()
	if !valid {
		return refuse()
	}
	active := ""
	var tracked *targetSet
	if tracking {
		tracked = targetSetFor(port)
		tracked.mu.Lock()
		active = tracked.active
		refused := tracked.refusedID != ""
		tracked.mu.Unlock()
		if active == "" || refused {
			return refuse()
		}
	}
	// This fence is intentionally pure local state. Every network/policy await
	// is followed by it, and the per-page reader additionally fences each CDP
	// command. Captured URLs never appear in an error or partial result.
	fence := func() error {
		if ctx.Err() != nil {
			return errors.New("browser capture deadline expired")
		}
		s.mu.Lock()
		current := s.proc == proc && s.generation == generation && s.id == id &&
			proc.debugPort == port && proc.browserID == browserID &&
			s.ownerUserID == owner && s.regDir != "" && s.tracking == tracking &&
			(!s.persistent || s.encProfile != nil) &&
			checkSuspendState(string(s.state), s.humanDrivingLocked(), ownerInitiated) == nil
		if current {
			select {
			case <-proc.exited:
				current = false
			default:
			}
		}
		s.mu.Unlock()
		if current && tracked != nil {
			tracked.mu.Lock()
			current = tracked.active == active && tracked.refusedID == ""
			tracked.mu.Unlock()
		}
		if !current {
			return errors.New("browser capture ownership changed")
		}
		return nil
	}
	verify := func() bool { return fence() == nil && source.verify(ctx, port, browserID) == nil && fence() == nil }
	if !verify() {
		return refuse()
	}
	targets, err := source.list(ctx, port)
	if err != nil || fence() != nil || len(targets) == 0 || len(targets) > maxSuspendTabs {
		return refuse()
	}
	seen := make(map[string]struct{}, len(targets))
	endpoints := make(map[string]string, len(targets))
	foundActive := false
	for _, target := range targets {
		u, err := url.Parse(target.WebSocketDebuggerURL)
		if err != nil || u == nil || u.Scheme != "ws" || u.Host != "127.0.0.1:"+strconv.Itoa(port) ||
			u.User != nil || u.RawQuery != "" || u.Fragment != "" || target.Type != "page" ||
			target.ID == "" || len(target.ID) > 128 || strings.ContainsAny(target.ID, "/?#\\") ||
			u.Path != "/devtools/page/"+target.ID {
			return refuse()
		}
		if _, duplicate := seen[target.ID]; duplicate {
			return refuse()
		}
		seen[target.ID] = struct{}{}
		endpoints[target.ID] = target.WebSocketDebuggerURL
		foundActive = foundActive || target.ID == active
	}
	if tracking && !foundActive {
		return refuse()
	}
	if !tracking {
		active = targets[0].ID
	}
	manifest := suspendManifest{Version: suspendManifestVersion, SessionID: id, Generation: generation}
	capturedURLs := make(map[string]string, len(targets))
	for _, target := range targets {
		if policy != nil {
			if policy(ctx, target.URL) != nil || fence() != nil {
				return refuse()
			}
		}
		if !verify() {
			return refuse()
		}
		tab, err := source.read(ctx, pin{session: s, background: true, port: port, id: target.ID, ws: target.WebSocketDebuggerURL}, fence)
		if err != nil || fence() != nil {
			return refuse()
		}
		// Evaluate may observe navigation newer than discovery. Apply policy to
		// the actual captured URL too, never just the discovery document.
		if policy != nil && (policy(ctx, tab.URL) != nil || fence() != nil) {
			return refuse()
		}
		tab.Active = target.ID == active
		manifest.Tabs = append(manifest.Tabs, tab)
		capturedURLs[target.ID] = tab.URL
	}
	// A tab opened, closed or navigated during capture must not disappear
	// silently from a supposedly complete manifest. Do not use status's
	// truncated URL projection for this comparison.
	after, err := source.list(ctx, port)
	if err != nil || fence() != nil || len(after) != len(targets) {
		return refuse()
	}
	clear(seen)
	for _, target := range after {
		_, exists := endpoints[target.ID]
		if _, duplicate := seen[target.ID]; duplicate || !exists || target.Type != "page" ||
			endpoints[target.ID] != target.WebSocketDebuggerURL || capturedURLs[target.ID] != target.URL {
			return refuse()
		}
		seen[target.ID] = struct{}{}
	}
	if !verify() || validateSuspendManifest(manifest, id, generation) != nil {
		return refuse()
	}
	// Enforce the aggregate encoding limit before returning any page data.
	data, err := encodeSuspendManifest(manifest)
	clear(data)
	if err != nil {
		return refuse()
	}
	return manifest, nil
}
