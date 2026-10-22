package workflow

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Only this test dial seam may route a validated public address to an owned
// loopback server. Production policy has no localhost exemption.
func httpExecutorForTestServer(t *testing.T, server *httptest.Server) *HTTPNodeExecutor {
	t.Helper()
	return &HTTPNodeExecutor{policy: HTTPPolicy{
		resolve: func(_ context.Context, host string) ([]netip.Addr, error) {
			if host != "public.example" {
				t.Errorf("unexpected DNS lookup: %q", host)
			}
			return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
		},
		dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			if address != "8.8.8.8:80" {
				t.Errorf("dial was not pinned: %q", address)
			}
			return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
		},
	}}
}

func TestHTTPAddressClasses(t *testing.T) {
	for _, raw := range []string{
		"127.0.0.1", "127.255.255.254", "0.0.0.0", "0.1.2.3", "::", "::1",
		"169.254.169.254", "fe80::1", "224.0.0.1", "ff02::1", "255.255.255.255",
		"100.100.100.100", "fec0::1", "fe80::1%test",
		"::ffff:127.0.0.1", "::127.0.0.1", "::ffff:169.254.169.254", "::169.254.169.254",
		"fd7a:115c:a1e0::53", "64:ff9b::a00:1", "2002:a00:1::1",
	} {
		t.Run(raw, func(t *testing.T) {
			class, hard := httpAddressClass(netip.MustParseAddr(raw))
			if class == "" || !hard {
				t.Fatalf("%s must be unconditionally denied: %q/%v", raw, class, hard)
			}
		})
	}
	for _, raw := range []string{
		"10.0.0.1", "172.16.0.1", "192.168.1.1", "100.64.0.1", "198.18.0.1", "fd00::1",
		"192.0.2.1", "198.51.100.1", "203.0.113.1", "240.0.0.1", "2001:db8::1",
		"::ffff:10.0.0.1", "::10.0.0.1",
	} {
		t.Run(raw, func(t *testing.T) {
			class, hard := httpAddressClass(netip.MustParseAddr(raw))
			if class == "" || hard {
				t.Fatalf("%s needs explicit operator exception: %q/%v", raw, class, hard)
			}
		})
	}
	for _, raw := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111", "::ffff:8.8.8.8", "::8.8.8.8"} {
		if class, hard := httpAddressClass(netip.MustParseAddr(raw)); class != "" || hard {
			t.Errorf("public %s refused: %q/%v", raw, class, hard)
		}
	}
}

func TestHTTPPolicyRefusesUnsafeURLsBeforeDNS(t *testing.T) {
	policy := HTTPPolicy{resolve: func(context.Context, string) ([]netip.Addr, error) {
		t.Fatal("invalid or literal URL must not trigger DNS")
		return nil, nil
	}}
	for _, raw := range []string{
		"file:///etc/passwd", "gopher://public.example", "http:///missing", "http://user:pass@public.example/",
		"http://public.example/has space", "http://public.example/\n", "http://public.example:0/",
		"http://public.example:65536/", "http://public.example:bad/", "http://[fe80::1%25test]/",
		"http://127.0.0.1", "http://169.254.169.254/latest/meta-data", "http://10.0.0.1",
		"http://100.100.100.100", "http://[::ffff:127.0.0.1]", "http://[::127.0.0.1]",
	} {
		if _, _, err := policy.resolveURL(context.Background(), raw); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
}

func TestHTTPPolicyValidatesEveryAnswer(t *testing.T) {
	for _, denied := range []string{"127.0.0.1", "169.254.169.254", "10.0.0.1", "::ffff:127.0.0.1", "fd00::1"} {
		for _, privateFirst := range []bool{false, true} {
			answers := []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr(denied)}
			if privateFirst {
				answers[0], answers[1] = answers[1], answers[0]
			}
			calls := 0
			e := &HTTPNodeExecutor{policy: HTTPPolicy{
				resolve: func(context.Context, string) ([]netip.Addr, error) {
					calls++
					return answers, nil
				},
				dial: func(context.Context, string, string) (net.Conn, error) {
					t.Error("denied answer reached dialer")
					return nil, errors.New("denied answer reached dialer")
				},
			}}
			_, err := e.Execute(context.Background(), &Node{ID: "test", Params: map[string]any{"url": "http://public.example"}}, nil)
			if err == nil || calls != 1 || strings.Contains(err.Error(), denied) {
				t.Fatalf("mixed answers %v: calls=%d err=%v", answers, calls, err)
			}
		}
	}
}

