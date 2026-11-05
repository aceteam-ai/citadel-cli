// internal/platform/cobrowse_session_actions_test.go
//
// Tests for the #978 session-scoped CDP actions and driver arbitration.
//
// Two layers, deliberately kept separate:
//   - Arbitration-only tests use fakeLauncher (from cobrowse_session_test.go): the
//     session's debug port is a fabricated number with nothing listening on it, so
//     these prove the ErrHandedOff/ErrNotStarted gate fires BEFORE any CDP round
//     trip -- exactly where the state machine lives -- without needing a real or
//     fake browser at all.
//   - Functional tests use a real listening fake CDP HTTP+WS server (fakeCDPServer
//     below) so click/type/extract/navigate/screenshot exercise the actual CDP
//     wire protocol end to end. This is necessary (not just nice to have) because
//     pickTarget/cdpCommand in cobrowse.go hardcode "127.0.0.1:<port>" with no
//     injectable seam (unlike cobrowsestream's cdpTargetURL var) -- so the only way
//     to intercept a real request is to actually listen on the port the session
//     reports as its debug_port.
package platform

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// installFixedPortLauncher points launchCobrowseProc at a fake session whose
// debug port is a caller-chosen, already-listening port (a real fake CDP
// server's port, or an arbitrary unused number for arbitration-only tests).
func installFixedPortLauncher(t *testing.T, port int) {
	t.Helper()
	prev := launchCobrowseProc
	launchCobrowseProc = func(profileDir, startURL string) (*cobrowseProc, error) {
		exited := make(chan struct{})
		return &cobrowseProc{
			debugPort: port,
			display:   ":99",
			exited:    exited,
			stop:      func() error { return nil },
		}, nil
	}
	t.Cleanup(func() { launchCobrowseProc = prev })
}

// ---------------------------------------------------------------------------
// Driver arbitration state machine (no real/fake browser needed: every case
// below is decided by requireDrivablePort before any CDP call is attempted).
// ---------------------------------------------------------------------------

func TestCobrowseSessionActions_DriverDefaultsAI(t *testing.T) {
	f := &fakeLauncher{}
	f.install(t)
	m := newCobrowseSessionManager(trustedBaseDir(t), 8)

	st, err := m.StartSession("")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if st.Driver != DriverAI {
		t.Errorf("new session driver = %q, want %q", st.Driver, DriverAI)
	}
}

func TestCobrowseSessionActions_HandoffRefusesWrites(t *testing.T) {
	f := &fakeLauncher{}
	f.install(t)
	m := newCobrowseSessionManager(trustedBaseDir(t), 8)

	st, err := m.StartSession("")
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	handedOff, err := m.Handoff(st.ID)
	if err != nil {
		t.Fatalf("handoff: %v", err)
	}
	if handedOff.Driver != DriverHuman {
		t.Errorf("driver after handoff = %q, want %q", handedOff.Driver, DriverHuman)
	}

	x, y := 1.0, 2.0
	if _, err := m.Navigate(st.ID, "https://example.com"); !errors.Is(err, ErrHandedOff) {
		t.Errorf("navigate while handed off: got %v, want ErrHandedOff", err)
	}
	if _, err := m.Click(st.ID, "", &x, &y); !errors.Is(err, ErrHandedOff) {
		t.Errorf("click while handed off: got %v, want ErrHandedOff", err)
	}
	if _, err := m.Type(st.ID, "hi"); !errors.Is(err, ErrHandedOff) {
		t.Errorf("type while handed off: got %v, want ErrHandedOff", err)
	}
	// Read-only actions are refused too -- mirrors CobrowseManager.Screenshot's
	// actual behavior (see the package doc comment in
	// cobrowse_session_actions.go for why this is NOT "likely allowed").
	if _, err := m.Screenshot(st.ID); !errors.Is(err, ErrHandedOff) {
		t.Errorf("screenshot while handed off: got %v, want ErrHandedOff", err)
	}
	if _, err := m.Extract(st.ID, "a", nil); !errors.Is(err, ErrHandedOff) {
		t.Errorf("extract while handed off: got %v, want ErrHandedOff", err)
	}
}

