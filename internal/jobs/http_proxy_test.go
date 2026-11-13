package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

// Every httptest server binds loopback, so each test allowlists exactly the
// ports it means to reach, standing in for the production {8000}.

func portOf(t *testing.T, srvURL string) int {
	t.Helper()
	_, p, err := net.SplitHostPort(strings.TrimPrefix(strings.TrimPrefix(srvURL, "http://"), "https://"))
	if err != nil {
		t.Fatal(err)
	}
	n, _ := strconv.Atoi(p)
	return n
}

func runProxy(t *testing.T, h *HTTPProxyHandler, url string) (map[string]any, error) {
	t.Helper()
	out, err := h.Execute(JobContext{}, &nexus.Job{ID: "proxy", Type: "HTTP_PROXY", Payload: map[string]string{"url": url}})
	if err != nil {
		return nil, err
	}
	var res map[string]any
	if jerr := json.Unmarshal(out, &res); jerr != nil {
		t.Fatalf("result not JSON: %v", jerr)
	}
	return res, nil
}

func countingServer(t *testing.T, body string) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// A loopback target on a port that is not allowlisted is refused before any
// connection reaches it.
func TestHTTPProxyRefusesLoopbackOffAllowlist(t *testing.T) {
	srv, hits := countingServer(t, "local-api")
	h := &HTTPProxyHandler{localPorts: map[int]bool{8000: true}}
	_, err := runProxy(t, h, srv.URL+"/api/status")
	if err == nil || !errors.Is(err, errHTTPProxyDestinationBlocked) {
		t.Fatalf("want a blocked-destination error, got %v", err)
	}
	if atomic.LoadInt32(hits) != 0 {
		t.Fatal("the refused loopback server was contacted")
	}
}

// A DNS name that resolves to loopback is checked at its resolved address.
func TestHTTPProxyRefusesNameResolvingToLoopback(t *testing.T) {
	srv, hits := countingServer(t, "local-api")
	h := &HTTPProxyHandler{localPorts: map[int]bool{8000: true}}
	_, err := runProxy(t, h, "http://localhost:"+strconv.Itoa(portOf(t, srv.URL))+"/")
	if err == nil || !errors.Is(err, errHTTPProxyDestinationBlocked) {
		t.Fatalf("want a blocked-destination error for localhost, got %v", err)
	}
	if atomic.LoadInt32(hits) != 0 {
		t.Fatal("the refused loopback server was contacted")
	}
}

// An allowlisted local port (the WeChat bridge on :8000 in production) works.
func TestHTTPProxyAllowsAllowlistedLocalPort(t *testing.T) {
	srv, hits := countingServer(t, "bridge-ok")
	h := &HTTPProxyHandler{localPorts: map[int]bool{portOf(t, srv.URL): true}}
	res, err := runProxy(t, h, srv.URL+"/health")
	if err != nil {
		t.Fatalf("allowlisted bridge refused: %v", err)
	}
	if res["body"] != "bridge-ok" || res["status_code"] != float64(200) || atomic.LoadInt32(hits) != 1 {
		t.Fatalf("unexpected result %v hits=%d", res, atomic.LoadInt32(hits))
	}
}

// An allowlisted server that redirects to a non-allowlisted local port: the
// redirect is not followed; the caller gets the 3xx and the target is never hit.
func TestHTTPProxyDoesNotFollowRedirects(t *testing.T) {
	target, targetHits := countingServer(t, "local-api-secret")
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/api/status", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(bridge.Close)
	h := &HTTPProxyHandler{localPorts: map[int]bool{portOf(t, bridge.URL): true}}
	res, err := runProxy(t, h, bridge.URL+"/health")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res["status_code"] != float64(http.StatusTemporaryRedirect) {
		t.Fatalf("want the 307 returned as-is, got %v", res["status_code"])
	}
	if strings.Contains(res["body"].(string), "local-api-secret") || atomic.LoadInt32(targetHits) != 0 {
		t.Fatal("the redirect target was contacted")
	}
}

