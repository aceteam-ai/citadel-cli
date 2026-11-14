package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/gorilla/websocket"
)

func noOpeners(string) (string, bool) { return "", false }

func openersOf(m map[string]string) func(string) (string, bool) {
	return func(id string) (string, bool) { o, ok := m[id]; return o, ok }
}

func TestReconcileTargets(t *testing.T) {
	// First observation adopts the first page.
	active, stack := reconcileTargets("", nil, nil, []string{"a", "b"}, noOpeners)
	if active != "a" || len(stack) != 0 {
		t.Fatalf("first: active=%q stack=%v", active, stack)
	}
	// A popup opened by the active page becomes active; the opener is remembered.
	active, stack = reconcileTargets(active, stack, []string{"a", "b"}, []string{"a", "b", "c"}, openersOf(map[string]string{"c": "a"}))
	if active != "c" || !reflect.DeepEqual(stack, []string{"a"}) {
		t.Fatalf("popup: active=%q stack=%v", active, stack)
	}
	// A popup opened by the popup (a nested sign-in step).
	active, stack = reconcileTargets(active, stack, []string{"a", "b", "c"}, []string{"a", "b", "c", "d"}, openersOf(map[string]string{"d": "c"}))
	if active != "d" || !reflect.DeepEqual(stack, []string{"a", "c"}) {
		t.Fatalf("nested: active=%q stack=%v", active, stack)
	}
	// The nested popup closes: back to its opener, not the first page.
	active, stack = reconcileTargets(active, stack, []string{"a", "b", "c", "d"}, []string{"a", "b", "c"}, noOpeners)
	if active != "c" || !reflect.DeepEqual(stack, []string{"a"}) {
		t.Fatalf("close nested: active=%q stack=%v", active, stack)
	}
	// Everything but one page closed: fall to what remains.
	active, stack = reconcileTargets(active, stack, []string{"a", "b", "c"}, []string{"b"}, noOpeners)
	if active != "b" || len(stack) != 0 {
		t.Fatalf("fallback: active=%q stack=%v", active, stack)
	}
}

func TestReconcileTargetsIgnoresPagesNotOpenedByTheActivePage(t *testing.T) {
	prev := []string{"a", "b"}
	ids := []string{"a", "b", "c"}
	// Opened by a background page, by nothing known, or with no answer at all.
	for name, op := range map[string]func(string) (string, bool){
		"background": openersOf(map[string]string{"c": "b"}),
		"none":       openersOf(map[string]string{"c": ""}),
		"unknown":    noOpeners,
	} {
		active, stack := reconcileTargets("a", nil, prev, ids, op)
		if active != "a" || len(stack) != 0 {
			t.Fatalf("%s: active=%q stack=%v", name, active, stack)
		}
	}
	// A long chain keeps the stack bounded.
	var order []string
	stack := []string{}
	active := "p0"
	op := map[string]string{}
	order = append(order, "p0")
	for i := 1; i < 40; i++ {
		id := fmt.Sprintf("p%d", i)
		op[id] = active
		next := append(append([]string(nil), order...), id)
		active, stack = reconcileTargets(active, stack, order, next, openersOf(op))
		order = next
	}
	if len(stack) > maxTargetStack {
		t.Fatalf("stack grew to %d", len(stack))
	}
}

// fakeDevTools serves /json, /json/close and a recording page socket for pages.
type fakeDevTools struct {
	mu     sync.Mutex
	pages  []map[string]string
	closed []string
	calls  []string // "<page id> <method>"
	srv    *httptest.Server
	port   int
}

func newFakeDevTools(t *testing.T) *fakeDevTools {
	f := &fakeDevTools{}
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	mux := http.NewServeMux()
	mux.HandleFunc("/json", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(f.pages)
	})
	mux.HandleFunc("/json/close/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/json/close/")
		f.mu.Lock()
		defer f.mu.Unlock()
		f.closed = append(f.closed, id)
		for i, p := range f.pages {
			if p["id"] == id {
				f.pages = append(f.pages[:i], f.pages[i+1:]...)
				break
			}
		}
		fmt.Fprint(w, "Target is closing")
	})
	mux.HandleFunc("/devtools/page/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/devtools/page/")
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			var msg struct {
				ID     int    `json:"id"`
				Method string `json:"method"`
			}
			if conn.ReadJSON(&msg) != nil {
				return
			}
			f.mu.Lock()
			f.calls = append(f.calls, id+" "+msg.Method)
			f.mu.Unlock()
			_ = conn.WriteJSON(map[string]any{"id": msg.ID, "result": map[string]any{
				"result": map[string]any{"value": map[string]any{"x": 10, "y": 10, "url": "http://app.test/", "ready": "complete", "title": "t", "text": id}},
			}})
		}
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	u, _ := url.Parse(f.srv.URL)
	f.port, _ = strconv.Atoi(u.Port())
	return f
}

