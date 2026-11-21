package localcobrowse

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

// phaseLog records the time each named phase first happened.
type phaseLog struct {
	mu    sync.Mutex
	t0    time.Time
	seen  map[string]time.Duration
	order []string
}

func newPhaseLog() *phaseLog { return &phaseLog{t0: time.Now(), seen: map[string]time.Duration{}} }
func (p *phaseLog) mark(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.seen[name]; !ok {
		p.seen[name] = time.Since(p.t0)
		p.order = append(p.order, name)
	}
}
func (p *phaseLog) report(t *testing.T) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, n := range p.order {
		t.Logf("phase %-28s +%s", n, p.seen[n].Round(time.Millisecond))
	}
}

// firstLoadBudget is the most the first page may take from StartSession to
// the page being readable, on a hosted runner (citadel-cli#1694).
const firstLoadBudget = 10 * time.Second

// TestRealBrowserFirstLoadThroughGuard times each phase of a local-mode style
// session start (real browser, real egress guard, the platform's launch-URL
// validation) up to the first loaded page, and asserts the first load fits
// firstLoadBudget. The local URL policy (Config.URLPolicy) is not in this path.
func TestRealBrowserFirstLoadThroughGuard(t *testing.T) {
	if os.Getenv("CITADEL_COBROWSE_BROWSER_IT") == "" && os.Getenv("CITADEL_REAL_BROWSER_TESTS") == "" {
		t.Skip("set CITADEL_COBROWSE_BROWSER_IT=1 (or CITADEL_REAL_BROWSER_TESTS=1) to run the first-load timing test")
	}
	pl := newPhaseLog()
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pl.mark("page server request")
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<!doctype html><title>first load</title><h1 id="t">first load ok</h1>`)
	}))
	defer page.Close()

	inner := fixedResolver(map[string][]string{"allowed.test": {"203.0.113.10"}})
	resolve := func(ctx context.Context, host string) ([]netip.Addr, error) {
		pl.mark("guard dns resolve " + host)
		return inner(ctx, host)
	}
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		pl.mark("guard dial upstream")
		return (&net.Dialer{}).DialContext(ctx, network, page.Listener.Addr().String())
	}
	g, err := StartEgressGuard(resolve, dial)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	pl.mark("guard listener ready")

	m := platform.NewCobrowseSessionManager(filepath.Join(t.TempDir(), "sessions"), 1)
	m.SetLaunchOptions(platform.LaunchOptions{ProxyServer: g.ProxyURL(), Quiet: true})
	defer m.StopAll()

	pl.mark("StartSession called")
	st, err := m.StartSession("http://allowed.test/")
	pl.mark("StartSession returned")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Logf("start status URL=%q title=%q", st.URL, st.Title)
	deadline := time.Now().Add(60 * time.Second)
	for {
		res, err := m.ExtractBounded(st.ID, "#t", nil, 100)
		if err == nil && res.Text == "first load ok" {
			pl.mark("first page readable")
			break
		}
		if time.Now().After(deadline) {
			pl.report(t)
			t.Fatalf("page never loaded: %+v %v", res, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	pl.report(t)
	if el := pl.seen["first page readable"] - pl.seen["StartSession called"]; el > firstLoadBudget {
		t.Errorf("first load took %s, budget %s", el, firstLoadBudget)
	}
}