// Link-local (cloud metadata) and unspecified destinations are refused at the
// dial check, so no packet is sent.
func TestHTTPProxyRefusesLinkLocalAndUnspecified(t *testing.T) {
	h := &HTTPProxyHandler{localPorts: map[int]bool{8000: true}}
	for _, u := range []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://0.0.0.0:8080/",
		"http://[::1]:8080/",
		"http://[fe80::1]:8080/",
		"http://[::ffff:127.0.0.1]:8080/",
	} {
		if _, err := runProxy(t, h, u); err == nil || !errors.Is(err, errHTTPProxyDestinationBlocked) {
			t.Errorf("%s: want a blocked-destination error, got %v", u, err)
		}
	}
}

// Anything but an absolute http(s) URL fails closed.
func TestHTTPProxyRefusesBadSchemesAndURLs(t *testing.T) {
	h := &HTTPProxyHandler{}
	for _, u := range []string{"file:///etc/passwd", "ftp://example.com/", "gopher://x/", "/relative/path", "http://", "://bad"} {
		if _, err := runProxy(t, h, u); err == nil {
			t.Errorf("%q: want an error, got none", u)
		}
	}
}

// The pure decision. Node-local (loopback, or one of the node's own interface
// addresses) needs an allowlisted port; link-local and unspecified are always
// refused; other mesh (CGNAT), LAN and public addresses are allowed.
func TestHTTPProxyDestinationAllowed(t *testing.T) {
	ports := map[int]bool{8000: true}
	own := map[netip.Addr]bool{
		netip.MustParseAddr("192.168.1.10"): true, // this node's LAN address
		netip.MustParseAddr("172.17.0.1"):   true, // docker0
		netip.MustParseAddr("100.64.0.42"):  true, // this node's mesh address (TUN mode)
	}
	cases := []struct {
		ip   string
		port int
		want bool
	}{
		{"127.0.0.1", 8080, false},
		{"127.0.0.1", 8000, true},
		{"127.8.9.10", 8080, false},
		{"::1", 8080, false},
		{"::1", 8000, true},
		{"::ffff:127.0.0.1", 8080, false},
		{"169.254.169.254", 80, false},
		{"169.254.169.254", 8000, false},
		{"fe80::1", 8080, false},
		{"fe80::1", 8000, false},
		{"0.0.0.0", 8080, false},
		{"0.0.0.0", 8000, false},
		{"::", 8000, false},
		{"192.168.1.10", 8080, false},
		{"192.168.1.10", 8000, true},
		{"172.17.0.1", 8080, false},
		{"100.64.0.42", 8080, false},
		{"::ffff:100.64.0.42", 8080, false},
		{"100.64.0.39", 8000, true},
		{"100.64.0.39", 8080, true},
		{"192.168.1.50", 8000, true},
		{"10.0.0.5", 443, true},
		{"93.184.216.34", 443, true},
	}
	for _, tc := range cases {
		if got := httpProxyDestinationAllowed(netip.MustParseAddr(tc.ip), tc.port, ports, own); got != tc.want {
			t.Errorf("%s:%d allowed=%v, want %v", tc.ip, tc.port, got, tc.want)
		}
	}
	if httpProxyDestinationAllowed(netip.Addr{}, 8000, ports, own) {
		t.Error("an invalid address must be refused")
	}
}

// Zone-qualified IPv6 forms reach the dial check as the address actually
// dialed and are refused.
func TestHTTPProxyRefusesZonedIPv6(t *testing.T) {
	h := &HTTPProxyHandler{localPorts: map[int]bool{8000: true}}
	for _, u := range []string{"http://[fe80::1%25lo]:8000/", "http://[::1%25lo]:8080/"} {
		if _, err := runProxy(t, h, u); err == nil || !errors.Is(err, errHTTPProxyDestinationBlocked) {
			t.Errorf("%s: want a blocked-destination error, got %v", u, err)
		}
	}
}

