package cobrowsestream

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// fakeChrome is a minimal CDP endpoint over a WebSocket: it emits screencast
// frames gated on acks (so a test can prove acking is what keeps frames flowing)
// and records the Input.dispatch* commands it receives.
type fakeChrome struct {
	server *httptest.Server

	totalFrames  int     // frames to emit across the session
	deviceWidth  float64 // advertised frame width
	deviceHeight float64 // advertised frame height
	jpeg         []byte  // raw bytes each frame carries (base64 on the wire)

	mu      sync.Mutex
	acks    int
	mouse   []map[string]any
	keys    []map[string]any
	conns   []*websocket.Conn
	started bool

	// connReg is signaled once per accepted CDP conn, AFTER it has been appended
	// to conns. dialCDP returns to the test the instant the client sees the 101
	// upgrade response, which can be strictly before this handler goroutine runs
	// the append -- so a test that wants to act on a registered conn (e.g.
	// killConns) must wait on this first, or it races an empty conns slice.
	connReg chan struct{}
}

func newFakeChrome() *fakeChrome {
	f := &fakeChrome{
		totalFrames:  3,
		deviceWidth:  800,
		deviceHeight: 600,
		jpeg:         []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x01, 0x02, 0x03}, // JPEG-ish
		connReg:      make(chan struct{}, 16),
	}
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	mux := http.NewServeMux()
	mux.HandleFunc("/devtools/page/1", func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		f.mu.Lock()
		f.conns = append(f.conns, conn)
		f.mu.Unlock()
		// Registration is now visible to conns; signal any waiter (best-effort,
		// buffered) before we block reading so killConns can't race the append.
		select {
		case f.connReg <- struct{}{}:
		default:
		}
		f.serve(conn)
	})
	f.server = httptest.NewServer(mux)
	return f
}

// wsURL returns the DevTools WebSocket URL cdpTargetURL should resolve to.
func (f *fakeChrome) wsURL() string {
	return "ws" + strings.TrimPrefix(f.server.URL, "http") + "/devtools/page/1"
}