func TestCobrowseSessionActions_ResumeRestoresAIControl(t *testing.T) {
	// Fixed, unused port: after resume the action reaches the CDP call and
	// fails with a connection error -- the point is that it is NOT ErrHandedOff.
	installFixedPortLauncher(t, freeTCPPort(t))
	m := newCobrowseSessionManager(trustedBaseDir(t), 8)

	st, err := m.StartSession("")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := m.Handoff(st.ID); err != nil {
		t.Fatalf("handoff: %v", err)
	}
	resumed, err := m.Resume(st.ID)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if resumed.Driver != DriverAI {
		t.Errorf("driver after resume = %q, want %q", resumed.Driver, DriverAI)
	}

	_, err = m.Navigate(st.ID, "https://example.com")
	if err == nil {
		t.Fatalf("expected navigate to fail against a port nothing listens on")
	}
	if errors.Is(err, ErrHandedOff) {
		t.Errorf("navigate after resume should not be refused for arbitration reasons, got %v", err)
	}
}

// A viewer may observe without taking control or blocking agent actions.
func TestCobrowseSessionActions_MarkAttachedAllowsAgentWrites(t *testing.T) {
	f := &fakeLauncher{}
	f.install(t)
	m := newCobrowseSessionManager(trustedBaseDir(t), 8)

	st, err := m.StartSession("")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if !m.MarkAttached(st.ID) {
		t.Fatalf("attach should succeed")
	}
	got, _ := m.SessionStatus(st.ID)
	if got.State != SessionAttached {
		t.Fatalf("expected attached state, got %q", got.State)
	}
	if got.Driver != DriverAI {
		t.Errorf("an observing viewer should report driver=ai, got %q", got.Driver)
	}
	if m.ViewerHasControl(st.ID) {
		t.Error("observing viewer has input control")
	}
	if _, err := m.Type(st.ID, "hi"); errors.Is(err, ErrHandedOff) {
		t.Errorf("type while observing was blocked: %v", err)
	}
}

// TestCobrowseSessionActions_DetachWithoutExplicitHandoffRestoresAIControl
// pins the other half: a BARE attach/detach cycle (no `handoff` action ever
// called) is itself "a human grabbing a live scripted session mid-run and
// handing it back" -- detaching alone restores agent scripting, with no
// change needed to setAttached/MarkDetached (see the package doc comment in
// cobrowse_session_actions.go for why this is deliberately NOT a one-way
// latch).
func TestCobrowseSessionActions_DetachWithoutExplicitHandoffRestoresAIControl(t *testing.T) {
	installFixedPortLauncher(t, freeTCPPort(t))
	m := newCobrowseSessionManager(trustedBaseDir(t), 8)

	st, err := m.StartSession("")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	m.MarkAttached(st.ID)
	if _, err := m.Type(st.ID, "hi"); errors.Is(err, ErrHandedOff) {
		t.Fatalf("type while observing was blocked: %v", err)
	}

	if !m.MarkDetached(st.ID) {
		t.Fatalf("detach should succeed")
	}
	got, _ := m.SessionStatus(st.ID)
	if got.State != SessionRunning {
		t.Errorf("expected running state after detach, got %q", got.State)
	}
	if got.Driver != DriverAI {
		t.Errorf("a bare detach (no explicit handoff) should restore driver=ai, got %q", got.Driver)
	}

	_, err = m.Navigate(st.ID, "https://example.com")
	if errors.Is(err, ErrHandedOff) {
		t.Errorf("navigate after a bare detach should not be refused for arbitration reasons, got %v", err)
	}
}