// ownInterfaceAddrs reads the injected interface list and returns enumeration
// errors instead of an empty set.
func TestOwnInterfaceAddrs(t *testing.T) {
	stubInterfaceAddrs(t, []net.Addr{
		&net.IPNet{IP: net.ParseIP("192.168.1.10"), Mask: net.CIDRMask(24, 32)},
		&net.IPAddr{IP: net.ParseIP("100.64.0.42")},
		&net.IPAddr{IP: net.ParseIP("fd7a:115c:a1e0::42")},
	}, nil)
	own, err := ownInterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, ip := range []string{"192.168.1.10", "100.64.0.42", "::ffff:100.64.0.42", "fd7a:115c:a1e0::42", "fd7a:115c:a1e0::42%eth0"} {
		if !own.has(netip.MustParseAddr(ip)) {
			t.Errorf("%s not recognized as an own address", ip)
		}
	}
	stubInterfaceAddrs(t, nil, errors.New("boom"))
	if _, err := ownInterfaceAddrs(); err == nil {
		t.Error("an enumeration error must be returned, not an empty set")
	}
}

func stubInterfaceAddrs(t *testing.T, addrs []net.Addr, err error) {
	t.Helper()
	orig := interfaceAddrsFn
	t.Cleanup(func() { interfaceAddrsFn = orig })
	interfaceAddrsFn = func() ([]net.Addr, error) { return addrs, err }
}

// If the node's own addresses cannot be determined, HTTP_PROXY refuses the job
// before any connection, even to an otherwise allowed destination.
func TestHTTPProxyFailsClosedWhenOwnAddrsUnknown(t *testing.T) {
	srv, hits := countingServer(t, "bridge")
	stubInterfaceAddrs(t, nil, errors.New("enumeration failed"))
	h := &HTTPProxyHandler{localPorts: map[int]bool{portOf(t, srv.URL): true}}
	_, err := runProxy(t, h, srv.URL+"/health")
	if err == nil || !errors.Is(err, errHTTPProxyDestinationBlocked) {
		t.Fatalf("want a blocked-destination error, got %v", err)
	}
	if atomic.LoadInt32(hits) != 0 {
		t.Fatal("a request was sent although own addresses were unknown")
	}
}

// This node's own zoned IPv6 address is node-local through the real dial
// check; another host's zoned address is not.
func TestHTTPProxyOwnZonedIPv6IsNodeLocal(t *testing.T) {
	own := ownAddrSet{netip.MustParseAddr("fd7a:115c:a1e0::42"): true}
	control := guardedDialControl(map[int]bool{8000: true}, own)
	if err := control("tcp6", "[fd7a:115c:a1e0::42%eth0]:8080", nil); err == nil || !errors.Is(err, errHTTPProxyDestinationBlocked) {
		t.Fatalf("own zoned address on 8080: want refused, got %v", err)
	}
	if err := control("tcp6", "[fd7a:115c:a1e0::42]:8080", nil); err == nil {
		t.Fatal("own unzoned address on 8080: want refused")
	}
	if err := control("tcp6", "[fd7a:115c:a1e0::99%eth0]:8080", nil); err != nil {
		t.Fatalf("another host's zoned address must stay allowed: %v", err)
	}
}

// WEB_FETCH refuses the node's own interface addresses even with allow_private.
func TestWebFetchRefusesOwnInterfaceAddrWithAllowPrivate(t *testing.T) {
	stubInterfaceAddrs(t, []net.Addr{&net.IPAddr{IP: net.ParseIP("192.168.1.10")}, &net.IPAddr{IP: net.ParseIP("100.64.0.42")}}, nil)
	for _, ip := range []string{"192.168.1.10", "100.64.0.42", "::ffff:100.64.0.42"} {
		if err := wfAssertIPAllowed(net.ParseIP(ip), true); err == nil {
			t.Errorf("%s: own address must be refused even with allow_private", ip)
		}
	}
	if err := wfAssertIPAllowed(net.ParseIP("192.168.1.50"), true); err != nil {
		t.Errorf("another LAN host with allow_private must stay allowed: %v", err)
	}
}