func TestHTTPPolicyResolutionFailure(t *testing.T) {
	for _, answers := range [][]netip.Addr{nil, {netip.Addr{}}, make([]netip.Addr, 65)} {
		p := HTTPPolicy{resolve: func(context.Context, string) ([]netip.Addr, error) {
			return answers, nil
		}}
		if _, _, err := p.resolveURL(context.Background(), "https://public.example"); err == nil {
			t.Fatalf("accepted invalid answers: %v", answers)
		}
	}
	p := HTTPPolicy{resolve: func(context.Context, string) ([]netip.Addr, error) {
		return nil, errors.New("resolver detail must not reach client: 10.2.3.4")
	}}
	if _, _, err := p.resolveURL(context.Background(), "http://public.example"); err == nil || strings.Contains(err.Error(), "10.2.3.4") {
		t.Fatalf("unsafe resolution error: %v", err)
	}
}

func TestHTTPPolicyAllowlistNeverOverridesHardDeny(t *testing.T) {
	p, err := parseHTTPPolicy([]byte("allow:\n  - cidr: 192.168.2.0/24\n  - cidr: 100.64.0.0/10\n  - host: nas.internal\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		host, address string
		allowed       bool
	}{
		{"nas.internal", "10.2.3.4", true},
		{"NAS.INTERNAL.", "fd00::1", true},
		{"other.internal", "192.168.2.23", true},
		{"other.internal", "192.168.3.23", false},
		{"other.internal", "100.64.0.8", true},
		{"other.internal", "100.100.100.100", false},
		{"nas.internal", "127.0.0.1", false},
		{"nas.internal", "169.254.169.254", false},
		{"nas.internal", "::ffff:127.0.0.1", false},
		{"nas.internal", "::169.254.169.254", false},
		{"nas.internal", "fec0::1", false},
	} {
		p.resolve = func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr(tc.address)}, nil
		}
		_, _, err := p.resolveURL(context.Background(), "http://"+tc.host)
		if (err == nil) != tc.allowed {
			t.Errorf("%s -> %s allowed=%v: %v", tc.host, tc.address, tc.allowed, err)
		}
	}
}

func TestHTTPPolicyStrictConfig(t *testing.T) {
	for _, raw := range []string{
		"", "null", "{}", "allow: null", "allow: nope", "unknown: true", "allow:\n  - host: nas.internal\n    cidr: 10.0.0.0/8",
		"allow:\n  - host: nas.internal\n    host: other.internal", "allow: []\nallow: []",
		"allow: []\n---\nallow: []", "allow:\n  - &alias {host: nas.internal}\n  - *alias",
		"allow:\n  - cidr: 0.0.0.0/0", "allow:\n  - cidr: 127.0.0.0/8", "allow:\n  - cidr: 169.254.0.0/16",
		"allow:\n  - cidr: 100.100.100.100/32", "allow:\n  - cidr: fe80::/10", "allow:\n  - cidr: fec0::/10",
		"allow:\n  - cidr: 8.8.8.0/24", "allow:\n  - host: 127.0.0.1", "allow:\n  - host: ::ffff:127.0.0.1",
		"allow:\n  - host: '*.internal'", "allow:\n  - host: https://nas.internal", "allow:\n  - host: nas.internal:80",
		"allow:\n  - host: 123", "allow:\n  - host: true", "allow:\n" + strings.Repeat("  - host: nas.internal\n", 65),
		strings.Repeat("allow: []\n", 65),
	} {
		if _, err := parseHTTPPolicy([]byte(raw)); err == nil {
			t.Errorf("accepted invalid policy: %q", raw)
		}
	}
	for _, raw := range []string{"allow: []", "allow:\n  - cidr: 10.0.0.0/8", "allow:\n  - cidr: ::ffff:192.168.2.0/120", "allow:\n  - host: nas.internal"} {
		if _, err := parseHTTPPolicy([]byte(raw)); err != nil {
			t.Errorf("refused valid policy %q: %v", raw, err)
		}
	}
}

