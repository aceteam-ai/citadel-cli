package ingress

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestSlugFromHost(t *testing.T) {
	const domain = "apps.example.com"
	cases := []struct {
		host     string
		wantSlug string
		wantOK   bool
	}{
		{"app.apps.example.com", "app", true},
		{"App.Apps.Example.Com", "app", true},     // case-insensitive
		{"app.apps.example.com:443", "app", true}, // port stripped
		{"app.apps.example.com.", "app", true},    // trailing dot
		{"apps.example.com", "", false},           // bare domain
		{"a.b.apps.example.com", "", false},       // multi-label
		{"other.example.org", "", false},          // unrelated host
		{"_health.apps.example.com", "", false},   // reserved health host (underscore)
		{"100.64.0.1", "", false},                 // IP literal
		{"-bad.apps.example.com", "", false},      // invalid label
	}
	for _, tc := range cases {
		t.Run(tc.host, func(t *testing.T) {
			slug, ok := slugFromHost(tc.host, domain)
			if ok != tc.wantOK || slug != tc.wantSlug {
				t.Errorf("slugFromHost(%q) = (%q,%v), want (%q,%v)", tc.host, slug, ok, tc.wantSlug, tc.wantOK)
			}
		})
	}
}

// --- test doubles -----------------------------------------------------------

type fakeResolver struct {
	routes  map[string]Route
	login   string
	cookies []string
	stale   bool // when true, Fresh() reports not-fresh (config-absent / expired)
}

func (f *fakeResolver) Resolve(_ context.Context, slug string) (Route, bool) {
	r, ok := f.routes[slug]
	return r, ok
}
func (f *fakeResolver) LoginURL() string      { return f.login }
func (f *fakeResolver) CookieNames() []string { return f.cookies }
func (f *fakeResolver) Fresh() bool           { return !f.stale }

type fakeAuthorizer struct {
	allow      bool
	subject    string
	err        error
	calls      int32
	lastCookie string // the cookie material the proxy forwarded (read after the round trip)
}

func (f *fakeAuthorizer) Authorize(_ context.Context, _, cookie string) (bool, string, error) {
	atomic.AddInt32(&f.calls, 1)
	f.lastCookie = cookie
	return f.allow, f.subject, f.err
}

type countingDialer struct {
	target string
	calls  int32
}

func (d *countingDialer) dial(_ context.Context, _, _ string) (net.Conn, error) {
	atomic.AddInt32(&d.calls, 1)
	return net.Dial("tcp", d.target)
}

func publicRoute() Route {
	return Route{Slug: "app", MeshIP: netip.MustParseAddr("100.64.0.10"), Port: 8080, Visibility: VisibilityPublic}
}
func gatedRoute() Route {
	return Route{Slug: "app", MeshIP: netip.MustParseAddr("100.64.0.10"), Port: 8080, Visibility: VisibilityGated}
}

// startProxy wires a Proxy in front of upstreamAddr and returns an httptest
// server hosting it plus the dialer (to assert dial counts).
func startProxy(t *testing.T, res Resolver, authz Authorizer, upstreamAddr string) (*httptest.Server, *countingDialer) {
	t.Helper()
	d := &countingDialer{target: upstreamAddr}
	p := NewProxy(ProxyConfig{
		AppsDomain:         "apps.example.com",
		Resolver:           res,
		Authorizer:         authz,
		DialContext:        d.dial,
		DefaultCookieNames: []string{"sess"},
	})
	ts := httptest.NewServer(p)
	t.Cleanup(ts.Close)
	return ts, d
}

func get(t *testing.T, ts *httptest.Server, host, path string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Timeout:       5 * time.Second,
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	return resp
}

// --- integration tests ------------------------------------------------------

