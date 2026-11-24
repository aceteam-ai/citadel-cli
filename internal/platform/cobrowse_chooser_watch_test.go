package platform

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// chooserCDP is a fake browser for the watcher: /json lists one page,
// /json/version reports browserID, and the page socket pushes `push` once the
// watcher has sent its two enable calls.
type chooserCDP struct {
	port    int
	conns   atomic.Int32
	srv     *httptest.Server
	release chan struct{}
}

func startChooserCDP(t *testing.T, browserID string, push map[string]any) *chooserCDP {
	t.Helper()
	c := &chooserCDP{release: make(chan struct{})}
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	mux := http.NewServeMux()
	var port int
	mux.HandleFunc("/json", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `[{"type":"page","url":"about:blank","webSocketDebuggerUrl":"ws://127.0.0.1:%d/devtools/page/1"}]`, port)
	})
	mux.HandleFunc("/json/version", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"webSocketDebuggerUrl":"ws://127.0.0.1:%d/devtools/browser/%s"}`, port, browserID)
	})
	mux.HandleFunc("/devtools/page/1", func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		c.conns.Add(1)
		for i := 0; i < 2; i++ {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
		if push != nil {
			_ = conn.WriteJSON(push)
		}
		<-c.release
	})
	c.srv = httptest.NewServer(mux)
	port = c.srv.Listener.Addr().(*net.TCPAddr).Port
	c.port = port
	t.Cleanup(func() { close(c.release); c.srv.Close() })
	return c
}

func watcherSession(port int, browserID string) *cobrowseSession {
	return &cobrowseSession{
		id:    "w",
		state: SessionRunning,
		proc:  &cobrowseProc{debugPort: port, browserID: browserID, exited: make(chan struct{})},
	}
}

func runWatcherBriefly(s *cobrowseSession, port int, d time.Duration) {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { s.watchChooserOnce(port, stop); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		close(stop)
		<-done
	}
}

func openedEvent(pad int) map[string]any {
	return map[string]any{
		"method": "Page.fileChooserOpened",
		"params": map[string]any{"backendNodeId": 5, "mode": "selectSingle", "frameId": "F", "pad": strings.Repeat("x", pad)},
	}
}

// Control: a normal-sized event is recorded, so the next test's silence is
// the read limit and nothing else.
func TestChooserWatcherRecordsASmallEvent(t *testing.T) {
	c := startChooserCDP(t, "B1", openedEvent(0))
	s := watcherSession(c.port, "B1")
	runWatcherBriefly(s, c.port, 1500*time.Millisecond)
	st, _ := s.statusBase()
	if st.FileChooser == nil || st.FileChooser.BackendNodeID != 5 {
		t.Fatalf("chooser = %+v", st.FileChooser)
	}
}

// An oversized CDP message ends the connection before it is parsed. Removing
// SetReadLimit makes this event record.
func TestChooserWatcherReadLimitDropsOversizedMessage(t *testing.T) {
	c := startChooserCDP(t, "B1", openedEvent(chooserMaxMessageBytes+1024))
	s := watcherSession(c.port, "B1")
	runWatcherBriefly(s, c.port, 3*time.Second)
	if st, _ := s.statusBase(); st.FileChooser != nil {
		t.Fatalf("an oversized message was processed: %+v", st.FileChooser)
	}
}

// The watcher attaches only to its own session's browser. With a different
// browser id on the port it never opens the page socket. Disabling
// verifyCDPBrowserID makes this connect.
func TestChooserWatcherRefusesAnotherBrowserOnThePort(t *testing.T) {
	c := startChooserCDP(t, "SOMEONE-ELSE", openedEvent(0))
	s := watcherSession(c.port, "MINE")
	runWatcherBriefly(s, c.port, 1500*time.Millisecond)
	if n := c.conns.Load(); n != 0 {
		t.Fatalf("the watcher connected to another browser (%d connections)", n)
	}
	if st, _ := s.statusBase(); st.FileChooser != nil {
		t.Fatal("a chooser from another browser was recorded")
	}
	// Same server, matching id: it does connect.
	c2 := startChooserCDP(t, "MINE", nil)
	s2 := watcherSession(c2.port, "MINE")
	runWatcherBriefly(s2, c2.port, 1500*time.Millisecond)
	if c2.conns.Load() != 1 {
		t.Fatal("the watcher should attach to its own browser")
	}
}