// TestCobrowseSessionActions_ExplicitHandoffSurvivesDetach pins the STICKY
// half: an explicit `handoff` (unlike a bare attach) is NOT undone by a
// viewer disconnecting -- the mid-2FA scenario, where a transient WebSocket
// blip must not silently resume agent scripting on a session the human
// explicitly claimed. Only an explicit `resume` clears it.
func TestCobrowseSessionActions_ExplicitHandoffSurvivesDetach(t *testing.T) {
	f := &fakeLauncher{}
	f.install(t)
	m := newCobrowseSessionManager(trustedBaseDir(t), 8)

	st, err := m.StartSession("")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := m.Handoff(st.ID); err != nil {
		t.Fatalf("handoff: %v", err)
	}
	// Attach then detach (a viewer connected and disconnected while handed off).
	m.MarkAttached(st.ID)
	m.MarkDetached(st.ID)

	got, _ := m.SessionStatus(st.ID)
	if got.State != SessionRunning {
		t.Errorf("expected running state after detach, got %q", got.State)
	}
	if got.Driver != DriverHuman {
		t.Errorf("explicit handoff must survive a detach, got driver=%q", got.Driver)
	}
	if _, err := m.Type(st.ID, "hi"); !errors.Is(err, ErrHandedOff) {
		t.Errorf("type after detach with an outstanding explicit handoff: got %v, want ErrHandedOff", err)
	}

	resumed, err := m.Resume(st.ID)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if resumed.Driver != DriverAI {
		t.Errorf("driver after explicit resume = %q, want %q", resumed.Driver, DriverAI)
	}
}

// Resume restores the agent and revokes viewer input with the tab still open.
func TestCobrowseSessionActions_ResumeWhileStillAttachedRestoresAgent(t *testing.T) {
	f := &fakeLauncher{}
	f.install(t)
	m := newCobrowseSessionManager(trustedBaseDir(t), 8)

	st, err := m.StartSession("")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	m.MarkAttached(st.ID)
	if _, err := m.Handoff(st.ID); err != nil {
		t.Fatalf("handoff: %v", err)
	}
	if !m.ViewerHasControl(st.ID) {
		t.Fatal("viewer should have control after explicit handoff")
	}
	resumed, err := m.Resume(st.ID)
	if err != nil {
		t.Fatalf("resume while attached should succeed (idempotent): %v", err)
	}
	if resumed.Driver != DriverAI {
		t.Errorf("driver after resume with viewer attached = %q, want %q", resumed.Driver, DriverAI)
	}
	if resumed.State != SessionAttached {
		t.Errorf("resume must not change the attach/view state, got %q", resumed.State)
	}
	if m.ViewerHasControl(st.ID) {
		t.Error("viewer retained input control after resume")
	}
	if _, err := m.Type(st.ID, "hi"); errors.Is(err, ErrHandedOff) {
		t.Errorf("type after resume was blocked: %v", err)
	}

	m.MarkDetached(st.ID)
	got, _ := m.SessionStatus(st.ID)
	if got.Driver != DriverAI {
		t.Errorf("driver after detach following resume = %q, want %q", got.Driver, DriverAI)
	}
}

func TestCobrowseSessionActions_ExitedSessionRefusesActions(t *testing.T) {
	f := &fakeLauncher{exitedNow: true}
	f.install(t)
	m := newCobrowseSessionManager(trustedBaseDir(t), 8)

	st, err := m.StartSession("")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := m.Navigate(st.ID, "https://example.com"); !errors.Is(err, ErrNotStarted) {
		t.Errorf("navigate on an exited session: got %v, want ErrNotStarted", err)
	}
}