func TestProxy_UnknownSlug404NoDial(t *testing.T) {
	res := &fakeResolver{routes: map[string]Route{}} // nothing resolves
	ts, d := startProxy(t, res, nil, "127.0.0.1:1")  // dial target intentionally dead
	resp := get(t, ts, "ghost.apps.example.com", "/", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if n := atomic.LoadInt32(&d.calls); n != 0 {
		t.Fatalf("dialer called %d times for an unknown slug; must be 0 (map is the authz boundary)", n)
	}
}

func TestProxy_BareDomainAndBadHost404(t *testing.T) {
	res := &fakeResolver{routes: map[string]Route{"app": publicRoute()}}
	ts, _ := startProxy(t, res, nil, "127.0.0.1:1")
	for _, host := range []string{"apps.example.com", "unrelated.example.org"} {
		resp := get(t, ts, host, "/", nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("host %q: status = %d, want 404", host, resp.StatusCode)
		}
	}
}

func TestProxy_PublicGET_PreservesPublicHost(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "host=%s", r.Host)
	}))
	defer upstream.Close()
	res := &fakeResolver{routes: map[string]Route{"app": publicRoute()}}
	ts, d := startProxy(t, res, nil, upstream.Listener.Addr().String())

	resp := get(t, ts, "app.apps.example.com", "/", nil)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if string(body) != "host=app.apps.example.com" {
		t.Fatalf("pod saw %q, want the public host preserved", string(body))
	}
	if atomic.LoadInt32(&d.calls) == 0 {
		t.Fatal("dialer should have been used")
	}
}

func TestProxy_ChunkedStreamedUnbuffered(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		w.WriteHeader(200)
		io.WriteString(w, "chunk1")
		fl.Flush()
		<-release // block: the client must receive chunk1 before we send chunk2
		io.WriteString(w, "chunk2")
		fl.Flush()
	}))
	defer upstream.Close()
	res := &fakeResolver{routes: map[string]Route{"app": publicRoute()}}
	ts, _ := startProxy(t, res, nil, upstream.Listener.Addr().String())

	resp := get(t, ts, "app.apps.example.com", "/", nil)
	defer resp.Body.Close()

	// Read exactly chunk1. If the proxy buffered the whole response, this read
	// would block until chunk2 is written -- which only happens after we
	// close(release), which we have not done yet. So a successful read proves
	// unbuffered streaming.
	buf := make([]byte, 6)
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatalf("reading first chunk: %v", err)
	}
	if string(buf) != "chunk1" {
		t.Fatalf("first chunk = %q, want chunk1", string(buf))
	}
	close(release)
	rest, _ := io.ReadAll(resp.Body)
	if string(rest) != "chunk2" {
		t.Fatalf("rest = %q, want chunk2", string(rest))
	}
}

func TestProxy_SSEIncremental(t *testing.T) {
	gate := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		for i := 0; i < 3; i++ {
			fmt.Fprintf(w, "data: %d\n\n", i)
			fl.Flush()
			if i == 0 {
				<-gate // hold after the first event
			}
		}
	}))
	defer upstream.Close()
	res := &fakeResolver{routes: map[string]Route{"app": publicRoute()}}
	ts, _ := startProxy(t, res, nil, upstream.Listener.Addr().String())

	resp := get(t, ts, "app.apps.example.com", "/", nil)
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	line, err := br.ReadString('\n')
	if err != nil || !strings.Contains(line, "data: 0") {
		t.Fatalf("first SSE event = %q err=%v, want data: 0 before stream completes", line, err)
	}
	close(gate)
}

func TestProxy_WebSocketEcho(t *testing.T) {
	upgrader := websocket.Upgrader{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		for {
			mt, msg, err := c.ReadMessage()
			if err != nil {
				return
			}
			if err := c.WriteMessage(mt, msg); err != nil {
				return
			}
		}
	}))
	defer upstream.Close()
	res := &fakeResolver{routes: map[string]Route{"app": publicRoute()}}
	ts, d := startProxy(t, res, nil, upstream.Listener.Addr().String())

	// Dial the WebSocket through the proxy: URL host is the public app host (so
	// the proxy resolves the slug), but the TCP connection targets the proxy.
	proxyAddr := ts.Listener.Addr().String()
	dialer := websocket.Dialer{
		NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return net.Dial("tcp", proxyAddr)
		},
		HandshakeTimeout: 5 * time.Second,
	}
	c, _, err := dialer.Dial("ws://app.apps.example.com/", nil)
	if err != nil {
		t.Fatalf("ws dial through proxy: %v", err)
	}
	defer c.Close()
	if err := c.WriteMessage(websocket.TextMessage, []byte("ping")); err != nil {
		t.Fatal(err)
	}
	_, msg, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("ws read: %v", err)
	}
	if string(msg) != "ping" {
		t.Fatalf("echo = %q, want ping", string(msg))
	}
	if atomic.LoadInt32(&d.calls) == 0 {
		t.Fatal("dialer should have been used for the WS upstream")
	}
}

