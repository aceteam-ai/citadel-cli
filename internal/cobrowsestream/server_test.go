package cobrowsestream

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// fakeSession is an in-memory Session for handler tests.
type fakeSession struct {
	mu         sync.Mutex
	port       int
	attachable bool
	attached   int
	detached   int
}

func (s *fakeSession) AttachTarget(id string) (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.port, s.attachable
}

func (s *fakeSession) MarkAttached(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attached++
	return true
}

func (s *fakeSession) MarkDetached(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.detached++
	return true
}

func (s *fakeSession) counts() (attached, detached int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attached, s.detached
}

func newHarness(t *testing.T, sess Session, f *fakeChrome) *httptest.Server {
	t.Helper()
	withFakeCDP(t, f) // registers the cdpTargetURL restore cleanup FIRST
	srv := NewServer(Config{}, sess)
	srv.SetSilent()
	mux := http.NewServeMux()
	mux.HandleFunc(StreamPath, srv.handleStream)
	ts := httptest.NewServer(mux)

	// Drain in-flight stream handlers BEFORE withFakeCDP's cdpTargetURL restore
	// runs. handleStream reads the cdpTargetURL package var (via dialCDP) from the
	// httptest server's hijacked-connection goroutine, and httptest.Server.Close()
	// does NOT join those goroutines -- nothing else does either -- so without an
	// explicit barrier the restore write races that read (citadel #989). It is a
	// test-only race: in production cdpTargetURL is set once at init, never mutated.
	//
	// t.Cleanup runs LIFO (last registered runs first). Registering this drain
	// AFTER withFakeCDP but BEFORE ts.Close makes the three cleanups execute in
	// this order: ts.Close (drop the listener) -> this drain -> withFakeCDP
	// restore. activeConns is incremented before the dialCDP read and decremented
	// (via an atomic, which carries a happens-before) when the handler returns, so
	// once it reaches 0 every dialCDP read has completed and the restore is safe.
	// Get this ordering wrong -- register before withFakeCDP, or after ts.Close --
	// and the race is NOT actually closed.
	//
	// Note: ts.Close() does NOT close hijacked WebSocket conns (http.Server drops
	// hijacked conns from its tracked set), so it alone does not drive handlers to
	// return. What actually does is each test's own viewer-conn close, which runs
	// as a test-body defer -- i.e. before any t.Cleanup. A server test that leaves
	// its viewer conn open will trip the timeout below.
	t.Cleanup(func() {
		deadline := time.Now().Add(10 * time.Second)
		for srv.ActiveConnections() != 0 {
			if time.Now().After(deadline) {
				t.Errorf("stream handlers did not drain before cleanup: %d still active", srv.ActiveConnections())
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	})
	t.Cleanup(ts.Close)
	return ts
}

func dialViewer(t *testing.T, ts *httptest.Server, id string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	u := "ws" + strings.TrimPrefix(ts.URL, "http") + StreamPath
	if id != "" {
		u += "?id=" + id
	}
	return websocket.DefaultDialer.Dial(u, nil)
}

func waitDetached(t *testing.T, s *fakeSession, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, d := s.counts(); d >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, d := s.counts()
	t.Fatalf("detached = %d, want >= %d", d, want)
}

// TestViewerReceivesInitThenFrameAndMarksAttached is the happy path: a viewer
// gets the TEXT init, then a BINARY JPEG frame; the session flips attached on
// connect and detached on disconnect.
func TestViewerReceivesInitThenFrameAndMarksAttached(t *testing.T) {
	f := newFakeChrome()
	defer f.close()
	sess := &fakeSession{port: 9222, attachable: true}
	ts := newHarness(t, sess, f)

	conn, _, err := dialViewer(t, ts, "cb-1")
	if err != nil {
		t.Fatalf("dial viewer: %v", err)
	}

	// First frame: TEXT init.
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	mt, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read init: %v", err)
	}
	if mt != websocket.TextMessage {
		t.Fatalf("first frame type = %d, want TEXT", mt)
	}
	var init InitMessage
	if err := json.Unmarshal(data, &init); err != nil || init.Type != "init" {
		t.Fatalf("init = %q (%v)", data, err)
	}

	// Then a BINARY JPEG frame.
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	mt, bin, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	if mt != websocket.BinaryMessage {
		t.Fatalf("frame type = %d, want BINARY", mt)
	}
	if string(bin) != string(f.jpeg) {
		t.Fatalf("frame bytes mismatch")
	}

	if a, _ := sess.counts(); a != 1 {
		t.Errorf("attached = %d, want 1", a)
	}

	_ = conn.Close()
	waitDetached(t, sess, 1)
}

// TestUnknownSessionRefused: an unattachable session is rejected and never
// touches attach state.
func TestUnknownSessionRefused(t *testing.T) {
	f := newFakeChrome()
	defer f.close()
	sess := &fakeSession{port: 9222, attachable: false}
	ts := newHarness(t, sess, f)

	conn, _, err := dialViewer(t, ts, "cb-missing")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _, rerr := conn.ReadMessage()
	if rerr == nil {
		t.Fatal("expected close for unattachable session")
	}
	if a, _ := sess.counts(); a != 0 {
		t.Errorf("attached = %d, want 0 for refused session", a)
	}
}

// TestMissingIDRefused: no id query param is rejected.
func TestMissingIDRefused(t *testing.T) {
	f := newFakeChrome()
	defer f.close()
	sess := &fakeSession{port: 9222, attachable: true}
	ts := newHarness(t, sess, f)

	conn, _, err := dialViewer(t, ts, "")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, _, rerr := conn.ReadMessage(); rerr == nil {
		t.Fatal("expected close for missing id")
	}
	if a, _ := sess.counts(); a != 0 {
		t.Errorf("attached = %d, want 0", a)
	}
}

// TestSecondViewerRefused: only one viewer may stream a session; the second is
// refused and does NOT detach the first.
func TestSecondViewerRefused(t *testing.T) {
	f := newFakeChrome()
	defer f.close()
	sess := &fakeSession{port: 9222, attachable: true}
	ts := newHarness(t, sess, f)

	c1, _, err := dialViewer(t, ts, "cb-1")
	if err != nil {
		t.Fatalf("dial viewer1: %v", err)
	}
	defer c1.Close()
	// Drain viewer1's init so we know its handler has claimed the session.
	_ = c1.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, _, err := c1.ReadMessage(); err != nil {
		t.Fatalf("viewer1 init: %v", err)
	}

	c2, _, err := dialViewer(t, ts, "cb-1")
	if err != nil {
		t.Fatalf("dial viewer2: %v", err)
	}
	defer c2.Close()
	_ = c2.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, _, rerr := c2.ReadMessage(); rerr == nil {
		t.Fatal("expected viewer2 to be refused")
	}

	// Viewer2's refusal must not have detached viewer1's session.
	if _, d := sess.counts(); d != 0 {
		t.Errorf("detached = %d after refused second viewer, want 0", d)
	}
	if a, _ := sess.counts(); a != 1 {
		t.Errorf("attached = %d, want 1 (only viewer1)", a)
	}
}

// TestViewerInputForwardedToCDP: a key input from the viewer reaches CDP.
func TestViewerInputForwardedToCDP(t *testing.T) {
	f := newFakeChrome()
	defer f.close()
	sess := &fakeSession{port: 9222, attachable: true}
	ts := newHarness(t, sess, f)

	conn, _, err := dialViewer(t, ts, "cb-1")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	// Read init + at least one frame so the CDP attach + screencast are live.
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _, _ = conn.ReadMessage() // init
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _, _ = conn.ReadMessage() // frame

	in, _ := json.Marshal(InputMessage{Type: InputTypeKey, KeyEvent: "keyDown", Key: "a", Text: "a"})
	if err := conn.WriteMessage(websocket.TextMessage, in); err != nil {
		t.Fatalf("write input: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		f.mu.Lock()
		n := len(f.keys)
		f.mu.Unlock()
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("key input never reached CDP")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestTeardownOnBrowserStop: the browser (session) going away tears the viewer
// down and detaches the session -- the "teardown on session stop" acceptance.
func TestTeardownOnBrowserStop(t *testing.T) {
	f := newFakeChrome()
	defer f.close()
	sess := &fakeSession{port: 9222, attachable: true}
	ts := newHarness(t, sess, f)

	conn, _, err := dialViewer(t, ts, "cb-1")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, _, err := conn.ReadMessage(); err != nil { // init
		t.Fatalf("init: %v", err)
	}
	// Read one BINARY frame so we KNOW the CDP attach + screencast are live
	// before we drop the browser (otherwise killConns can race ahead of the
	// handler's CDP dial and close nothing).
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if mt, _, err := conn.ReadMessage(); err != nil || mt != websocket.BinaryMessage {
		t.Fatalf("frame: mt=%d err=%v", mt, err)
	}

	// Simulate session stop: the browser's CDP socket drops.
	f.killConns()

	// The viewer socket is closed by the handler; its next read errors.
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		if _, _, rerr := conn.ReadMessage(); rerr != nil {
			break
		}
	}
	waitDetached(t, sess, 1)
}

// TestStartStopLifecycle exercises the real localhost listener + graceful stop.
func TestStartStopLifecycle(t *testing.T) {
	f := newFakeChrome()
	defer f.close()
	sess := &fakeSession{port: 9222, attachable: true}
	srv := NewServer(Config{Port: 0}, sess) // ephemeral
	srv.SetSilent()
	if err := srv.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	if !srv.IsRunning() {
		t.Fatal("not running after Start")
	}
	if srv.BoundAddr() == "" {
		t.Fatal("no bound addr")
	}

	// Health endpoint answers.
	resp, err := http.Get("http://" + srv.BoundAddr() + HealthPath)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d", resp.StatusCode)
	}

	srv.Stop()
	if srv.IsRunning() {
		t.Fatal("still running after Stop")
	}
	// Double stop is safe.
	srv.Stop()
}
