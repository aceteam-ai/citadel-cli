package ingress

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"html"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"
)

// slugRegexp matches a single DNS label. The wildcard cert covers exactly one
// level (*.apps-domain), so a slug must be one label -- a multi-label host, the
// bare apps domain (empty slug), or any host with an underscore (e.g. the
// reserved _health host) is rejected here and 404s without a dial.
var slugRegexp = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

const (
	exchangeMarkerVersion = "v1"
	exchangeMarkerMaxAge  = 30 * time.Second
	exchangeMarkerSkew    = 5 * time.Second
)

// Resolver is the slug->route boundary the proxy consults. *Client implements
// it; a fake is used in proxy tests. Resolve returning ok=false is a 404, while
// an error is a retryable 503; neither may cause an upstream dial because the
// map is the authorization boundary.
// Fresh reports whether the route data is present and within its max-age bound;
// a false Fresh fails closed (503, no dial) before Resolve is consulted.
type Resolver interface {
	Resolve(ctx context.Context, slug string) (Route, bool, error)
	LoginURL() string
	CookieNames() []string
	Fresh() bool
}

type routeCtxKey struct{}
type cookieNamesCtxKey struct{}

// Proxy is the per-request reverse proxy handler. It resolves the host to a
// slug, the slug to a mesh pod, enforces gated authz, strips inbound trust
// headers and session cookies, and reverse-proxies over the mesh with SSE and
// WebSocket support.
type Proxy struct {
	appsDomain         string
	resolver           Resolver
	authorizer         Authorizer
	exchanger          Exchanger
	defaultCookieNames []string
	logf               func(format string, args ...any)
	rp                 *httputil.ReverseProxy
	// authzGroup collapses concurrent identical (slug, session) authz calls into
	// ONE upstream call. It is NOT a cache: the verdict is used by all in-flight
	// waiters and then discarded (the control plane marks it no-store), so a
	// revoked session loses access on the very next request.
	authzGroup singleflight.Group
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
	Exchanger  Exchanger
	// DialContext dials the mesh pod. In production this is network.Dial; tests
	// inject a dialer that targets an httptest server (and can assert it is
	// never called for an unknown slug).
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)
	// DefaultCookieNames is always unioned with the route-feed list. Feed changes
	// may add protected cookies but cannot remove local fail-safe defaults.
	DefaultCookieNames []string
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
	defaults := mergeCookieNames(
		[]string{AppCredentialCookieName, AppNonceCookieName},
		cfg.DefaultCookieNames,
	)
	p := &Proxy{
		appsDomain:         strings.ToLower(cfg.AppsDomain),
		resolver:           cfg.Resolver,
		authorizer:         cfg.Authorizer,
		exchanger:          cfg.Exchanger,
		defaultCookieNames: defaults,
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
				augment, _ := resp.Request.Context().Value(cookieNamesCtxKey{}).([]string)
				resp.Header.Del("Set-Cookie")
				for _, c := range cookies {
					if protectedSetCookie(c, augment) {
						continue
					}
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

	route, ok, err := p.resolver.Resolve(r.Context(), slug)
	if err != nil {
		p.logf("[ingress] route lookup unavailable for slug=%q: %v", slug, err)
		serviceUnavailable(w)
		return
	}
	if !ok {
		// Unknown / torn-down / malformed slug: 404, no dial.
		notFound(w)
		return
	}

	augment := p.cookieNames()
	if reservedIngressPath(r.URL.Path) {
		p.serveReserved(w, r, slug)
		return
	}
	subject := ""
	if route.Visibility == VisibilityGated {
		// Always consult authz, even with no session presented: the control plane
		// is the authority on visibility. A link-visible / unlisted app allows an
		// anonymous viewer (authz returns allow); a private app does not. The
		// session cookie is read here and MUST be stripped before the pod below.
		sessionCookie := gatedSessionCookie(r, augment)
		if len(sessionCookie) > maxGatedSessionCookieBytes {
			// An oversize (over-chunked) session would be rejected by the control
			// plane's header/size cap; route to a clean re-login instead of a doomed
			// upstream call and an opaque 403. Log the byte count only, never a value.
			p.logf("[ingress] session material over cap (%d bytes) for slug=%q; re-login", len(sessionCookie), slug)
			p.bounce(w, r, slug)
			return
		}
		decision, err := p.authorize(r.Context(), slug, sessionCookie)
		switch {
		case err != nil:
			// Authz backend unavailable (transport/timeout/5xx all map to err in
			// httpAuthorizer). Fail closed as a RETRYABLE 503 -- never serve on an
			// authz error, and never a redirect loop.
			p.logf("[ingress] authz unavailable for slug=%q: %v", slug, err)
			serviceUnavailable(w)
			return
		case decision.Allow:
			subject = decision.Subject
			if hasCookie(r, AppExchangeCookieName) {
				p.clearExchangeMarker(w)
			}
		case decision.Reason != "":
			p.clearCredential(w)
			if p.hasFreshExchangeMarker(r, slug) {
				p.clearExchangeMarker(w)
				p.clearNonce(w)
				p.handoffFailed(w)
				return
			}
			p.bounce(w, r, slug)
			return
		case !hasCookie(r, AppCredentialCookieName):
			// Explicit deny with no session presented: steer the visitor to log in.
			p.bounce(w, r, slug)
			return
		default:
			// Explicit deny with a session: forbidden.
			forbidden(w)
			return
		}
	}

	// Strip session credentials and trust headers before the pod sees the request
	// (both public and gated), then verify no platform session cookie survived --
	// if one did, refuse rather than forward (guaranteed removal or reject).
	p.stripFn(r, augment)
	if outboundSessionCookieLeaked(r, augment) {
		p.logf("[ingress] session isolation failed for slug=%q; refusing", slug)
		sessionIsolationFailed(w)
		return
	}
	setForwardHeaders(r, host, subject)

	ctx := context.WithValue(r.Context(), routeCtxKey{}, route)
	ctx = context.WithValue(ctx, cookieNamesCtxKey{}, augment)
	p.rp.ServeHTTP(w, r.WithContext(ctx))
}

// authzResult is the shared value singleflight passes to every in-flight waiter.
// It is NOT retained after the call returns.
type authzResult struct {
	decision AuthzDecision
}

// authorize resolves the gated authz decision. It does NOT cache across requests
// -- the control plane marks the verdict Cache-Control: no-store, so a revoked or
// downgraded session must lose access on the very next request. It only collapses
// CONCURRENT identical (slug, session) calls into ONE upstream call via
// singleflight (keyed on decisionKey), cutting brownout amplification without
// widening the revocation window at all. An authz error (backend unavailable) is
// propagated so the caller fails closed; it is never turned into an allow.
func (p *Proxy) authorize(ctx context.Context, slug, cookie string) (AuthzDecision, error) {
	if p.authorizer == nil {
		return AuthzDecision{}, errors.New("no authorizer configured")
	}
	res, err, _ := p.authzGroup.Do(decisionKey(slug, cookie), func() (any, error) {
		decision, aerr := p.authorizer.Authorize(ctx, slug, cookie)
		if aerr != nil {
			return nil, aerr
		}
		return authzResult{decision: decision}, nil
	})
	if err != nil {
		return AuthzDecision{}, err
	}
	return res.(authzResult).decision, nil
}

func (p *Proxy) cookieNames() []string {
	return mergeCookieNames(p.defaultCookieNames, p.resolver.CookieNames())
}

func mergeCookieNames(groups ...[]string) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, names := range groups {
		for _, name := range names {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			if _, ok := seen[name]; ok {
				continue
			}
			seen[name] = struct{}{}
			out = append(out, name)
		}
	}
	return out
}

