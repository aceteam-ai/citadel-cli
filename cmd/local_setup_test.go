package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/localviewer"
	"github.com/spf13/cobra"
)

// setupFixture is a started setup server plus a client that has exchanged the
// one-time launch file for a cookie and bearer, like the user's browser.
type setupFixture struct {
	t      *testing.T
	srv    *localviewer.Server
	base   string
	cookie *http.Cookie
	bearer string
	home   string
	exe    string
	opened []string
	full   int
}

func newSetupFixture(t *testing.T) *setupFixture {
	t.Helper()
	f := &setupFixture{t: t, home: t.TempDir(), exe: `C:\Users\demo\AppData\Local\Citadel\citadel.exe`}
	env := harnessEnv{goos: "linux", home: f.home}
	d := localSetupDeps{
		stateDir: func() string { return filepath.Join(f.home, "state") },
		env:      func() harnessEnv { return env },
		exe:      f.exe,
		openURL:  func(u string) error { f.opened = append(f.opened, u); return nil },
		now:      func() time.Time { return time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC) },
		doctor: func(full bool) []localviewer.SetupCheck {
			if full {
				f.full++
			}
			return []localviewer.SetupCheck{
				{Status: "ok", What: "Browser", Detail: "Microsoft Edge at C:\\edge.exe"},
				{Status: "FAIL", What: "Viewer port", Detail: "cannot listen. Fix: check security software"},
			}
		},
	}
	f.srv = newLocalSetupServer(d)
	if err := f.srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.srv.Close)
	if err := f.srv.Open(); err != nil || len(f.opened) != 1 {
		t.Fatalf("open: %v %v", err, f.opened)
	}
	launch, _ := os.ReadFile(f.opened[0])
	link := regexp.MustCompile(`http://127\.0\.0\.1:\d+/open\?code=[0-9a-f]+`).FindString(string(launch))
	if link == "" || strings.Contains(f.opened[0], "code=") {
		t.Fatal("the code must be only in the owner-only launch file")
	}
	f.base = link[:strings.Index(link, "/open")]
	resp, err := http.Get(link)
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	m := regexp.MustCompile(`name="bearer" content="([0-9a-f]+)"`).FindSubmatch(page)
	if resp.StatusCode != 200 || len(resp.Cookies()) != 1 || m == nil {
		t.Fatalf("exchange: %d", resp.StatusCode)
	}
	f.cookie, f.bearer = resp.Cookies()[0], string(m[1])
	return f
}

