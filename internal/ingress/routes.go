// Package ingress implements `citadel ingress`: an in-process, mesh-joined
// reverse proxy that fronts hosted web apps (per-node pods) at
// <slug>.<apps-domain> over public HTTPS. It reuses the existing tsnet
// userspace mesh join (see cmd/ingress.go), so the same binary can run as a
// public ingress anywhere with a public IP -- no host-networking changes.
//
// The three seams that keep this package unit-testable without live infra are:
//   - RoutesSource   (routes.go): the control-plane routes/authz feed.
//   - Authorizer     (authz.go):  the server-to-server gated-app check.
//   - CertProvider   (tls.go):    the public wildcard TLS material.
//
// THE ROUTES MAP IS THE AUTHORIZATION BOUNDARY. The ingress never dials
// anything that is not present in the map with a valid mesh IP and port; an
// unknown, torn-down, or malformed slug is a 404 and no upstream dial happens.
package ingress

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

// jitter returns d perturbed by up to +/-10% so a fleet of ingresses polling
// the same control plane does not synchronize into a thundering herd.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	delta := time.Duration(rand.Int63n(int64(d)/5 + 1)) // up to 20% of d
	return d - d/10 + delta                             // d*0.9 .. d*1.1
}

// meshPrefix is the CGNAT range Headscale/tsnet assigns mesh IPs from
// (100.64.0.0/10). A routes-map entry whose address is outside this range is
// rejected at decode time -- the ingress must never be steered into dialing a
// non-mesh (e.g. RFC1918 or public) address by a control-plane typo or a
// compromised feed. This is the decode-time half of the map-is-authz rule.
var meshPrefix = netip.MustParsePrefix("100.64.0.0/10")

// Visibility controls whether a gated-app authz call is made before proxying.
type Visibility string

const (
	VisibilityPublic Visibility = "public"
	VisibilityGated  Visibility = "gated"
)

// Route is one resolved slug -> pod target. MeshIP is already validated to be
// inside meshPrefix; Port is already validated to be in (0, 65535].
type Route struct {
	Slug       string
	MeshIP     netip.Addr
	Port       uint16
	Visibility Visibility
}

// RouteMap is an immutable snapshot of the control-plane routes feed plus the
// payload-level fields the ingress needs (login URL for gated 401 redirects,
// the session-cookie names to strip before the pod sees a request). It is
// swapped atomically by the poller and never mutated in place.
type RouteMap struct {
	etag        string
	routes      map[string]Route
	loginURL    string
	cookieNames []string
}

// ETag returns the ETag the map was fetched with (for If-None-Match).
func (m *RouteMap) ETag() string {
	if m == nil {
		return ""
	}
	return m.etag
}

// LoginURL is the control-plane login URL a gated app redirects an
// unauthenticated visitor to. Carried in the payload (not hardcoded) so the
// binary stays generic.
func (m *RouteMap) LoginURL() string {
	if m == nil {
		return ""
	}
	return m.loginURL
}

// CookieNames are the session-cookie names stripped from an inbound request
// before it reaches the pod. Empty means "use the caller-configured default".
func (m *RouteMap) CookieNames() []string {
	if m == nil {
		return nil
	}
	return m.cookieNames
}

// routesPayload is the wire shape. The control plane owns it; this is the
// minimal subset the ingress needs.
type routesPayload struct {
	Routes         map[string]routeEntry `json:"routes"`
	LoginURL       string                `json:"login_url"`
	SessionCookies []string              `json:"session_cookies"`
}

type routeEntry struct {
	MeshIP     string `json:"mesh_ip"`
	Port       int    `json:"port"`
	Visibility string `json:"visibility"`
}

// applyRoutesResponse is the PURE core of the poller. Given the previous map
// and the raw HTTP result, it decides the next map. It mirrors the repo's
// resolveEgressRelayFrom convention (a pure function tested without HTTP):
//
//   - 304 Not Modified            -> keep last-good (return prev, nil).
//   - 200 OK                      -> parse and build a fresh map. Individual
//     malformed entries (missing/non-mesh IP, bad port) are DROPPED, not fatal:
//     one control-plane typo must not take down every other app. A body that
//     fails to parse at all keeps last-good and returns the error.
//   - anything else (5xx, etc.)   -> keep last-good and return an error so the
//     caller logs the state change.
//
// It never returns a nil map together with a nil error on a non-200: last-good
// is preserved so the ingress keeps serving through a control-plane outage.
func applyRoutesResponse(prev *RouteMap, status int, etag string, body []byte) (*RouteMap, error) {
	switch {
	case status == 304:
		return prev, nil
	case status == 200:
		var p routesPayload
		if err := json.Unmarshal(body, &p); err != nil {
			return prev, fmt.Errorf("routes: malformed JSON body: %w", err)
		}
		next := &RouteMap{
			etag:        etag,
			routes:      make(map[string]Route, len(p.Routes)),
			loginURL:    p.LoginURL,
			cookieNames: p.SessionCookies,
		}
		for slug, e := range p.Routes {
			r, ok := decodeRoute(slug, e)
			if !ok {
				continue // drop this entry, keep the rest
			}
			next.routes[slug] = r
		}
		return next, nil
	default:
		return prev, fmt.Errorf("routes: unexpected status %d", status)
	}
}

