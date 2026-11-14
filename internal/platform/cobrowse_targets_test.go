package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestReconcileTargets(t *testing.T) {
	// First observation adopts the first page; others present then are not
	// treated as popups.
	active, stack, seen := reconcileTargets("", nil, nil, []string{"a", "b"})
	if active != "a" || len(stack) != 0 {
		t.Fatalf("first: active=%q stack=%v", active, stack)
	}
	// A popup appears: it becomes active, the opener is remembered.
	active, stack, seen = reconcileTargets(active, stack, seen, []string{"a", "b", "c"})
	if active != "c" || !reflect.DeepEqual(stack, []string{"a"}) {
		t.Fatalf("popup: active=%q stack=%v", active, stack)
	}
	// A popup opens a popup (nested sign-in step).
	active, stack, seen = reconcileTargets(active, stack, seen, []string{"a", "b", "c", "d"})
	if active != "d" || !reflect.DeepEqual(stack, []string{"a", "c"}) {
		t.Fatalf("nested: active=%q stack=%v", active, stack)
	}
	// The nested popup closes: back to its opener, not the first page.
	active, stack, seen = reconcileTargets(active, stack, seen, []string{"a", "b", "c"})
	if active != "c" || !reflect.DeepEqual(stack, []string{"a"}) {
		t.Fatalf("close nested: active=%q stack=%v", active, stack)
	}
	// The popup closes after its opener was closed too: fall to a remaining page.
	active, stack, _ = reconcileTargets(active, stack, seen, []string{"b"})
	if active != "b" || len(stack) != 0 {
		t.Fatalf("fallback: active=%q stack=%v", active, stack)
	}
}

// fakeDevTools serves /json and /json/close for a set of pages.
type fakeDevTools struct {
	mu     sync.Mutex
	pages  []map[string]string
	closed []string
	srv    *httptest.Server
	port   int
}

func newFakeDevTools(t *testing.T) *fakeDevTools {
	f := &fakeDevTools{}
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
			"webSocketDebuggerUrl": "ws://127.0.0.1:1/devtools/page/" + p[0],
		})
	}
}

func (f *fakeDevTools) closedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.closed...)
}

func TestTargetSetFollowsPopupThenOpener(t *testing.T) {
	f := newFakeDevTools(t)
	ts := &targetSet{seen: map[string]bool{}, known: map[string]string{}}
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
	// Scripted actions follow the active page.
	got, err := ts.activeTarget(ctx, f.port)
	if err != nil || got.ID != "p" {
		t.Fatalf("activeTarget: %+v %v", got, err)
	}
	f.set([2]string{"a", "http://app.test/"})
	snap, _ = ts.sync(ctx, f.port)
	if snap.Active != "a" || len(snap.Targets) != 1 {
		t.Fatalf("after popup closed: %+v", snap)
	}
}

func TestTargetSetClosesPopupRefusedByPolicy(t *testing.T) {
	f := newFakeDevTools(t)
	ts := &targetSet{seen: map[string]bool{}, known: map[string]string{}}
	var refused []string
	ts.configure("cb-1",
		func(_ context.Context, raw string) error {
			if strings.Contains(raw, "private.test") {
				return fmt.Errorf("navigation refused: private address")
			}
			return nil
		},
		func(id, raw string, err error) { refused = append(refused, id+" "+raw) })
	ctx := context.Background()

	f.set([2]string{"a", "http://app.test/"})
	if _, err := ts.sync(ctx, f.port); err != nil {
		t.Fatal(err)
	}
	f.set([2]string{"a", "http://app.test/"}, [2]string{"p", "http://private.test/x"})
	snap, _ := ts.sync(ctx, f.port)
	if snap.Active != "a" || len(snap.Targets) != 1 {
		t.Fatalf("refused popup stayed: %+v", snap)
	}
	if got := f.closedIDs(); len(got) != 1 || got[0] != "p" {
		t.Fatalf("closed = %v", got)
	}
	if len(refused) != 1 || !strings.HasPrefix(refused[0], "cb-1 ") {
		t.Fatalf("refusal not reported: %v", refused)
	}

	// A popup that opens blank and later navigates to a refused address is
	// closed when the address is seen.
	f.set([2]string{"a", "http://app.test/"}, [2]string{"q", "about:blank"})
	snap, _ = ts.sync(ctx, f.port)
	if snap.Active != "q" {
		t.Fatalf("blank popup: %+v", snap)
	}
	f.set([2]string{"a", "http://app.test/"}, [2]string{"q", "http://private.test/"})
	snap, _ = ts.sync(ctx, f.port)
	if snap.Active != "a" || len(snap.Targets) != 1 {
		t.Fatalf("navigated popup stayed: %+v", snap)
	}
}

