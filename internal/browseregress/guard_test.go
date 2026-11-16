package browseregress

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"sync/atomic"
	"testing"
)

func TestAddressDecisions(t *testing.T) {
	resolver := func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}
	g, err := StartEgressGuard(resolver, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	for _, raw := range []string{"http://127.0.0.1/", "http://2130706433/", "http://169.254.169.254/", "http://[::1]/", "http://[::ffff:127.0.0.1]/", "http://[fd00:ec2::254]/", "http://100.100.100.200/", "http://metadata.google.internal/", "http://metadata.goog/", "file:///tmp/x", "data:text/plain,x"} {
		t.Run(raw, func(t *testing.T) {
			err := g.CheckURL(context.Background(), raw)
			if !errors.Is(err, ErrEgressRefused) || errors.Is(err, ErrPrivateNetworkApprovalRequired) {
				t.Fatalf("hard refusal: %v", err)
			}
		})
	}
	for _, raw := range []string{"http://10.0.0.1/", "http://192.168.1.1/", "http://100.64.0.1/", "http://[fd12::1]/", "http://nas/", "http://nas.internal/", "http://nas.home.arpa/"} {
		t.Run(raw, func(t *testing.T) {
			if err := g.CheckURL(context.Background(), raw); !errors.Is(err, ErrPrivateNetworkApprovalRequired) {
				t.Fatalf("private hold: %v", err)
			}
		})
	}
	for _, raw := range []string{"", "about:blank", "https://public.test/path", "https://8.8.8.8/"} {
		if err := g.CheckURL(context.Background(), raw); err != nil {
			t.Fatalf("public page %s: %v", raw, err)
		}
	}
}

func TestProxyRedirectRebindingAndSubresources(t *testing.T) {
	var hits, dials atomic.Int32
	var rebound atomic.Bool
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Connection", "close")
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
			return
		}
		_, _ = io.WriteString(w, "public fixture")
	}))
	defer fixture.Close()
	resolver := func(_ context.Context, host string) ([]netip.Addr, error) {
		if host == "rebind.test" && rebound.Load() {
			return []netip.Addr{netip.MustParseAddr("10.0.0.1")}, nil
		}
		if host == "mixed.test" {
			return []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("127.0.0.1")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		dials.Add(1)
		if addr != "8.8.8.8:80" {
			t.Errorf("unpinned dial: %s", addr)
		}
		return (&net.Dialer{}).DialContext(ctx, network, fixture.Listener.Addr().String())
	}
	g, err := StartEgressGuard(resolver, dial)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	proxy, _ := url.Parse(g.ProxyURL())
	tr := &http.Transport{Proxy: http.ProxyURL(proxy), DisableKeepAlives: true}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr}
	request := func(raw string, want int) {
		t.Helper()
		resp, err := client.Get(raw)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("%s: status %d", raw, resp.StatusCode)
		}
	}
	request("http://public.test/redirect", http.StatusForbidden)
	if hits.Load() != 1 || dials.Load() != 1 {
		t.Fatal("redirect reached metadata")
	}
	request("http://rebind.test/first", http.StatusOK)
	rebound.Store(true)
	request("http://rebind.test/second", http.StatusForbidden)
	request("http://127.0.0.1/script.js", http.StatusForbidden)
	request("http://mixed.test/frame", http.StatusForbidden)
	if hits.Load() != 2 || dials.Load() != 2 || g.PrivateRefused() != 1 {
		t.Fatalf("unsafe egress hits=%d dials=%d private=%d", hits.Load(), dials.Load(), g.PrivateRefused())
	}
}

func TestGuardCloseRefusesLateTunnelRegistration(t *testing.T) {
	g, err := StartEgressGuard(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	g.Close()
	a, b := net.Pipe()
	c, d := net.Pipe()
	defer b.Close()
	defer d.Close()
	if g.register(a, c) {
		t.Fatal("registered a tunnel after close")
	}
	if g.Active() {
		t.Fatal("closed proxy advertises active")
	}
	g.mu.Lock()
	n := len(g.conns)
	g.mu.Unlock()
	if n != 0 {
		t.Fatalf("leaked %d tunnels", n)
	}
}

// Keep both browser-to-proxy and proxy-to-origin keepalives enabled. A current
// DNS refusal must stop forwarding even when the prior public socket is reusable.
func TestProxyKeepAliveRechecksCurrentDestination(t *testing.T) {
	for _, tc := range []struct {
		name    string
		answers []netip.Addr
		private int64
	}{
		{"private", []netip.Addr{netip.MustParseAddr("10.0.0.1")}, 1},
		{"mixed", []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("10.0.0.1")}, 1},
		{"metadata", []netip.Addr{netip.MustParseAddr("169.254.169.254")}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var changed atomic.Bool
			var hits, dials, lookups atomic.Int32
			fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); _, _ = io.WriteString(w, "public fixture") }))
			defer fixture.Close()
			resolver := func(context.Context, string) ([]netip.Addr, error) {
				lookups.Add(1)
				if changed.Load() {
					return tc.answers, nil
				}
				return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
			}
			dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
				dials.Add(1)
				if addr != "8.8.8.8:80" {
					t.Errorf("unpinned dial: %s", addr)
				}
				return (&net.Dialer{}).DialContext(ctx, network, fixture.Listener.Addr().String())
			}
			g, err := StartEgressGuard(resolver, dial)
			if err != nil {
				t.Fatal(err)
			}
			defer g.Close()
			proxy, _ := url.Parse(g.ProxyURL())
			tr := &http.Transport{Proxy: http.ProxyURL(proxy)}
			defer tr.CloseIdleConnections()
			client := &http.Client{Transport: tr}
			get := func(want int) {
				t.Helper()
				resp, err := client.Get("http://probe.test/path")
				if err != nil {
					t.Fatal(err)
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				if resp.StatusCode != want {
					t.Fatalf("status=%d want=%d hits=%d dials=%d lookups=%d", resp.StatusCode, want, hits.Load(), dials.Load(), lookups.Load())
				}
			}
			get(http.StatusOK)
			get(http.StatusOK)
			if dials.Load() != 1 || hits.Load() != 2 {
				t.Fatalf("positive control did not pool upstream: dials=%d hits=%d", dials.Load(), hits.Load())
			}
			before := lookups.Load()
			changed.Store(true)
			get(http.StatusForbidden)
			if lookups.Load() <= before || hits.Load() != 2 || dials.Load() != 1 || g.PrivateRefused() != tc.private || g.Refused() != 1 {
				t.Fatalf("pooled request skipped current policy: lookups=%d hits=%d dials=%d private=%d refused=%d", lookups.Load(), hits.Load(), dials.Load(), g.PrivateRefused(), g.Refused())
			}
		})
	}
}
