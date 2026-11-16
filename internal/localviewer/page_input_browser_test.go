package localviewer

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/aceteam-ai/citadel-cli/internal/cobrowsestream"
	"github.com/aceteam-ai/citadel-cli/internal/localcobrowse"
	"github.com/aceteam-ai/citadel-cli/internal/localcobrowse/fakebrowser"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

// fakeCDPPage stands in for the streamed page's browser: it serves the
// loopback discovery list and a CDP socket that emits one tall frame and
// records the input the viewer forwards.
type fakeCDPPage struct {
	ln   net.Listener
	jpeg []byte
	w, h int

	mu    sync.Mutex
	mouse []map[string]any
	keys  []map[string]any
}

func newFakeCDPPage(t *testing.T, w, h int) *fakeCDPPage {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(y % 256), G: 120, B: 200, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeCDPPage{ln: ln, jpeg: buf.Bytes(), w: w, h: h}
	port := ln.Addr().(*net.TCPAddr).Port
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	mux := http.NewServeMux()
	mux.HandleFunc("/json", func(rw http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(rw).Encode([]map[string]any{{
			"id": "1", "type": "page", "url": "https://example.com/", "title": "t",
			"webSocketDebuggerUrl": "ws://127.0.0.1:" + strconv.Itoa(port) + "/devtools/page/1",
		}})
	})
	mux.HandleFunc("/devtools/page/1", func(rw http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(rw, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		f.serve(conn)
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return f
}

func (f *fakeCDPPage) serve(conn *websocket.Conn) {
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
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		frame := func(seq int) {
			_ = conn.WriteJSON(map[string]any{"method": "Page.screencastFrame", "params": map[string]any{
				"data": base64.StdEncoding.EncodeToString(f.jpeg), "sessionId": seq,
				"metadata": map[string]any{"deviceWidth": f.w, "deviceHeight": f.h},
			}})
		}
		switch msg.Method {
		case "Page.startScreencast":
			frame(1)
		case "Page.screencastFrameAck":
			// One frame is enough.
		case "Page.captureScreenshot":
			_ = conn.WriteJSON(map[string]any{"id": msg.ID, "result": map[string]any{"data": base64.StdEncoding.EncodeToString(f.jpeg)}})
		case "Page.getLayoutMetrics":
			_ = conn.WriteJSON(map[string]any{"id": msg.ID, "result": map[string]any{"cssVisualViewport": map[string]any{
				"clientWidth": f.w, "clientHeight": f.h,
			}}})
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

func (f *fakeCDPPage) snapshot() (mouse, keys []map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append(mouse, f.mouse...), append(keys, f.keys...)
}

// handoffSession reports control only once the user has taken it, as the
// production adapter does.
type handoffSession struct {
	port int
	svc  *localcobrowse.Service
}

func (s handoffSession) AttachTarget(string) (int, bool) { return s.port, true }
func (s handoffSession) MarkAttached(string) bool        { return true }
func (s handoffSession) MarkDetached(string) bool        { return true }
func (s handoffSession) ViewerHasControl(id string) bool {
	for _, v := range s.svc.Sessions(id) {
		if v.Driver == "human" {
			return true
		}
	}
	return false
}

type viewerGeo struct{ Top, Bottom, Left, Right, InnerW, InnerH, ScrollY float64 }

type viewerFixture struct {
	t    *testing.T
	page *fakeCDPPage
	cdp  *cobrowsestream.Client
	m    *platform.CobrowseSessionManager
	vp   platform.CobrowseSessionStatus
}

func (v *viewerFixture) geo() (g viewerGeo) {
	v.t.Helper()
	cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := v.cdp.Command(cctx, "Runtime.evaluate", map[string]any{"returnByValue": true, "expression": `JSON.stringify((function(){
		var r = document.getElementById("screen").getBoundingClientRect();
		return {Top: r.top, Bottom: r.bottom, Left: r.left, Right: r.right,
			InnerW: window.innerWidth, InnerH: window.innerHeight, ScrollY: window.scrollY};
	})())`})
	if err != nil {
		v.t.Fatalf("read viewer geometry: %v", err)
	}
	inner, _ := res["result"].(map[string]any)
	str, _ := inner["value"].(string)
	if err := json.Unmarshal([]byte(str), &g); err != nil {
		v.t.Fatalf("viewer geometry %v: %v", res, err)
	}
	return g
}

// startHandedOffViewer runs the viewer in a real browser, hands the session to
// the user the way the agent does, and waits for the streamed frame and a
// settled layout.
func startHandedOffViewer(t *testing.T) *viewerFixture {
	t.Helper()
	if os.Getenv("CITADEL_COBROWSE_BROWSER_IT") == "" {
		t.Skip("set CITADEL_COBROWSE_BROWSER_IT=1 to run the real-browser viewer test")
	}
	page := newFakeCDPPage(t, 600, 2400)

	fake := fakebrowser.New()
	svc := localcobrowse.New(localcobrowse.Config{
		Browser:   fake,
		URLPolicy: func(context.Context, string) error { return nil },
	})
	srv := New(Config{
		Service:  svc,
		Stream:   handoffSession{port: page.ln.Addr().(*net.TCPAddr).Port, svc: svc},
		OpenURL:  func(string) error { return nil },
		TokenDir: t.TempDir(),
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Serve(ln); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)

	ctx := context.Background()
	st, err := svc.Start(ctx, "", "Fixture goal", "")
	if err != nil {
		t.Fatal(err)
	}
	m := platform.NewCobrowseSessionManager(filepath.Join(t.TempDir(), "sessions"), 1)
	t.Cleanup(func() { _ = m.StopAll() })
	vp, err := m.StartSession(srv.MintURL())
	if err != nil {
		t.Fatalf("start viewer browser: %v", err)
	}

	waitText := func(sel, want string) {
		t.Helper()
		deadline := time.Now().Add(60 * time.Second)
		var last string
		for time.Now().Before(deadline) {
			if res, err := m.ExtractBounded(vp.ID, sel, nil, 500); err == nil {
				last = res.Text
				if strings.Contains(last, want) {
					return
				}
			}
			time.Sleep(250 * time.Millisecond)
		}
		t.Fatalf("%s: got %q, want %q", sel, last, want)
	}
	waitText("#session-goal", "Fixture goal")
	// The agent hands off, as it does when it needs the user (the "Waiting
	// for you" banner appears while the stream starts). The streamed picture
	// keeps repainting, so the controls are driven through the service rather
	// than by selector clicks, which wait for a still layout.
	if _, err := svc.Handoff(ctx, st.SessionID, "sign in", ""); err != nil {
		t.Fatalf("handoff: %v", err)
	}
	waitText("#waiting", "Waiting for you: sign in")

	cdp, err := cobrowsestream.DialCDP(vp.DebugPort)
	if err != nil {
		t.Fatalf("dial viewer browser: %v", err)
	}
	t.Cleanup(func() { cdp.Close() })
	v := &viewerFixture{t: t, page: page, cdp: cdp, m: m, vp: vp}

	// Wait for the streamed frame to be painted.
	deadline := time.Now().Add(30 * time.Second)
	for {
		res, err := m.ExtractBounded(vp.ID, "#screen", []string{"src"}, 500)
		if err == nil && strings.HasPrefix(res.Attrs["src"], "blob:") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the streamed frame never appeared")
		}
		time.Sleep(200 * time.Millisecond)
	}
	// Let layout settle after the image loads: poll until #screen's rect holds
	// still across consecutive reads.
	var prev viewerGeo
	stable := 0
	deadline = time.Now().Add(30 * time.Second)
	for stable < 3 {
		if time.Now().After(deadline) {
			t.Fatal("the viewer layout never settled")
		}
		g := v.geo()
		if g == prev && g.Bottom > g.Top {
			stable++
		} else {
			stable = 0
		}
		prev = g
		time.Sleep(100 * time.Millisecond)
	}
	return v

}

// TestViewerFirstInputAfterHandoffInRealBrowser pins #1681: the first click
// after the user takes control must reach the streamed page where the pointer
// was, and the keystroke after it must be forwarded, without the viewer page
// scrolling. Focusing the stream used to scroll the viewer document before the
// click position was read, so the click landed about 140px off and the page
// moved under the user.
func TestViewerFirstInputAfterHandoffInRealBrowser(t *testing.T) {
	v := startHandedOffViewer(t)
	page, m, vp := v.page, v.m, v.vp
	// One click inside the picture, then one key: exactly what a user does
	// right after the handoff. The click sends mouseMoved first (before the
	// focus), then the press and release.
	// The window size and pixel ratio differ per platform, so aim at the middle
	// of the part of the picture that is actually on screen.
	g0 := v.geo()
	top, bottom := math.Max(g0.Top, 0), math.Min(g0.Bottom, g0.InnerH)
	if bottom-top < 20 || g0.Right-g0.Left < 20 || g0.ScrollY != 0 {
		t.Fatalf("the picture is not on screen to click: %+v", g0)
	}
	x, y := (g0.Left+g0.Right)/2, (top+bottom)/2
	if _, err := m.Click(vp.ID, "", &x, &y); err != nil {
		t.Fatalf("click: %v", err)
	}
	if _, err := m.Key(vp.ID, "a"); err != nil {
		t.Fatalf("key: %v", err)
	}

	if g := v.geo(); g.ScrollY != 0 {
		t.Fatalf("the viewer page scrolled after the first click and key: %+v (before: %+v)", g, g0)
	}

	var moved, pressed, released map[string]any
	var keys []map[string]any
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		mouse, ks := page.snapshot()
		moved, pressed, released = nil, nil, nil
		for _, ev := range mouse {
			switch ev["type"] {
			case "mouseMoved":
				if moved == nil {
					moved = ev
				}
			case "mousePressed":
				pressed = ev
			case "mouseReleased":
				released = ev
			}
		}
		keys = ks
		if pressed != nil && released != nil && len(keys) >= 2 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if moved == nil || pressed == nil || released == nil {
		t.Fatalf("the click at (%.0f,%.0f) did not reach the streamed page (%+v): moved=%v pressed=%v released=%v", x, y, g0, moved, pressed, released)
	}
	num := func(ev map[string]any, k string) float64 { f, _ := ev[k].(float64); return f }
	py := num(pressed, "y")
	if py <= 0 || py >= float64(page.h) {
		t.Fatalf("press y=%v is outside the picture", py)
	}
	// The pointer did not move between the hover and the press, so both must
	// map to the same spot. A scroll in between shifts the press (#1681).
	if d := math.Abs(py - num(moved, "y")); d > 2 {
		t.Fatalf("the viewer scrolled under the first click: hover y=%.1f, press y=%.1f", num(moved, "y"), py)
	}
	if math.Abs(num(pressed, "x")-num(moved, "x")) > 2 || math.Abs(num(released, "y")-py) > 2 {
		t.Fatalf("click positions disagree: moved=%v pressed=%v released=%v", moved, pressed, released)
	}
	if got := fmt.Sprint(keys[0]["key"], keys[0]["text"]); !strings.Contains(got, "a") {
		t.Fatalf("the key after the click was not forwarded to the streamed page: %v", keys)
	}
}

// TestViewerFocusAfterHandoffInRealBrowser pins #1687: after the user takes
// control, the first keystroke, sent with no click on the picture first, must
// be forwarded to the streamed page, and Space must not scroll the viewer.
func TestViewerFocusAfterHandoffInRealBrowser(t *testing.T) {
	v := startHandedOffViewer(t)
	if g := v.geo(); g.ScrollY != 0 {
		t.Fatalf("the viewer starts scrolled: %+v", g)
	}
	for _, k := range []string{"a", " "} {
		if _, err := v.m.Key(v.vp.ID, k); err != nil {
			t.Fatalf("key %q: %v", k, err)
		}
	}
	var keys []map[string]any
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_, keys = v.page.snapshot()
		if len(keys) >= 4 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(keys) < 4 {
		t.Fatalf("a key pressed right after the handoff was not forwarded to the streamed page: %v", keys)
	}
	if got := fmt.Sprint(keys[0]["key"], keys[0]["text"]); !strings.Contains(got, "a") {
		t.Fatalf("unexpected first key: %v", keys)
	}
	if g := v.geo(); g.ScrollY != 0 {
		t.Fatalf("the viewer page scrolled on a key before any click: %+v", g)
	}
}