func (f *fakeDevTools) set(pages ...[2]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pages = nil
	for _, p := range pages {
		f.pages = append(f.pages, map[string]string{
			"id": p[0], "type": "page", "url": p[1], "title": "t-" + p[0],
			"webSocketDebuggerUrl": fmt.Sprintf("ws://127.0.0.1:%d/devtools/page/%s", f.port, p[0]),
		})
	}
}

func (f *fakeDevTools) closedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.closed...)
}

func (f *fakeDevTools) callsOn(id string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, id+" ") {
			out = append(out, strings.TrimPrefix(c, id+" "))
		}
	}
	return out
}

func withOpeners(t *testing.T, m map[string]string) {
	t.Helper()
	prev := fetchTargetOpeners
	fetchTargetOpeners = func(context.Context, int) (map[string]string, error) { return m, nil }
	t.Cleanup(func() { fetchTargetOpeners = prev })
}

func TestTargetSetFollowsPopupThenOpener(t *testing.T) {
	f := newFakeDevTools(t)
	withOpeners(t, map[string]string{"p": "a"})
	ts := newTargetSet()
	ctx := context.Background()

	f.set([2]string{"a", "http://app.test/"})
	snap, err := ts.sync(ctx, f.port)
	if err != nil || snap.Active != "a" {
		t.Fatalf("first: %+v %v", snap, err)
	}
	f.set([2]string{"a", "http://app.test/"}, [2]string{"p", "http://login.test/"})
	snap, _ = ts.sync(ctx, f.port)
	if snap.Active != "p" || len(snap.Targets) != 2 || !snap.Targets[1].Active {
		t.Fatalf("popup: %+v", snap)
	}
	if !strings.HasSuffix(snap.ActiveWebSocketURL, "/devtools/page/p") {
		t.Fatalf("active ws = %q", snap.ActiveWebSocketURL)
	}
	f.set([2]string{"a", "http://app.test/"})
	snap, _ = ts.sync(ctx, f.port)
	if snap.Active != "a" || len(snap.Targets) != 1 {
		t.Fatalf("after popup closed: %+v", snap)
	}
}

func TestTargetSetKeepsBrowserListOrderOutOfTheSnapshot(t *testing.T) {
	f := newFakeDevTools(t)
	withOpeners(t, map[string]string{})
	ts := newTargetSet()
	ctx := context.Background()
	f.set([2]string{"a", "http://app.test/"})
	_, _ = ts.sync(ctx, f.port)
	f.set([2]string{"a", "http://app.test/"}, [2]string{"b", "http://app.test/b"})
	_, _ = ts.sync(ctx, f.port)
	// The browser now lists the most recently active page first.
	f.set([2]string{"b", "http://app.test/b"}, [2]string{"a", "http://app.test/"})
	snap, _ := ts.sync(ctx, f.port)
	if snap.Targets[0].ID != "a" || snap.Targets[1].ID != "b" || snap.Active != "a" {
		t.Fatalf("order followed the browser list: %+v", snap)
	}
}

func TestTargetSetBackgroundPageDoesNotStealTheActivePage(t *testing.T) {
	f := newFakeDevTools(t)
	withOpeners(t, map[string]string{"c": "b"}) // c was opened by background page b
	ts := newTargetSet()
	ctx := context.Background()
	f.set([2]string{"a", "http://app.test/"}, [2]string{"b", "http://app.test/b"})
	_, _ = ts.sync(ctx, f.port)
	f.set([2]string{"a", "http://app.test/"}, [2]string{"b", "http://app.test/b"}, [2]string{"c", "http://app.test/c"})
	snap, _ := ts.sync(ctx, f.port)
	if snap.Active != "a" || len(snap.Targets) != 3 {
		t.Fatalf("background page took over: %+v", snap)
	}
}

