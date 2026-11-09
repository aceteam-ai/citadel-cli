package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/localaudit"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

const winExe = `C:\Users\demo\AppData\Local\Citadel\citadel.exe`

func TestHarnessPaths(t *testing.T) {
	win := harnessEnv{goos: "windows", home: `C:\Users\demo`, appData: `C:\Users\demo\AppData\Roaming`}
	mac := harnessEnv{goos: "darwin", home: "/Users/demo"}
	want := map[string][2]string{
		"copilot":        {filepath.Join(win.home, ".copilot", "mcp-config.json"), filepath.Join(mac.home, ".copilot", "mcp-config.json")},
		"claude-desktop": {filepath.Join(win.appData, "Claude", "claude_desktop_config.json"), filepath.Join(mac.home, "Library", "Application Support", "Claude", "claude_desktop_config.json")},
		"codex":          {filepath.Join(win.home, ".codex", "config.toml"), filepath.Join(mac.home, ".codex", "config.toml")},
	}
	for name, paths := range want {
		h, ok := findHarness(name)
		if !ok {
			t.Fatalf("missing harness %s", name)
		}
		if got := h.path(win); got != paths[0] {
			t.Errorf("%s windows: %s", name, got)
		}
		if got := h.path(mac); got != paths[1] {
			t.Errorf("%s macOS: %s", name, got)
		}
	}
}

func TestRenderBlocks(t *testing.T) {
	cp, _ := findHarness("copilot")
	var doc map[string]map[string]map[string]any
	if err := json.Unmarshal([]byte(renderBlock(cp, winExe)), &doc); err != nil {
		t.Fatal(err)
	}
	e := doc["mcpServers"]["citadel"]
	if e["command"] != winExe || e["type"] != "local" || len(e["args"].([]any)) != 2 {
		t.Fatalf("copilot entry: %v", e)
	}
	cx, _ := findHarness("codex")
	got := renderBlock(cx, winExe)
	want := "[mcp_servers.citadel]\ncommand = '" + winExe + "'\nargs = ['local', 'mcp']\n"
	if got != want {
		t.Fatalf("codex block:\n%s\nwant\n%s", got, want)
	}
	if !strings.Contains(renderBlock(cx, `C:\it's\citadel.exe`), `"C:\\it's\\citadel.exe"`) {
		t.Fatal("a path with a quote needs a basic TOML string")
	}
}

func TestMergeJSONKeepsOtherServersAndBacksUp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude_desktop_config.json")
	orig := `{"theme":"dark","mcpServers":{"other":{"command":"x"}}}`
	_ = os.WriteFile(path, []byte(orig), 0o600)
	h, _ := findHarness("claude-desktop")
	now := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	if _, err := mergeHarnessConfig(h, path, winExe, func(string) bool { return false }, now); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	b, _ := os.ReadFile(path)
	_ = json.Unmarshal(b, &doc)
	servers := doc["mcpServers"].(map[string]any)
	if doc["theme"] != "dark" || servers["other"] == nil || servers["citadel"] == nil {
		t.Fatalf("merge lost settings: %s", b)
	}
	if bak, err := os.ReadFile(path + ".bak-20261108T100000Z"); err != nil || string(bak) != orig {
		t.Fatalf("backup: %v", err)
	}
	// A different existing entry is replaced only with confirmation.
	_ = os.WriteFile(path, []byte(`{"mcpServers":{"citadel":{"command":"old"}}}`), 0o600)
	if _, err := mergeHarnessConfig(h, path, winExe, func(string) bool { return false }, now); !errors.Is(err, errWriteDeclined) {
		t.Fatalf("declined replace: %v", err)
	}
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), `"old"`) {
		t.Fatal("declined replace must not write")
	}
	_ = os.WriteFile(path, []byte("{not json"), 0o600)
	if _, err := mergeHarnessConfig(h, path, winExe, func(string) bool { return true }, now); err == nil {
		t.Fatal("invalid JSON must not be overwritten")
	}
}

func TestMergeCodexAppendsOnceAndRerunIsNoOp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	_ = os.WriteFile(path, []byte("model = \"x\""), 0o644)
	h, _ := findHarness("codex")
	if _, err := mergeHarnessConfig(h, path, winExe, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(b), "model = \"x\"\n\n[mcp_servers.citadel]") {
		t.Fatalf("codex append: %q", b)
	}
	if fi, _ := os.Stat(path); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o644 {
		t.Fatalf("the file mode must be kept, got %o", fi.Mode().Perm())
	}
	if _, err := mergeHarnessConfig(h, path, winExe, nil, time.Now()); !errors.Is(err, errAlreadyConfigured) {
		t.Fatalf("re-running with the same entry must be a no-op, got %v", err)
	}
}