func TestLoadHTTPPolicyOwnedDirectory(t *testing.T) {
	dir := t.TempDir()
	if p := LoadHTTPPolicy(dir); p.loadErr != nil || len(p.hosts) != 0 || len(p.prefixes) != 0 {
		t.Fatalf("missing policy is not public-only: %+v", p)
	}
	path := filepath.Join(dir, HTTPPolicyFile)
	if err := os.WriteFile(path, []byte("allow:\n  - host: nas.internal\n"), 0600); err != nil {
		t.Fatal(err)
	}
	p := LoadHTTPPolicy(dir)
	if p.loadErr != nil || !p.hosts["nas.internal"] {
		t.Fatalf("valid local policy refused: %+v", p)
	}
	for _, raw := range []string{"malformed: true", strings.Repeat(" ", maxHTTPPolicyBytes+1)} {
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if p := LoadHTTPPolicy(dir); p.loadErr == nil {
			t.Fatal("invalid policy became permissive")
		}
	}
	// An already constructed executor retains its immutable snapshot.
	if !p.hosts["nas.internal"] {
		t.Fatal("file change mutated existing policy")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if p := LoadHTTPPolicy(dir); p.loadErr == nil {
		t.Fatal("non-regular policy accepted")
	}
	if p := LoadHTTPPolicy(""); p.loadErr == nil {
		t.Fatal("implicit working directory accepted")
	}
}

func TestLoadHTTPPolicyRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.yaml")
	if err := os.WriteFile(target, []byte("allow:\n  - host: nas.internal\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, HTTPPolicyFile)); err != nil {
		t.Fatal(err)
	}
	if p := LoadHTTPPolicy(dir); p.loadErr == nil {
		t.Fatal("symlink policy accepted")
	}
}

func TestHTTPPolicyMalformedFileRefusesBeforeLookup(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, HTTPPolicyFile), []byte("invalid: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	p := LoadHTTPPolicy(dir)
	p.resolve = func(context.Context, string) ([]netip.Addr, error) {
		t.Error("invalid policy reached resolver")
		return nil, errors.New("invalid policy reached resolver")
	}
	p.dial = func(context.Context, string, string) (net.Conn, error) {
		t.Error("invalid policy reached dialer")
		return nil, errors.New("invalid policy reached dialer")
	}
	_, err := (&HTTPNodeExecutor{policy: p}).Execute(context.Background(), &Node{ID: "invalid", Params: map[string]any{"url": "http://public.example"}}, nil)
	if err == nil || !strings.Contains(err.Error(), "policy unreadable or invalid") {
		t.Fatalf("malformed policy became permissive: %v", err)
	}
}

func TestHTTPNodePinnedDialAndNoRedirect(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Host != "public.example" {
			t.Errorf("lost original Host: %q", r.Host)
		}
		w.Header().Set("Location", "http://169.254.169.254/latest/meta-data/")
		w.WriteHeader(http.StatusFound)
	}))
	defer server.Close()
	e := httpExecutorForTestServer(t, server)
	lookups := 0
	e.policy.resolve = func(context.Context, string) ([]netip.Addr, error) {
		lookups++
		if lookups > 1 {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}
	output, err := e.Execute(context.Background(), &Node{ID: "redirect", Params: map[string]any{"url": "http://public.example"}}, nil)
	if err != nil || output["status_code"] != http.StatusFound || output["location"] != "http://169.254.169.254/latest/meta-data/" || requests.Load() != 1 || lookups != 1 {
		t.Fatalf("redirect/rebinding guard: output=%v err=%v requests=%d lookups=%d", output, err, requests.Load(), lookups)
	}
}

func TestHTTPTransportNoProxyAndOriginalTLSName(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	p := HTTPPolicy{resolve: func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}}
	u, ip, err := p.resolveURL(context.Background(), "https://public.example:8443/")
	if err != nil {
		t.Fatal(err)
	}
	p.dial = func(_ context.Context, _, address string) (net.Conn, error) {
		if address != "8.8.8.8:8443" {
			t.Errorf("unexpected pin: %q", address)
		}
		return nil, errors.New("owned test dial refusal")
	}
	c := p.clientFor(u, ip, 30*time.Second)
	defer c.CloseIdleConnections()
	tr := c.Transport.(*http.Transport)
	if tr.Proxy != nil || tr.TLSClientConfig.ServerName != "public.example" || tr.TLSClientConfig.InsecureSkipVerify {
		t.Fatalf("unsafe transport: %+v", tr)
	}
	if _, err := tr.DialContext(context.Background(), "tcp", "unvalidated.invalid:4444"); err == nil {
		t.Fatal("test dial unexpectedly succeeded")
	}
}

func TestHTTPNodeIgnoresGraphAllowlist(t *testing.T) {
	e := NewExecutor(ExecutorConfig{})
	n := &Node{ID: "untrusted", Params: map[string]any{
		"url": "http://10.2.3.4", "allow": []string{"10.0.0.0/8"}, "allow_private": true,
	}}
	_, err := e.nodeRegistry[NodeTypeHTTP].Execute(context.Background(), n, map[string]any{"allow_private": true})
	if err == nil || !strings.Contains(err.Error(), "destination not allowed") {
		t.Fatalf("graph widened operator policy: %v", err)
	}
}

func TestHTTPTimeoutBounds(t *testing.T) {
	for seconds, want := range map[int]time.Duration{-1: 30 * time.Second, 0: 30 * time.Second, 1: time.Second, 120: 120 * time.Second, 121: 120 * time.Second} {
		if got := workflowHTTPTimeout(seconds); got != want {
			t.Errorf("timeout %d = %s, want %s", seconds, got, want)
		}
	}
}