func TestTargetSetClosesPopupRefusedByPolicy(t *testing.T) {
	f := newFakeDevTools(t)
	withOpeners(t, map[string]string{"p": "a", "q": "a"})
	ts := newTargetSet()
	var events []string
	ts.configure("cb-1",
		func(_ context.Context, raw string) error {
			if strings.Contains(raw, "private.test") {
				return fmt.Errorf("navigation refused: private address")
			}
			return nil
		},
		func(id, action, raw string, err error) { events = append(events, id+" "+action+" "+raw) })
	ctx := context.Background()

	f.set([2]string{"a", "http://app.test/"})
	_, _ = ts.sync(ctx, f.port)
	f.set([2]string{"a", "http://app.test/"}, [2]string{"p", "http://private.test/x"})
	snap, _ := ts.sync(ctx, f.port)
	if snap.Active != "a" || len(snap.Targets) != 1 {
		t.Fatalf("refused popup stayed: %+v", snap)
	}
	if got := f.closedIDs(); len(got) != 1 || got[0] != "p" {
		t.Fatalf("closed = %v", got)
	}
	if len(events) != 1 || !strings.HasPrefix(events[0], "cb-1 target_closed ") {
		t.Fatalf("event: %v", events)
	}

	// A popup with no address yet is not refused; one that later navigates to a
	// refused address is closed when the address is seen.
	f.set([2]string{"a", "http://app.test/"}, [2]string{"q", ""})
	snap, _ = ts.sync(ctx, f.port)
	if snap.Active != "q" {
		t.Fatalf("empty popup: %+v", snap)
	}
	f.set([2]string{"a", "http://app.test/"}, [2]string{"q", "http://private.test/"})
	snap, _ = ts.sync(ctx, f.port)
	if snap.Active != "a" || len(snap.Targets) != 1 {
		t.Fatalf("navigated popup stayed: %+v", snap)
	}
}

func TestUnconfiguredTargetSetRefusesNonWebPages(t *testing.T) {
	f := newFakeDevTools(t)
	withOpeners(t, map[string]string{})
	for _, bad := range []string{"file:///etc/hosts", "data:text/html,x", "chrome://settings", "edge://settings", "javascript:1", "chrome-error://chromewebdata/"} {
		ts := newTargetSet() // never configured: no policy, no callback
		f.set([2]string{"a", "http://app.test/"})
		if _, err := ts.sync(context.Background(), f.port); err != nil {
			t.Fatal(err)
		}
		f.set([2]string{"a", "http://app.test/"}, [2]string{"p", bad})
		snap, _ := ts.sync(context.Background(), f.port)
		if len(snap.Targets) != 1 || snap.Active != "a" {
			t.Errorf("%s stayed open: %+v", bad, snap)
		}
	}
}

func TestTargetSetJudgesAnOriginOnceAndAReportedOnlyPageOnce(t *testing.T) {
	f := newFakeDevTools(t)
	withOpeners(t, map[string]string{})
	ts := newTargetSet()
	calls, events := 0, 0
	ts.configure("cb-1",
		func(_ context.Context, raw string) error {
			calls++
			if strings.Contains(raw, "private.test") {
				return fmt.Errorf("refused")
			}
			return nil
		},
		func(string, string, string, error) { events++ })
	ctx := context.Background()
	f.set([2]string{"a", "http://app.test/1"})
	_, _ = ts.sync(ctx, f.port)
	f.set([2]string{"a", "http://app.test/2"})
	_, _ = ts.sync(ctx, f.port)
	f.set([2]string{"a", "http://app.test/3"})
	_, _ = ts.sync(ctx, f.port)
	if calls != 1 {
		t.Fatalf("policy ran %d times for one origin", calls)
	}

	// The only page on a refused address is left open, reported once.
	f2 := newFakeDevTools(t)
	ts2 := newTargetSet()
	events = 0
	ts2.configure("cb-1", func(context.Context, string) error { return fmt.Errorf("refused") },
		func(string, string, string, error) { events++ })
	f2.set([2]string{"a", "http://private.test/"})
	for i := 0; i < 5; i++ {
		snap, err := ts2.sync(ctx, f2.port)
		if err != nil || len(snap.Targets) != 1 {
			t.Fatalf("only page: %+v %v", snap, err)
		}
	}
	if events != 1 || len(f2.closedIDs()) != 0 {
		t.Fatalf("events=%d closed=%v", events, f2.closedIDs())
	}
}