func TestProxy_GatedNoCookieDeniedIs401Redirect(t *testing.T) {
	// A gated route with no session presented consults authz (the control plane
	// is the authority on visibility); when authz DENIES, an anonymous visitor is
	// steered to log in. Note authz IS called now (calls==1): the pre-auth
	// short-circuit was removed so a link-visible app that authz would allow is
	// reachable anonymously (see TestProxy_GatedAnonymousAllowedIsProxied).
	res := &fakeResolver{routes: map[string]Route{"app": gatedRoute()}, login: "https://login.example.com/start"}
	authz := &fakeAuthorizer{allow: false}
	ts, d := startProxy(t, res, authz, "127.0.0.1:1")
	resp := get(t, ts, "app.apps.example.com", "/", nil) // no cookie
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "https://login.example.com/start" {
		t.Fatalf("Location = %q, want the login URL", loc)
	}
	if atomic.LoadInt32(&authz.calls) != 1 {
		t.Fatalf("authz calls = %d, want 1 (authz is the visibility authority even for anonymous)", atomic.LoadInt32(&authz.calls))
	}
	if atomic.LoadInt32(&d.calls) != 0 {
		t.Fatal("no dial should happen for a denied gated request")
	}
}

// TestProxy_GatedAnonymousAllowedIsProxied is the link-visible / unlisted case:
// a gated route, NO session cookie at all, but authz ALLOWS the anonymous viewer
// (aceteam's canGatewayServeApp(slug, "") contract). The request is proxied with
// no X-Ingress-Subject, and the authorizer received an empty cookie.
func TestProxy_GatedAnonymousAllowedIsProxied(t *testing.T) {
	var sawSubject string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawSubject = r.Header.Get("X-Ingress-Subject")
		w.WriteHeader(200)
	}))
	defer upstream.Close()
	res := &fakeResolver{routes: map[string]Route{"app": gatedRoute()}}
	authz := &fakeAuthorizer{allow: true, subject: ""} // anonymous allow
	ts, d := startProxy(t, res, authz, upstream.Listener.Addr().String())

	resp := get(t, ts, "app.apps.example.com", "/", nil) // no cookie at all
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 (authz allowed the anonymous viewer)", resp.StatusCode)
	}
	if atomic.LoadInt32(&authz.calls) != 1 {
		t.Fatalf("authz calls = %d, want 1", atomic.LoadInt32(&authz.calls))
	}
	if authz.lastCookie != "" {
		t.Fatalf("authz received cookie %q, want empty (the aceteam anonymous contract)", authz.lastCookie)
	}
	if atomic.LoadInt32(&d.calls) == 0 {
		t.Fatal("an allowed request should dial the pod")
	}
	if sawSubject != "" {
		t.Fatalf("pod saw subject %q, want empty for an anonymous viewer", sawSubject)
	}
}

// TestProxy_GatedAllowedStripsPlatformSessionCookie pins that a stray platform
// session cookie is stripped even on an allowed gated request, while an unrelated
// cookie survives.
func TestProxy_GatedAllowedStripsPlatformSessionCookie(t *testing.T) {
	var sawCookie string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawCookie = r.Header.Get("Cookie")
		w.WriteHeader(200)
	}))
	defer upstream.Close()
	res := &fakeResolver{routes: map[string]Route{"app": gatedRoute()}}
	authz := &fakeAuthorizer{allow: true, subject: "real-user"}
	ts, _ := startProxy(t, res, authz, upstream.Listener.Addr().String())

	resp := get(t, ts, "app.apps.example.com", "/", map[string]string{
		"Cookie": "sb-projref-auth-token=leak; keep=1",
	})
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if strings.Contains(sawCookie, "sb-projref-auth-token") {
		t.Fatalf("platform session cookie leaked to pod: %q", sawCookie)
	}
	if !strings.Contains(sawCookie, "keep=1") {
		t.Fatalf("unrelated cookie should survive, got %q", sawCookie)
	}
}

