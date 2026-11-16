// Package localgate is the CI end-to-end of the local-mode gate (#1683).
//
// It builds the real citadel binary and drives `citadel local mcp` over stdio
// with a scripted MCP client, the way an agent app would. The human is a set of
// scripted requests to the viewer's own HTTP routes and stream WebSocket. The
// page is a local httptest login page; no internet and no AI model is used.
package localgate

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"image/jpeg"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const loginPage = `<!doctype html><title>Login</title>
<h1 id="title" style="position:fixed;top:60vh;margin:0">Sign in</h1>
<form method="post" action="/login"><input id="user" name="user" autocomplete="off"
 style="position:fixed;left:0;top:0;width:100vw;height:40vh;box-sizing:border-box;font-size:32px"></form>`

const homePage = `<!doctype html><title>Home</title>
<h1 id="welcome">Welcome, ada</h1><div id="flash">Signed in</div>`

func loginServer() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, loginPage)
	})
	mux.HandleFunc("POST /login", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.PostForm.Get("user") != "ada" {
			http.Redirect(w, r, "/?failed=1", http.StatusSeeOther)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "gate", Value: "1", Path: "/"})
		http.Redirect(w, r, "/home", http.StatusSeeOther)
	})
	mux.HandleFunc("GET /home", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("gate"); err != nil || c.Value != "1" {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, homePage)
	})
	return httptest.NewServer(mux)
}

// mcpClient is a minimal scripted MCP client over a child process's stdio.
type mcpClient struct {
	t      *testing.T
	cmd    *exec.Cmd
	in     io.WriteCloser
	mu     sync.Mutex
	nextID int
	pend   map[int]chan map[string]any
	stderr *bytes.Buffer
	done   chan struct{}
}

func startMCP(t *testing.T, exe string, env []string) *mcpClient {
	t.Helper()
	c := &mcpClient{t: t, pend: map[int]chan map[string]any{}, stderr: &bytes.Buffer{}, done: make(chan struct{})}
	c.cmd = exec.Command(exe, "local", "mcp")
	c.cmd.Env = env
	c.cmd.Stderr = &lockedWriter{w: c.stderr}
	in, err := c.cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	c.in = in
	out, err := c.cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		defer close(c.done)
		sc := bufio.NewScanner(out)
		sc.Buffer(make([]byte, 1<<20), 16<<20)
		for sc.Scan() {
			var m map[string]any
			if json.Unmarshal(sc.Bytes(), &m) != nil {
				continue
			}
			id, ok := m["id"].(float64)
			if !ok {
				continue
			}
			c.mu.Lock()
			ch := c.pend[int(id)]
			delete(c.pend, int(id))
			c.mu.Unlock()
			if ch != nil {
				ch <- m
			}
		}
	}()
	t.Cleanup(func() {
		_ = c.in.Close()
		select {
		case <-c.done:
		case <-time.After(15 * time.Second):
			_ = c.cmd.Process.Kill()
		}
		_ = c.cmd.Wait()
	})
	return c
}

type lockedWriter struct {
	mu sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// send writes a request and returns the channel its response arrives on.
func (c *mcpClient) send(method string, params any) chan map[string]any {
	c.t.Helper()
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan map[string]any, 1)
	c.pend[id] = ch
	c.mu.Unlock()
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if _, err := c.in.Write(append(b, '\n')); err != nil {
		c.t.Fatalf("write %s: %v", method, err)
	}
	return ch
}

func (c *mcpClient) await(ch chan map[string]any, what string) map[string]any {
	c.t.Helper()
	select {
	case m := <-ch:
		return m
	case <-time.After(90 * time.Second):
		c.t.Fatalf("no response to %s\nserver stderr:\n%s", what, c.stderr.String())
		return nil
	}
}

// toolResult is one tools/call outcome.
type toolResult struct {
	IsError bool
	Text    string
}

func (c *mcpClient) parseTool(resp map[string]any) toolResult {
	c.t.Helper()
	res, ok := resp["result"].(map[string]any)
	if !ok {
		c.t.Fatalf("no result in %v", resp)
	}
	var r toolResult
	r.IsError, _ = res["isError"].(bool)
	if content, _ := res["content"].([]any); len(content) > 0 {
		m, _ := content[0].(map[string]any)
		r.Text, _ = m["text"].(string)
	}
	return r
}