func TestMergeCodexFindsEveryTOMLForm(t *testing.T) {
	h, _ := findHarness("codex")
	for name, body := range map[string]string{
		"parent table": "[mcp_servers]\ncitadel = { command = 'C:\\old.exe', args = ['local', 'mcp'] }\n",
		"quoted key":   "[mcp_servers.\"citadel\"]\ncommand = 'C:\\old.exe'\nargs = ['local', 'mcp']\n",
		"dotted key":   "mcp_servers.citadel.command = 'C:\\old.exe'\n",
	} {
		path := filepath.Join(t.TempDir(), "config.toml")
		_ = os.WriteFile(path, []byte(body), 0o600)
		if _, err := mergeHarnessConfig(h, path, winExe, nil, time.Now()); err == nil || errors.Is(err, errAlreadyConfigured) {
			t.Errorf("%s: a different existing citadel entry must be refused, got %v", name, err)
		}
		if b, _ := os.ReadFile(path); string(b) != body {
			t.Errorf("%s: the file must not change", name)
		}
	}
	// An inline mcp_servers table cannot be extended by appending a table.
	path := filepath.Join(t.TempDir(), "config.toml")
	_ = os.WriteFile(path, []byte("mcp_servers = { other = { command = 'x' } }\n"), 0o600)
	if _, err := mergeHarnessConfig(h, path, winExe, nil, time.Now()); err == nil {
		t.Fatal("appending after an inline table must be refused, not written")
	}
	// An identical entry in another form is a no-op.
	path = filepath.Join(t.TempDir(), "config.toml")
	_ = os.WriteFile(path, []byte("[mcp_servers]\ncitadel = { command = '"+winExe+"', args = ['local', 'mcp'] }\n"), 0o600)
	if _, err := mergeHarnessConfig(h, path, winExe, nil, time.Now()); !errors.Is(err, errAlreadyConfigured) {
		t.Fatalf("identical inline entry: %v", err)
	}
}

func TestMergeJSONIsLossless(t *testing.T) {
	h, _ := findHarness("claude-desktop")
	path := filepath.Join(t.TempDir(), "c.json")
	orig := `{"zeta":1,"big":12345678901234567890,"html":"a&b<c>","mcpServers":{"other":{"command":"x"}},"alpha":true}`
	_ = os.WriteFile(path, []byte(orig), 0o600)
	if _, err := mergeHarnessConfig(h, path, winExe, func(string) bool { return false }, time.Now()); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	out := string(b)
	for _, want := range []string{"12345678901234567890", `"a&b<c>"`, `"other"`, `"citadel"`} {
		if !strings.Contains(out, want) {
			t.Errorf("merge lost %s:\n%s", want, out)
		}
	}
	if !(strings.Index(out, `"zeta"`) < strings.Index(out, `"big"`) && strings.Index(out, `"mcpServers"`) < strings.Index(out, `"alpha"`)) {
		t.Errorf("key order changed:\n%s", out)
	}
	if _, err := mergeHarnessConfig(h, path, winExe, func(string) bool { return false }, time.Now()); !errors.Is(err, errAlreadyConfigured) {
		t.Fatalf("re-run: %v", err)
	}
}

func TestMergeWritesThroughSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	dir := t.TempDir()
	real := filepath.Join(dir, "dotfiles", "c.json")
	_ = os.MkdirAll(filepath.Dir(real), 0o700)
	_ = os.WriteFile(real, []byte(`{"mcpServers":{}}`), 0o640)
	link := filepath.Join(dir, "c.json")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	h, _ := findHarness("claude-desktop")
	written, err := mergeHarnessConfig(h, link, winExe, nil, time.Now())
	if err != nil || written != real {
		t.Fatalf("write-through: %q %v", written, err)
	}
	if fi, _ := os.Lstat(link); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the symlink must stay a symlink")
	}
	if b, _ := os.ReadFile(real); !strings.Contains(string(b), "citadel") {
		t.Fatal("the target was not updated")
	}
	if fi, _ := os.Stat(real); fi.Mode().Perm() != 0o640 {
		t.Fatalf("mode: %o", fi.Mode().Perm())
	}
}

func TestHarnessHomesAndUnverifiedNotes(t *testing.T) {
	env := harnessEnv{goos: "linux", home: "/h", codexHome: "/c", copilotDir: "/p"}
	cx, _ := findHarness("codex")
	cp, _ := findHarness("copilot")
	cd, _ := findHarness("claude-desktop")
	if cx.path(env) != filepath.Join("/c", "config.toml") || cp.path(env) != filepath.Join("/p", "mcp-config.json") {
		t.Fatalf("CODEX_HOME / COPILOT_HOME not honored: %s %s", cx.path(env), cp.path(env))
	}
	if cd.unverified(env) == "" || cd.unverified(harnessEnv{goos: "windows"}) != "" {
		t.Fatal("only the Linux Claude Desktop path is unverified")
	}
	var out bytes.Buffer
	_ = runLocalConfig("claude-desktop", "/opt/citadel", env, false, false, nil, &out, time.Now())
	if !strings.Contains(out.String(), "unverified") {
		t.Fatalf("an unverified path must say so:\n%s", out.String())
	}
}

