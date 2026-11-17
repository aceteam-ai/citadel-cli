package platform

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// humanAttachEnv is a node-wide session on the fake CDP page of the set_file_input
// tests, with a pending chooser for backend node 7 in frame R at portalURL, the
// viewer in control and Files permitted.
type humanAttachEnv struct {
	m     *CobrowseSessionManager
	st    CobrowseSessionStatus
	f     *fakeCDPServer
	ff    *fileInputFake
	s     *cobrowseSession
	files atomic.Bool
	data  []byte
}

func newHumanAttachEnv(t *testing.T) *humanAttachEnv {
	t.Helper()
	m, st, f := startSessionOnFakeCDP(t)
	e := &humanAttachEnv{m: m, st: st, f: f, data: []byte("PRIVATE-STAGED-BYTES-%PDF")}
	e.ff = &fileInputFake{frames: []fakeFrame{portalFrame}}
	e.ff.install(f)
	e.files.Store(true)
	m.SetFilesAllowed(e.files.Load)
	if _, err := m.StageFile(st.ID, "a.pdf", e.data); err != nil {
		t.Fatal(err)
	}
	s, err := m.sessionForAction(st.ID)
	if err != nil {
		t.Fatal(err)
	}
	e.s = s
	e.openChooser(7)
	if _, err := m.Handoff(st.ID); err != nil {
		t.Fatal(err)
	}
	if !m.MarkAttached(st.ID) {
		t.Fatal("MarkAttached")
	}
	if !m.ViewerHasControl(st.ID) {
		t.Fatal("viewer should have control")
	}
	return e
}

// openChooser records a pending chooser the way the watcher does, with the
// frame URL the browser reported.
func (e *humanAttachEnv) openChooser(node int) {
	p := e.s.handleChooserEvent("Page.fileChooserOpened", map[string]any{
		"backendNodeId": float64(node), "mode": "selectSingle", "frameId": "R",
	})
	e.s.setChooserFrameURL(p, portalURL)
}

func (e *humanAttachEnv) sets() int { return len(e.f.callsFor("DOM.setFileInputFiles")) }

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(log.Writer()) })
	return &buf
}

func TestHumanAttachFillsPendingChooserAndAuditsNameSizeHash(t *testing.T) {
	e := newHumanAttachEnv(t)
	buf := captureLog(t)
	got, err := e.m.HumanAttachStagedFile(e.st.ID, 7, "a.pdf")
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	sum := sha256.Sum256(e.data)
	if got.Name != "a.pdf" || got.Size != int64(len(e.data)) || got.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("result %+v", got)
	}
	calls := e.f.callsFor("DOM.setFileInputFiles")
	if len(calls) != 1 || calls[0].Params["objectId"] != "obj1" {
		t.Fatalf("setFileInputFiles calls = %+v", calls)
	}
	if w := e.f.callsFor("Page.createIsolatedWorld"); len(w) != 1 || w[0].Params["frameId"] != "R" {
		t.Fatalf("isolated world calls = %+v", w)
	}
	noPageWorldFenceReads(t, e.f)
	if s, _ := e.m.SessionStatus(e.st.ID); s.FileChooser != nil {
		t.Fatalf("the chooser should clear after the attach: %+v", s.FileChooser)
	}
	line := buf.String()
	for _, want := range []string{"audit file_attach", "actor=human", `name="a.pdf"`, fmt.Sprintf("size=%d", len(e.data)), "sha256=" + got.SHA256} {
		if !strings.Contains(line, want) {
			t.Errorf("audit line lacks %q: %s", want, line)
		}
	}
	if strings.Contains(line, "PRIVATE-STAGED-BYTES") || strings.Contains(line, e.st.Profile) {
		t.Fatalf("audit line carries bytes or a path: %s", line)
	}
}

