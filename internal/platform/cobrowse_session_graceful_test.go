package platform

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// fakeCDPBrowser serves /json/version and a browser-level WebSocket. A
// Browser.close closes exited, as a real browser exiting would.
type fakeCDPBrowser struct {
	srv    *httptest.Server
	port   int
	id     string
	exited chan struct{}
	// hang makes the fake ignore Browser.close (no reply, no exit).
	hang bool

	mu     sync.Mutex
	events []string
}

func newFakeCDPBrowser(t *testing.T, id string) *fakeCDPBrowser {
	t.Helper()
	f := &fakeCDPBrowser{id: id, exited: make(chan struct{})}
	up := websocket.Upgrader{}
	var once sync.Once
	mux := http.NewServeMux()
	mux.HandleFunc("/json/version", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"webSocketDebuggerUrl":"ws://127.0.0.1:%d/devtools/browser/%s"}`, f.port, f.id)
	})
	mux.HandleFunc("/devtools/browser/", func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		var msg map[string]any
		if c.ReadJSON(&msg) != nil {
			return
		}
		f.record(fmt.Sprint(msg["method"]))
		if f.hang {
			<-r.Context().Done()
			return
		}
		if msg["method"] == "Browser.close" {
			_ = c.WriteJSON(map[string]any{"id": msg["id"], "result": map[string]any{}})
			once.Do(func() { close(f.exited) })
		}
	})
	f.srv = httptest.NewServer(mux)
	f.port = f.srv.Listener.Addr().(*net.TCPAddr).Port
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeCDPBrowser) record(e string) {
	f.mu.Lock()
	f.events = append(f.events, e)
	f.mu.Unlock()
}

func (f *fakeCDPBrowser) log() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.events, ",")
}

func installFakeCDPLauncher(t *testing.T, f *fakeCDPBrowser) {
	t.Helper()
	prev := launchCobrowseProc
	launchCobrowseProc = func(profileDir, startURL string, _ LaunchOptions) (*cobrowseProc, error) {
		return &cobrowseProc{
			debugPort: f.port,
			browserID: f.id,
			exited:    f.exited,
			stop: func() error {
				f.record("kill")
				return nil
			},
		}, nil
	}
	t.Cleanup(func() { launchCobrowseProc = prev })
}

// TestPersistentProfile_GracefulCloseBeforeKill: a persistent session's
// browser is asked to close (so it flushes cookies) before it is killed and
// before the profile is sealed. Mutation check: dropping the graceful close
// leaves only "kill" in the log.
func TestPersistentProfile_GracefulCloseBeforeKill(t *testing.T) {
	f := newFakeCDPBrowser(t, "fake-browser-id")
	installFakeCDPLauncher(t, f)
	m := newCobrowseSessionManager(trustedBaseDir(t), 2)
	p := &fakeProfile{}
	st, err := m.StartSessionWithProfile("", p)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := m.Stop(st.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if got := f.log(); got != "Browser.close,kill" {
		t.Fatalf("teardown order = %q, want Browser.close,kill", got)
	}
	if _, per, c := p.counts(); per != 1 || c != 1 {
		t.Fatalf("persisted=%d closed=%d, want 1,1", per, c)
	}
}

// A throwaway session is killed directly, as before.
func TestThrowawaySession_NoGracefulClose(t *testing.T) {
	f := newFakeCDPBrowser(t, "fake-browser-id")
	installFakeCDPLauncher(t, f)
	m := newCobrowseSessionManager(trustedBaseDir(t), 2)
	st, err := m.StartSession("")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := m.Stop(st.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if got := f.log(); got != "kill" {
		t.Fatalf("throwaway teardown = %q, want kill only", got)
	}
}

// The graceful close is sent only to the browser this session launched.
func TestGracefulCloseRefusesOtherBrowser(t *testing.T) {
	f := newFakeCDPBrowser(t, "someone-else")
	prev := gracefulCloseWait
	gracefulCloseWait = 50 * time.Millisecond
	t.Cleanup(func() { gracefulCloseWait = prev })
	closeBrowserGracefully(&cobrowseProc{debugPort: f.port, browserID: "ours", exited: make(chan struct{})})
	if got := f.log(); got != "" {
		t.Fatalf("closed a browser this session did not launch: %q", got)
	}
}

func TestStartSessionWithID(t *testing.T) {
	l := &sentinelCheckingLauncher{}
	l.install(t)
	m := newCobrowseSessionManager(trustedBaseDir(t), 4)

	bad := &fakeProfile{}
	if _, err := m.StartSessionWithID("../escape", "", bad); err == nil {
		t.Fatal("an id outside the reserved shape must be refused")
	}
	if _, _, c := bad.counts(); c != 1 {
		t.Fatalf("a refused start must Close the profile, closed=%d", c)
	}

	id := NewCobrowseSessionID()
	st, err := m.StartSessionWithID(id, "", nil)
	if err != nil || st.ID != id {
		t.Fatalf("StartSessionWithID = %+v, %v", st, err)
	}
	dup := &fakeProfile{}
	if _, err := m.StartSessionWithID(id, "", dup); err == nil {
		t.Fatal("a duplicate id must be refused")
	}
	if _, _, c := dup.counts(); c != 1 {
		t.Fatalf("a duplicate start must Close the profile, closed=%d", c)
	}
	_ = m.StopAll()
}

// A browser that never answers or exits costs at most gracefulCloseWait in
// total before teardown kills it.
func TestGracefulCloseIsBoundedInTotal(t *testing.T) {
	f := newFakeCDPBrowser(t, "fake-browser-id")
	f.hang = true
	prev := gracefulCloseWait
	gracefulCloseWait = 200 * time.Millisecond
	t.Cleanup(func() { gracefulCloseWait = prev })
	start := time.Now()
	closeBrowserGracefully(&cobrowseProc{debugPort: f.port, browserID: f.id, exited: make(chan struct{})})
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("graceful close took %s, want about %s", d, gracefulCloseWait)
	}
	if got := f.log(); got != "Browser.close" {
		t.Fatalf("close was not attempted: %q", got)
	}
}