func TestRunLocalConfigWriteAsksFirst(t *testing.T) {
	home := t.TempDir()
	env := harnessEnv{goos: "linux", home: home}
	var out bytes.Buffer
	if err := runLocalConfig("copilot", winExe, env, true, false, strings.NewReader("n\n"), &out, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".copilot", "mcp-config.json")); !os.IsNotExist(err) {
		t.Fatal("declining must not write")
	}
	out.Reset()
	if err := runLocalConfig("copilot", winExe, env, true, false, strings.NewReader("y\n"), &out, time.Now()); err != nil {
		t.Fatal(err)
	}
	st := detectHarnessConfigs(env)
	if !st[0].Found {
		t.Fatalf("detect after write: %+v", st[0])
	}
	if err := runLocalConfig("unknown", winExe, env, false, false, nil, &out, time.Now()); err == nil {
		t.Fatal("unknown harness")
	}
}

func TestDetectHarnessConfigs(t *testing.T) {
	home := t.TempDir()
	env := harnessEnv{goos: "linux", home: home}
	exe := filepath.Join(home, "bin", "citadel")
	_ = os.MkdirAll(filepath.Dir(exe), 0o700)
	_ = os.WriteFile(exe, []byte("x"), 0o700)
	_ = os.MkdirAll(filepath.Join(home, ".codex"), 0o700)
	h, _ := findHarness("codex")
	_ = os.WriteFile(filepath.Join(home, ".codex", "config.toml"), []byte(renderBlock(h, exe)), 0o600)
	_ = os.MkdirAll(filepath.Join(home, ".config", "Claude"), 0o700)
	// Mentions citadel, local and mcp, but runs nothing of ours.
	_ = os.WriteFile(filepath.Join(home, ".config", "Claude", "claude_desktop_config.json"),
		[]byte(`{"mcpServers":{"notes":{"command":"/usr/bin/notes","args":["local","mcp"],"env":{"X":"citadel"}}}}`), 0o600)
	_ = os.MkdirAll(filepath.Join(home, ".copilot"), 0o700)
	_ = os.WriteFile(filepath.Join(home, ".copilot", "mcp-config.json"),
		[]byte(`{"mcpServers":{"citadel":{"command":"`+filepath.Join(home, "gone", "citadel")+`","args":["local","mcp"]}}}`), 0o600)
	got := map[string]harnessConfigStatus{}
	for _, s := range detectHarnessConfigs(env) {
		got[s.Harness] = s
	}
	if !got["codex"].Points || got["codex"].Command != exe {
		t.Fatalf("codex: %+v", got["codex"])
	}
	if !got["claude-desktop"].Found || got["claude-desktop"].Points {
		t.Fatalf("a look-alike entry must not count: %+v", got["claude-desktop"])
	}
	if !got["copilot"].Found || got["copilot"].Points {
		t.Fatalf("an entry whose exe no longer exists must not count: %+v", got["copilot"])
	}
}

func TestLocalDoctor(t *testing.T) {
	base := localDoctorDeps{
		goos:   "windows",
		runner: "headless",
		discover: func() (platform.BrowserInfo, error) {
			return platform.BrowserInfo{Path: `C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`, Kind: "edge"}, nil
		},
		launchTest: func() (string, string, error) { return "edge", "Edg/131.0.2903.70", nil },
		bindTest:   func() error { return nil },
		vault:      func() (bool, error) { return false, nil },
		owner:      func() error { return nil },
		audit:      func() (localaudit.VerifyResult, error) { return localaudit.VerifyResult{}, nil },
	}
	var out bytes.Buffer
	if err := runLocalDoctor(base, &out); err != nil {
		t.Fatalf("healthy doctor failed: %v\n%s", err, out.String())
	}
	for _, want := range []string{"Microsoft Edge at C:\\Program Files (x86)", "(Edg/131.0.2903.70) started hidden", "127.0.0.1", "no calls of its own", "public addresses", "local config copilot"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("doctor output missing %q:\n%s", want, out.String())
		}
	}

	broken := base
	broken.discover = func() (platform.BrowserInfo, error) {
		return platform.BrowserInfo{}, errors.New("no supported browser found. Install Microsoft Edge or Google Chrome")
	}
	out.Reset()
	if err := runLocalDoctor(broken, &out); err == nil || !strings.Contains(out.String(), "[FAIL] Browser") || !strings.Contains(out.String(), "Install Microsoft Edge") {
		t.Fatalf("missing browser must fail with a fix:\n%s", out.String())
	}

	blocked := base
	blocked.launchTest = func() (string, string, error) { return "", "", platform.ErrBrowserAutomationBlocked }
	out.Reset()
	if err := runLocalDoctor(blocked, &out); err == nil || !strings.Contains(out.String(), "per-user") {
		t.Fatalf("policy-blocked launch must name the fix:\n%s", out.String())
	}

	old := base
	old.launchTest = func() (string, string, error) { return "edge", "Edg/101.0.1210.53", nil }
	out.Reset()
	if err := runLocalDoctor(old, &out); err == nil || !strings.Contains(out.String(), "too old") {
		t.Fatalf("a browser older than 109 must fail with an update fix:\n%s", out.String())
	}
	if browserMajor("HeadlessChrome/131.0.6778.86") != 131 || browserMajor("") != 0 {
		t.Fatal("browserMajor")
	}
}
