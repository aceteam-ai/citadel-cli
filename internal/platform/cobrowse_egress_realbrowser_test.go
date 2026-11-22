package platform

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/browserurl"
)

// runEgressGuardRealBrowser is the shared public-only scenario for the
// node-wide manager (citadel-cli#1867 C4b), run against the real Linux runner
// (TestCobrowseRealBrowserEgressGuard) and the Windows headless runner
// (TestWindowsRealBrowserEgressGuard). m must be a node-wide shaped manager.
//
// An allowed public name reaches its page. A loopback page, and loopback, home
// network and mesh subresources requested by the allowed page, never connect,
// and each refusal is reported as scheme plus host only.
func runEgressGuardRealBrowser(t *testing.T, m *CobrowseSessionManager) {
	t.Helper()
	var secretHits atomic.Int64
	secret := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secretHits.Add(1)
		fmt.Fprint(w, "local secret")
	}))
	defer secret.Close()
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<!doctype html><h1 id="t">Allowed page</h1>`+
			`<img src="%s/pixel"><iframe src="%s/frame"></iframe>`+
			`<img src="http://router.test/admin?token=x"><img src="http://mesh.test/status">`, secret.URL, secret.URL)
	}))
	defer page.Close()

	// "allowed.test" resolves to a public documentation address that the
	// guard's dialer sends to the local page server; the other names resolve
	// to a home-network and a mesh address the guard must refuse.
	addrs := map[string]string{
		"allowed.test": "203.0.113.10",
		"router.test":  "192.168.1.1",
		"mesh.test":    "100.64.0.9",
	}
	resolve := func(ctx context.Context, host string) ([]netip.Addr, error) {
		a, ok := addrs[host]
		if !ok {
			return nil, fmt.Errorf("no such host %s", host)
		}
		return []netip.Addr{netip.MustParseAddr(a)}, nil
	}
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		if !strings.HasPrefix(addr, "203.0.113.10:") {
			return nil, fmt.Errorf("unexpected dial %s", addr)
		}
		return (&net.Dialer{}).DialContext(ctx, network, page.Listener.Addr().String())
	}
	var mu sync.Mutex
	refused := map[string]bool{}
	var guard *browserurl.EgressGuard
	prev := startSessionEgressGuard
	startSessionEgressGuard = func(onRefused func(scheme, host string)) (*browserurl.EgressGuard, error) {
		g, err := browserurl.StartEgressGuardWithOptions(browserurl.EgressGuardOptions{
			Resolve: resolve, Dial: dial,
			OnRefused: func(scheme, host string) {
				mu.Lock()
				refused[scheme+"://"+host] = true
				mu.Unlock()
				onRefused(scheme, host)
			},
		})
		guard = g
		return g, err
	}
	t.Cleanup(func() { startSessionEgressGuard = prev })
	m.SetEgressRequired(func() bool { return true })

	st, err := m.StartSession("http://allowed.test/")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = m.Stop(st.ID) }()
	if st.Egress != EgressPublicOnly {
		t.Fatalf("status egress = %q, want %q", st.Egress, EgressPublicOnly)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		res, err := m.ExtractBounded(st.ID, "#t", nil, 100)
		if err == nil && res.Text == "Allowed page" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("allowed page never loaded through the guard: %+v %v", res, err)
		}
		time.Sleep(250 * time.Millisecond)
	}
	if _, err := m.Navigate(st.ID, secret.URL+"/direct"); err != nil {
		t.Logf("navigate to the loopback page: %v", err)
	}
	time.Sleep(2 * time.Second)
	if n := secretHits.Load(); n != 0 {
		t.Fatalf("the browser reached a loopback server %d time(s) through the guard", n)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, want := range []string{"http://127.0.0.1", "http://router.test", "http://mesh.test"} {
		if !refused[want] {
			t.Errorf("guard did not report refusing %s (reported %v)", want, refused)
		}
	}
	for k := range refused {
		if strings.ContainsAny(strings.TrimPrefix(strings.TrimPrefix(k, "https://"), "http://"), "/?") {
			t.Errorf("refusal reported more than scheme and host: %q", k)
		}
	}
	if guard == nil || guard.Refused() == 0 {
		t.Fatal("the guard should have refused the private requests")
	}
}
