package cobrowsestream

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func newMountedHarness(t *testing.T, sess Session, f *fakeChrome, allowed string) *httptest.Server {
	t.Helper()
	withFakeCDP(t, f)
	h := NewStreamHandler(Config{}, sess, func(r *http.Request) bool { return r.Header.Get("Origin") == allowed })
	h.SetSilent()
	ts := httptest.NewServer(h)
	t.Cleanup(func() {
		deadline := time.Now().Add(10 * time.Second)
		for h.ActiveConnections() != 0 && time.Now().Before(deadline) {
			time.Sleep(2 * time.Millisecond)
		}
	})
	t.Cleanup(ts.Close)
	return ts
}

func TestMountedStreamRefusesForeignOrigin(t *testing.T) {
	f := newFakeChrome()
	defer f.close()
	sess := &fakeSession{port: 9222, attachable: true}
	ts := newMountedHarness(t, sess, f, "http://127.0.0.1:1")
	u := "ws" + strings.TrimPrefix(ts.URL, "http") + "/?id=cb-1"
	for _, origin := range []string{"", "http://evil.test", "null"} {
		hdr := http.Header{}
		if origin != "" {
			hdr.Set("Origin", origin)
		}
		conn, resp, err := websocket.DefaultDialer.Dial(u, hdr)
		if err == nil {
			conn.Close()
			t.Fatalf("origin %q: upgrade must be refused", origin)
		}
		if resp == nil || resp.StatusCode != http.StatusForbidden {
			t.Fatalf("origin %q: want 403, got %v", origin, resp)
		}
	}
	if a, _ := sess.counts(); a != 0 {
		t.Fatal("a refused viewer must never attach")
	}
}

// TestMountedStreamForwardsInputOnlyAfterHandoff is the S4 acceptance item
// "input is forwarded only after handoff" through the mountable handler.
func TestMountedStreamForwardsInputOnlyAfterHandoff(t *testing.T) {
	f := newFakeChrome()
	defer f.close()
	sess := &fakeSession{port: 9222, attachable: true}
	ts := newMountedHarness(t, sess, f, "http://127.0.0.1:1")
	hdr := http.Header{"Origin": []string{"http://127.0.0.1:1"}}
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+"/?id=cb-1", hdr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _, _ = conn.ReadMessage()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _, _ = conn.ReadMessage()

	in, _ := json.Marshal(InputMessage{Type: InputTypeKey, KeyEvent: "keyDown", Key: "a", Text: "a"})
	_ = conn.WriteMessage(websocket.TextMessage, in)
	sess.waitInputChecks(t, 1)
	f.mu.Lock()
	n := len(f.keys)
	f.mu.Unlock()
	if n != 0 {
		t.Fatalf("input reached the browser before handoff (%d keys)", n)
	}
	sess.setHumanControl(true)
	_ = conn.WriteMessage(websocket.TextMessage, in)
	deadline := time.Now().Add(3 * time.Second)
	for {
		f.mu.Lock()
		n = len(f.keys)
		f.mu.Unlock()
		if n == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("input after handoff never reached the browser")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