// decodeRoute validates and normalizes a single wire entry. ok=false means the
// entry is dropped (missing IP, non-mesh IP, or out-of-range port).
func decodeRoute(slug string, e routeEntry) (Route, bool) {
	ip, err := netip.ParseAddr(e.MeshIP)
	if err != nil || !meshPrefix.Contains(ip) {
		return Route{}, false
	}
	if e.Port <= 0 || e.Port > 65535 {
		return Route{}, false
	}
	vis := Visibility(e.Visibility)
	if vis != VisibilityGated {
		// Anything not explicitly "gated" is public. An unknown visibility
		// string fails safe toward public because gating is enforced by the
		// authz call, and defaulting an unknown value to gated would 401 a
		// public app; the safe direction here is to treat only the explicit
		// "gated" marker as gated.
		vis = VisibilityPublic
	}
	return Route{Slug: slug, MeshIP: ip, Port: uint16(e.Port), Visibility: vis}, true
}

// decision is the outcome of the pure lookup: hit, needs an on-miss lookup, or
// suppressed by the negative cache.
type decision int

const (
	decisionHit      decision = iota // slug is in the map; route is populated
	decisionMiss                     // not in map, not neg-cached: do one on-miss lookup
	decisionNegative                 // not in map, neg-cached: 404 without a lookup
)

// lookup is the PURE resolution step. It consults the map first, then the
// negative cache. It never performs I/O; the on-miss lookup itself is the
// caller's job (Client.Resolve), gated on a decisionMiss return so a scan of
// random slugs cannot amplify into control-plane traffic.
func lookup(m *RouteMap, slug string, neg *negativeCache) (Route, decision) {
	if m != nil {
		if r, ok := m.routes[slug]; ok {
			return r, decisionHit
		}
	}
	if neg.has(slug) {
		return Route{}, decisionNegative
	}
	return Route{}, decisionMiss
}

// negativeCache remembers slugs that were looked up and not found, for negTTL,
// so repeated requests for a nonexistent slug make at most one on-miss call per
// TTL window. It is bounded (maxEntries) so a scan of random slugs cannot grow
// it without limit -- the exact amplification the negative cache exists to
// prevent must not reappear as a memory-exhaustion vector.
type negativeCache struct {
	mu         sync.Mutex
	ttl        time.Duration
	maxEntries int
	entries    map[string]time.Time // slug -> expiry
	now        func() time.Time     // injectable for tests
}

func newNegativeCache(ttl time.Duration, maxEntries int) *negativeCache {
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	if maxEntries <= 0 {
		maxEntries = 4096
	}
	return &negativeCache{
		ttl:        ttl,
		maxEntries: maxEntries,
		entries:    make(map[string]time.Time),
		now:        time.Now,
	}
}