func (c *mcpClient) callAsync(tool string, args map[string]any) chan map[string]any {
	return c.send("tools/call", map[string]any{"name": tool, "arguments": args})
}

func (c *mcpClient) call(tool string, args map[string]any) toolResult {
	c.t.Helper()
	return c.parseTool(c.await(c.callAsync(tool, args), tool))
}

// ok calls a tool and requires success, returning its JSON object.
func (c *mcpClient) ok(tool string, args map[string]any) map[string]any {
	c.t.Helper()
	r := c.call(tool, args)
	if r.IsError {
		c.t.Fatalf("%s failed: %s\nserver stderr:\n%s", tool, r.Text, c.stderr.String())
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(r.Text), &m); err != nil {
		c.t.Fatalf("%s result is not JSON: %q", tool, r.Text)
	}
	return m
}

// viewerClient is the scripted human: it talks to the viewer's real routes.
type viewerClient struct {
	t      *testing.T
	base   string
	host   string
	cookie *http.Cookie
	bearer string
}

var bearerRE = regexp.MustCompile(`<meta name="bearer" content="([0-9a-f]+)"`)
var refreshRE = regexp.MustCompile(`url=(http://[^"]+)"`)

// openViewer reads the owner-only one-time launch file the way the browser is
// pointed at it, and exchanges the single-use code at /open for the cookie and
// the bearer.
func openViewer(t *testing.T, launchFile string) *viewerClient {
	t.Helper()
	page, err := os.ReadFile(launchFile)
	if err != nil {
		t.Fatalf("read launch file: %v", err)
	}
	m := refreshRE.FindSubmatch(page)
	if m == nil {
		t.Fatalf("launch file has no link: %q", page)
	}
	link, err := url.Parse(string(m[1]))
	if err != nil || link.Path != "/open" {
		t.Fatalf("launch link %q: %v", m[1], err)
	}
	v := &viewerClient{t: t, base: link.Scheme + "://" + link.Host, host: link.Host}
	req, _ := http.NewRequest("GET", link.String(), nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("/open: %d %s", resp.StatusCode, body)
	}
	if len(resp.Cookies()) != 1 {
		t.Fatalf("/open must set exactly the viewer cookie, got %v", resp.Cookies())
	}
	v.cookie = resp.Cookies()[0]
	b := bearerRE.FindSubmatch(body)
	if b == nil {
		t.Fatalf("/open page carries no bearer: %s", body)
	}
	v.bearer = string(b[1])
	// The code is single use and the launch file is removed once it is.
	if _, err := os.Stat(launchFile); !os.IsNotExist(err) {
		t.Errorf("the launch file must be removed once its code is used")
	}
	reuse, err := (&http.Client{}).Get(link.String())
	if err == nil {
		reuse.Body.Close()
		if reuse.StatusCode != http.StatusForbidden {
			t.Errorf("a used one-time code must be refused, got %d", reuse.StatusCode)
		}
	}
	return v
}

func (v *viewerClient) do(method, path string, body any) (*http.Response, []byte) {
	v.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, v.base+path, rd)
	req.AddCookie(v.cookie)
	req.Header.Set("X-Citadel-CSRF", v.bearer)
	req.Header.Set("Origin", v.base)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		v.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

type viewerSession struct {
	ID          string `json:"session_id"`
	State       string `json:"state"`
	Driver      string `json:"driver"`
	Domain      string `json:"domain"`
	AskedDomain string `json:"asked_domain"`
	Approval    string `json:"approval"`
}

func (v *viewerClient) session(id string) viewerSession {
	v.t.Helper()
	resp, b := v.do("GET", "/api/state", nil)
	if resp.StatusCode != 200 {
		v.t.Fatalf("/api/state: %d %s", resp.StatusCode, b)
	}
	var st struct {
		Sessions []viewerSession `json:"sessions"`
	}
	if err := json.Unmarshal(b, &st); err != nil {
		v.t.Fatal(err)
	}
	for _, s := range st.Sessions {
		if s.ID == id {
			return s
		}
	}
	v.t.Fatalf("session %s not in viewer state: %s", id, b)
	return viewerSession{}
}

// action posts to /api/sessions/{id}/{action}, as the viewer page does.
func (v *viewerClient) action(id, action string, body any) {
	v.t.Helper()
	if body == nil {
		body = map[string]any{}
	}
	resp, b := v.do("POST", "/api/sessions/"+id+"/"+action, body)
	if resp.StatusCode != 200 {
		v.t.Fatalf("%s: %d %s", action, resp.StatusCode, b)
	}
}