func TestProxy_GatedDeny403(t *testing.T) {
	res := &fakeResolver{routes: map[string]Route{"app": gatedRoute()}}
	authz := &fakeAuthorizer{allow: false}
	ts, d := startProxy(t, res, authz, "127.0.0.1:1")
	resp := get(t, ts, "app.apps.example.com", "/", map[string]string{"Cookie": "sess=abc"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if atomic.LoadInt32(&d.calls) != 0 {
		t.Fatal("denied gated request must not dial the pod")
	}
}

func TestProxy_GatedAllowSetsSubjectAndStripsSpoof(t *testing.T) {
	var sawSubject string
	var sawCookie string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawSubject = r.Header.Get("X-Ingress-Subject")
		sawCookie = r.Header.Get("Cookie")
		w.WriteHeader(200)
	}))
	defer upstream.Close()
	res := &fakeResolver{routes: map[string]Route{"app": gatedRoute()}, cookies: []string{"sess"}}
	authz := &fakeAuthorizer{allow: true, subject: "real-user"}
	ts, _ := startProxy(t, res, authz, upstream.Listener.Addr().String())

	resp := get(t, ts, "app.apps.example.com", "/", map[string]string{
		"Cookie":            "sess=abc; keep=1",
		"X-Ingress-Subject": "spoofed", // must be stripped before the pod
	})
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if sawSubject != "real-user" {
		t.Fatalf("pod saw subject %q, want the authz subject 'real-user' (spoof stripped)", sawSubject)
	}
	if strings.Contains(sawCookie, "sess=") {
		t.Fatalf("pod saw session cookie %q, want it stripped", sawCookie)
	}
	if !strings.Contains(sawCookie, "keep=1") {
		t.Fatalf("pod should still see unrelated cookies, got %q", sawCookie)
	}
}

func TestProxy_UpstreamErrorReturns502(t *testing.T) {
	res := &fakeResolver{routes: map[string]Route{"app": publicRoute()}}
	// Dialer that always fails, exercising the ErrorHandler.
	p := NewProxy(ProxyConfig{
		AppsDomain:  "apps.example.com",
		Resolver:    res,
		DialContext: func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("boom") },
	})
	ts := httptest.NewServer(p)
	defer ts.Close()
	resp := get(t, ts, "app.apps.example.com", "/", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
}

func TestProxy_NilDialerFailsClosed(t *testing.T) {
	// A Proxy built with a nil DialContext must NOT fall back to the host
	// network (which on a citadel up / TUN box could route 100.64.x.x). A
	// resolvable route should surface a 502 from the fail-closed dialer, never a
	// real connection.
	res := &fakeResolver{routes: map[string]Route{"app": publicRoute()}}
	p := NewProxy(ProxyConfig{AppsDomain: "apps.example.com", Resolver: res, DialContext: nil})
	ts := httptest.NewServer(p)
	defer ts.Close()
	resp := get(t, ts, "app.apps.example.com", "/", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (fail-closed on nil dialer)", resp.StatusCode)
	}
}

func TestProxy_ModifyResponseStripsCookieDomain(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Set-Cookie", "id=abc; Domain=apps.example.com; Path=/")
		w.WriteHeader(200)
	}))
	defer upstream.Close()
	res := &fakeResolver{routes: map[string]Route{"app": publicRoute()}}
	ts, _ := startProxy(t, res, nil, upstream.Listener.Addr().String())
	resp := get(t, ts, "app.apps.example.com", "/", nil)
	defer resp.Body.Close()
	sc := resp.Header.Get("Set-Cookie")
	if strings.Contains(strings.ToLower(sc), "domain=") {
		t.Fatalf("Set-Cookie still has Domain: %q", sc)
	}
	if !strings.Contains(sc, "Secure") {
		t.Fatalf("Set-Cookie should have Secure added: %q", sc)
	}
}