// If the node's own addresses cannot be determined, WEB_FETCH refuses through
// its real admission path (allow_private set) before any connection: the
// error is the blocked-destination error, not a dial error.
func TestWebFetchFailsClosedWhenOwnAddrsUnknown(t *testing.T) {
	stubInterfaceAddrs(t, nil, errors.New("enumeration failed"))
	for _, ip := range []string{"192.168.1.10", "100.64.0.42", "93.184.216.34"} {
		if err := wfAssertIPAllowed(net.ParseIP(ip), true); err == nil {
			t.Errorf("%s: want refused while own addresses are unknown", ip)
		}
	}
	h := &WebFetchHandler{}
	_, err := h.Execute(JobContext{}, &nexus.Job{ID: "wf", Type: "WEB_FETCH", Payload: map[string]string{
		"url": "http://192.0.2.10:9/", "allow_private": "true",
	}})
	if err == nil || !errors.Is(err, errBlockedDestination) {
		t.Fatalf("want errBlockedDestination before any dial, got %v", err)
	}
}

// The production default allowlists exactly the WeChat bridge port. The other
// fixtures override localPorts, so this is the only pin on the zero-value
// default.
func TestHTTPProxyDefaultLocalPorts(t *testing.T) {
	got := (&HTTPProxyHandler{}).allowedLocalPorts()
	if len(got) != 1 || !got[8000] {
		t.Fatalf("default local-port allowlist = %v, want {8000}", got)
	}
}