func hasCookie(r *http.Request, name string) bool {
	ck, err := r.Cookie(name)
	return err == nil && ck.Value != ""
}

func reservedIngressPath(requestPath string) bool {
	isReserved := func(candidate string) bool {
		candidate = strings.ToLower(candidate)
		return candidate == "/_ace" || strings.HasPrefix(candidate, "/_ace/")
	}
	return isReserved(requestPath) || isReserved(path.Clean(requestPath))
}

// bounce binds a one-time login handoff to this browser. Navigations receive a
// 303; programmatic fetches retain the 401 contract but still get Location.
func (p *Proxy) bounce(w http.ResponseWriter, r *http.Request, slug string) {
	if !isNavigation(r) {
		nonce := ""
		if existing, err := r.Cookie(AppNonceCookieName); err == nil && validHandoffNonce(existing.Value) {
			nonce = existing.Value
		}
		location := p.loginLocation(slug, nonce, requestNext(r))
		if location == "" {
			p.logf("[ingress] login URL unavailable for slug=%q", slug)
			serviceUnavailable(w)
			return
		}
		w.Header().Set("Location", location)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("authentication required\n"))
		return
	}

	nonceBytes := make([]byte, 16)
	if _, err := rand.Read(nonceBytes); err != nil {
		p.logf("[ingress] cannot mint handoff nonce for slug=%q: %v", slug, err)
		serviceUnavailable(w)
		return
	}
	nonce := base64.RawURLEncoding.EncodeToString(nonceBytes)
	location := p.loginLocation(slug, nonce, requestNext(r))
	if location == "" {
		p.logf("[ingress] login URL unavailable for slug=%q", slug)
		serviceUnavailable(w)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: AppNonceCookieName, Value: nonce, Path: "/", Secure: true,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 300,
	})
	p.clearExchangeMarker(w)
	w.Header().Set("Location", location)
	w.WriteHeader(http.StatusSeeOther)
	_, _ = w.Write([]byte("authentication required\n"))
}