func TestTargetSetBoundsPopupsAndSurvivesAFlood(t *testing.T) {
	f := newFakeDevTools(t)
	withOpeners(t, map[string]string{})
	ts := newTargetSet()
	f.set([2]string{"p0", "http://app.test/"})
	if _, err := ts.sync(context.Background(), f.port); err != nil {
		t.Fatal(err)
	}
	var pages [][2]string
	for i := 0; i < MaxCobrowseTargets+3; i++ {
		pages = append(pages, [2]string{fmt.Sprintf("p%d", i), "http://app.test/"})
	}
	f.set(pages...)
	snap, _ := ts.sync(context.Background(), f.port)
	if len(snap.Targets) != MaxCobrowseTargets || snap.Targets[0].ID != "p0" {
		t.Fatalf("kept %d pages: %+v", len(snap.Targets), snap.Targets)
	}

	// More pages than discovery will list must not wedge the session.
	pages = nil
	for i := 0; i < 80; i++ {
		pages = append(pages, [2]string{fmt.Sprintf("f%d", i), "http://app.test/"})
	}
	pages = append([][2]string{{"p0", "http://app.test/"}}, pages...)
	f.set(pages...)
	snap, err := ts.sync(context.Background(), f.port)
	if err != nil {
		t.Fatalf("flood wedged the session: %v", err)
	}
	if len(snap.Targets) > MaxCobrowseTargets || snap.Targets[0].ID != "p0" {
		t.Fatalf("flood: %d pages", len(snap.Targets))
	}
}

func TestTargetSetNeverClosesLastPage(t *testing.T) {
	f := newFakeDevTools(t)
	ts := newTargetSet()
	f.set([2]string{"a", "chrome://newtab/"})
	snap, err := ts.sync(context.Background(), f.port)
	if err != nil || len(snap.Targets) != 1 || len(f.closedIDs()) != 0 {
		t.Fatalf("last page: %+v %v closed=%v", snap, err, f.closedIDs())
	}
}

func TestSetActiveUsesPreviousAsOpener(t *testing.T) {
	f := newFakeDevTools(t)
	ts := newTargetSet()
	f.set([2]string{"a", "http://app.test/"}, [2]string{"b", "http://app.test/b"})
	if _, err := ts.sync(context.Background(), f.port); err != nil {
		t.Fatal(err)
	}
	if err := ts.setActive("b"); err != nil {
		t.Fatal(err)
	}
	if err := ts.setActive("nope"); err == nil {
		t.Fatal("switched to an unknown page")
	}
	f.set([2]string{"a", "http://app.test/"})
	snap, _ := ts.sync(context.Background(), f.port)
	if snap.Active != "a" {
		t.Fatalf("after closing the chosen page: %+v", snap)
	}
}

func TestCloseTargetByIDRejectsBadID(t *testing.T) {
	for _, id := range []string{"", "../x", "a/b", "a b", strings.Repeat("a", 200)} {
		if closeTargetByID(context.Background(), 1, id) == nil {
			t.Errorf("accepted id %q", id)
		}
	}
}

