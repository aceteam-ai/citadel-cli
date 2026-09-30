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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"
	"golang.org/x/time/rate"
)

// RouteMaxAge bounds how long a fetched route map may be served after it was
// last confirmed fresh by the control plane. Past this age the map is EXPIRED:
// every request fails closed (503, no upstream dial) until a poll refreshes it.
// This is the request-path half of the map-is-authz rule -- a stale map must not
// keep steering dials indefinitely through a control-plane outage. 60s is the
// owner-chosen bound (citadel-cli#1099).
const RouteMaxAge = 60 * time.Second

const (
	defaultOnMissRate  = rate.Limit(10)
	defaultOnMissBurst = 20
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
	appsDomain  string
	loginURL    string
	cookieNames []string
	// fetchedAt is when the control plane last confirmed this map fresh (a 200
	// that built it, or a 304 that re-validated it). The request path enforces
	// RouteMaxAge against it; a zero value is never fresh.
	fetchedAt time.Time
}

// fresh reports whether the map is present and within maxAge of now. A nil map
// (never fetched) is never fresh. maxAge <= 0 disables the bound (defensive; the
// default is RouteMaxAge).
func (m *RouteMap) fresh(now time.Time, maxAge time.Duration) bool {
	if m == nil {
		return false
	}
	if maxAge <= 0 {
		return true
	}
	return now.Sub(m.fetchedAt) <= maxAge
}

