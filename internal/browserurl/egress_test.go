package browserurl

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
)

func testResolver(m map[string]string) Resolver {
	return func(ctx context.Context, host string) ([]netip.Addr, error) {
		a, ok := m[host]
		if !ok {
			return nil, fmt.Errorf("no such host %s", host)
		}
		return []netip.Addr{netip.MustParseAddr(a)}, nil
	}
}

// TestEgressGuardReportsRefusalsAsSchemeAndHost pins the audit contract: a
// refusal reaches OnRefused as scheme plus host only, never a port, path or
// query, and home-network, mesh and link-local addresses are all refused.
func TestEgressGuardReportsRefusalsAsSchemeAndHost(t *testing.T) {
	fix := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") }))
	defer fix.Close()
	var mu sync.Mutex
	var got []string
	g, err := StartEgressGuardWithOptions(EgressGuardOptions{
		Resolve: testResolver(map[string]string{
			"public.test": "203.0.113.10",
			"router.test": "192.168.1.1",
			"mesh.test":   "100.64.0.7",
		}),
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if !strings.HasPrefix(addr, "203.0.113.10:") {
				t.Errorf("dialed a refused address %s", addr)
			}
			return (&net.Dialer{}).DialContext(ctx, network, fix.Listener.Addr().String())
		},
		OnRefused: func(scheme, host string) {
			mu.Lock()
			got = append(got, scheme+"://"+host)
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	pu, _ := url.Parse(g.ProxyURL())
	c := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu)}}
	for _, u := range []string{
		"http://router.test:8080/admin?token=secret",
		"http://mesh.test/x",
		"http://169.254.169.254/latest/meta-data",
		"http://10.0.0.5/",
	} {
		resp, err := c.Get(u)
		if err != nil {
			t.Fatalf("%s: %v", u, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s: want 403, got %d", u, resp.StatusCode)
		}
	}
	resp, err := c.Get("http://public.test/page?q=1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("public: %d", resp.StatusCode)
	}

	// A CONNECT tunnel to a private address is refused and reported as https.
	conn, err := net.Dial("tcp", strings.TrimPrefix(g.ProxyURL(), "http://"))
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(conn, "CONNECT router.test:443 HTTP/1.1\r\nHost: router.test:443\r\n\r\n")
	cr, err := http.ReadResponse(bufio.NewReader(conn), nil)
	conn.Close()
	if err != nil {
		t.Fatal(err)
	}
	if cr.StatusCode != http.StatusForbidden {
		t.Fatalf("CONNECT: want 403, got %d", cr.StatusCode)
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{"http://router.test", "http://mesh.test", "http://169.254.169.254", "http://10.0.0.5", "https://router.test"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("refusals reported as %v, want %v", got, want)
	}
	for _, r := range got {
		if strings.Count(r, "/") != 2 || strings.Contains(r, "?") || strings.Contains(r, "secret") || strings.Contains(r, "8080") {
			t.Fatalf("refusal report leaked more than scheme and host: %q", r)
		}
	}
}

func TestLogHostStripsUnsafeCharacters(t *testing.T) {
	cases := map[string]string{
		"Example.COM":            "example.com",
		"bad\nhost\x1b[1m":       "badhost[1m",
		"":                       "-",
		"[::1]":                  "[::1]",
		strings.Repeat("a", 400): strings.Repeat("a", 253),
	}
	for in, want := range cases {
		if got := logHost(in); got != want {
			t.Errorf("logHost(%q) = %q, want %q", in, got, want)
		}
	}
}
