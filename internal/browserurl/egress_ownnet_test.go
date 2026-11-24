package browserurl

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"
)

func fakeIfaces(list ...string) InterfacePrefixes {
	return func() ([]netip.Prefix, error) {
		out := make([]netip.Prefix, 0, len(list))
		for _, s := range list {
			out = append(out, netip.MustParsePrefix(s))
		}
		return out, nil
	}
}

// TestOwnNetRefusesOwnAndOnLinkAddresses covers IPv4 and IPv6, the equal
// address and inside-prefix cases, and a too-short (VPN-wide) prefix.
func TestOwnNetRefusesOwnAndOnLinkAddresses(t *testing.T) {
	o := newOwnNet(fakeIfaces(
		"198.51.100.7/24",      // a public IPv4 LAN
		"2606:4700:10::5/64",   // a global IPv6 address and its /64
		"8.8.4.4/1",            // a VPN route far wider than a LAN
		"2606:4700:20::9/16",   // likewise for IPv6
		"2606:4700:30::77/128", // an address with no on-link prefix
		"192.0.2.255/32",       // single address
	))
	for _, tc := range []struct {
		addr string
		want bool
	}{
		{"198.51.100.7", true},           // own IPv4
		{"198.51.100.1", true},           // the router on the same LAN
		{"::ffff:198.51.100.9", true},    // IPv4-mapped form
		{"198.51.101.1", false},          // the next network
		{"2606:4700:10::5", true},        // own IPv6
		{"2606:4700:10::1", true},        // prefix::1, the router
		{"2606:4700:10:0:abcd::1", true}, // another host in the /64
		{"2606:4700:11::1", false},       // outside the /64
		{"8.8.4.4", true},                // equal address under a short prefix
		{"9.9.9.9", false},               // short prefix does not block the internet
		{"2606:4700:20::9", true},
		{"2606:4700:21::1", false},
		{"2606:4700:30::77", true},
		{"2606:4700:30::78", false},
		{"192.0.2.255", true},
	} {
		if got := o.contains(netip.MustParseAddr(tc.addr)); got != tc.want {
			t.Errorf("contains(%s) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}

// TestOwnNetRefreshAndFailClosed: the list refreshes after the TTL, a failed
// refresh keeps the last list, and no list at all refuses everything.
func TestOwnNetRefreshAndFailClosed(t *testing.T) {
	now := time.Unix(1000, 0)
	var list []string
	var fail bool
	o := newOwnNet(func() ([]netip.Prefix, error) {
		if fail {
			return nil, errors.New("no interfaces")
		}
		return fakeIfaces(list...)()
	})
	o.now = func() time.Time { return now }

	fail = true
	if !o.contains(netip.MustParseAddr("1.1.1.1")) {
		t.Fatal("with no interface list the guard must fail closed")
	}
	fail = false
	list = []string{"2606:4700:10::5/64"}
	if !o.contains(netip.MustParseAddr("2606:4700:10::1")) || o.contains(netip.MustParseAddr("2606:4700:99::1")) {
		t.Fatal("first list not applied")
	}
	// A rotated temporary address appears; it is used once the TTL passes.
	list = []string{"2606:4700:99::5/64"}
	if o.contains(netip.MustParseAddr("2606:4700:99::1")) {
		t.Fatal("refreshed before the TTL")
	}
	now = now.Add(ownNetTTL)
	if !o.contains(netip.MustParseAddr("2606:4700:99::1")) {
		t.Fatal("not refreshed after the TTL")
	}
	fail = true
	now = now.Add(ownNetTTL)
	if !o.contains(netip.MustParseAddr("2606:4700:99::1")) || o.contains(netip.MustParseAddr("1.1.1.1")) {
		t.Fatal("a failed refresh must keep the last list")
	}
}

func TestDefaultInterfacePrefixesIncludesLoopback(t *testing.T) {
	l, err := DefaultInterfacePrefixes()
	if err != nil {
		t.Skipf("no interface list here: %v", err)
	}
	for _, p := range l {
		if p.Addr().Is4() && p.Addr() == netip.MustParseAddr("127.0.0.1") && p.Bits() == 8 {
			return
		}
	}
	t.Logf("loopback 127.0.0.1/8 not listed (ok on some hosts): %v", l)
}

// TestEgressGuardRefusesOwnNetworkThroughProxy: a literal or a resolved name
// on this machine's own network is refused and reported, never dialed.
func TestEgressGuardRefusesOwnNetworkThroughProxy(t *testing.T) {
	var got []string
	g, err := StartEgressGuardWithOptions(EgressGuardOptions{
		Resolve: testResolver(map[string]string{"nas.test": "198.51.100.20"}),
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			t.Errorf("dialed %s", addr)
			return nil, errors.New("no")
		},
		OnRefused:  func(scheme, host string) { got = append(got, scheme+"://"+host) },
		Interfaces: fakeIfaces("198.51.100.7/24", "2606:4700:10::5/64"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	pu, _ := url.Parse(g.ProxyURL())
	c := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu)}}
	for _, u := range []string{"http://[2606:4700:10::5]/", "http://[2606:4700:10::1]:8080/", "http://198.51.100.7/", "http://nas.test/"} {
		resp, err := c.Get(u)
		if err != nil {
			t.Fatalf("%s: %v", u, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s: want 403, got %d", u, resp.StatusCode)
		}
	}
	want := "http://2606:4700:10::5,http://2606:4700:10::1,http://198.51.100.7,http://nas.test"
	if strings.Join(got, ",") != want {
		t.Fatalf("reported %v", got)
	}
}

// TestEgressGuardCanary: the canary host is answered by the guard itself over
// plain HTTP and CONNECT, and never dialed.
func TestEgressGuardCanary(t *testing.T) {
	for _, connect := range []bool{false, true} {
		g, err := StartEgressGuardWithOptions(EgressGuardOptions{
			Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
				t.Errorf("canary dialed %s", addr)
				return nil, errors.New("no")
			},
			Interfaces: fakeIfaces(),
		})
		if err != nil {
			t.Fatal(err)
		}
		if g.CanarySeen() {
			t.Fatal("seen before any request")
		}
		cu, _ := url.Parse(g.CanaryURL())
		if !strings.HasSuffix(cu.Hostname(), ".invalid") {
			t.Fatalf("canary host %q is not under .invalid", cu.Hostname())
		}
		if connect {
			conn, err := net.Dial("tcp", strings.TrimPrefix(g.ProxyURL(), "http://"))
			if err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(conn, "CONNECT %s:443 HTTP/1.1\r\nHost: %s:443\r\n\r\n", cu.Hostname(), cu.Hostname())
			if _, err := http.ReadResponse(bufio.NewReader(conn), nil); err != nil {
				t.Fatal(err)
			}
			conn.Close()
		} else {
			pu, _ := url.Parse(g.ProxyURL())
			c := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu)}}
			resp, err := c.Get(g.CanaryURL())
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusNoContent {
				t.Fatalf("canary status %d", resp.StatusCode)
			}
		}
		if !g.CanarySeen() {
			t.Fatalf("canary not seen (connect=%v)", connect)
		}
		g.Close()
	}
}

// TestEgressGuardFailedOnlyOnUnexpectedExit: Close does not report a failure;
// a listener that dies under the guard does.
func TestEgressGuardFailedOnlyOnUnexpectedExit(t *testing.T) {
	g, err := StartEgressGuardWithOptions(EgressGuardOptions{Interfaces: fakeIfaces()})
	if err != nil {
		t.Fatal(err)
	}
	g.Close()
	select {
	case <-g.Failed():
		t.Fatal("Close reported as a failure")
	case <-time.After(200 * time.Millisecond):
	}

	g2, err := StartEgressGuardWithOptions(EgressGuardOptions{Interfaces: fakeIfaces()})
	if err != nil {
		t.Fatal(err)
	}
	defer g2.Close()
	_ = g2.ln.Close()
	select {
	case <-g2.Failed():
	case <-time.After(5 * time.Second):
		t.Fatal("an unexpected serve exit was not reported")
	}
}
