package jobs

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

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

// The pure decision: local-only ranges need an allowlisted port; mesh (CGNAT),
// LAN and public addresses do not.
func TestHTTPProxyDestinationAllowed(t *testing.T) {
	ports := map[int]bool{8000: true}
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
		{"fe80::1", 8080, false},
		{"0.0.0.0", 8080, false},
		{"::", 8080, false},
		{"100.64.0.39", 8000, true},
		{"100.64.0.39", 8080, true},
		{"192.168.1.50", 8000, true},
		{"10.0.0.5", 443, true},
		{"93.184.216.34", 443, true},
	}
	for _, tc := range cases {
		if got := httpProxyDestinationAllowed(netip.MustParseAddr(tc.ip), tc.port, ports); got != tc.want {
			t.Errorf("%s:%d allowed=%v, want %v", tc.ip, tc.port, got, tc.want)
		}
	}
	if httpProxyDestinationAllowed(netip.Addr{}, 8000, ports) {
		t.Error("an invalid address must be refused")
	}
}

// The production default allowlists exactly the WeChat bridge port.
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
	c := newGuardedHTTPClient(map[int]bool{8000: true})
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
}