func validHandoffNonce(value string) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	return len(value) == 22 && err == nil && len(decoded) == 16
}

func (p *Proxy) loginLocation(slug, nonce, next string) string {
	u, err := url.Parse(p.resolver.LoginURL())
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	q := u.Query()
	q.Set("slug", slug)
	q.Set("nonce", nonce)
	q.Set("next", next)
	u.RawQuery = q.Encode()
	return u.String()
}

func requestNext(r *http.Request) string {
	next := r.URL.EscapedPath()
	if next == "" {
		next = "/"
	}
	if r.URL.RawQuery != "" {
		next += "?" + r.URL.RawQuery
	}
	return next
}

func isNavigation(r *http.Request) bool {
	if strings.EqualFold(strings.TrimSpace(r.Header.Get("Sec-Fetch-Mode")), "navigate") {
		return true
	}
	var htmlQ, otherQ float64 = -1, -1
	for _, item := range strings.Split(r.Header.Get("Accept"), ",") {
		parts := strings.Split(item, ";")
		media := strings.ToLower(strings.TrimSpace(parts[0]))
		q := 1.0
		for _, parameter := range parts[1:] {
			key, value, ok := strings.Cut(strings.TrimSpace(parameter), "=")
			if ok && strings.EqualFold(key, "q") {
				if parsed, err := strconv.ParseFloat(value, 64); err == nil {
					q = parsed
				}
			}
		}
		switch media {
		case "text/html", "application/xhtml+xml":
			if q > htmlQ {
				htmlQ = q
			}
		case "", "*/*", "text/*":
			// A wildcard does not express a preference for a non-HTML format.
		default:
			if q > otherQ {
				otherQ = q
			}
		}
	}
	return htmlQ > 0 && htmlQ >= otherQ
}

func (p *Proxy) serveReserved(w http.ResponseWriter, r *http.Request, slug string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	switch r.URL.Path {
	case "/_ace/session":
		p.serveSession(w, r, slug)
	case "/_ace/logout":
		p.clearCredential(w)
		p.clearExchangeMarker(w)
		http.Redirect(w, r, "/", http.StatusSeeOther)
	default:
		notFound(w)
	}
}