// TestProxy_StripsPlatformSessionCookiesWithEmptyPayload is the core invariant-1
// regression: even when the routes payload names NO session cookies (the default
// empty case that used to no-op stripCookies), the hardcoded platform family --
// including the chunked sb-<ref>-auth-token.0 / .1 pair -- is stripped from the
// pod-forwarded request, while an unrelated cookie survives.
func TestProxy_StripsPlatformSessionCookiesWithEmptyPayload(t *testing.T) {
	var sawCookie string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawCookie = r.Header.Get("Cookie")
		w.WriteHeader(200)
	}))
	defer upstream.Close()
	res := &fakeResolver{routes: map[string]Route{"app": publicRoute()}} // no cookies configured
	// Build a proxy with NO default cookie names either, so nothing but the
	// hardcoded family can drive the strip.
	d := &countingDialer{target: upstream.Listener.Addr().String()}
	p := NewProxy(ProxyConfig{
		AppsDomain:  "apps.example.com",
		Resolver:    res,
		DialContext: d.dial,
		// DefaultCookieNames deliberately empty.
	})
	ts := httptest.NewServer(p)
	defer ts.Close()

	resp := get(t, ts, "app.apps.example.com", "/", map[string]string{
		"Cookie": "sb-projref-auth-token.0=aaa; sb-projref-auth-token.1=bbb; theme=dark",
	})
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if strings.Contains(sawCookie, "sb-projref-auth-token") {
		t.Fatalf("chunked platform session cookie leaked to pod with empty payload: %q", sawCookie)
	}
	if !strings.Contains(sawCookie, "theme=dark") {
		t.Fatalf("unrelated cookie should survive, got %q", sawCookie)
	}
}

// TestProxy_RejectsWhenSessionCookieLeaks pins the guaranteed-removal-or-reject
// backstop: if stripping fails to remove a platform session cookie, the request
// is refused (500) and NO pod dial happens. The strip is no-op'd via the stripFn
// seam because the real strip is deterministic and could never leave a leak.
func TestProxy_RejectsWhenSessionCookieLeaks(t *testing.T) {
	res := &fakeResolver{routes: map[string]Route{"app": publicRoute()}}
	d := &countingDialer{target: "127.0.0.1:1"}
	p := NewProxy(ProxyConfig{
		AppsDomain:  "apps.example.com",
		Resolver:    res,
		DialContext: d.dial,
	})
	p.stripFn = func(*http.Request, []string) {} // simulate a strip that fails to remove anything
	ts := httptest.NewServer(p)
	defer ts.Close()

	resp := get(t, ts, "app.apps.example.com", "/", map[string]string{
		"Cookie": "sb-projref-auth-token=stillhere",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (fail closed when a session cookie survives)", resp.StatusCode)
	}
	if n := atomic.LoadInt32(&d.calls); n != 0 {
		t.Fatalf("dialer called %d times; a leaked session cookie must not reach the pod", n)
	}
}

// TestProxy_StaleRoutesFailClosed503 pins the route-freshness gate: when the
// resolver reports not-fresh (config-absent or expired), the request is 503 with
// no dial and no Resolve consultation.
func TestProxy_StaleRoutesFailClosed503(t *testing.T) {
	res := &fakeResolver{routes: map[string]Route{"app": publicRoute()}, stale: true}
	ts, d := startProxy(t, res, nil, "127.0.0.1:1")
	resp := get(t, ts, "app.apps.example.com", "/", nil)
	defer resp.Body.Close()
	// Config-absent (never fetched) and expired (past RouteMaxAge) are
	// indistinguishable to the proxy and must BOTH be 503 -- never a 404 a client
	// might cache as "this app does not exist".
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (stale/absent routes fail closed, not 404)", resp.StatusCode)
	}
	if resp.StatusCode == http.StatusNotFound {
		t.Fatal("stale routes must not 404 (cacheable); they must 503")
	}
	if n := atomic.LoadInt32(&d.calls); n != 0 {
		t.Fatalf("dialer called %d times; a stale route map must not dial", n)
	}
}