// stream opens the viewer's stream WebSocket and waits for the first screencast
// frame, whose size the human's normalized pointer coordinates map onto.
type stream struct {
	t    *testing.T
	conn *websocket.Conn
}

func (v *viewerClient) openStream(id string) *stream {
	v.t.Helper()
	h := http.Header{}
	h.Set("Origin", v.base)
	h.Set("Cookie", v.cookie.String())
	u := "ws://" + v.host + "/api/stream?id=" + url.QueryEscape(id) + "&csrf=" + v.bearer
	conn, resp, err := websocket.DefaultDialer.Dial(u, h)
	if err != nil {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		v.t.Fatalf("stream dial: %v (status %d)", err, code)
	}
	v.t.Cleanup(func() { _ = conn.Close() })
	s := &stream{t: v.t, conn: conn}
	deadline := time.Now().Add(30 * time.Second)
	sawInit := false
	for time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(deadline)
		mt, data, err := conn.ReadMessage()
		if err != nil {
			v.t.Fatalf("stream read before the first frame: %v", err)
		}
		if mt == websocket.TextMessage {
			var m struct{ Type string }
			_ = json.Unmarshal(data, &m)
			if m.Type == "init" {
				sawInit = true
			}
			continue
		}
		if !sawInit {
			v.t.Fatal("a frame arrived before the init message")
		}
		if _, err := jpeg.DecodeConfig(bytes.NewReader(data)); err != nil {
			v.t.Fatalf("first frame is not a JPEG: %v", err)
		}
		// Keep draining so the server's writer never blocks on us.
		go func() {
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()
		return s
	}
	v.t.Fatal("no screencast frame within 30s")
	return nil
}

func (s *stream) send(m map[string]any) {
	s.t.Helper()
	if err := s.conn.WriteJSON(m); err != nil {
		s.t.Fatalf("stream write: %v", err)
	}
}

func (s *stream) click(x, y float64) {
	s.send(map[string]any{"type": "mouse", "event": "mouseMoved", "x": x, "y": y})
	s.send(map[string]any{"type": "mouse", "event": "mousePressed", "x": x, "y": y, "button": "left", "buttons": 1, "clickCount": 1})
	s.send(map[string]any{"type": "mouse", "event": "mouseReleased", "x": x, "y": y, "button": "left", "clickCount": 1})
}

func (s *stream) typeText(text string) {
	for _, r := range text {
		ch := string(r)
		s.send(map[string]any{"type": "key", "keyEvent": "keyDown", "key": ch, "text": ch})
		s.send(map[string]any{"type": "key", "keyEvent": "keyUp", "key": ch})
	}
}

func (s *stream) enter() {
	s.send(map[string]any{"type": "key", "keyEvent": "keyDown", "key": "Enter", "code": "Enter", "keyCode": 13, "text": "\r"})
	s.send(map[string]any{"type": "key", "keyEvent": "keyUp", "key": "Enter", "code": "Enter", "keyCode": 13})
}

// sandboxEnv is the child environment: the parent's, minus every location the
// user's own config could come from, with each pointed into the temp dir.
func sandboxEnv(home string, extra ...string) []string {
	drop := map[string]bool{
		"HOME": true, "USERPROFILE": true, "APPDATA": true, "LOCALAPPDATA": true,
		"XDG_CONFIG_HOME": true, "XDG_DATA_HOME": true, "XDG_CACHE_HOME": true, "XDG_STATE_HOME": true,
		"COPILOT_HOME": true, "CODEX_HOME": true, "SUDO_USER": true, "TMP": true, "TEMP": true, "TMPDIR": true,
		"CITADEL_LOCAL_AUDIT_RETENTION_DAYS": true,
	}
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if !drop[strings.ToUpper(k)] && !drop[k] {
			env = append(env, kv)
		}
	}
	tmp := filepath.Join(home, "tmp")
	_ = os.MkdirAll(tmp, 0o700)
	env = append(env,
		"HOME="+home, "USERPROFILE="+home,
		"APPDATA="+filepath.Join(home, "AppData", "Roaming"),
		"LOCALAPPDATA="+filepath.Join(home, "AppData", "Local"),
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		"XDG_DATA_HOME="+filepath.Join(home, ".local", "share"),
		"XDG_CACHE_HOME="+filepath.Join(home, ".cache"),
		"TMP="+tmp, "TEMP="+tmp, "TMPDIR="+tmp,
	)
	return append(env, extra...)
}