func TestHumanAttachRefusalsDispatchNothing(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(e *humanAttachEnv)
		node   int
		staged string
		want   error
		reason string
	}{
		{"no pending chooser", func(e *humanAttachEnv) { e.s.clearChooser() }, 7, "a.pdf", ErrNoPendingChooser, "no_pending_chooser"},
		{"mismatched node", nil, 8, "a.pdf", ErrChooserMismatch, "chooser_mismatch"},
		{"zero node", nil, 0, "a.pdf", ErrChooserMismatch, "chooser_mismatch"},
		{"viewer without control", func(e *humanAttachEnv) { _, _ = e.m.Resume(e.st.ID) }, 7, "a.pdf", ErrViewerNoControl, "no_control"},
		{"viewer not attached", func(e *humanAttachEnv) { e.m.MarkDetached(e.st.ID) }, 7, "a.pdf", ErrViewerNoControl, "no_control"},
		{"files off", func(e *humanAttachEnv) { e.files.Store(false) }, 7, "a.pdf", ErrAttachFilesDisabled, "files_disabled"},
		{"traversal name", nil, 7, "../a.pdf", ErrStagedNameInvalid, "invalid_staged_name"},
		{"empty name", nil, 7, "", ErrStagedNameInvalid, "invalid_staged_name"},
		{"absolute name", nil, 7, "/etc/passwd", ErrStagedNameInvalid, "invalid_staged_name"},
		{"unknown name", nil, 7, "nope.pdf", ErrStagedPathRefused, "staged_file_unavailable"},
		{"chooser frame navigated", func(e *humanAttachEnv) {
			e.ff.mu.Lock()
			e.ff.frames = []fakeFrame{{ID: "R", URL: "https://evil.example/"}}
			e.ff.mu.Unlock()
		}, 7, "a.pdf", ErrTargetChanged, "page_changed"},
		{"chooser frame fragment changed", func(e *humanAttachEnv) {
			e.ff.mu.Lock()
			e.ff.frames = []fakeFrame{{ID: "R", URL: "https://portal.example/file?x=1", Fragment: "#other"}}
			e.ff.mu.Unlock()
		}, 7, "a.pdf", ErrTargetChanged, "page_changed"},
		{"chooser frame URL unknown", func(e *humanAttachEnv) { e.openChooserNoURL(7) }, 7, "a.pdf", errAttachChooserMissing, "page_changed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newHumanAttachEnv(t)
			if c.mutate != nil {
				c.mutate(e)
			}
			_, err := e.m.HumanAttachStagedFile(e.st.ID, c.node, c.staged)
			if !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
			if n := e.sets(); n != 0 {
				t.Fatalf("%d setFileInputFiles calls on a refusal", n)
			}
			if r := AttachFailureReason(err); r != c.reason {
				t.Fatalf("reason = %q, want %q", r, c.reason)
			}
			if err != nil && (strings.Contains(err.Error(), e.st.Profile) || strings.Contains(AttachFailureReason(err), "/")) {
				t.Fatalf("error carries a path: %v", err)
			}
		})
	}
}

func (e *humanAttachEnv) openChooserNoURL(node int) {
	e.s.handleChooserEvent("Page.fileChooserOpened", map[string]any{
		"backendNodeId": float64(node), "mode": "selectSingle", "frameId": "R",
	})
}

// Control handed back after every up-front check, while the final frame URL is
// being re-read: nothing is dispatched.
func TestHumanAttachHandBackBetweenCheckAndDispatchRefuses(t *testing.T) {
	e := newHumanAttachEnv(t)
	// Reads: 1 the early frame check, 2 the re-read right before the dispatch.
	e.ff.onFrameTree = func(n int) {
		if n >= 2 {
			_, _ = e.m.Resume(e.st.ID)
		}
	}
	_, err := e.m.HumanAttachStagedFile(e.st.ID, 7, "a.pdf")
	if !errors.Is(err, ErrViewerNoControl) {
		t.Fatalf("err = %v, want ErrViewerNoControl", err)
	}
	if n := e.sets(); n != 0 {
		t.Fatalf("%d dispatches after control was handed back", n)
	}
	if s, _ := e.m.SessionStatus(e.st.ID); s.FileChooser == nil {
		t.Fatal("a refused attach must leave the chooser pending")
	}
}

// Files switched off between the up-front check and the dispatch also stops it.
func TestHumanAttachFilesRevokedBeforeDispatchRefuses(t *testing.T) {
	e := newHumanAttachEnv(t)
	e.ff.onFrameTree = func(n int) {
		if n >= 2 {
			e.files.Store(false)
		}
	}
	if _, err := e.m.HumanAttachStagedFile(e.st.ID, 7, "a.pdf"); !errors.Is(err, ErrAttachFilesDisabled) {
		t.Fatalf("err = %v", err)
	}
	if n := e.sets(); n != 0 {
		t.Fatalf("%d dispatches after Files was revoked", n)
	}
}

// A main-frame navigation clears the chooser while the attach is looking the
// node up: the attach refuses.
func TestHumanAttachChooserClearedDuringLookupRefuses(t *testing.T) {
	e := newHumanAttachEnv(t)
	// The early frame read is call 1; the clear lands during it, before the
	// post-lookup fence.
	e.ff.onFrameTree = func(n int) {
		if n == 1 {
			e.s.clearChooser()
		}
	}
	if _, err := e.m.HumanAttachStagedFile(e.st.ID, 7, "a.pdf"); !errors.Is(err, ErrTargetChanged) {
		t.Fatalf("err = %v", err)
	}
	if n := e.sets(); n != 0 {
		t.Fatalf("%d dispatches", n)
	}
}