// TestProxy_OversizeSessionCookie401NoAuthzNoDial pins FIX 2: an over-cap session
// (too many Supabase chunks to fit the control plane's header wall) is routed to
// a clean re-login BEFORE any authz call or pod dial.
func TestProxy_OversizeSessionCookie401NoAuthzNoDial(t *testing.T) {
	res := &fakeResolver{routes: map[string]Route{"app": gatedRoute()}, cookies: []string{"sess"}, login: "https://login.example.com/start"}
	authz := &fakeAuthorizer{allow: true} // would allow, but must never be consulted
	ts, d := startProxy(t, res, authz, "127.0.0.1:1")

	big := strings.Repeat("x", maxGatedSessionCookieBytes+1000) // exceeds the cap
	resp := get(t, ts, "app.apps.example.com", "/", map[string]string{"Cookie": "sess=" + big})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for an oversize session", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "https://login.example.com/start" {
		t.Fatalf("Location = %q, want the login URL", loc)
	}
	if atomic.LoadInt32(&authz.calls) != 0 {
		t.Fatal("oversize session must not call authz")
	}
	if atomic.LoadInt32(&d.calls) != 0 {
		t.Fatal("oversize session must not dial the pod")
	}
}

// TestProxy_AuthzUnavailableFailsClosed503 pins FIX 1c: an authz-backend error
// (transport/timeout/5xx all map to err in httpAuthorizer) fails closed as a
// retryable 503, never a serve, never a redirect loop.
func TestProxy_AuthzUnavailableFailsClosed503(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"server 5xx", errors.New("authz: unexpected status 503")},
		{"transport timeout", context.DeadlineExceeded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := &fakeResolver{routes: map[string]Route{"app": gatedRoute()}, cookies: []string{"sess"}, login: "https://login.example.com/start"}
			authz := &fakeAuthorizer{err: tc.err}
			ts, d := startProxy(t, res, authz, "127.0.0.1:1")
			resp := get(t, ts, "app.apps.example.com", "/", map[string]string{"Cookie": "sess=abc"})
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503 (authz unavailable fails closed)", resp.StatusCode)
			}
			if atomic.LoadInt32(&d.calls) != 0 {
				t.Fatal("authz-unavailable request must not dial the pod")
			}
		})
	}
}

// blockingAuthorizer signals on entry and blocks until released, so a test can
// hold one authz call in-flight and observe whether a concurrent identical
// request makes a second upstream call.
type blockingAuthorizer struct {
	calls   int32
	entered chan struct{}
	release chan struct{}
	allow   bool
	subject string
}

func (b *blockingAuthorizer) Authorize(_ context.Context, _, _ string) (bool, string, error) {
	atomic.AddInt32(&b.calls, 1)
	b.entered <- struct{}{}
	<-b.release
	return b.allow, b.subject, nil
}

// TestProxy_ConcurrentIdenticalAuthzCollapsesToOneCall pins FIX 1b: two
// concurrent identical (slug, session) requests collapse into exactly ONE
// upstream authz call via singleflight.
func TestProxy_ConcurrentIdenticalAuthzCollapsesToOneCall(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer upstream.Close()
	res := &fakeResolver{routes: map[string]Route{"app": gatedRoute()}, cookies: []string{"sess"}}
	authz := &blockingAuthorizer{entered: make(chan struct{}, 2), release: make(chan struct{}), allow: true, subject: "u"}
	ts, _ := startProxy(t, res, authz, upstream.Listener.Addr().String())

	done := make(chan int, 2)
	fire := func() {
		resp := get(t, ts, "app.apps.example.com", "/", map[string]string{"Cookie": "sess=abc"})
		done <- resp.StatusCode
		resp.Body.Close()
	}
	go fire()
	<-authz.entered // first request is the singleflight leader, now in-flight
	go fire()
	// The second identical request must join the in-flight singleflight, NOT make
	// its own upstream call. Assert no second entry within a window.
	select {
	case <-authz.entered:
		t.Fatal("second identical request must NOT make a second upstream authz call (singleflight)")
	case <-time.After(250 * time.Millisecond):
	}
	close(authz.release)
	for i := 0; i < 2; i++ {
		if code := <-done; code != 200 {
			t.Fatalf("request %d status = %d, want 200", i, code)
		}
	}
	if n := atomic.LoadInt32(&authz.calls); n != 1 {
		t.Fatalf("authz calls = %d, want exactly 1 (singleflight collapse)", n)
	}
}