func TestTargetSetNeverClosesLastPageAndBoundsPopups(t *testing.T) {
	f := newFakeDevTools(t)
	ts := &targetSet{seen: map[string]bool{}, known: map[string]string{}}
	ts.configure("cb-1", func(context.Context, string) error { return fmt.Errorf("refused") }, nil)
	f.set([2]string{"a", "chrome://newtab/"})
	snap, err := ts.sync(context.Background(), f.port)
	if err != nil || len(snap.Targets) != 1 || len(f.closedIDs()) != 0 {
		t.Fatalf("last page: %+v %v closed=%v", snap, err, f.closedIDs())
	}

	ts = &targetSet{seen: map[string]bool{}, known: map[string]string{}}
	f.set([2]string{"p0", "http://app.test/"})
	if _, err := ts.sync(context.Background(), f.port); err != nil {
		t.Fatal(err)
	}
	var pages [][2]string
	for i := 0; i < MaxCobrowseTargets+3; i++ {
		pages = append(pages, [2]string{fmt.Sprintf("p%d", i), "http://app.test/"})
	}
	f.set(pages...)
	snap, _ = ts.sync(context.Background(), f.port)
	if len(snap.Targets) != MaxCobrowseTargets {
		t.Fatalf("kept %d pages, want %d", len(snap.Targets), MaxCobrowseTargets)
	}
	if snap.Targets[0].ID != "p0" {
		t.Fatalf("oldest page was closed: %+v", snap.Targets)
	}
}

func TestSetActiveUsesPreviousAsOpener(t *testing.T) {
	f := newFakeDevTools(t)
	ts := &targetSet{seen: map[string]bool{}, known: map[string]string{}}
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

func TestManagerPageChoiceNeedsUserControl(t *testing.T) {
	f := newFakeDevTools(t)
	f.set([2]string{"a", "http://app.test/"}, [2]string{"b", "http://app.test/b"})
	installFixedPortLauncher(t, f.port)
	m := newCobrowseSessionManager(trustedBaseDir(t), 8)
	var refused []string
	m.SetTargetPolicy(func(context.Context, string) error { return nil },
		func(id, raw string, err error) { refused = append(refused, raw) })

	st, err := m.StartSession("")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop(st.ID)
	if err := m.SwitchTarget(st.ID, "b"); err == nil || !strings.Contains(err.Error(), "not taken control") {
		t.Fatalf("agent-driven switch: %v", err)
	}
	if err := m.CloseTarget(st.ID, "b"); err == nil {
		t.Fatal("agent-driven close allowed")
	}
	if _, err := m.Handoff(st.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.SwitchTarget(st.ID, "b"); err != nil {
		t.Fatalf("user switch: %v", err)
	}
	snap, err := m.Targets(st.ID)
	if err != nil || snap.Active != "b" {
		t.Fatalf("targets: %+v %v", snap, err)
	}
	status, _ := m.SessionStatus(st.ID)
	if status.ActiveTarget != "b" || len(status.Targets) != 2 || status.URL != "http://app.test/b" {
		t.Fatalf("status: %+v", status)
	}
	if err := m.CloseTarget(st.ID, "b"); err != nil {
		t.Fatalf("user close: %v", err)
	}
	if got := f.closedIDs(); len(got) != 1 || got[0] != "b" {
		t.Fatalf("closed = %v", got)
	}
	if err := m.CloseTarget(st.ID, "a"); err == nil {
		t.Fatal("closed the last page")
	}
}
