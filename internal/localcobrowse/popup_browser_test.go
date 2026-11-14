package localcobrowse

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/cobrowsestream"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/gorilla/websocket"
)

// TestRealBrowserPopupSignIn runs a real browser (Windows CI: Chrome and Edge)
// through a synthetic sign-in flow on local pages: the app opens a popup on a
// second origin, the popup posts a token back to its opener and closes itself.
// It also checks that a popup that stays open becomes the active page for
// scripted actions and for the viewer stream, that the user can switch and
// close pages, and that a popup to a loopback address is closed and never
// connects.
func TestRealBrowserPopupSignIn(t *testing.T) {
	if os.Getenv("CITADEL_COBROWSE_BROWSER_IT") == "" {
		t.Skip("set CITADEL_COBROWSE_BROWSER_IT=1 to run the real-browser popup test")
	}
	var secretHits atomic.Int64
	secret := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secretHits.Add(1)
		fmt.Fprint(w, "local secret")
	}))
	defer secret.Close()

	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		host := strings.Split(r.Host, ":")[0]
		switch {
		case host == "app.test":
			fmt.Fprintf(w, `<!doctype html><title>App</title>
<button id="open" onclick="window.open('http://login.test/popup','login')">sign in</button>
<button id="stay" onclick="window.open('http://login.test/stay')">stay</button>
<button id="bad" onclick="window.open('%s/direct')">bad</button>
<div id="token"></div>
<script>addEventListener('message', function (e) {
  if (e.origin === 'http://login.test') document.getElementById('token').textContent = e.data;
});</script>`, secret.URL)
		case r.URL.Path == "/popup":
			fmt.Fprint(w, `<!doctype html><title>Login</title><script>
opener.postMessage('tok-123', 'http://app.test'); window.close();</script>`)
		default:
			fmt.Fprint(w, `<!doctype html><title>Login</title><h1 id="who">popup</h1>`)
		}
	}))
	defer site.Close()

	addrs := map[string]string{"203.0.113.10": "app.test", "203.0.113.11": "login.test"}
	resolve := fixedResolver(map[string][]string{"app.test": {"203.0.113.10"}, "login.test": {"203.0.113.11"}})
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, _ := net.SplitHostPort(addr)
		if _, ok := addrs[host]; !ok {
			return nil, fmt.Errorf("unexpected dial %s", addr)
		}
		return (&net.Dialer{}).DialContext(ctx, network, site.Listener.Addr().String())
	}
	g, err := StartEgressGuard(resolve, dial)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	m := platform.NewCobrowseSessionManager(filepath.Join(t.TempDir(), "sessions"), 1)
	m.SetLaunchOptions(platform.LaunchOptions{ProxyServer: g.ProxyURL(), Quiet: true})
	var mu sync.Mutex
	var refused []string
	m.EnableTargetTracking(NewURLPolicy(resolve, nil), func(_, action, raw string, _ error) {
		if action != "target_closed" {
			return
		}
		mu.Lock()
		refused = append(refused, raw)
		mu.Unlock()
	})
	defer m.StopAll()
	st, err := m.StartSession("http://app.test/")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	id := st.ID

	diag := func(what string) {
		ts, terr := m.Targets(id)
		mu.Lock()
		r := append([]string(nil), refused...)
		mu.Unlock()
		t.Logf("diag (%s): targets=%+v err=%v refused=%v guardRefused=%d",
			what, ts, terr, r, g.Refused())
		if res, err := m.ExtractBounded(id, "#token", nil, 100); true {
			t.Logf("diag token: %+v %v", res, err)
		}
	}
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if cond() {
				return
			}
			time.Sleep(250 * time.Millisecond)
		}
		diag(what)
		t.Fatalf("timed out waiting for %s", what)
	}
	text := func(sel string) string {
		res, err := m.ExtractBounded(id, sel, nil, 100)
		if err != nil {
			return ""
		}
		return res.Text
	}
	targets := func() platform.CobrowseTargets {
		ts, _ := m.Targets(id)
		return ts
	}
	var opener string
	waitFor("the app page", func() bool {
		ts := targets()
		if len(ts.Targets) == 1 && text("#open") != "" {
			opener = ts.Active
			return true
		}
		return false
	})

	// 1. The sign-in popup posts its token back and closes; the flow completes
	//    on the opener.
	waitFor("the token to reach the opener", func() bool {
		if text("#token") == "tok-123" {
			return true
		}
		_, _ = m.Click(id, "#open", nil, nil)
		time.Sleep(time.Second)
		return text("#token") == "tok-123"
	})
	waitFor("the popup to be gone", func() bool { return len(targets().Targets) == 1 })

	// 2. A popup that stays open becomes the active page.
	waitFor("the staying popup to become active", func() bool {
		ts := targets()
		if len(ts.Targets) == 2 && ts.Active != opener {
			return true
		}
		if len(ts.Targets) == 1 {
			_, _ = m.Click(id, "#stay", nil, nil)
		}
		return false
	})
	waitFor("scripted actions to follow the popup", func() bool { return text("#who") == "popup" })
	popup := targets().Active

	// 3. The viewer stream reports the pages and lets the user switch and close.
	h := cobrowsestream.NewStreamHandler(cobrowsestream.Config{}, cobrowsestream.NewManagerSessionFrom(m),
		func(*http.Request) bool { return true })
	h.SetSilent()
	vs := httptest.NewServer(http.HandlerFunc(h.ServeHTTP))
	defer vs.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(vs.URL, "http")+"?id="+id, nil)
	if err != nil {
		t.Fatalf("viewer: %v", err)
	}
	defer conn.Close()
	read := func(active string, pages int) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			_ = conn.SetReadDeadline(time.Now().Add(20 * time.Second))
			mt, data, err := conn.ReadMessage()
			if err != nil {
				t.Fatalf("viewer read: %v", err)
			}
			if mt != websocket.TextMessage {
				continue
			}
			var msg cobrowsestream.TargetsMessage
			if json.Unmarshal(data, &msg) == nil && msg.Type == cobrowsestream.TargetsType &&
				msg.Active == active && len(msg.Targets) == pages {
				return
			}
		}
		t.Fatalf("no targets message with active=%s pages=%d", active, pages)
	}
	read(popup, 2)
	if _, err := m.Handoff(id); err != nil {
		t.Fatal(err)
	}
	send := func(typ, target string) {
		b, _ := json.Marshal(cobrowsestream.InputMessage{Type: typ, Target: target})
		if err := conn.WriteMessage(websocket.TextMessage, b); err != nil {
			t.Fatal(err)
		}
	}
	send(cobrowsestream.InputTypeTargetSwitch, opener)
	read(opener, 2)
	send(cobrowsestream.InputTypeTargetClose, popup)
	read(opener, 1)
	if _, err := m.Resume(id); err != nil {
		t.Fatal(err)
	}

	// 4. A popup to a loopback address is closed and never connects.
	waitFor("the loopback popup to be closed", func() bool {
		_, _ = m.Click(id, "#bad", nil, nil)
		time.Sleep(time.Second)
		mu.Lock()
		n := len(refused)
		mu.Unlock()
		return n > 0 && len(targets().Targets) == 1
	})
	if n := secretHits.Load(); n != 0 {
		t.Fatalf("a popup reached a loopback server %d time(s)", n)
	}
}
