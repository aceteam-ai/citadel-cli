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
}

func (f *fakeResolver) Resolve(_ context.Context, slug string) (Route, bool) {
	r, ok := f.routes[slug]
	return r, ok
}
func (f *fakeResolver) LoginURL() string      { return f.login }
func (f *fakeResolver) CookieNames() []string { return f.cookies }

type fakeAuthorizer struct {
	allow   bool
	subject string
	err     error
	calls   int32
}

func (f *fakeAuthorizer) Authorize(_ context.Context, _, _ string) (bool, string, error) {
	atomic.AddInt32(&f.calls, 1)
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

func TestProxy_GatedNoCookie401Redirect(t *testing.T) {
	res := &fakeResolver{routes: map[string]Route{"app": gatedRoute()}, login: "https://login.example.com/start"}
	authz := &fakeAuthorizer{}
	ts, d := startProxy(t, res, authz, "127.0.0.1:1")
	resp := get(t, ts, "app.apps.example.com", "/", nil) // no cookie
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "https://login.example.com/start" {
		t.Fatalf("Location = %q, want the login URL", loc)
	}
	if atomic.LoadInt32(&authz.calls) != 0 {
		t.Fatal("authz must not be called when no cookie is present")
	}
	if atomic.LoadInt32(&d.calls) != 0 {
		t.Fatal("no dial should happen for an unauthenticated gated request")
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