// The guarded client must not route through an environment proxy (which would
// bypass the dial check: the proxy, not the node, picks the destination), and
// must never follow redirects. Go skips the proxy for loopback targets, so the
// end-to-end tests cannot observe this; pin the client configuration directly.
func TestGuardedHTTPClientConfiguration(t *testing.T) {
	stubInterfaceAddrs(t, []net.Addr{&net.IPAddr{IP: net.ParseIP("192.168.1.10")}}, nil)
	c, err := newGuardedHTTPClient(map[int]bool{8000: true}, 45*time.Second)
	if err != nil {
		t.Fatalf("client construction with known own addresses: %v", err)
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T, want *http.Transport", c.Transport)
	}
	if tr.Proxy != nil {
		t.Fatal("the guarded transport must not use any proxy")
	}
	if tr.DialContext == nil {
		t.Fatal("the guarded transport must use the checked dialer")
	}
	if c.CheckRedirect == nil || c.CheckRedirect(nil, nil) != http.ErrUseLastResponse {
		t.Fatal("redirects must not be followed")
	}
	if c.Timeout != 45*time.Second || tr.ResponseHeaderTimeout != 45*time.Second {
		t.Fatalf("client timeout %s and response-header timeout %s must both follow the job budget (45s)", c.Timeout, tr.ResponseHeaderTimeout)
	}
	// Zero means unbounded to net/http; it must never reach the client.
	c, err = newGuardedHTTPClient(map[int]bool{8000: true}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if tr := c.Transport.(*http.Transport); c.Timeout != 30*time.Second || tr.ResponseHeaderTimeout != 30*time.Second {
		t.Fatalf("a zero budget must fall back to 30s, got %s/%s", c.Timeout, tr.ResponseHeaderTimeout)
	}
}

// The job's timeout_ms (the node's per-job budget key) sets the request
// timeout, clamped to httpProxyMaxTimeout; anything absent, unusable or below
// a millisecond keeps the 30 s default. A zero Duration would mean no timeout
// at all, so sub-millisecond values must never reach the client.
func TestHTTPProxyTimeoutFromPayload(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload map[string]string
		want    time.Duration
	}{
		{"absent", map[string]string{}, 30 * time.Second},
		{"nil payload", nil, 30 * time.Second},
		{"empty", map[string]string{"timeout_ms": ""}, 30 * time.Second},
		{"50s", map[string]string{"timeout_ms": "50000"}, 50 * time.Second},
		{"60s", map[string]string{"timeout_ms": "60000"}, 60 * time.Second},
		{"fractional", map[string]string{"timeout_ms": "45500.0"}, 45500 * time.Millisecond},
		{"below default", map[string]string{"timeout_ms": "5000"}, 5 * time.Second},
		{"one millisecond", map[string]string{"timeout_ms": "1"}, time.Millisecond},
		{"120s clamped", map[string]string{"timeout_ms": "120000"}, 60 * time.Second},
		{"600s clamped", map[string]string{"timeout_ms": "600000"}, 60 * time.Second},
		{"exponent clamped", map[string]string{"timeout_ms": "1e+06"}, 60 * time.Second},
		{"garbage", map[string]string{"timeout_ms": "soon"}, 30 * time.Second},
		{"zero", map[string]string{"timeout_ms": "0"}, 30 * time.Second},
		{"negative", map[string]string{"timeout_ms": "-5000"}, 30 * time.Second},
		{"NaN", map[string]string{"timeout_ms": "NaN"}, 30 * time.Second},
		{"Inf", map[string]string{"timeout_ms": "+Inf"}, 30 * time.Second},
		{"tiny", map[string]string{"timeout_ms": "1e-7"}, 30 * time.Second},
		{"hex tiny", map[string]string{"timeout_ms": "0x1p-30"}, 30 * time.Second},
		{"denormal", map[string]string{"timeout_ms": "5e-324"}, 30 * time.Second},
		{"sub-millisecond", map[string]string{"timeout_ms": "0.5"}, 30 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := httpProxyTimeout(&nexus.Job{Payload: tc.payload}); got != tc.want {
				t.Fatalf("httpProxyTimeout = %s, want %s", got, tc.want)
			}
		})
	}
}

// The worker's legacy adapter turns a JSON number into a string with
// fmt.Sprint, so numeric budgets arrive in that form (1e-07, 1e+06).
func TestHTTPProxyTimeoutFromAdapterStringifiedNumbers(t *testing.T) {
	for _, tc := range []struct {
		number float64
		want   time.Duration
	}{
		{1e-7, 30 * time.Second},
		{0, 30 * time.Second},
		{50000, 50 * time.Second},
		{1e6, 60 * time.Second},
	} {
		raw := fmt.Sprint(tc.number)
		if got := httpProxyTimeout(&nexus.Job{Payload: map[string]string{"timeout_ms": raw}}); got != tc.want {
			t.Fatalf("timeout_ms %q (from %v) = %s, want %s", raw, tc.number, got, tc.want)
		}
	}
}

func TestHTTPProxyHonorsJobBudget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write([]byte("partial result"))
	}))
	t.Cleanup(srv.Close)
	port, _ := strconv.Atoi(srv.URL[strings.LastIndex(srv.URL, ":")+1:])
	h := &HTTPProxyHandler{localPorts: map[int]bool{port: true}}
	run := func(timeoutMs string) error {
		_, err := h.Execute(JobContext{}, &nexus.Job{ID: "j", Payload: map[string]string{"url": srv.URL, "timeout_ms": timeoutMs}})
		return err
	}
	err := run("50")
	var netErr net.Error
	if !errors.Is(err, context.DeadlineExceeded) && !(errors.As(err, &netErr) && netErr.Timeout()) {
		t.Fatalf("a 50 ms budget must time out a 300 ms endpoint, got %v", err)
	}
	if err := run("2000"); err != nil {
		t.Fatalf("a 2 s budget must let a 300 ms endpoint answer: %v", err)
	}
}
