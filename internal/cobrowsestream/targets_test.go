package cobrowsestream

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// fakeTargetSession is a TargetSession over two fake pages: an opener "a" and
// a popup "b".
type fakeTargetSession struct {
	*fakeSession
	pages map[string]*fakeChrome
	open  []string // ids in browser order

	active   string
	switched []string
	closed   []string
}

func (s *fakeTargetSession) snapshot() TargetSnapshot {
	snap := TargetSnapshot{Active: s.active, ActiveWebSocketURL: s.pages[s.active].wsURL()}
	for _, id := range s.open {
		snap.Targets = append(snap.Targets, TargetInfo{ID: id, URL: "http://page.test/" + id, Active: id == s.active})
	}
	return snap
}

func (s *fakeTargetSession) Targets(id string) (TargetSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshot(), nil
}

func (s *fakeTargetSession) SwitchTarget(id, target string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.switched = append(s.switched, target)
	s.active = target
	return nil
}

func (s *fakeTargetSession) CloseTarget(id, target string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = append(s.closed, target)
	return nil
}

func (s *fakeTargetSession) set(active string, open ...string) {
	s.mu.Lock()
	s.active, s.open = active, open
	s.mu.Unlock()
}

func readTargets(t *testing.T, conn *websocket.Conn, wantActive string) TargetsMessage {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		mt, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if mt != websocket.TextMessage {
			continue
		}
		var m TargetsMessage
		if json.Unmarshal(data, &m) == nil && m.Type == TargetsType && m.Active == wantActive {
			return m
		}
	}
	t.Fatalf("no targets message with active=%q", wantActive)
	return TargetsMessage{}
}

func waitConns(t *testing.T, f *fakeChrome, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		n := len(f.conns)
		f.mu.Unlock()
		if n >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("fake page never saw %d connections", want)
}

func TestStreamFollowsPopupAndReturnsToOpener(t *testing.T) {
	a, b := newFakeChrome(), newFakeChrome()
	defer a.close()
	defer b.close()
	sess := &fakeTargetSession{fakeSession: &fakeSession{port: 9222, attachable: true},
		pages: map[string]*fakeChrome{"a": a, "b": b}}
	sess.set("a", "a")
	ts := newHarness(t, sess, a)

	conn, _, err := dialViewer(t, ts, "cb-1")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	first := readTargets(t, conn, "a")
	if len(first.Targets) != 1 {
		t.Fatalf("first targets = %+v", first)
	}
	waitConns(t, a, 1)

	// A popup opens and becomes active: the viewer is told and the screencast
	// moves to the popup.
	sess.set("b", "a", "b")
	m := readTargets(t, conn, "b")
	if len(m.Targets) != 2 || !m.Targets[1].Active {
		t.Fatalf("popup targets = %+v", m)
	}
	waitConns(t, b, 1)

	// The popup closes itself: back to the opener.
	sess.set("a", "a")
	readTargets(t, conn, "a")
	waitConns(t, a, 2)
}

func TestViewerTargetSwitchNeedsControl(t *testing.T) {
	a, b := newFakeChrome(), newFakeChrome()
	defer a.close()
	defer b.close()
	sess := &fakeTargetSession{fakeSession: &fakeSession{port: 9222, attachable: true},
		pages: map[string]*fakeChrome{"a": a, "b": b}}
	sess.set("a", "a", "b")
	ts := newHarness(t, sess, a)

	conn, _, err := dialViewer(t, ts, "cb-1")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	readTargets(t, conn, "a")

	in, _ := json.Marshal(InputMessage{Type: InputTypeTargetSwitch, Target: "b"})
	if err := conn.WriteMessage(websocket.TextMessage, in); err != nil {
		t.Fatal(err)
	}
	sess.waitInputChecks(t, 1)
	sess.mu.Lock()
	n := len(sess.switched)
	sess.mu.Unlock()
	if n != 0 {
		t.Fatal("an observing viewer switched pages")
	}

	sess.setHumanControl(true)
	if err := conn.WriteMessage(websocket.TextMessage, in); err != nil {
		t.Fatal(err)
	}
	readTargets(t, conn, "b")
	waitConns(t, b, 1)

	cl, _ := json.Marshal(InputMessage{Type: InputTypeTargetClose, Target: "b"})
	if err := conn.WriteMessage(websocket.TextMessage, cl); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		sess.mu.Lock()
		c := len(sess.closed)
		sess.mu.Unlock()
		if c == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("close request never reached the session")
}

func TestParseTargetMessages(t *testing.T) {
	for _, tc := range []struct {
		in string
		ok bool
	}{
		{`{"type":"target_switch","target":"abc"}`, true},
		{`{"type":"target_close","target":"abc"}`, true},
		{`{"type":"target_switch"}`, false},
		{`{"type":"target_close","target":"` + string(make([]byte, 200)) + `"}`, false},
	} {
		if _, ok := parseInputMessage([]byte(tc.in)); ok != tc.ok {
			t.Errorf("%.40s: ok=%v want %v", tc.in, ok, tc.ok)
		}
	}
}

// A popup that closes itself drops its CDP socket while the browser still lists
// it for a moment. The viewer must stay connected and return to the opener.
func TestStreamSurvivesPopupClosingItself(t *testing.T) {
	a, b := newFakeChrome(), newFakeChrome()
	defer a.close()
	defer b.close()
	sess := &fakeTargetSession{fakeSession: &fakeSession{port: 9222, attachable: true},
		pages: map[string]*fakeChrome{"a": a, "b": b}}
	sess.set("a", "a")
	ts := newHarness(t, sess, a)

	conn, _, err := dialViewer(t, ts, "cb-1")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	readTargets(t, conn, "a")
	sess.set("b", "a", "b")
	readTargets(t, conn, "b")
	waitConns(t, b, 1)

	b.killConns()
	time.AfterFunc(400*time.Millisecond, func() { sess.set("a", "a") })
	readTargets(t, conn, "a")
	waitConns(t, a, 2)

	// Still connected: a later page change still reaches the viewer.
	sess.set("b", "a", "b")
	readTargets(t, conn, "b")
}
