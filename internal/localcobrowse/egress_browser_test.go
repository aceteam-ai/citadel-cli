package localcobrowse

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

// TestRealBrowserEgressGuard runs a real browser (Windows CI: Chrome and Edge)
// through the egress guard. An allowed name reaches its page; a loopback page
// and a loopback subresource requested by an allowed page never connect.
func TestRealBrowserEgressGuard(t *testing.T) {
	if os.Getenv("CITADEL_COBROWSE_BROWSER_IT") == "" {
		t.Skip("set CITADEL_COBROWSE_BROWSER_IT=1 to run the real-browser egress test")
	}
	var secretHits atomic.Int64
	secret := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secretHits.Add(1)
		fmt.Fprint(w, "local secret")
	}))
	defer secret.Close()
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<!doctype html><h1 id="t">Allowed page</h1><img src="%s/pixel"><iframe src="%s/frame"></iframe>`, secret.URL, secret.URL)
	}))
	defer page.Close()

	// "allowed.test" resolves to a public documentation address; the guard's
	// dialer sends that one address to the local page server.
	resolve := fixedResolver(map[string][]string{"allowed.test": {"203.0.113.10"}})
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		if !strings.HasPrefix(addr, "203.0.113.10:") {
			return nil, fmt.Errorf("unexpected dial %s", addr)
		}
		return (&net.Dialer{}).DialContext(ctx, network, page.Listener.Addr().String())
	}
	g, err := StartEgressGuard(resolve, dial)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	m := platform.NewCobrowseSessionManager(filepath.Join(t.TempDir(), "sessions"), 1)
	m.SetLaunchOptions(platform.LaunchOptions{ProxyServer: g.ProxyURL(), Quiet: true})
	defer m.StopAll()
	st, err := m.StartSession("http://allowed.test/")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	deadline := time.Now().Add(20 * time.Second)
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
		t.Fatalf("navigate: %v", err)
	}
	time.Sleep(2 * time.Second)
	if n := secretHits.Load(); n != 0 {
		t.Fatalf("the browser reached a loopback server %d time(s) through the guard", n)
	}
	if g.Refused() == 0 {
		t.Fatal("the guard should have refused the loopback requests")
	}
}
