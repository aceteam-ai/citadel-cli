package ingress

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// slugRegexp matches a single DNS label. The wildcard cert covers exactly one
// level (*.apps-domain), so a slug must be one label -- a multi-label host, the
// bare apps domain (empty slug), or any host with an underscore (e.g. the
// reserved _health host) is rejected here and 404s without a dial.
var slugRegexp = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

// Resolver is the slug->route boundary the proxy consults. *Client implements
// it; a fake is used in proxy tests. Resolve returning ok=false is a 404 and
// MUST NOT cause any upstream dial -- the map is the authorization boundary.
// Fresh reports whether the route data is present and within its max-age bound;
// a false Fresh fails closed (503, no dial) before Resolve is consulted.
type Resolver interface {
	Resolve(ctx context.Context, slug string) (Route, bool)
	LoginURL() string
	CookieNames() []string
	Fresh() bool
}

type routeCtxKey struct{}

// Proxy is the per-request reverse proxy handler. It resolves the host to a
// slug, the slug to a mesh pod, enforces gated authz, strips inbound trust
// headers and session cookies, and reverse-proxies over the mesh with SSE and
// WebSocket support.
type Proxy struct {
	appsDomain         string
	resolver           Resolver
	authorizer         Authorizer
	decisions          *decisionCache
	defaultCookieNames []string
	logf               func(format string, args ...any)
	rp                 *httputil.ReverseProxy
	// stripFn removes trust headers and session cookies before the pod sees the
	// request. It is a seam (defaults to stripInbound) so a test can no-op it and
	// exercise the guaranteed-removal-or-reject backstop, which is otherwise a
	// tautology against the real deterministic strip.
	stripFn func(r *http.Request, augment []string)
}

// ProxyConfig configures a Proxy.
type ProxyConfig struct {
	AppsDomain string
	Resolver   Resolver
	Authorizer Authorizer
	// DialContext dials the mesh pod. In production this is network.Dial; tests
	// inject a dialer that targets an httptest server (and can assert it is
	// never called for an unknown slug).
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)
	// DefaultCookieNames is used when the routes payload carries none.
	DefaultCookieNames []string
	AuthzTTL           time.Duration
	AuthzMax           int
	Logf               func(format string, args ...any)
}

// NewProxy builds a Proxy with a single shared ReverseProxy whose Director reads
// the resolved route from the request context.
func NewProxy(cfg ProxyConfig) *Proxy {
	logf := cfg.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	dial := cfg.DialContext
	if dial == nil {
		// Fail CLOSED: a nil DialContext would make http.Transport fall back to
		// the HOST network, and on a citadel up / TUN box a 100.64.x.x target is
		// host-routable -- exactly the dial-the-wrong-thing hazard the
		// map-is-authz boundary exists to prevent. A wiring mistake must error,
		// not silently escape the mesh.
		dial = func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("ingress: no mesh dialer configured")
		}
	}
	p := &Proxy{
		appsDomain:         strings.ToLower(cfg.AppsDomain),
		resolver:           cfg.Resolver,
		authorizer:         cfg.Authorizer,
		decisions:          newDecisionCache(cfg.AuthzTTL, cfg.AuthzMax),
		defaultCookieNames: cfg.DefaultCookieNames,
		logf:               logf,
		stripFn:            stripInbound,
	}
	transport := &http.Transport{
		DialContext:       dial,
		ForceAttemptHTTP2: false, // pods speak h1
		// ResponseHeaderTimeout stays 0: an SSE endpoint can legitimately hold a
		// connection open before the first byte.
		IdleConnTimeout:     90 * time.Second,
		MaxIdleConns:        128,
		MaxIdleConnsPerHost: 32,
		DisableCompression:  true, // pass responses through as-is
	}
	p.rp = &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			route, _ := req.Context().Value(routeCtxKey{}).(Route)
			req.URL.Scheme = "http"
			req.URL.Host = net.JoinHostPort(route.MeshIP.String(), strconv.Itoa(int(route.Port)))
			// req.Host is deliberately left as the public host so the pod sees
			// the hostname the client used (standard ingress behavior). It is
			// pinned by TestProxy_PreservesPublicHost.
		},
		Transport: transport,
		// -1 flushes each write immediately so SSE and chunked streaming reach
		// the client as they arrive (same as internal/gateway/chat_route.go).
		FlushInterval: -1,
		ModifyResponse: func(resp *http.Response) error {
			if cookies := resp.Header.Values("Set-Cookie"); len(cookies) > 0 {
				resp.Header.Del("Set-Cookie")
				for _, c := range cookies {
					resp.Header.Add("Set-Cookie", rewriteSetCookie(c))
				}
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			status := http.StatusBadGateway
			if errors.Is(err, context.DeadlineExceeded) {
				status = http.StatusGatewayTimeout
			}
			route, _ := r.Context().Value(routeCtxKey{}).(Route)
			// Log slug + mesh IP + error only -- never the request body.
			p.logf("[ingress] upstream error slug=%q mesh=%s: %v", route.Slug, route.MeshIP, err)
			w.WriteHeader(status)
			_, _ = w.Write([]byte("upstream unavailable\n"))
		},
	}
	return p
}