// refreshed returns a shallow copy of m with fetchedAt advanced to now. routes,
// appsDomain, loginURL, and cookieNames are shared by reference (immutable after
// construction), so this is cheap. Used on a 304 Not Modified: the content is
// unchanged but the control plane has just CONFIRMED it is current, so the
// freshness clock resets -- otherwise a healthy feed returning 304 every poll
// would expire after RouteMaxAge and dark the ingress.
func (m *RouteMap) refreshed(now time.Time) *RouteMap {
	if m == nil {
		return nil
	}
	cp := *m
	cp.fetchedAt = now
	return &cp
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

// AppsDomain is the control-plane domain this route feed was generated for.
// The client compares it with its configured public domain after every poll so
// a crossed environment/feed remains visible to operators during outages too.
func (m *RouteMap) AppsDomain() string {
	if m == nil {
		return ""
	}
	return m.appsDomain
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
	AppsDomain     string                `json:"apps_domain"`
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
// resolveEgressRelayFrom convention (a pure function tested without HTTP); now
// is threaded in (not read from the clock inside) so freshness stamping is
// deterministic under test:
//
//   - 304 Not Modified            -> re-stamp last-good fresh (a new map sharing
//     prev's routes, fetchedAt = now). The content is unchanged but was just
//     re-validated, so the freshness clock must reset.
//   - 200 OK                      -> parse and build a fresh map (fetchedAt =
//     now). Individual malformed entries (missing/non-mesh IP, bad port) are
//     DROPPED, not fatal: one control-plane typo must not take down every other
//     app. A body that fails to parse at all keeps last-good and returns the
//     error.
//   - anything else (5xx, etc.)   -> keep last-good UNCHANGED (a 5xx is not a
//     freshness confirmation, so fetchedAt is NOT advanced) and return an error
//     so the caller logs the state change; RouteMaxAge still expires it.
//
// It never returns a nil map together with a nil error on a non-200: last-good
// is preserved so the ingress keeps serving through a control-plane outage,
// bounded by RouteMaxAge.
func applyRoutesResponse(prev *RouteMap, status int, etag string, body []byte, now time.Time) (*RouteMap, error) {
	switch {
	case status == 304:
		return prev.refreshed(now), nil
	case status == 200:
		var p routesPayload
		if err := json.Unmarshal(body, &p); err != nil {
			return prev, fmt.Errorf("routes: malformed JSON body: %w", err)
		}
		next := &RouteMap{
			etag:        etag,
			routes:      make(map[string]Route, len(p.Routes)),
			appsDomain:  p.AppsDomain,
			loginURL:    p.LoginURL,
			cookieNames: p.SessionCookies,
			fetchedAt:   now,
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
	return Route{Slug: slug, MeshIP: ip, Port: uint16(e.Port), Visibility: normalizeVisibility(e.Visibility)}, true
}

// normalizeVisibility maps a wire visibility string to a Route visibility,
// failing closed. Only the explicit "public" marker skips the gated authz call;
// EVERYTHING else -- "gated" and, crucially, any unknown or absent value --
// resolves to VisibilityGated so an unrecognized value requires authz rather
// than silently bypassing it. This is the deliberate fail-closed direction: the
// control plane folds private/org/unlisted into "gated" and the authz endpoint
// is the authority on who may view (it allows an anonymous viewer for a
// link-visible app and denies one for a private app), so defaulting the unknown
// case to gated costs at most one authz call, while defaulting to public would
// expose a mislabeled app with no check.
func normalizeVisibility(s string) Visibility {
	switch Visibility(strings.ToLower(strings.TrimSpace(s))) {
	case VisibilityPublic:
		return VisibilityPublic
	default:
		return VisibilityGated
	}
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
	n.addAt(slug, n.now())
}

func (n *negativeCache) addAt(slug string, fetchedAt time.Time) {
	if n == nil {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	expiresAt := fetchedAt.Add(n.ttl)
	if !n.now().Before(expiresAt) {
		// A slow lookup consumed the whole negative TTL. Do not evict a live
		// entry merely to insert one that is already expired on arrival.
		delete(n.entries, slug)
		return
	}
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
	n.entries[slug] = expiresAt
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

// overlayEntry is one on-miss positive result plus the time it was fetched, so
// the entry can expire independently of the (possibly perpetually-304-fresh)
// full map.
type overlayEntry struct {
	route     Route
	fetchedAt time.Time
}

// Client holds the live routes map, polls the source, and resolves slugs. It is
// safe for concurrent use: the full map is swapped via an atomic pointer, and
// on-miss positive results are stored in a separate mutex-guarded overlay
// (never mutated into the shared map in place -- that would be a data race).
type Client struct {
	src          RoutesSource
	pollInterval time.Duration
	appsDomain   string
	logf         func(format string, args ...any)

	current     atomic.Pointer[RouteMap]
	fetchedOnce atomic.Bool

	// now and maxAge back the request-path freshness gate (Fresh/Resolve). now is
	// injectable so tests drive staleness deterministically; maxAge defaults to
	// RouteMaxAge.
	now    func() time.Time
	maxAge time.Duration

	neg *negativeCache

	missGroup   singleflight.Group
	missLimiter *rate.Limiter

	overlayMu sync.RWMutex
	// overlay holds positive on-miss single-slug results. Cleared on a fresh full
	// map (200), AND each entry carries its own fetchedAt so it expires after
	// maxAge on its own -- otherwise, under a perpetual-304 feed (whole map
	// re-stamped fresh but never replaced), a slug absent from the full map and
	// later tombstoned could resolve forever.
	overlay map[string]overlayEntry
	// generation advances with every authoritative 200 while overlayMu is held.
	// An on-miss request snapshots it before I/O and may commit only if it is
	// unchanged, preventing an older FetchOne from resurrecting a route after a
	// newer full map superseded it.
	generation uint64

	// lastState tracks the last-logged feed health so we log at most once per
	// state change (healthy <-> failing), not on every failed poll.
	stateMu    sync.Mutex
	lastFailed bool
}

// ClientConfig configures a Client. Only Source is required.
type ClientConfig struct {
	Source       RoutesSource
	PollInterval time.Duration
	// AppsDomain is the public domain this ingress is configured to serve. A
	// feed whose apps_domain differs is still applied, but warns on every poll
	// (including 304 and failures retaining last-good) so crossed environment
	// wiring is not silent.
	AppsDomain  string
	NegativeTTL time.Duration
	NegativeMax int
	// OnMissRateLimit and OnMissBurst bound the single global FetchOne budget.
	// Non-positive values use conservative defaults. Admission is immediate;
	// Resolve never waits for a token.
	OnMissRateLimit rate.Limit
	OnMissBurst     int
	// MaxAge bounds route freshness on the request path; 0 uses RouteMaxAge.
	MaxAge time.Duration
	Logf   func(format string, args ...any)
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
	maxAge := cfg.MaxAge
	if maxAge <= 0 {
		maxAge = RouteMaxAge
	}
	onMissRate := cfg.OnMissRateLimit
	if onMissRate <= 0 {
		onMissRate = defaultOnMissRate
	}
	onMissBurst := cfg.OnMissBurst
	if onMissBurst <= 0 {
		onMissBurst = defaultOnMissBurst
	}
	c := &Client{
		src:          cfg.Source,
		pollInterval: poll,
		appsDomain:   strings.TrimSpace(cfg.AppsDomain),
		logf:         logf,
		now:          time.Now,
		maxAge:       maxAge,
		neg:          newNegativeCache(cfg.NegativeTTL, cfg.NegativeMax),
		missLimiter:  rate.NewLimiter(onMissRate, onMissBurst),
		overlay:      make(map[string]overlayEntry),
	}
	// Keep negative-cache expiry on the same injectable clock as freshness and
	// rate admission so slow-response and limiter tests remain deterministic.
	c.neg.now = func() time.Time { return c.now() }
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
// An ingress that has never loaded routes must not be advertised as ready.
func (c *Client) FetchedOnce() bool { return c.fetchedOnce.Load() }

// Fresh implements Resolver: it reports whether the live route map is present
// AND within RouteMaxAge of the last time the control plane confirmed it fresh.
// A nil map (never fetched) or an expired one fails closed, so the proxy serves
// 503 and dials nothing, and the health check pulls a stale ingress from
// rotation (invariant: bounded route freshness). It subsumes FetchedOnce: a
// never-fetched map is not fresh.
func (c *Client) Fresh() bool {
	return c.current.Load().fresh(c.now(), c.maxAge)
}

// pollOnce performs one fetch cycle and swaps the map on a 200. It is separated
// from Poll so tests can drive a single cycle deterministically.
func (c *Client) pollOnce(ctx context.Context) {
	prev := c.current.Load()
	requestStartedAt := c.now()
	status, etag, body, err := c.src.Fetch(ctx, prev.ETag())
	if err != nil {
		c.warnAppsDomainMismatch(prev)
		c.noteFailure(fmt.Sprintf("routes fetch failed: %v", err))
		return
	}
	next, aerr := applyRoutesResponse(prev, status, etag, body, requestStartedAt)
	if aerr != nil {
		c.warnAppsDomainMismatch(prev)
		c.noteFailure(fmt.Sprintf("routes response rejected: %v", aerr))
		return
	}
	c.warnAppsDomainMismatch(next)
	c.noteHealthy()
	if next == prev {
		return // 5xx path: last-good kept, no re-stamp
	}
	// Store on any change, INCLUDING a 304's re-stamped clone -- that is what
	// resets the freshness clock so a healthy feed returning 304 every poll does
	// not expire after RouteMaxAge.
	if status == 200 {
		// Publish the new authoritative map, generation, and empty overlay as one
		// commit relative to FetchOne insertion. Readers that already observed the
		// old map recheck inside their singleflight leader before doing I/O.
		c.overlayMu.Lock()
		c.current.Store(next)
		c.generation++
		c.overlay = make(map[string]overlayEntry)
		c.overlayMu.Unlock()
		c.fetchedOnce.Store(true)
		return
	}
	c.current.Store(next)
}

func (c *Client) warnAppsDomainMismatch(m *RouteMap) {
	if c.appsDomain == "" || m == nil {
		return
	}
	feedDomain := strings.TrimSpace(m.AppsDomain())
	if strings.EqualFold(feedDomain, c.appsDomain) {
		return
	}
	c.logf("[ingress] WARNING: routes feed apps_domain=%q differs from configured apps domain=%q", feedDomain, c.appsDomain)
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
// AUTHORIZATION BOUNDARY -- ok=false or err!=nil means no dial. Same-slug
// misses collapse into one request, and a global immediate-deny token bucket
// bounds distinct misses so random-slug scans cannot amplify into control-plane
// traffic. Control-plane failures remain distinguishable so the proxy can
// return retryable 503 instead of misreporting them as an unknown-slug 404.
func (c *Client) Resolve(ctx context.Context, slug string) (Route, bool, error) {
	if r, dec := c.resolveCached(slug, c.now()); dec != decisionMiss {
		return r, dec == decisionHit, nil
	}

	result := c.missGroup.DoChan(slug, func() (any, error) {
		return c.resolveMiss(ctx, slug)
	})
	select {
	case <-ctx.Done():
		return Route{}, false, ctx.Err()
	case call := <-result:
		if call.Err != nil {
			return Route{}, false, call.Err
		}
		resolved := call.Val.(resolveResult)
		return resolved.route, resolved.ok, nil
	}
}

type resolveResult struct {
	route Route
	ok    bool
}

// resolveCached performs the lock-bounded, I/O-free resolution shared by the
// fast path and the singleflight leader's mandatory recheck.
func (c *Client) resolveCached(slug string, now time.Time) (Route, decision) {
	m := c.current.Load()
	if !m.fresh(now, c.maxAge) {
		return Route{}, decisionNegative
	}
	if r, dec := lookup(m, slug, c.neg); dec != decisionMiss {
		return r, dec
	}
	c.overlayMu.RLock()
	oe, ok := c.overlay[slug]
	c.overlayMu.RUnlock()
	if ok && now.Sub(oe.fetchedAt) <= c.maxAge {
		return oe.route, decisionHit
	}
	return Route{}, decisionMiss
}

func (c *Client) resolveMiss(ctx context.Context, slug string) (resolveResult, error) {
	// Another request or a full-map 200 may have populated/suppressed this slug
	// while this caller waited to become the per-slug leader.
	if r, dec := c.resolveCached(slug, c.now()); dec != decisionMiss {
		return resolveResult{route: r, ok: dec == decisionHit}, nil
	}
	if !c.missLimiter.AllowN(c.now(), 1) {
		return resolveResult{}, fmt.Errorf("routes: single-slug lookup rate limited")
	}

	// Snapshot the authoritative generation immediately before request send.
	// No client lock is held across control-plane I/O.
	c.overlayMu.RLock()
	generation := c.generation
	c.overlayMu.RUnlock()
	requestStartedAt := c.now()
	status, body, err := c.src.FetchOne(ctx, slug)
	if current, superseded := c.resolveAfterGenerationChange(slug, generation); superseded {
		return current, nil
	}
	if err != nil {
		return resolveResult{}, fmt.Errorf("routes: single-slug fetch failed: %w", err)
	}
	if status == 404 {
		c.neg.addAt(slug, requestStartedAt)
		return resolveResult{}, nil
	}
	if status != 200 {
		return resolveResult{}, fmt.Errorf("routes: unexpected single-slug status %d", status)
	}
	var p routesPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return resolveResult{}, fmt.Errorf("routes: malformed single-slug response: %w", err)
	}
	e, ok := p.Routes[slug]
	if !ok {
		c.neg.addAt(slug, requestStartedAt)
		return resolveResult{}, nil
	}
	route, ok := decodeRoute(slug, e)
	if !ok {
		c.neg.addAt(slug, requestStartedAt)
		return resolveResult{}, nil
	}

	c.overlayMu.Lock()
	defer c.overlayMu.Unlock()
	if c.generation != generation {
		// A newer authoritative map won while FetchOne was blocked. Never return
		// or reinsert the older result; resolve only from that winning map.
		m := c.current.Load()
		if m.fresh(c.now(), c.maxAge) {
			if current, exists := m.routes[slug]; exists {
				return resolveResult{route: current, ok: true}, nil
			}
		}
		return resolveResult{}, nil
	}
	now := c.now()
	if !c.current.Load().fresh(now, c.maxAge) || now.Sub(requestStartedAt) > c.maxAge {
		return resolveResult{}, fmt.Errorf("routes: single-slug response exceeded freshness bound")
	}
	c.overlay[slug] = overlayEntry{route: route, fetchedAt: requestStartedAt}
	return resolveResult{route: route, ok: true}, nil
}

// resolveAfterGenerationChange discards every result from a FetchOne request
// that overlapped a newer authoritative map. This check must happen before
// interpreting either successful or failed FetchOne responses: the winning
// map may already contain the slug even when the older request returned a
// transport error, redirect, 404, or malformed payload.
func (c *Client) resolveAfterGenerationChange(slug string, generation uint64) (resolveResult, bool) {
	c.overlayMu.RLock()
	defer c.overlayMu.RUnlock()
	if c.generation == generation {
		return resolveResult{}, false
	}
	m := c.current.Load()
	if m.fresh(c.now(), c.maxAge) {
		if current, exists := m.routes[slug]; exists {
			return resolveResult{route: current, ok: true}, true
		}
	}
	return resolveResult{}, true
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