func startTracked(t *testing.T, f *fakeDevTools, events *[]string) (*CobrowseSessionManager, string) {
	t.Helper()
	installFixedPortLauncher(t, f.port)
	m := newCobrowseSessionManager(trustedBaseDir(t), 8)
	m.EnableTargetTracking(func(context.Context, string) error { return nil },
		func(id, action, raw string, err error) { *events = append(*events, action) })
	st, err := m.StartSession("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Stop(st.ID) })
	return m, st.ID
}

func TestManagerPageChoiceNeedsUserControl(t *testing.T) {
	f := newFakeDevTools(t)
	withOpeners(t, map[string]string{})
	f.set([2]string{"a", "http://app.test/"}, [2]string{"b", "http://app.test/b"})
	var events []string
	m, id := startTracked(t, f, &events)

	if err := m.SwitchTarget(id, "b"); err == nil || !strings.Contains(err.Error(), "not taken control") {
		t.Fatalf("agent-driven switch: %v", err)
	}
	if err := m.CloseTarget(id, "b"); err == nil {
		t.Fatal("agent-driven close allowed")
	}
	if _, err := m.Handoff(id); err != nil {
		t.Fatal(err)
	}
	if err := m.SwitchTarget(id, "b"); err != nil {
		t.Fatalf("user switch: %v", err)
	}
	status, _ := m.SessionStatus(id)
	if status.ActiveTarget != "b" || len(status.Targets) != 2 || status.URL != "http://app.test/b" {
		t.Fatalf("status: %+v", status)
	}
	if err := m.CloseTarget(id, "b"); err != nil {
		t.Fatalf("user close: %v", err)
	}
	if err := m.CloseTarget(id, "a"); err == nil {
		t.Fatal("closed the last page")
	}
	if !reflect.DeepEqual(events, []string{"target_switch", "target_close"}) {
		t.Fatalf("human page events not reported: %v", events)
	}
}

// The page that passed the checks must be the page that gets the input.
func TestActionRefusedWhenAnotherPageBecameActive(t *testing.T) {
	f := newFakeDevTools(t)
	withOpeners(t, map[string]string{"p": "a"})
	f.set([2]string{"a", "http://app.test/"})
	var events []string
	m, id := startTracked(t, f, &events)

	m.ExpectTarget(id, "a")
	// A popup opens from the checked page before the click is dispatched.
	f.set([2]string{"a", "http://app.test/"}, [2]string{"p", "http://login.test/"})
	x, y := 5.0, 5.0
	if _, err := m.Click(id, "", &x, &y); !errors.Is(err, ErrTargetChanged) {
		t.Fatalf("click on a stale page: %v", err)
	}
	if _, err := m.Type(id, "secret"); !errors.Is(err, ErrTargetChanged) {
		t.Fatalf("type on a stale page: %v", err)
	}
	if _, err := m.Key(id, "Enter"); err != nil && !errors.Is(err, ErrTargetChanged) {
		t.Fatal(err)
	}
	for _, c := range f.callsOn("a") {
		if strings.HasPrefix(c, "Input.") {
			t.Fatalf("input reached the opener after the page changed: %v", f.callsOn("a"))
		}
	}
	for _, c := range f.callsOn("p") {
		if strings.HasPrefix(c, "Input.") {
			t.Fatalf("input reached the popup: %v", f.callsOn("p"))
		}
	}
	// Once the caller re-resolves, the next action targets the popup.
	m.ExpectTarget(id, "")
	if _, err := m.Click(id, "", &x, &y); err != nil {
		t.Fatalf("click after re-resolving: %v", err)
	}
	if len(f.callsOn("p")) == 0 {
		t.Fatal("action did not reach the new active page")
	}
}

func TestActionsAreSentToTheOnePinnedPage(t *testing.T) {
	f := newFakeDevTools(t)
	withOpeners(t, map[string]string{})
	f.set([2]string{"a", "http://app.test/"}, [2]string{"b", "http://app.test/b"})
	var events []string
	m, id := startTracked(t, f, &events)
	if _, err := m.Click(id, "#go", nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := f.callsOn("b"); len(got) != 0 {
		t.Fatalf("a background page got CDP commands: %v", got)
	}
	var mouse int
	for _, c := range f.callsOn("a") {
		if c == "Input.dispatchMouseEvent" {
			mouse++
		}
	}
	if mouse != 3 {
		t.Fatalf("mouse events on the pinned page: %d (%v)", mouse, f.callsOn("a"))
	}
}

// Everything but local mode must behave exactly as before.
func TestNodeWideManagerDoesNotTrackPages(t *testing.T) {
	f := newFakeDevTools(t)
	f.set([2]string{"a", "http://app.test/"}, [2]string{"p", "http://login.test/"})
	installFixedPortLauncher(t, f.port)
	m := newCobrowseSessionManager(trustedBaseDir(t), 8)
	st, err := m.StartSession("")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop(st.ID)

	status, _ := m.SessionStatus(st.ID)
	raw, _ := json.Marshal(status)
	for _, k := range []string{"targets", "active_target"} {
		if strings.Contains(string(raw), k) {
			t.Fatalf("node-wide status gained %q: %s", k, raw)
		}
	}
	if status.URL != "http://app.test/" {
		t.Fatalf("status URL follows a popup: %q", status.URL)
	}
	if _, err := m.Targets(st.ID); !errors.Is(err, ErrTargetsDisabled) {
		t.Fatalf("Targets: %v", err)
	}
	if err := m.SwitchTarget(st.ID, "p"); !errors.Is(err, ErrTargetsDisabled) {
		t.Fatalf("SwitchTarget: %v", err)
	}
	if m.TargetsEnabled() {
		t.Fatal("tracking enabled by default")
	}
	if len(f.closedIDs()) != 0 {
		t.Fatalf("node-wide manager closed pages: %v", f.closedIDs())
	}
	// Actions still go to the first page.
	x, y := 1.0, 1.0
	if _, err := m.Click(st.ID, "", &x, &y); err != nil {
		t.Fatal(err)
	}
	if len(f.callsOn("p")) != 0 {
		t.Fatalf("node-wide action reached the popup: %v", f.callsOn("p"))
	}
}