// ServeHTTP is the ingress request path.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := hostWithoutPort(r.Host)

	if host == p.appsDomain {
		// Bare apps domain: a small static 404. No default backend.
		notFound(w)
		return
	}
	slug, ok := slugFromHost(host, p.appsDomain)
	if !ok {
		notFound(w)
		return
	}

	// Route freshness gate (invariant: bounded route max-age). A nil map
	// (config-absent, never fetched) or one past RouteMaxAge fails closed: 503
	// and NO upstream dial, so the ingress never proxies to a pod named by stale
	// route data. This is also what the health check reads, so a stale ingress is
	// pulled from rotation rather than serving dark.
	if !p.resolver.Fresh() {
		p.logf("[ingress] routes not fresh; refusing slug=%q", slug)
		serviceUnavailable(w)
		return
	}

	route, ok := p.resolver.Resolve(r.Context(), slug)
	if !ok {
		// Unknown / torn-down / malformed slug: 404, no dial.
		notFound(w)
		return
	}

	augment := p.cookieNames()
	subject := ""
	if route.Visibility == VisibilityGated {
		// Always consult authz, even with no session presented: the control plane
		// is the authority on visibility. A link-visible / unlisted app allows an
		// anonymous viewer (authz returns allow); a private app does not. The
		// session cookie is read here and MUST be stripped before the pod below.
		sessionCookie := gatedSessionCookie(r, augment)
		allow, subj, err := p.authorize(r.Context(), slug, sessionCookie)
		switch {
		case err == nil && allow:
			subject = subj
		case err == nil && sessionCookie == "":
			// Denied with no session presented: steer the visitor to log in.
			p.loginRedirect(w)
			return
		default:
			// Denied with a session, or an authz error: fail closed.
			forbidden(w)
			return
		}
	}

	// Strip session credentials and trust headers before the pod sees the request
	// (both public and gated), then verify no platform session cookie survived --
	// if one did, refuse rather than forward (guaranteed removal or reject).
	p.stripFn(r, augment)
	if outboundSessionCookieLeaked(r) {
		p.logf("[ingress] session isolation failed for slug=%q; refusing", slug)
		sessionIsolationFailed(w)
		return
	}
	setForwardHeaders(r, host, subject)

	ctx := context.WithValue(r.Context(), routeCtxKey{}, route)
	p.rp.ServeHTTP(w, r.WithContext(ctx))
}

// authorize consults the short-TTL decision cache, then the Authorizer on a
// miss. Both allow and deny decisions are cached (not errors) so a burst for one
// gated app makes at most one authz call per TTL window.
func (p *Proxy) authorize(ctx context.Context, slug, cookie string) (bool, string, error) {
	if e, ok := p.decisions.get(slug, cookie); ok {
		return e.allow, e.subject, nil
	}
	if p.authorizer == nil {
		return false, "", errors.New("no authorizer configured")
	}
	allow, subject, err := p.authorizer.Authorize(ctx, slug, cookie)
	if err != nil {
		return false, "", err
	}
	p.decisions.put(slug, cookie, allow, subject)
	return allow, subject, nil
}

func (p *Proxy) cookieNames() []string {
	if names := p.resolver.CookieNames(); len(names) > 0 {
		return names
	}
	return p.defaultCookieNames
}

// loginRedirect responds to an unauthenticated gated request. Per the DoR this
// is a "401 redirect": a 401 status carrying the control-plane login URL (from
// the routes payload) in the Location header, so an SPA/fetch client can steer
// the browser without the ingress hardcoding any login endpoint.
func (p *Proxy) loginRedirect(w http.ResponseWriter) {
	if url := p.resolver.LoginURL(); url != "" {
		w.Header().Set("Location", url)
	}
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte("authentication required\n"))
}

func forbidden(w http.ResponseWriter) {
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte("forbidden\n"))
}

func notFound(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte("not found\n"))
}

// serviceUnavailable refuses a request the ingress cannot safely serve yet: the
// route map is absent (never fetched) or expired past RouteMaxAge. Fail closed
// with 503 and no upstream dial.
func serviceUnavailable(w http.ResponseWriter) {
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte("service unavailable\n"))
}

// sessionIsolationFailed refuses a request the ingress could not sanitize: a
// platform session cookie survived stripping, so forwarding it would leak
// platform credentials to the pod. Fail closed with 500 rather than proxy.
func sessionIsolationFailed(w http.ResponseWriter) {
	w.WriteHeader(http.StatusInternalServerError)
	_, _ = w.Write([]byte("session isolation failed\n"))
}

// hostWithoutPort lowercases the Host, strips a trailing dot, and removes any
// :port. It returns the bare host (or IP literal, which slugFromHost rejects).
func hostWithoutPort(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	host = strings.TrimSuffix(host, ".")
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

// slugFromHost extracts the single-label slug from <slug>.<appsDomain>. It
// rejects IP literals, multi-label hosts, the bare apps domain, and any label
// that is not a valid DNS label (including anything with an underscore, e.g. the
// reserved _health host).
func slugFromHost(host, appsDomain string) (string, bool) {
	host = hostWithoutPort(host)
	appsDomain = strings.ToLower(appsDomain)
	if net.ParseIP(host) != nil {
		return "", false
	}
	suffix := "." + appsDomain
	if !strings.HasSuffix(host, suffix) {
		return "", false
	}
	slug := strings.TrimSuffix(host, suffix)
	if !slugRegexp.MatchString(slug) {
		return "", false
	}
	return slug, true
}