func runCLI(t *testing.T, exe string, env []string, stdin string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(exe, args...)
	cmd.Env = env
	cmd.Stdin = strings.NewReader(stdin)
	done := make(chan struct{})
	var out []byte
	var err error
	go func() { out, err = cmd.CombinedOutput(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Minute):
		_ = cmd.Process.Kill()
		<-done
		t.Fatalf("%v timed out:\n%s", args, out)
	}
	return string(out), err
}

func TestLocalModeGateThroughMCP(t *testing.T) {
	if os.Getenv("CITADEL_COBROWSE_BROWSER_IT") == "" && os.Getenv("CITADEL_REAL_BROWSER_TESTS") == "" {
		t.Skip("set CITADEL_COBROWSE_BROWSER_IT=1 (or CITADEL_REAL_BROWSER_TESTS=1) to run the local-mode gate end to end")
	}

	// The binary is built with the citadel_e2e tag, which only adds a seam that
	// resolves one made-up page name to a public-looking address and redirects
	// the egress guard's final dial to the test server. The URL policy and the
	// guard are the production ones.
	exe := filepath.Join(t.TempDir(), "citadel")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	if out, err := exec.Command("go", "build", "-tags", "citadel_e2e", "-o", exe,
		"github.com/aceteam-ai/citadel-cli/cmd/citadel").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	page := loginServer()
	defer page.Close()
	pageURL, _ := url.Parse(page.URL)
	_, pagePort, _ := net.SplitHostPort(pageURL.Host)
	target := "http://gate.test:" + pagePort + "/"

	home := t.TempDir()
	opened := filepath.Join(home, "opened.txt")
	env := sandboxEnv(home,
		"CITADEL_E2E_PAGE_ADDR="+pageURL.Host,
		"CITADEL_E2E_OPENED_FILE="+opened,
	)

	// Step 1: harness config and doctor, under the temp HOME.
	out, err := runCLI(t, exe, env, "y\n", "local", "config", "copilot", "--write")
	if err != nil {
		t.Fatalf("local config copilot --write: %v\n%s", err, out)
	}
	cfgPath := filepath.Join(home, ".copilot", "mcp-config.json")
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("the harness config was not written: %v\n%s", err, out)
	}
	var cfg struct {
		MCPServers map[string]struct {
			Type    string   `json:"type"`
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	entry, ok := cfg.MCPServers["citadel"]
	if !ok || strings.Join(entry.Args, " ") != "local mcp" || filepath.Base(entry.Command) != filepath.Base(exe) {
		t.Fatalf("copilot config entry wrong: %s", raw)
	}
	if out, err := runCLI(t, exe, env, "", "local", "doctor"); err != nil {
		t.Fatalf("local doctor: %v\n%s", err, out)
	}

	// Step 2: start.
	c := startMCP(t, exe, env)
	init := c.await(c.send("initialize", map[string]any{
		"protocolVersion": "2025-03-26", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "scripted-gate", "version": "1"},
	}), "initialize")
	if init["error"] != nil {
		t.Fatalf("initialize: %v", init)
	}
	start := c.ok("local_cobrowse_start", map[string]any{"url": target, "goal": "log in"})
	id, _ := start["session_id"].(string)
	if id == "" {
		t.Fatalf("start: %v", start)
	}
	var title map[string]any
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(300 * time.Millisecond) {
		r := c.call("local_cobrowse_extract", map[string]any{"session_id": id, "selector": "#title"})
		if !r.IsError && json.Unmarshal([]byte(r.Text), &title) == nil && title["text"] == "Sign in" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the login page never loaded: %s\nstderr:\n%s", r.Text, c.stderr.String())
		}
	}

	// Step 3: default deny. The click is a write and nothing is approved yet.
	denied := c.call("local_cobrowse_click", map[string]any{"session_id": id, "selector": "#user"})
	if !denied.IsError || !strings.Contains(denied.Text, "approval_required") {
		t.Fatalf("the first click must be refused with approval_required, got error=%v %q", denied.IsError, denied.Text)
	}

	// Step 4: the human approves in the viewer, through its own routes.
	var launch string
	for deadline := time.Now().Add(20 * time.Second); launch == "" && time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if b, err := os.ReadFile(opened); err == nil {
			launch = strings.TrimSpace(strings.Split(strings.TrimSpace(string(b)), "\n")[0])
		}
	}
	if launch == "" {
		t.Fatalf("the viewer was never opened through a launch file\nstderr:\n%s", c.stderr.String())
	}
	viewer := openViewer(t, launch)
	s := viewer.session(id)
	if s.Approval == "approved" {
		t.Fatal("the session must not be approved before the human approves")
	}
	domain := s.AskedDomain
	if domain == "" {
		domain = s.Domain
	}
	viewer.action(id, "approve", map[string]any{"hours": 1, "domain": domain})
	w := c.ok("local_cobrowse_wait", map[string]any{"session_id": id, "until": "approved", "timeout_seconds": 20})
	if w["satisfied"] != true {
		t.Fatalf("wait approved: %v", w)
	}
	clicked := c.ok("local_cobrowse_click", map[string]any{"session_id": id, "selector": "#user"})
	if clicked["dispatched"] != true {
		t.Fatalf("the approved click must dispatch: %v", clicked)
	}

	// Step 5: handoff. The agent is fenced out, the human drives.
	if h := c.ok("local_cobrowse_handoff", map[string]any{"session_id": id, "reason": "log in, then hand back"}); h["session_id"] != id {
		t.Fatalf("handoff: %v", h)
	}
	fenced := c.call("local_cobrowse_click", map[string]any{"session_id": id, "selector": "#user"})
	if !fenced.IsError {
		t.Fatalf("an agent click during a handoff must be refused: %q", fenced.Text)
	}
	if read := c.call("local_cobrowse_extract", map[string]any{"session_id": id, "selector": "#title"}); !read.IsError {
		t.Fatalf("an agent read during a handoff must be refused: %q", read.Text)
	}
	st := viewer.openStream(id)
	// The first click after the handoff, then typing, then submit: all through
	// the stream's input messages. (#1681's scroll of the viewer page is in
	// app.js and is covered by the page tests, not by this wire-level path.)
	st.click(0.5, 0.2)
	time.Sleep(300 * time.Millisecond)
	st.typeText("ada")
	st.enter()

	// Step 6: hand back while the agent is already waiting for it.
	waiting := c.callAsync("local_cobrowse_wait", map[string]any{"session_id": id, "until": "resumed", "timeout_seconds": 50})
	select {
	case m := <-waiting:
		t.Fatalf("wait resumed returned before the human handed back: %v", m)
	case <-time.After(1500 * time.Millisecond):
	}
	viewer.action(id, "hand-back", nil)
	var resumed map[string]any
	if r := c.parseTool(c.await(waiting, "wait resumed")); r.IsError || json.Unmarshal([]byte(r.Text), &resumed) != nil || resumed["satisfied"] != true {
		t.Fatalf("wait resumed: %+v", r)
	}

	// Step 7: the agent reads what the human logged in to.
	var welcome, flash map[string]any
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(300 * time.Millisecond) {
		welcome = c.ok("local_cobrowse_extract", map[string]any{"session_id": id, "selector": "#welcome"})
		if welcome["text"] == "Welcome, ada" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the human's typing did not log in (heading %v)", welcome)
		}
	}
	flash = c.ok("local_cobrowse_extract", map[string]any{"session_id": id, "selector": "#flash"})
	if flash["text"] != "Signed in" {
		t.Fatalf("flash: %v", flash)
	}

	// Step 8: stop, then the audit chain verifies.
	if stopped := c.ok("local_cobrowse_stop", map[string]any{"session_id": id}); stopped["stopped"] != true {
		t.Fatalf("stop: %v", stopped)
	}
	_ = c.in.Close()
	select {
	case <-c.done:
	case <-time.After(30 * time.Second):
		t.Fatal("citadel local mcp did not exit after stdin closed")
	}
	if out, err := runCLI(t, exe, env, "", "local", "audit", "--verify"); err != nil || !strings.Contains(out, "Audit log OK") {
		t.Fatalf("audit verify: %v\n%s", err, out)
	}
	if out, _ := runCLI(t, exe, env, "", "local", "audit", "--json", "--limit", "0"); !strings.Contains(out, id) {
		t.Fatalf("the audit log does not name the session:\n%s", out)
	}
}