func TestCobrowseSessionActions_UnknownSessionErrors(t *testing.T) {
	m := newCobrowseSessionManager(trustedBaseDir(t), 8)
	x, y := 1.0, 2.0

	if _, err := m.Navigate("cb-nope", "https://example.com"); err == nil {
		t.Error("navigate on unknown session should error")
	}
	if _, err := m.Screenshot("cb-nope"); err == nil {
		t.Error("screenshot on unknown session should error")
	}
	if _, err := m.Click("cb-nope", "", &x, &y); err == nil {
		t.Error("click on unknown session should error")
	}
	if _, err := m.Type("cb-nope", "hi"); err == nil {
		t.Error("type on unknown session should error")
	}
	if _, err := m.Extract("cb-nope", "a", nil); err == nil {
		t.Error("extract on unknown session should error")
	}
	if _, err := m.Handoff("cb-nope"); err == nil {
		t.Error("handoff on unknown session should error")
	}
	if _, err := m.Resume("cb-nope"); err == nil {
		t.Error("resume on unknown session should error")
	}
}

func TestCobrowseSessionActions_ClickRequiresSelectorOrCoords(t *testing.T) {
	installFixedPortLauncher(t, freeTCPPort(t))
	m := newCobrowseSessionManager(trustedBaseDir(t), 8)

	st, err := m.StartSession("")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := m.Click(st.ID, "", nil, nil); err == nil {
		t.Fatal("click with neither selector nor coords should error")
	}
}

// freeTCPPort returns a currently-unused local TCP port by briefly listening
// and closing. Good enough for a "reaches the CDP call but nothing answers"
// test; a real listener replaces this via fakeCDPServer below.
func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

// ---------------------------------------------------------------------------
// Functional CDP round trips against a real fake CDP HTTP+WS server.
// ---------------------------------------------------------------------------

type recordedCDPCall struct {
	Method string
	Params map[string]any
}

// fakeCDPServer is a minimal CDP endpoint compatible with pickTarget/
// cdpCommand: it serves GET /json (the target list) and upgrades
// /devtools/page/1 to a WebSocket that answers one request/response per
// connection, matching cdpDialAndSend's connect-act-disconnect model.
type fakeCDPServer struct {
	port int

	mu             sync.Mutex
	calls          []recordedCDPCall
	evaluateValue  func(expr string) any
	screenshotData string
}

func startFakeCDPServer(t *testing.T) *fakeCDPServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	f := &fakeCDPServer{port: port}

	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	mux := http.NewServeMux()
	mux.HandleFunc("/json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `[{"type":"page","url":"about:blank","webSocketDebuggerUrl":"ws://127.0.0.1:%d/devtools/page/1"}]`, port)
	})
	mux.HandleFunc("/devtools/page/1", func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			var msg struct {
				ID     int            `json:"id"`
				Method string         `json:"method"`
				Params map[string]any `json:"params"`
			}
			if err := conn.ReadJSON(&msg); err != nil {
				return
			}
			f.record(msg.Method, msg.Params)
			result := f.respond(msg.Method, msg.Params)
			_ = conn.WriteJSON(map[string]any{"id": msg.ID, "result": result})
		}
	})

	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() { _ = srv.Close() })
	return f
}

func (f *fakeCDPServer) record(method string, params map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, recordedCDPCall{Method: method, Params: params})
}

func (f *fakeCDPServer) callsFor(method string) []recordedCDPCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []recordedCDPCall
	for _, c := range f.calls {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeCDPServer) respond(method string, params map[string]any) map[string]any {
	switch method {
	case "Runtime.evaluate":
		expr, _ := params["expression"].(string)
		var val any
		f.mu.Lock()
		responder := f.evaluateValue
		f.mu.Unlock()
		if responder != nil {
			val = responder(expr)
		}
		return map[string]any{"result": map[string]any{"type": "object", "value": val}}
	case "Page.captureScreenshot":
		f.mu.Lock()
		data := f.screenshotData
		f.mu.Unlock()
		return map[string]any{"data": data}
	default:
		return map[string]any{}
	}
}