func (n *negativeCache) has(slug string) bool {
	if n == nil {
		return false
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	exp, ok := n.entries[slug]
	if !ok {
		return false
	}
	if n.now().After(exp) {
		delete(n.entries, slug)
		return false
	}
	return true
}

func (n *negativeCache) add(slug string) {
	if n == nil {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.entries) >= n.maxEntries {
		n.purgeExpiredLocked()
		if len(n.entries) >= n.maxEntries {
			// Still full of live entries: evict one arbitrary entry so the cache
			// stays bounded. Bounded churn is acceptable here -- a dropped
			// negative entry just costs one extra on-miss lookup later.
			for k := range n.entries {
				delete(n.entries, k)
				break
			}
		}
	}
	n.entries[slug] = n.now().Add(n.ttl)
}

func (n *negativeCache) purgeExpiredLocked() {
	now := n.now()
	for k, exp := range n.entries {
		if now.After(exp) {
			delete(n.entries, k)
		}
	}
}

// RoutesSource is the injectable control-plane feed. The production
// implementation (httpRoutesSource) is an HTTP client with a bearer token; a
// fake is used in every unit test so the poller, on-miss lookup, and negative
// cache are exercised without a live endpoint.
type RoutesSource interface {
	// Fetch returns the full routes map. etag is the caller's last-known ETag
	// (sent as If-None-Match); newETag is the response ETag. status is the raw
	// HTTP status (304 = unchanged, 200 = replace).
	Fetch(ctx context.Context, etag string) (status int, newETag string, body []byte, err error)
	// FetchOne performs a single-slug on-miss lookup. status 404 = unknown.
	FetchOne(ctx context.Context, slug string) (status int, body []byte, err error)
}

// Client holds the live routes map, polls the source, and resolves slugs. It is
// safe for concurrent use: the full map is swapped via an atomic pointer, and
// on-miss positive results are stored in a separate mutex-guarded overlay
// (never mutated into the shared map in place -- that would be a data race).
type Client struct {
	src          RoutesSource
	pollInterval time.Duration
	logf         func(format string, args ...any)

	current     atomic.Pointer[RouteMap]
	fetchedOnce atomic.Bool

	neg *negativeCache

	overlayMu sync.RWMutex
	overlay   map[string]Route // positive on-miss inserts; cleared on a fresh full map

	// lastState tracks the last-logged feed health so we log at most once per
	// state change (healthy <-> failing), not on every failed poll.
	stateMu    sync.Mutex
	lastFailed bool
}

// ClientConfig configures a Client. Only Source is required.
type ClientConfig struct {
	Source       RoutesSource
	PollInterval time.Duration
	NegativeTTL  time.Duration
	NegativeMax  int
	Logf         func(format string, args ...any)
}

// NewClient builds a routes Client. It does not start polling; call Poll.
func NewClient(cfg ClientConfig) *Client {
	poll := cfg.PollInterval
	if poll <= 0 {
		poll = 10 * time.Second
	}
	logf := cfg.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	c := &Client{
		src:          cfg.Source,
		pollInterval: poll,
		logf:         logf,
		neg:          newNegativeCache(cfg.NegativeTTL, cfg.NegativeMax),
		overlay:      make(map[string]Route),
	}
	return c
}

// Current returns the live full-map snapshot (may be nil before the first
// successful fetch).
func (c *Client) Current() *RouteMap { return c.current.Load() }

// LoginURL implements Resolver: the payload-carried gated-app login URL.
func (c *Client) LoginURL() string { return c.current.Load().LoginURL() }

// CookieNames implements Resolver: the payload-carried session-cookie names.
func (c *Client) CookieNames() []string { return c.current.Load().CookieNames() }

// FetchedOnce reports whether at least one 200 full-map fetch has succeeded.
// The health check gates on this: an ingress that has never loaded routes must
// not be advertised as ready.
func (c *Client) FetchedOnce() bool { return c.fetchedOnce.Load() }

// pollOnce performs one fetch cycle and swaps the map on a 200. It is separated
// from Poll so tests can drive a single cycle deterministically.
func (c *Client) pollOnce(ctx context.Context) {
	prev := c.current.Load()
	status, etag, body, err := c.src.Fetch(ctx, prev.ETag())
	if err != nil {
		c.noteFailure(fmt.Sprintf("routes fetch failed: %v", err))
		return
	}
	next, aerr := applyRoutesResponse(prev, status, etag, body)
	if aerr != nil {
		c.noteFailure(fmt.Sprintf("routes response rejected: %v", aerr))
		return
	}
	c.noteHealthy()
	if status == 200 && next != prev {
		c.current.Store(next)
		c.fetchedOnce.Store(true)
		// A fresh authoritative map supersedes any on-miss overlay entries.
		c.overlayMu.Lock()
		c.overlay = make(map[string]Route)
		c.overlayMu.Unlock()
	}
}

// Poll runs the fetch loop until ctx is cancelled. It fetches immediately, then
// every pollInterval with jitter (so a fleet of ingresses does not hammer the
// control plane in lockstep).
func (c *Client) Poll(ctx context.Context) {
	c.pollOnce(ctx)
	for {
		d := jitter(c.pollInterval)
		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
			c.pollOnce(ctx)
		}
	}
}

// Resolve returns the pod route for a slug, or ok=false (which the caller turns
// into a 404). It combines the pure lookup with the on-miss single-slug fetch:
// a decisionMiss triggers exactly one FetchOne; a positive result is inserted
// into the overlay and a negative result is negative-cached. THE MAP IS THE
// AUTHORIZATION BOUNDARY -- ok=false means no dial.
func (c *Client) Resolve(ctx context.Context, slug string) (Route, bool) {
	m := c.current.Load()
	r, dec := lookup(m, slug, c.neg)
	switch dec {
	case decisionHit:
		return r, true
	case decisionNegative:
		return Route{}, false
	}

	// decisionMiss: check the overlay (a prior on-miss positive) before making
	// another control-plane call.
	c.overlayMu.RLock()
	or, ok := c.overlay[slug]
	c.overlayMu.RUnlock()
	if ok {
		return or, true
	}

	status, body, err := c.src.FetchOne(ctx, slug)
	if err != nil || status != 200 {
		// Unknown or transient failure: negative-cache so a scan cannot amplify.
		c.neg.add(slug)
		return Route{}, false
	}
	var p routesPayload
	if err := json.Unmarshal(body, &p); err != nil {
		c.neg.add(slug)
		return Route{}, false
	}
	e, ok := p.Routes[slug]
	if !ok {
		c.neg.add(slug)
		return Route{}, false
	}
	route, ok := decodeRoute(slug, e)
	if !ok {
		c.neg.add(slug)
		return Route{}, false
	}
	c.overlayMu.Lock()
	c.overlay[slug] = route
	c.overlayMu.Unlock()
	return route, true
}

func (c *Client) noteFailure(msg string) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if !c.lastFailed {
		c.lastFailed = true
		c.logf("[ingress] %s (serving last-good routes)", msg)
	}
}

func (c *Client) noteHealthy() {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if c.lastFailed {
		c.lastFailed = false
		c.logf("[ingress] routes feed recovered")
	}
}