// Local mode has no Files permission wiring and tracks pages: it fails closed.
func TestHumanAttachLocalModeFailsClosed(t *testing.T) {
	f := startFakeCDPServer(t)
	installFixedPortLauncher(t, f.port)
	m := newCobrowseSessionManager(trustedBaseDir(t), 8)
	m.EnableTargetTracking(nil, nil)
	if m.HumanFileAttachSupported() {
		t.Fatal("a tracking manager must not offer file attach")
	}
	st, err := m.StartSession("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.StopAll() })
	if _, err := m.HumanAttachStagedFile(st.ID, 7, "a.pdf"); !errors.Is(err, ErrAttachFilesDisabled) {
		t.Fatalf("no Files wiring: err = %v", err)
	}
	if _, ok := m.HumanFileAttachOffer(st.ID); ok {
		t.Fatal("no offer without Files wiring")
	}
	// Even if something wired Files on, a tracking session has no chooser
	// watcher and is refused.
	m.SetFilesAllowed(func() bool { return true })
	if m.HumanFileAttachSupported() {
		t.Fatal("a tracking manager must not offer file attach even with Files wired")
	}
	if _, err := m.HumanAttachStagedFile(st.ID, 7, "a.pdf"); !errors.Is(err, errAttachNotApplicable) {
		t.Fatalf("a tracking session must refuse as not applicable: %v", err)
	}
	if n := len(f.callsFor("DOM.setFileInputFiles")); n != 0 {
		t.Fatalf("%d dispatches", n)
	}
}

// A manager nobody wired with a Files source denies.
func TestHumanAttachUnwiredManagerDenies(t *testing.T) {
	m, st := startFakeSession(t)
	if m.HumanFileAttachSupported() {
		t.Fatal("unwired manager must not support attach")
	}
	if _, err := m.HumanAttachStagedFile(st.ID, 7, "a.pdf"); !errors.Is(err, ErrAttachFilesDisabled) {
		t.Fatalf("err = %v", err)
	}
}

func TestHumanAttachOfferNamesAndSizesOnlyAndNeedsControl(t *testing.T) {
	e := newHumanAttachEnv(t)
	if _, err := e.m.StageFile(e.st.ID, "b.txt", []byte("bb")); err != nil {
		t.Fatal(err)
	}
	o, ok := e.m.HumanFileAttachOffer(e.st.ID)
	if !ok || o.BackendNodeID != 7 || o.Multiple || len(o.Staged) != 2 || o.Staged[0].Name != "a.pdf" || o.Staged[0].Size != int64(len(e.data)) || o.Staged[1].Name != "b.txt" {
		t.Fatalf("offer = %+v ok=%v", o, ok)
	}
	e.files.Store(false)
	if _, ok := e.m.HumanFileAttachOffer(e.st.ID); ok {
		t.Fatal("no offer with Files off")
	}
	e.files.Store(true)
	_, _ = e.m.Resume(e.st.ID)
	if _, ok := e.m.HumanFileAttachOffer(e.st.ID); ok {
		t.Fatal("no offer without control")
	}
}

// The watcher asks the browser for the chooser frame's URL (Page.getFrameTree)
// when a chooser opens, and records it.
func TestChooserWatcherRecordsFrameURLFromBrowser(t *testing.T) {
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	mux := http.NewServeMux()
	var port int
	mux.HandleFunc("/json", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `[{"type":"page","url":"about:blank","webSocketDebuggerUrl":"ws://127.0.0.1:%d/devtools/page/1"}]`, port)
	})
	mux.HandleFunc("/json/version", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"webSocketDebuggerUrl":"ws://127.0.0.1:%d/devtools/browser/bid"}`, port)
	})
	mux.HandleFunc("/devtools/page/1", func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for i := 0; i < 2; i++ {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
		_ = conn.WriteJSON(map[string]any{"method": "Page.fileChooserOpened", "params": map[string]any{"backendNodeId": 7, "mode": "selectSingle", "frameId": "R"}})
		var req struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		if err := conn.ReadJSON(&req); err != nil || req.Method != "Page.getFrameTree" {
			return
		}
		_ = conn.WriteJSON(map[string]any{"id": req.ID, "result": frameTreeResult([]fakeFrame{portalFrame})})
		time.Sleep(2 * time.Second)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	port = srv.Listener.Addr().(*net.TCPAddr).Port
	s := watcherSession(port, "bid")
	runWatcherBriefly(s, port, 700*time.Millisecond)
	s.mu.Lock()
	c := s.chooser
	var url string
	if c != nil {
		url = c.FrameURL
	}
	s.mu.Unlock()
	if c == nil || url != portalURL {
		t.Fatalf("chooser = %+v, frame URL %q, want %q", c, url, portalURL)
	}
}

// A frame URL that arrives for a chooser that has since been replaced or
// cleared is dropped.
func TestSetChooserFrameURLIgnoresStaleChooser(t *testing.T) {
	s := &cobrowseSession{}
	old := s.handleChooserEvent("Page.fileChooserOpened", map[string]any{"backendNodeId": float64(1), "frameId": "R"})
	cur := s.handleChooserEvent("Page.fileChooserOpened", map[string]any{"backendNodeId": float64(2), "frameId": "R"})
	s.setChooserFrameURL(old, "https://old.example/")
	if old.FrameURL != "" || cur.FrameURL != "" {
		t.Fatal("a stale chooser must not take a frame URL")
	}
	s.setChooserFrameURL(cur, "https://cur.example/")
	if cur.FrameURL != "https://cur.example/" {
		t.Fatal("the pending chooser must take its frame URL")
	}
	s.clearChooser()
	s.setChooserFrameURL(cur, "https://later.example/")
	if cur.FrameURL != "https://cur.example/" {
		t.Fatal("a cleared chooser must not change")
	}
}