func startSessionOnFakeCDP(t *testing.T) (*CobrowseSessionManager, CobrowseSessionStatus, *fakeCDPServer) {
	t.Helper()
	f := startFakeCDPServer(t)
	installFixedPortLauncher(t, f.port)
	m := newCobrowseSessionManager(trustedBaseDir(t), 8)
	st, err := m.StartSession("")
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	return m, st, f
}

func TestCobrowseSessionActions_NavigateSendsPageNavigate(t *testing.T) {
	m, st, f := startSessionOnFakeCDP(t)
	if _, err := m.Navigate(st.ID, "https://example.com/path"); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	calls := f.callsFor("Page.navigate")
	if len(calls) != 1 {
		t.Fatalf("expected exactly 1 Page.navigate call, got %d", len(calls))
	}
	if calls[0].Params["url"] != "https://example.com/path" {
		t.Errorf("Page.navigate url = %v, want https://example.com/path", calls[0].Params["url"])
	}
}

func TestCobrowseSessionActions_ScreenshotReturnsData(t *testing.T) {
	m, st, f := startSessionOnFakeCDP(t)
	want := base64.StdEncoding.EncodeToString([]byte("fake-png-bytes"))
	f.mu.Lock()
	f.screenshotData = want
	f.mu.Unlock()

	got, err := m.Screenshot(st.ID)
	if err != nil {
		t.Fatalf("screenshot: %v", err)
	}
	if got != want {
		t.Errorf("screenshot data = %q, want %q", got, want)
	}
}

func TestCobrowseSessionActions_ScreenshotRejectsEmptyData(t *testing.T) {
	m, st, _ := startSessionOnFakeCDP(t)
	if _, err := m.Screenshot(st.ID); err == nil {
		t.Fatal("expected error for empty screenshot data")
	}
}

func TestCobrowseSessionActions_ClickBySelectorResolvesCenterThenClicks(t *testing.T) {
	m, st, f := startSessionOnFakeCDP(t)
	f.mu.Lock()
	f.evaluateValue = func(expr string) any {
		return map[string]any{"x": 111.0, "y": 222.0}
	}
	f.mu.Unlock()

	if _, err := m.Click(st.ID, "#submit", nil, nil); err != nil {
		t.Fatalf("click: %v", err)
	}

	evalCalls := f.callsFor("Runtime.evaluate")
	if len(evalCalls) != 1 {
		t.Fatalf("expected exactly 1 Runtime.evaluate call, got %d", len(evalCalls))
	}

	mouseCalls := f.callsFor("Input.dispatchMouseEvent")
	if len(mouseCalls) != 3 {
		t.Fatalf("expected 3 mouse events (move/press/release), got %d", len(mouseCalls))
	}
	wantTypes := []string{"mouseMoved", "mousePressed", "mouseReleased"}
	for i, c := range mouseCalls {
		if c.Params["type"] != wantTypes[i] {
			t.Errorf("mouse event %d type = %v, want %v", i, c.Params["type"], wantTypes[i])
		}
		if c.Params["x"] != 111.0 || c.Params["y"] != 222.0 {
			t.Errorf("mouse event %d coords = (%v,%v), want (111,222)", i, c.Params["x"], c.Params["y"])
		}
	}
}

func TestCobrowseSessionActions_ClickByCoordsSkipsSelectorResolution(t *testing.T) {
	m, st, f := startSessionOnFakeCDP(t)
	x, y := 5.0, 6.0
	if _, err := m.Click(st.ID, "", &x, &y); err != nil {
		t.Fatalf("click: %v", err)
	}
	if calls := f.callsFor("Runtime.evaluate"); len(calls) != 0 {
		t.Errorf("click by coords should not evaluate a selector, got %d calls", len(calls))
	}
	mouseCalls := f.callsFor("Input.dispatchMouseEvent")
	if len(mouseCalls) != 3 {
		t.Fatalf("expected 3 mouse events, got %d", len(mouseCalls))
	}
	if mouseCalls[1].Params["x"] != 5.0 || mouseCalls[1].Params["y"] != 6.0 {
		t.Errorf("mousePressed coords = (%v,%v), want (5,6)", mouseCalls[1].Params["x"], mouseCalls[1].Params["y"])
	}
}