func (p *Proxy) serveSession(w http.ResponseWriter, r *http.Request, slug string) {
	code := r.URL.Query().Get("code")
	nonce, nonceErr := r.Cookie(AppNonceCookieName)
	decoded, codeErr := base64.RawURLEncoding.DecodeString(code)
	if len(code) != 43 || codeErr != nil || len(decoded) != 32 || nonceErr != nil || nonce.Value == "" {
		p.handoffFailed(w)
		return
	}
	if p.exchanger == nil {
		serviceUnavailableRetry(w)
		return
	}
	result, status, err := p.exchanger.Exchange(r.Context(), slug, code, nonce.Value)
	if err != nil || status >= 500 {
		p.logf("[ingress] handoff exchange unavailable for slug=%q: status=%d err=%v", slug, status, err)
		serviceUnavailableRetry(w)
		return
	}
	if status >= 400 && status < 500 {
		p.handoffFailed(w)
		return
	}
	if status != http.StatusOK || result.Credential == "" || result.MaxAge <= 0 || !safeNext(result.Next) {
		p.handoffFailed(w)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: AppCredentialCookieName, Value: result.Credential, Path: "/", Secure: true,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: result.MaxAge,
	})
	p.setExchangeMarker(w, result.Credential, slug, time.Now())
	p.clearNonce(w)
	w.Header().Set("Location", result.Next)
	w.WriteHeader(http.StatusSeeOther)
}

func safeNext(next string) bool {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.Contains(next, "\\") || strings.ContainsAny(next, "\r\n") {
		return false
	}
	for i := 0; i < len(next); i++ {
		if next[i] <= 0x20 || next[i] == 0x7f {
			return false
		}
	}
	u, err := url.ParseRequestURI(next)
	if err != nil || u.IsAbs() || u.Host != "" {
		return false
	}
	for _, segment := range strings.Split(u.Path, "/") {
		if segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func exchangeMarker(credential, slug string, issued time.Time) string {
	issuedUnix := strconv.FormatInt(issued.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(credential))
	_, _ = mac.Write([]byte("ace-app-exchange\x00" + slug + "\x00" + issuedUnix))
	return exchangeMarkerVersion + "." + issuedUnix + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func validExchangeMarker(marker, credential, slug string, now time.Time) bool {
	parts := strings.Split(marker, ".")
	if len(parts) != 3 || parts[0] != exchangeMarkerVersion || credential == "" {
		return false
	}
	issuedUnix, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return false
	}
	issued := time.Unix(issuedUnix, 0)
	if issued.After(now.Add(exchangeMarkerSkew)) || now.Sub(issued) > exchangeMarkerMaxAge {
		return false
	}
	want := exchangeMarker(credential, slug, issued)
	return hmac.Equal([]byte(marker), []byte(want))
}

func (p *Proxy) hasFreshExchangeMarker(r *http.Request, slug string) bool {
	marker, markerErr := r.Cookie(AppExchangeCookieName)
	credential, credentialErr := r.Cookie(AppCredentialCookieName)
	return markerErr == nil && credentialErr == nil &&
		validExchangeMarker(marker.Value, credential.Value, slug, time.Now())
}

func (p *Proxy) setExchangeMarker(w http.ResponseWriter, credential, slug string, issued time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name: AppExchangeCookieName, Value: exchangeMarker(credential, slug, issued),
		Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
		MaxAge: int(exchangeMarkerMaxAge / time.Second),
	})
}

func (p *Proxy) handoffFailed(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusBadRequest)
	login := html.EscapeString(p.resolver.LoginURL())
	_, _ = w.Write([]byte("<!doctype html><title>Handoff failed</title><h1>handoff failed</h1><p><a href=\"" + login + "\">Return to login</a></p>"))
}

func (p *Proxy) clearCredential(w http.ResponseWriter) {
	expireCookie(w, AppCredentialCookieName)
}

func (p *Proxy) clearNonce(w http.ResponseWriter) {
	expireCookie(w, AppNonceCookieName)
}

func (p *Proxy) clearExchangeMarker(w http.ResponseWriter) {
	expireCookie(w, AppExchangeCookieName)
}

func expireCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: "", Path: "/", Secure: true, HttpOnly: true,
		SameSite: http.SameSiteLaxMode, MaxAge: -1, Expires: time.Unix(1, 0).UTC(),
	})
}

func serviceUnavailableRetry(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "1")
	serviceUnavailable(w)
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