func (f *fakeChrome) serve(conn *websocket.Conn) {
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var msg struct {
			ID     int            `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		switch msg.Method {
		case "Page.startScreencast":
			f.mu.Lock()
			f.started = true
			f.mu.Unlock()
			f.sendFrame(conn, 1)
		case "Page.screencastFrameAck":
			f.mu.Lock()
			f.acks++
			n := f.acks
			total := f.totalFrames
			f.mu.Unlock()
			if n < total {
				f.sendFrame(conn, n+1)
			}
		case "Input.dispatchMouseEvent":
			f.mu.Lock()
			f.mouse = append(f.mouse, msg.Params)
			f.mu.Unlock()
		case "Input.dispatchKeyEvent":
			f.mu.Lock()
			f.keys = append(f.keys, msg.Params)
			f.mu.Unlock()
		}
	}
}

func (f *fakeChrome) sendFrame(conn *websocket.Conn, seq int) {
	ev := map[string]any{
		"method": "Page.screencastFrame",
		"params": map[string]any{
			"data":      base64.StdEncoding.EncodeToString(f.jpeg),
			"sessionId": seq,
			"metadata": map[string]any{
				"deviceWidth":  f.deviceWidth,
				"deviceHeight": f.deviceHeight,
			},
		},
	}
	_ = conn.WriteJSON(ev)
}

// waitConnRegistered blocks until the server handler has accepted and appended
// at least one CDP conn (see connReg). Callers must do this before killConns:
// dialCDP returns as soon as the client reads the 101 upgrade response, which
// can precede the handler's append, so killing "now" may find conns empty and
// close nothing -- leaving both read loops blocked forever (citadel #819).
func (f *fakeChrome) waitConnRegistered(t *testing.T) {
	t.Helper()
	select {
	case <-f.connReg:
	case <-time.After(10 * time.Second):
		t.Fatal("server never registered a CDP conn")
	}
}

// killConns closes the server-side CDP sockets, simulating the browser going
// away (session stop) under a live viewer. A caller must first establish that
// the conn has been registered (appended to conns) -- either via
// waitConnRegistered, or by reading a frame that proves f.serve is already
// running (as TestTeardownOnBrowserStop does). Otherwise this can run before the
// handler's append and close nothing, leaving both read loops blocked forever.
func (f *fakeChrome) killConns() {
	f.mu.Lock()
	conns := append([]*websocket.Conn(nil), f.conns...)
	f.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

func (f *fakeChrome) close() { f.server.Close() }

func (f *fakeChrome) mouseEvents() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.mouse...)
}

// withFakeCDP points cdpTargetURL at the fake for the duration of a test.
func withFakeCDP(t *testing.T, f *fakeChrome) {
	t.Helper()
	orig := cdpTargetURL
	cdpTargetURL = func(int) (string, error) { return f.wsURL(), nil }
	t.Cleanup(func() { cdpTargetURL = orig })
}

// TestCDPScreencastAckKeepsFramesFlowing pins the load-bearing ack: the fake
// gates each subsequent frame on receiving an ack for the previous one, so if
// handleScreencastFrame stopped acking, only the first frame would arrive.
func TestCDPScreencastAckKeepsFramesFlowing(t *testing.T) {
	f := newFakeChrome()
	defer f.close()
	withFakeCDP(t, f)

	frames := make(chan screencastFrame, 16)
	cdp, err := dialCDP(9222, func(fr screencastFrame) { frames <- fr })
	if err != nil {
		t.Fatalf("dialCDP: %v", err)
	}
	defer cdp.Close()

	if err := cdp.startScreencast(60, 1280, 720, 1); err != nil {
		t.Fatalf("startScreencast: %v", err)
	}

	// Expect exactly totalFrames frames, each decoded back to the raw JPEG bytes.
	for i := 0; i < f.totalFrames; i++ {
		select {
		case fr := <-frames:
			if string(fr.jpeg) != string(f.jpeg) {
				t.Fatalf("frame %d jpeg mismatch: got %v", i, fr.jpeg)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out waiting for frame %d (ack not driving frames?)", i)
		}
	}
}

// TestCDPMouseCoordinateMapping verifies normalized [0,1] viewer coords are
// mapped to CDP viewport pixels using the latest frame's device dimensions.
func TestCDPMouseCoordinateMapping(t *testing.T) {
	f := newFakeChrome()
	defer f.close()
	withFakeCDP(t, f)

	frames := make(chan screencastFrame, 16)
	cdp, err := dialCDP(9222, func(fr screencastFrame) { frames <- fr })
	if err != nil {
		t.Fatalf("dialCDP: %v", err)
	}
	defer cdp.Close()
	if err := cdp.startScreencast(60, 1280, 720, 1); err != nil {
		t.Fatalf("startScreencast: %v", err)
	}
	// Wait for one frame so lastW/lastH are populated (800x600).
	select {
	case <-frames:
	case <-time.After(3 * time.Second):
		t.Fatal("no frame")
	}

	if err := cdp.dispatchMouse(InputMessage{
		Type: InputTypeMouse, Event: "mousePressed", X: 0.5, Y: 0.25, Button: "left", ClickCount: 1,
	}); err != nil {
		t.Fatalf("dispatchMouse: %v", err)
	}

	// Poll for the recorded event (dispatch is fire-and-forget over the wire).
	deadline := time.Now().Add(3 * time.Second)
	for {
		ev := f.mouseEvents()
		if len(ev) > 0 {
			x, _ := ev[0]["x"].(float64)
			y, _ := ev[0]["y"].(float64)
			if x != 400 { // 0.5 * 800
				t.Errorf("x = %v, want 400", x)
			}
			if y != 150 { // 0.25 * 600
				t.Errorf("y = %v, want 150", y)
			}
			if ev[0]["button"] != "left" {
				t.Errorf("button = %v, want left", ev[0]["button"])
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("mouse event never recorded")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestCDPCloseSignalsDone verifies the browser going away closes Done() so the
// viewer handler can tear down promptly.
func TestCDPCloseSignalsDone(t *testing.T) {
	f := newFakeChrome()
	defer f.close()
	withFakeCDP(t, f)

	cdp, err := dialCDP(9222, func(screencastFrame) {})
	if err != nil {
		t.Fatalf("dialCDP: %v", err)
	}
	defer cdp.Close()

	// Wait until the fake has actually registered the conn before killing it.
	// Without this, killConns can run before the handler appends the conn (dialCDP
	// returns on the 101 upgrade, which precedes the append under parallel load),
	// close nothing, and leave the read loop blocked forever -- the #819 flake,
	// which failed a release. The 3s wall-clock guard the select used before was a
	// red herring: the real defect was this unsynchronized close, not a tight
	// deadline. Once the kill is deterministic, Done() closes in ms.
	f.waitConnRegistered(t)
	f.killConns()
	select {
	case <-cdp.Done():
	case <-time.After(10 * time.Second):
		// Safety net only: with the kill synchronized above this never fires
		// unless Done() genuinely regresses.
		t.Fatal("Done() not closed after browser socket dropped")
	}
}

// TestCDPMouseDropsWithoutFrame verifies input is dropped (not dispatched at
// 0,0) before any frame establishes the coordinate scale.
func TestCDPMouseDropsWithoutFrame(t *testing.T) {
	f := newFakeChrome()
	defer f.close()
	withFakeCDP(t, f)

	cdp, err := dialCDP(9222, func(screencastFrame) {})
	if err != nil {
		t.Fatalf("dialCDP: %v", err)
	}
	defer cdp.Close()

	if err := cdp.dispatchMouse(InputMessage{Type: InputTypeMouse, Event: "mouseMoved", X: 0.5, Y: 0.5}); err != nil {
		t.Fatalf("dispatchMouse: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if got := len(f.mouseEvents()); got != 0 {
		t.Errorf("dispatched %d mouse events before any frame, want 0", got)
	}
}