func TestCobrowseSessionActions_ClickSelectorNotFoundErrors(t *testing.T) {
	m, st, f := startSessionOnFakeCDP(t)
	f.mu.Lock()
	f.evaluateValue = func(expr string) any { return nil } // querySelector found nothing
	f.mu.Unlock()

	if _, err := m.Click(st.ID, "#missing", nil, nil); err == nil {
		t.Fatal("expected error when the selector matches nothing")
	}
	if calls := f.callsFor("Input.dispatchMouseEvent"); len(calls) != 0 {
		t.Errorf("no mouse event should be dispatched when the selector is not found, got %d", len(calls))
	}
}

func TestCobrowseSessionActions_TypeSendsInsertText(t *testing.T) {
	m, st, f := startSessionOnFakeCDP(t)
	if _, err := m.Type(st.ID, "hello world"); err != nil {
		t.Fatalf("type: %v", err)
	}
	calls := f.callsFor("Input.insertText")
	if len(calls) != 1 {
		t.Fatalf("expected exactly 1 Input.insertText call, got %d", len(calls))
	}
	if calls[0].Params["text"] != "hello world" {
		t.Errorf("insertText text = %v, want %q", calls[0].Params["text"], "hello world")
	}
}

func TestCobrowseSessionActions_SelectUsesPageSetterAndReportsValue(t *testing.T) {
	m, st, f := startSessionOnFakeCDP(t)
	f.mu.Lock()
	f.evaluateValue = func(expr string) any {
		if !strings.Contains(expr, "HTMLSelectElement.prototype") || !strings.Contains(expr, "dispatchEvent(new Event(\"change\"") {
			t.Errorf("select must set the native value and fire change, got %q", expr)
		}
		if !strings.Contains(expr, `if (el.matches(":disabled") || !option`) {
			t.Errorf("select must reject effective disabled state, including fieldset inheritance, got %q", expr)
		}
		return map[string]any{"value": "VT", "label": "Vermont"}
	}
	f.mu.Unlock()
	got, err := m.Select(st.ID, "#state", "VT", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Value != "VT" || got.Label != "Vermont" {
		t.Fatalf("selected = %#v", got)
	}
	if len(f.callsFor("Input.dispatchMouseEvent")) != 0 {
		t.Fatal("native select must not click the popup")
	}
}

func TestCobrowseSessionActions_SelectRespectsFieldsetDisabledState(t *testing.T) {
	chrome, err := exec.LookPath("google-chrome")
	if err != nil {
		chrome, err = exec.LookPath("chromium")
	}
	if err != nil {
		t.Skip("local Chromium is unavailable")
	}

	m, st, f := startSessionOnFakeCDP(t)
	var expressions []string
	f.mu.Lock()
	f.evaluateValue = func(expr string) any {
		expressions = append(expressions, expr)
		return map[string]any{"value": "VT", "label": "Vermont"}
	}
	f.mu.Unlock()
	for _, selector := range []string{"#blocked", "#allowed"} {
		if _, err := m.Select(st.ID, selector, "VT", ""); err != nil {
			t.Fatal(err)
		}
	}
	if len(expressions) != 2 {
		t.Fatalf("captured %d select expressions, want 2", len(expressions))
	}

	page := fmt.Sprintf(`<html><body>
	<fieldset disabled>
	  <legend><select id="allowed"><option value="NY">New York</option><option value="VT">Vermont</option></select></legend>
	  <select id="blocked"><option value="NY">New York</option><option value="VT">Vermont</option></select>
	</fieldset>
	<script>
	const blocked = %s;
	const allowed = %s;
	document.body.dataset.testResult = [blocked.error || "", allowed.value || "", document.getElementById("blocked").value, document.getElementById("allowed").value].join("|");
	</script></body></html>`, expressions[0], expressions[1])
	path := filepath.Join(t.TempDir(), "select.html")
	if err := os.WriteFile(path, []byte(page), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	pageURL := (&url.URL{Scheme: "file", Path: path}).String()
	output, err := exec.CommandContext(ctx, chrome, "--headless", "--no-sandbox", "--disable-gpu", "--disable-dev-shm-usage", "--dump-dom", pageURL).CombinedOutput()
	if err != nil {
		t.Fatalf("local Chromium failed: %v: %s", err, output)
	}
	if !strings.Contains(string(output), `data-test-result="enabled option not found|VT|NY|VT"`) {
		t.Fatalf("fieldset-disabled select changed or first-legend select was blocked: %s", output)
	}
}

func TestCobrowseSessionActions_KeyAndScrollSendRealInput(t *testing.T) {
	m, st, f := startSessionOnFakeCDP(t)
	if _, err := m.Key(st.ID, "ArrowDown"); err != nil {
		t.Fatal(err)
	}
	keys := f.callsFor("Input.dispatchKeyEvent")
	if len(keys) != 2 || keys[0].Params["type"] != "rawKeyDown" || keys[1].Params["type"] != "keyUp" {
		t.Fatalf("key events = %#v", keys)
	}
	x, y := 10.0, 20.0
	if _, err := m.Scroll(st.ID, "", &x, &y, 0, 180); err != nil {
		t.Fatal(err)
	}
	mouse := f.callsFor("Input.dispatchMouseEvent")
	if len(mouse) != 1 || mouse[0].Params["type"] != "mouseWheel" || mouse[0].Params["deltaY"] != float64(180) {
		t.Fatalf("wheel = %#v", mouse)
	}
}

func TestCobrowseSessionActions_ExtractReturnsTextAndAttrs(t *testing.T) {
	m, st, f := startSessionOnFakeCDP(t)
	f.mu.Lock()
	f.evaluateValue = func(expr string) any {
		return map[string]any{
			"text":  "Click me",
			"attrs": map[string]any{"href": "https://example.com", "id": "cta"},
		}
	}
	f.mu.Unlock()

	got, err := m.Extract(st.ID, "a.cta", []string{"href", "id"})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if got.Text != "Click me" {
		t.Errorf("extract text = %q, want %q", got.Text, "Click me")
	}
	if got.Attrs["href"] != "https://example.com" {
		t.Errorf("extract attrs[href] = %q, want %q", got.Attrs["href"], "https://example.com")
	}
	if got.Attrs["id"] != "cta" {
		t.Errorf("extract attrs[id] = %q, want %q", got.Attrs["id"], "cta")
	}
}

func TestCobrowseSessionActions_ExtractNotFoundErrors(t *testing.T) {
	m, st, f := startSessionOnFakeCDP(t)
	f.mu.Lock()
	f.evaluateValue = func(expr string) any { return nil }
	f.mu.Unlock()

	if _, err := m.Extract(st.ID, "#missing", nil); err == nil {
		t.Fatal("expected error when the selector matches nothing")
	}
}

// TestCobrowseSessionActions_SelectorEmbeddingIsSafe pins that a selector
// containing characters that would break naive string concatenation (quotes,
// backslashes) is embedded safely via JSON encoding, not string-formatted
// directly into the JS expression.
func TestCobrowseSessionActions_SelectorEmbeddingIsSafe(t *testing.T) {
	tricky := `div[data-x="a\"b"]`
	lit := jsStringLiteral(tricky)
	var decoded string
	if err := json.Unmarshal([]byte(lit), &decoded); err != nil {
		t.Fatalf("jsStringLiteral produced invalid JSON/JS string literal: %v (%s)", err, lit)
	}
	if decoded != tricky {
		t.Errorf("round-trip mismatch: got %q, want %q", decoded, tricky)
	}
}