// TestProxy_SequentialAuthzNotCached pins FIX 1a: a second identical request
// AFTER the first completes makes a NEW upstream call (no cross-request cache).
func TestProxy_SequentialAuthzNotCached(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer upstream.Close()
	res := &fakeResolver{routes: map[string]Route{"app": gatedRoute()}, cookies: []string{"sess"}}
	authz := &fakeAuthorizer{allow: true, subject: "u"}
	ts, _ := startProxy(t, res, authz, upstream.Listener.Addr().String())

	r1 := get(t, ts, "app.apps.example.com", "/", map[string]string{"Cookie": "sess=abc"})
	r1.Body.Close()
	r2 := get(t, ts, "app.apps.example.com", "/", map[string]string{"Cookie": "sess=abc"})
	r2.Body.Close()
	if n := atomic.LoadInt32(&authz.calls); n != 2 {
		t.Fatalf("authz calls = %d, want 2 (no cross-request caching)", n)
	}
}

// TestProxy_AuthzDecisionNotCached_ImmediateRevocation pins FIX 1a end-to-end: a
// session allowed on one request is denied on the very next once authz flips,
// with no cached allow keeping it in.
func TestProxy_AuthzDecisionNotCached_ImmediateRevocation(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer upstream.Close()
	res := &fakeResolver{routes: map[string]Route{"app": gatedRoute()}, cookies: []string{"sess"}, login: "https://login.example.com/start"}
	authz := &fakeAuthorizer{allow: true, subject: "u"}
	ts, d := startProxy(t, res, authz, upstream.Listener.Addr().String())

	r1 := get(t, ts, "app.apps.example.com", "/", map[string]string{"Cookie": "sess=abc"})
	if r1.StatusCode != 200 {
		t.Fatalf("first request status = %d, want 200 (allowed)", r1.StatusCode)
	}
	r1.Body.Close()

	// Session revoked upstream: authz now denies. The next request must be denied
	// immediately -- no cached allow.
	authz.allow = false
	dialsBefore := atomic.LoadInt32(&d.calls)
	r2 := get(t, ts, "app.apps.example.com", "/", map[string]string{"Cookie": "sess=abc"})
	if r2.StatusCode != http.StatusForbidden {
		t.Fatalf("second request status = %d, want 403 (immediate revocation)", r2.StatusCode)
	}
	r2.Body.Close()
	if atomic.LoadInt32(&d.calls) != dialsBefore {
		t.Fatal("a revoked session must not reach the pod")
	}
}

// TestHTTPAuthorizer_FailsClosedOnServerErrorAndCanceledContext pins FIX 1c at
// the production authorizer: a 5xx response and a transport/context failure both
// map to (allow=false, err!=nil), never to a silent allow.
func TestHTTPAuthorizer_FailsClosedOnServerErrorAndCanceledContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	a := NewHTTPAuthorizer(srv.URL, "tok", srv.Client())

	allow, _, err := a.Authorize(context.Background(), "app", "sess=abc")
	if err == nil || allow {
		t.Fatalf("5xx must map to (allow=false, err!=nil); got allow=%v err=%v", allow, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // stands in for a transport/timeout failure
	allow, _, err = a.Authorize(ctx, "app", "sess=abc")
	if err == nil || allow {
		t.Fatalf("canceled context must map to (allow=false, err!=nil); got allow=%v err=%v", allow, err)
	}
}