func (f *setupFixture) do(method, path, body string, hdr map[string]string, cookie bool) *http.Response {
	f.t.Helper()
	req, _ := http.NewRequest(method, f.base+path, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	if cookie {
		req.AddCookie(f.cookie)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func (f *setupFixture) good() map[string]string {
	return map[string]string{"Origin": f.base, "X-Citadel-CSRF": f.bearer, "Content-Type": "application/json"}
}

func TestSetupRoutesAreGuarded(t *testing.T) {
	f := newSetupFixture(t)
	if err := os.MkdirAll(filepath.Join(f.home, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	codex := filepath.Join(f.home, ".codex", "config.toml")
	body := `{"confirm":true,"path":` + jsonString(codex) + `}`
	with := func(k, v string) map[string]string {
		h := f.good()
		if v == "" {
			delete(h, k)
		} else {
			h[k] = v
		}
		return h
	}
	cases := []struct {
		name, method, path, body string
		hdr                      map[string]string
		cookie                   bool
	}{
		{"state without bearer", "GET", "/api/setup/state", "", with("X-Citadel-CSRF", ""), true},
		{"state with wrong bearer", "GET", "/api/setup/state", "", with("X-Citadel-CSRF", "nope"), true},
		{"state with foreign origin", "GET", "/api/setup/state", "", with("Origin", "http://attacker.test"), true},
		{"state without cookie", "GET", "/api/setup/state", "", f.good(), false},
		{"connect without bearer", "POST", "/api/setup/connect/codex", body, with("X-Citadel-CSRF", ""), true},
		{"connect with foreign origin", "POST", "/api/setup/connect/codex", body, with("Origin", "http://attacker.test"), true},
		{"connect without origin", "POST", "/api/setup/connect/codex", body, with("Origin", ""), true},
		{"connect cross-site", "POST", "/api/setup/connect/codex", body, with("Sec-Fetch-Site", "cross-site"), true},
		{"connect without cookie", "POST", "/api/setup/connect/codex", body, f.good(), false},
		{"doctor without bearer", "POST", "/api/setup/doctor", "{}", with("X-Citadel-CSRF", ""), true},
		{"open-viewer without bearer", "POST", "/api/setup/open-viewer", "{}", with("X-Citadel-CSRF", ""), true},
		{"wrong host", "GET", "/api/setup/state", "", f.good(), true},
	}
	for _, c := range cases {
		req, _ := http.NewRequest(c.method, f.base+c.path, strings.NewReader(c.body))
		for k, v := range c.hdr {
			req.Header.Set(k, v)
		}
		if c.cookie {
			req.AddCookie(f.cookie)
		}
		if c.name == "wrong host" {
			req.Host = "attacker.test"
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s: want 403, got %d", c.name, resp.StatusCode)
		}
	}
	if _, err := os.Stat(codex); err == nil {
		t.Fatal("a refused request wrote the config")
	}
	// The viewer routes do not exist on the setup server, and the old mint
	// route is not served here.
	if r := f.do("GET", "/api/state", "", f.good(), true); r.StatusCode != http.StatusNotFound {
		t.Errorf("viewer state route on the setup server: %d", r.StatusCode)
	}
}

func jsonString(s string) string { b, _ := json.Marshal(s); return string(b) }

func TestSetupConnectMergesAndBacksUpLikeConfigWrite(t *testing.T) {
	f := newSetupFixture(t)
	dir := filepath.Join(f.home, ".copilot")
	_ = os.MkdirAll(dir, 0o700)
	path := filepath.Join(dir, "mcp-config.json")
	orig := `{"theme":"dark","mcpServers":{"other":{"command":"x"}}}`
	_ = os.WriteFile(path, []byte(orig), 0o600)

	// The same file through the CLI path, for comparison.
	cliHome := t.TempDir()
	_ = os.MkdirAll(filepath.Join(cliHome, ".copilot"), 0o700)
	cliPath := filepath.Join(cliHome, ".copilot", "mcp-config.json")
	_ = os.WriteFile(cliPath, []byte(orig), 0o600)
	var out bytes.Buffer
	if err := runLocalConfig("copilot", f.exe, harnessEnv{goos: "linux", home: cliHome}, true, true, strings.NewReader(""), &out, time.Now()); err != nil {
		t.Fatal(err)
	}

	st := f.state()
	var app localviewer.SetupApp
	for _, a := range st.Apps {
		if a.Name == "copilot" {
			app = a
		}
	}
	if app.State != "available" || app.Path != path {
		t.Fatalf("copilot should be available at %s: %+v", path, app)
	}
	// A stale page (different path) is refused.
	r := f.do("POST", "/api/setup/connect/copilot", `{"confirm":true,"path":"/elsewhere"}`, f.good(), true)
	if r.StatusCode != http.StatusConflict {
		t.Fatalf("stale path: %d", r.StatusCode)
	}
	// So is a request that does not confirm.
	if r := f.do("POST", "/api/setup/connect/copilot", `{"path":`+jsonString(path)+`}`, f.good(), true); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("no confirm: %d", r.StatusCode)
	}
	r = f.do("POST", "/api/setup/connect/copilot", `{"confirm":true,"path":`+jsonString(path)+`}`, f.good(), true)
	var res localviewer.SetupConnectResult
	_ = json.NewDecoder(r.Body).Decode(&res)
	if r.StatusCode != 200 || res.Outcome != "written" || !res.Backup || !strings.Contains(res.NextStep, "restart GitHub Copilot CLI") {
		t.Fatalf("connect: %d %+v", r.StatusCode, res)
	}
	got, _ := os.ReadFile(path)
	want, _ := os.ReadFile(cliPath)
	if !jsonEq(got, want) {
		t.Fatalf("setup merge differs from local config --write:\n%s\n%s", got, want)
	}
	baks, _ := filepath.Glob(path + ".bak-*")
	if len(baks) != 1 {
		t.Fatalf("want one backup, got %v", baks)
	}
	if b, _ := os.ReadFile(baks[0]); string(b) != orig {
		t.Fatal("backup is not the original file")
	}
	// Again: nothing to change. The page now reads connected.
	r = f.do("POST", "/api/setup/connect/copilot", `{"confirm":true,"path":`+jsonString(path)+`}`, f.good(), true)
	_ = json.NewDecoder(r.Body).Decode(&res)
	if res.Outcome != "already" {
		t.Fatalf("second connect: %+v", res)
	}
}

func jsonEq(a, b []byte) bool {
	var x, y any
	return json.Unmarshal(a, &x) == nil && json.Unmarshal(b, &y) == nil && jsonString2(x) == jsonString2(y)
}
func jsonString2(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestSetupConnectDoesNotReplaceADifferentEntry(t *testing.T) {
	f := newSetupFixture(t)
	dir := filepath.Join(f.home, ".copilot")
	_ = os.MkdirAll(dir, 0o700)
	path := filepath.Join(dir, "mcp-config.json")
	orig := `{"mcpServers":{"citadel":{"command":"old"}}}`
	_ = os.WriteFile(path, []byte(orig), 0o600)
	r := f.do("POST", "/api/setup/connect/copilot", `{"confirm":true,"path":`+jsonString(path)+`}`, f.good(), true)
	var res localviewer.SetupConnectResult
	_ = json.NewDecoder(r.Body).Decode(&res)
	if res.Outcome != "declined" || !strings.Contains(res.Message, "local config copilot --write") {
		t.Fatalf("%+v", res)
	}
	if b, _ := os.ReadFile(path); string(b) != orig {
		t.Fatal("a different entry was replaced by a click that did not consent to it")
	}
}

func (f *setupFixture) state() (st struct {
	Checks       []localviewer.SetupCheck   `json:"checks"`
	Apps         []localviewer.SetupApp     `json:"apps"`
	Sessions     []localviewer.SetupSession `json:"sessions"`
	SessionsNote string                     `json:"sessions_note"`
	MaxSessions  int                        `json:"max_sessions"`
	GenericEntry string                     `json:"generic_entry"`
}) {
	f.t.Helper()
	r := f.do("GET", "/api/setup/state", "", f.good(), true)
	if r.StatusCode != 200 {
		f.t.Fatalf("state: %d", r.StatusCode)
	}
	_ = json.NewDecoder(r.Body).Decode(&st)
	return st
}

func TestSetupStateRendersDoctorAppsLimitAndGenericEntry(t *testing.T) {
	f := newSetupFixture(t)
	_ = os.MkdirAll(filepath.Join(f.home, ".codex"), 0o700)
	st := f.state()
	if len(st.Checks) != 2 || st.Checks[1].Status != "FAIL" || !strings.Contains(st.Checks[1].Detail, "Fix:") {
		t.Fatalf("doctor results: %+v", st.Checks)
	}
	states := map[string]string{}
	for _, a := range st.Apps {
		states[a.Name] = a.State
	}
	if states["codex"] != "available" || states["copilot"] != "not_found" || states["claude-desktop"] != "not_found" {
		t.Fatalf("app states: %v", states)
	}
	if st.MaxSessions != 2 {
		t.Fatalf("limit: %d", st.MaxSessions)
	}
	if !strings.Contains(st.GenericEntry, `"mcpServers"`) || !strings.Contains(st.GenericEntry, `"local"`) {
		t.Fatalf("generic entry: %s", st.GenericEntry)
	}
	if st.SessionsNote == "" {
		t.Fatal("with no co-browse owner running the page should say so")
	}
	// The full check is a separate, explicit request.
	if f.full != 0 {
		t.Fatal("the state poll must not start a test browser")
	}
	if r := f.do("POST", "/api/setup/doctor", "{}", f.good(), true); r.StatusCode != 200 || f.full != 1 {
		t.Fatalf("full check: %d %d", r.StatusCode, f.full)
	}
	// The page itself and its script are served, and carry no secret.
	page := f.do("GET", "/", "", nil, true)
	b, _ := io.ReadAll(page.Body)
	if page.StatusCode != 200 || !strings.Contains(string(b), "Citadel setup") || strings.Contains(string(b), f.bearer) {
		t.Fatalf("setup page: %d", page.StatusCode)
	}
	if js := f.do("GET", "/assets/setup.js", "", nil, true); js.StatusCode != 200 {
		t.Fatalf("setup.js: %d", js.StatusCode)
	}
}

func TestSetupListsSessionsFromTheRunningOwner(t *testing.T) {
	ownerDir := t.TempDir()
	rt := newLocalCobrowseRuntime(localCobrowseOptions{
		ownerDir: func() string { return ownerDir }, browser: newCmdFakeBrowser(), policy: allowAllURLs,
		viewer: true, openURL: func(string) error { return nil },
	})
	defer rt.close()
	resp := runLocalBridge(t, rt, nil, call(1, "local_cobrowse_start", map[string]any{"goal": "demo"}))
	if _, isErr := toolResult(t, resp["1"]); isErr {
		t.Fatalf("start: %v", resp["1"])
	}
	sess, max, err := localviewer.RequestSummary(context.Background(), ownerDir, nil)
	if err != nil || len(sess) != 1 || sess[0].Goal != "demo" || max != 0 && max != 2 {
		t.Fatalf("summary: %v %v %d", sess, err, max)
	}
}

func TestExplorerLaunchDetection(t *testing.T) {
	orig := consoleProcessCount
	t.Cleanup(func() { consoleProcessCount = orig })
	for _, c := range []struct {
		n    int
		err  error
		want bool
	}{{1, nil, true}, {2, nil, false}, {3, nil, false}, {0, errors.New("no console"), false}} {
		consoleProcessCount = func() (int, error) { return c.n, c.err }
		if got := launchedFromExplorer(); got != c.want {
			t.Errorf("count %d err %v: got %v", c.n, c.err, got)
		}
	}
}

func TestBareCitadelOpensSetupOnlyFromExplorer(t *testing.T) {
	origCount, origRun := consoleProcessCount, bareRunLocalSetupFn
	origTTY, origState, origCreds := bareStdoutIsTTYFn, bareHasMeshStateFn, bareHasDeviceCredentialsFn
	t.Cleanup(func() {
		consoleProcessCount, bareRunLocalSetupFn = origCount, origRun
		bareStdoutIsTTYFn, bareHasMeshStateFn, bareHasDeviceCredentialsFn = origTTY, origState, origCreds
	})
	bareStdoutIsTTYFn = func() bool { return false }
	bareHasMeshStateFn = func() bool { return false }
	bareHasDeviceCredentialsFn = func() bool { return false }
	opened := 0
	bareRunLocalSetupFn = func(io.Writer) error { opened++; return nil }

	consoleProcessCount = func() (int, error) { return 1, nil }
	runRootCitadel(&cobra.Command{}, nil)
	if opened != 1 {
		t.Fatalf("a lone console process should open setup, opened=%d", opened)
	}
	consoleProcessCount = func() (int, error) { return 2, nil } // started from a terminal
	runRootCitadel(&cobra.Command{}, nil)
	if opened != 1 {
		t.Fatal("a terminal launch must not open setup")
	}
}

func TestRunLocalSetupStopsWhenTheWindowCloses(t *testing.T) {
	f := &setupFixture{home: t.TempDir()}
	opened := make(chan string, 1)
	d := localSetupDeps{
		stateDir: func() string { return f.home },
		env:      func() harnessEnv { return harnessEnv{goos: "linux", home: f.home} },
		exe:      "citadel", now: time.Now,
		openURL: func(u string) error { opened <- u; return nil },
		doctor:  func(bool) []localviewer.SetupCheck { return nil },
	}
	ctx, cancel := context.WithCancel(context.Background())
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- runLocalSetup(ctx, d, &out) }()
	<-opened
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Setup is open in your browser. Close this window to stop.") {
		t.Fatalf("console line: %q", out.String())
	}
	if m, _ := filepath.Glob(filepath.Join(f.home, localviewer.SetupDirName, "open-*.html")); len(m) != 0 {
		t.Fatalf("launch file left behind: %v", m)
	}
}
