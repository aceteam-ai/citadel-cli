package localcobrowse

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

// guardWithFixture starts a guard whose resolver maps the test names, and
// whose dialer sends any allowed address to the local fixture server (the
// fixture itself is on loopback, which the guard would refuse by address).
func guardWithFixture(t *testing.T) (*EgressGuard, *atomic.Int64, *httptest.Server) {
	t.Helper()
	var hits atomic.Int64
	fix := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, "fixture ok")
	}))
	t.Cleanup(fix.Close)
	resolve := fixedResolver(map[string][]string{
		"public.test": {"203.0.113.10"},
		"rebind.test": {"127.0.0.1"},
		"mixed.test":  {"203.0.113.10", "10.0.0.1"},
	})
	var dialed []string
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialed = append(dialed, addr)
		if !strings.HasPrefix(addr, "203.0.113.10:") {
			t.Errorf("guard dialed an address it should have refused: %s", addr)
		}
		return (&net.Dialer{}).DialContext(ctx, network, fix.Listener.Addr().String())
	}
	g, err := StartEgressGuard(resolve, dial)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(g.Close)
	return g, &hits, fix
}

func TestEgressGuardPlainHTTP(t *testing.T) {
	g, hits, _ := guardWithFixture(t)
	pu, _ := url.Parse(g.ProxyURL())
	c := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu)}}
	resp, err := c.Get("http://public.test/page")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "fixture ok" {
		t.Fatalf("allowed request: %d %q", resp.StatusCode, body)
	}
	for _, u := range []string{"http://rebind.test/", "http://mixed.test/", "http://127.0.0.1/", "http://localhost/", "http://169.254.169.254/", "http://[::1]/", "http://2130706433/"} {
		resp, err := c.Get(u)
		if err != nil {
			t.Fatalf("%s: %v", u, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s: want 403, got %d", u, resp.StatusCode)
		}
	}
	if hits.Load() != 1 || g.Refused() < 6 {
		t.Fatalf("hits=%d refused=%d", hits.Load(), g.Refused())
	}
}

func TestEgressGuardConnect(t *testing.T) {
	g, hits, _ := guardWithFixture(t)
	connect := func(target string) (int, net.Conn) {
		c, err := net.Dial("tcp", strings.TrimPrefix(g.ProxyURL(), "http://"))
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
		resp, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: http.MethodConnect})
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, c
	}
	code, c := connect("public.test:443")
	if code != 200 {
		t.Fatalf("allowed CONNECT: %d", code)
	}
	fmt.Fprint(c, "GET / HTTP/1.1\r\nHost: public.test\r\nConnection: close\r\n\r\n")
	b, _ := io.ReadAll(c)
	c.Close()
	if !strings.Contains(string(b), "fixture ok") || hits.Load() != 1 {
		t.Fatalf("tunnel did not reach the fixture: %q", b)
	}
	for _, target := range []string{"127.0.0.1:9222", "rebind.test:443", "[::ffff:127.0.0.1]:443", "10.1.2.3:443", "localhost:80"} {
		code, c := connect(target)
		c.Close()
		if code != http.StatusForbidden {
			t.Errorf("CONNECT %s: want 403, got %d", target, code)
		}
	}
}
