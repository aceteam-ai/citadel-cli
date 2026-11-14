package platform

import (
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// latencyCDP is a fake DevTools endpoint with injectable latency per step, used
// to measure how many discoveries and dials an action makes and how it behaves
// at its deadline.
type latencyCDP struct {
	port      int
	jsonDelay time.Duration // per /json discovery
	dialDelay time.Duration // per WebSocket upgrade
	callDelay time.Duration // per CDP call
	// methodDelay overrides callDelay for one method; evalDelay for an evaluate
	// whose expression contains the key.
	methodDelay map[string]time.Duration
	evalDelay   map[string]time.Duration

	discoveries atomic.Int64
	dials       atomic.Int64

	mu          sync.Mutex
	scrollEvals int
	methods     []string
	scrollY     float64
	url         string
	focus       string
}

func (f *latencyCDP) count(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, m := range f.methods {
		if m == method {
			n++
		}
	}
	return n
}

func startLatencyCDP(t *testing.T, mutate func(*latencyCDP)) *latencyCDP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &latencyCDP{port: ln.Addr().(*net.TCPAddr).Port, url: "http://example.test/page"}
	if mutate != nil {
		mutate(f)
	}
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	mux := http.NewServeMux()
	mux.HandleFunc("/json", func(w http.ResponseWriter, r *http.Request) {
		f.discoveries.Add(1)
		time.Sleep(f.jsonDelay)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `[{"type":"page","url":%q,"webSocketDebuggerUrl":"ws://127.0.0.1:%d/devtools/page/1"}]`, f.url, f.port)
	})
	mux.HandleFunc("/devtools/page/1", func(w http.ResponseWriter, r *http.Request) {
		f.dials.Add(1)
		time.Sleep(f.dialDelay)
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			var msg struct {
				ID     int            `json:"id"`
				Method string         `json:"method"`
				Params map[string]any `json:"params"`
			}
			if conn.ReadJSON(&msg) != nil {
				return
			}
			f.mu.Lock()
			f.methods = append(f.methods, msg.Method)
			f.mu.Unlock()
			delay := f.callDelay
			if d, ok := f.methodDelay[msg.Method]; ok {
				delay = d
			}
			result := map[string]any{}
			if msg.Method == "Runtime.evaluate" {
				expr, _ := msg.Params["expression"].(string)
				for k, d := range f.evalDelay {
					if strings.Contains(expr, k) {
						delay = d
						f.mu.Lock()
						if strings.Contains(k, "scrollIntoView") {
							f.scrollY = 480
							f.scrollEvals++
						}
						f.mu.Unlock()
					}
				}
				f.mu.Lock()
				var val any
				switch {
				case strings.Contains(expr, "window.scrollX"):
					val = map[string]any{"url": f.url, "x": 0.0, "y": f.scrollY, "tag": f.focus, "sig": f.focus}
				case strings.Contains(expr, "#missing"):
					val = nil
				case strings.Contains(expr, "scrollIntoView"):
					val = map[string]any{"x": 100.0, "y": 100.0}
				case strings.Contains(expr, "readyState"):
					val = map[string]any{"url": f.url, "ready": "complete", "title": "t"}
				}
				f.mu.Unlock()
				result = map[string]any{"result": map[string]any{"type": "object", "value": val}}
			}
			if msg.Method == "Page.captureScreenshot" {
				result = map[string]any{"data": base64.StdEncoding.EncodeToString([]byte("png"))}
			}
			time.Sleep(delay)
			_ = conn.WriteJSON(map[string]any{"id": msg.ID, "result": result})
		}
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() { _ = srv.Close() })
	return f
}

func newBudgetSession(t *testing.T, f *latencyCDP) (*CobrowseSessionManager, string) {
	t.Helper()
	installFixedPortLauncher(t, f.port)
	m := newCobrowseSessionManager(trustedBaseDir(t), 8)
	st, err := m.StartSession("")
	if err != nil {
		t.Fatal(err)
	}
	return m, st.ID
}

func shortBudget(t *testing.T, action, report time.Duration) {
	t.Helper()
	pa, pr := CobrowseActionDeadline, cobrowseReportReserve
	CobrowseActionDeadline, cobrowseReportReserve = action, report
	t.Cleanup(func() { CobrowseActionDeadline, cobrowseReportReserve = pa, pr })
}

// TestActionLatencyProfile prints discoveries, dials and wall time per action
// under fixed fake latencies (40ms discovery, 25ms dial, 5ms per call), and
// pins the new ceilings: at most one discovery and one dial per action.
func TestActionLatencyProfile(t *testing.T) {
	f := startLatencyCDP(t, func(f *latencyCDP) {
		f.jsonDelay, f.dialDelay, f.callDelay = 40*time.Millisecond, 25*time.Millisecond, 5*time.Millisecond
	})
	m, id := newBudgetSession(t, f)
	x, y := 10.0, 10.0
	cases := []struct {
		name string
		run  func() error
	}{
		{"status", func() error { _, ok := m.SessionStatus(id); _ = ok; return nil }},
		{"screenshot", func() error { _, err := m.Screenshot(id); return err }},
		{"click(selector)", func() error { _, err := m.Click(id, "#a", nil, nil); return err }},
		{"click(x,y)", func() error { _, err := m.Click(id, "", &x, &y); return err }},
		{"type", func() error { _, err := m.Type(id, "hello"); return err }},
		{"key", func() error { _, err := m.Key(id, "Enter"); return err }},
	}
	for _, c := range cases {
		d0, w0 := f.discoveries.Load(), f.dials.Load()
		start := time.Now()
		if err := c.run(); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		el := time.Since(start)
		d, w := f.discoveries.Load()-d0, f.dials.Load()-w0
		t.Logf("%-16s discoveries=%d dials=%d wall=%dms", c.name, d, w, el.Milliseconds())
		if c.name != "status" && (d > 1 || w > 1) {
			t.Errorf("%s made %d discoveries and %d dials, want at most 1 each", c.name, d, w)
		}
	}
}

