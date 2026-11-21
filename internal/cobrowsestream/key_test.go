package cobrowsestream

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

// A viewer Enter, however it arrives, goes to CDP as a real key press: keyDown
// with text "\r", code Enter and virtual key 13. A rawKeyDown never produces the
// keypress that submits a form (#1778).
func TestKeyParamsEnterIsARealKeyPress(t *testing.T) {
	for _, ev := range []string{"rawKeyDown", "keyDown"} {
		p := keyParams(InputMessage{Type: InputTypeKey, KeyEvent: ev, Key: "Enter", Code: "Enter", KeyCode: 13})
		if p["type"] != "keyDown" || p["text"] != "\r" || p["code"] != "Enter" || p["windowsVirtualKeyCode"] != 13 {
			t.Errorf("%s: %+v", ev, p)
		}
	}
	up := keyParams(InputMessage{Type: InputTypeKey, KeyEvent: "keyUp", Key: "Enter", Code: "Enter", KeyCode: 13})
	if up["type"] != "keyUp" || up["text"] != nil {
		t.Errorf("keyUp: %+v", up)
	}
	ctrl := keyParams(InputMessage{Type: InputTypeKey, KeyEvent: "rawKeyDown", Key: "Enter", Code: "Enter", KeyCode: 13, Modifiers: 2})
	if ctrl["type"] != "rawKeyDown" || ctrl["text"] != nil {
		t.Errorf("a chorded Enter is left alone: %+v", ctrl)
	}
	for _, mod := range []int{8, 1} {
		p := keyParams(InputMessage{Type: InputTypeKey, KeyEvent: "rawKeyDown", Key: "Enter", Code: "Enter", KeyCode: 13, Modifiers: mod})
		if mod == 8 && (p["type"] != "keyDown" || p["text"] != "\r") {
			t.Errorf("Shift+Enter is a real press: %+v", p)
		}
		if mod == 1 && p["type"] != "rawKeyDown" {
			t.Errorf("Alt+Enter is left alone: %+v", p)
		}
	}
	other := keyParams(InputMessage{Type: InputTypeKey, KeyEvent: "rawKeyDown", Key: "Tab", Code: "Tab", KeyCode: 9})
	if other["type"] != "rawKeyDown" || other["text"] != nil {
		t.Errorf("other keys are unchanged: %+v", other)
	}
}

// TestViewerEnterInRealBrowser sends the viewer's Enter through the
// stream's own CDP key path into a real browser: Enter in a focused field
// submits the form.
func TestViewerEnterInRealBrowser(t *testing.T) {
	if os.Getenv("CITADEL_COBROWSE_BROWSER_IT") == "" && os.Getenv("CITADEL_REAL_BROWSER_TESTS") == "" {
		t.Skip("set CITADEL_COBROWSE_BROWSER_IT=1 (or CITADEL_REAL_BROWSER_TESTS=1) to run the real-browser Enter test")
	}
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if r.URL.Path == "/login" {
			fmt.Fprint(w, `<!doctype html><h1 id="t">submitted</h1>`)
			return
		}
		fmt.Fprint(w, `<!doctype html><h1 id="t">form</h1><form method="get" action="/login"><input id="u" name="u" value="x"><input id="p" type="password" name="p"><button type="submit">Login</button></form>`)
	}))
	defer page.Close()
	m := platform.NewCobrowseSessionManager(filepath.Join(t.TempDir(), "sessions"), 1)
	m.SetLaunchOptions(platform.LaunchOptions{Quiet: true})
	defer m.StopAll()
	st, err := m.StartSession(page.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	read := func() string {
		out, err := m.ExtractBounded(st.ID, "#t", nil, 20)
		if err != nil {
			return ""
		}
		return out.Text
	}
	wait := func(want string) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for read() != want {
			if time.Now().After(deadline) {
				t.Fatalf("page never showed %q (got %q)", want, read())
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	wait("form")
	if _, err := m.Click(st.ID, "#p", nil, nil); err != nil {
		t.Fatal(err)
	}
	cur, ok := m.SessionStatus(st.ID)
	if !ok || cur.DebugPort == 0 {
		t.Fatalf("no debug port: %+v", cur)
	}
	cdp, err := dialCDP(cur.DebugPort, func(screencastFrame) {})
	if err != nil {
		t.Fatal(err)
	}
	defer cdp.Close()
	// Exactly what the viewer sends for Enter.
	if err := cdp.dispatchKey(InputMessage{Type: InputTypeKey, KeyEvent: "keyDown", Key: "Enter", Code: "Enter", Text: "\r", KeyCode: 13}); err != nil {
		t.Fatal(err)
	}
	if err := cdp.dispatchKey(InputMessage{Type: InputTypeKey, KeyEvent: "keyUp", Key: "Enter", Code: "Enter", KeyCode: 13}); err != nil {
		t.Fatal(err)
	}
	wait("submitted")
}