func TestClickTimeoutAfterScrollReportsPartialEffects(t *testing.T) {
	shortBudget(t, 300*time.Millisecond, time.Second)
	f := startLatencyCDP(t, func(f *latencyCDP) {
		// The selector resolution scrolls the element into view and then
		// stalls past the deadline.
		f.evalDelay = map[string]time.Duration{"scrollIntoView": 2 * time.Second}
	})
	m, id := newBudgetSession(t, f)
	start := time.Now()
	st, err := m.Click(id, "#field", nil, nil)
	if el := time.Since(start); el > 1500*time.Millisecond {
		t.Fatalf("click ran %s, past action deadline plus report reserve", el)
	}
	ae, ok := AsActionIncomplete(err)
	if !ok {
		t.Fatalf("want *ActionIncompleteError, got %v", err)
	}
	if !ae.TimedOut || !ae.Reportable() {
		t.Errorf("timed_out=%v reportable=%v", ae.TimedOut, ae.Reportable())
	}
	e := ae.Effects
	if !e.Scrolled || e.ScrollY != 480 {
		t.Errorf("scrolled=%v scroll_y=%v, want true 480", e.Scrolled, e.ScrollY)
	}
	if e.Clicked || e.Dispatched {
		t.Errorf("clicked=%v dispatched=%v, but no mouse event was sent", e.Clicked, e.Dispatched)
	}
	if st.Done == nil || *st.Done || st.Effects == nil {
		t.Errorf("status done/effects not set: %+v", st)
	}
	if n := f.count("Input.dispatchMouseEvent"); n != 0 {
		t.Errorf("%d mouse events sent after the deadline", n)
	}
	// Never retried: let the stalled call finish, then check it ran once.
	time.Sleep(2200 * time.Millisecond)
	f.mu.Lock()
	n := f.scrollEvals
	f.mu.Unlock()
	if n != 1 {
		t.Errorf("selector resolution ran %d times, want exactly 1 (no retry)", n)
	}
}

func TestTypeTimeoutReportsLengthNotText(t *testing.T) {
	shortBudget(t, 250*time.Millisecond, time.Second)
	f := startLatencyCDP(t, func(f *latencyCDP) {
		f.methodDelay = map[string]time.Duration{"Input.insertText": 2 * time.Second}
		f.focus = "textarea"
	})
	m, id := newBudgetSession(t, f)
	_, err := m.Type(id, "héllo")
	ae, ok := AsActionIncomplete(err)
	if !ok {
		t.Fatalf("want incomplete error, got %v", err)
	}
	if ae.Effects.TypedLength != 5 || !ae.Effects.TypedUnconfirmed || !ae.Effects.Dispatched {
		t.Errorf("effects %+v", ae.Effects)
	}
	if strings.Contains(ae.Error(), "héllo") {
		t.Errorf("error leaks typed text: %v", ae)
	}
	if n := f.count("Input.insertText"); n != 1 {
		t.Errorf("insertText sent %d times, want exactly 1", n)
	}
}

func TestPlainFailureWithoutEffectsStaysPlain(t *testing.T) {
	f := startLatencyCDP(t, nil)
	m, id := newBudgetSession(t, f)
	// A selector that matches nothing resolves to null: nothing changed.
	_, err := m.Click(id, "#missing", nil, nil)
	if err == nil {
		t.Fatal("missing selector must fail")
	}
	if ae, ok := AsActionIncomplete(err); ok && ae.Reportable() {
		t.Errorf("a failure that changed nothing must stay a plain error: %v", err)
	}
}

func TestSuccessfulActionReportsDoneAndEffects(t *testing.T) {
	f := startLatencyCDP(t, nil)
	m, id := newBudgetSession(t, f)
	x, y := 5.0, 6.0
	st, err := m.Click(id, "", &x, &y)
	if err != nil {
		t.Fatal(err)
	}
	if st.Done == nil || !*st.Done || st.Effects == nil || !st.Effects.Clicked || !st.Effects.Dispatched {
		t.Errorf("status %+v effects %+v", st, st.Effects)
	}
	if st.URL != f.url {
		t.Errorf("url %q", st.URL)
	}
}

func TestStatusIsBoundedAndFallsBackToLastURL(t *testing.T) {
	prev := cobrowseStatusProbeTimeout
	cobrowseStatusProbeTimeout = 200 * time.Millisecond
	t.Cleanup(func() { cobrowseStatusProbeTimeout = prev })
	f := startLatencyCDP(t, nil)
	m, id := newBudgetSession(t, f)
	if st, _ := m.SessionStatus(id); st.URL != f.url {
		t.Fatalf("first status url %q", st.URL)
	}
	f.jsonDelay = 3 * time.Second
	start := time.Now()
	st, _ := m.SessionStatus(id)
	if el := time.Since(start); el > time.Second {
		t.Errorf("status took %s with a stalled /json, want about 200ms", el)
	}
	if st.URL != "http://example.test/page" {
		t.Errorf("fallback url %q", st.URL)
	}
}
