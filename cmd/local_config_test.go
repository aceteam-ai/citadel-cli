package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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

func TestMergeCodexAppendsOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	_ = os.WriteFile(path, []byte("model = \"x\""), 0o600)
	h, _ := findHarness("codex")
	if _, err := mergeHarnessConfig(h, path, winExe, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(b), "model = \"x\"\n\n[mcp_servers.citadel]") {
		t.Fatalf("codex append: %q", b)
	}
	if _, err := mergeHarnessConfig(h, path, winExe, nil, time.Now()); err == nil {
		t.Fatal("an existing table must not be appended twice")
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
	if !st[0].Found || !st[0].Points {
		t.Fatalf("detect after write: %+v", st[0])
	}
	if err := runLocalConfig("unknown", winExe, env, false, false, nil, &out, time.Now()); err == nil {
		t.Fatal("unknown harness")
	}
}

func TestDetectHarnessConfigs(t *testing.T) {
	home := t.TempDir()
	env := harnessEnv{goos: "linux", home: home}
	_ = os.MkdirAll(filepath.Join(home, ".codex"), 0o700)
	h, _ := findHarness("codex")
	_ = os.WriteFile(filepath.Join(home, ".codex", "config.toml"), []byte(renderBlock(h, "/opt/citadel")), 0o600)
	_ = os.MkdirAll(filepath.Join(home, ".config", "Claude"), 0o700)
	_ = os.WriteFile(filepath.Join(home, ".config", "Claude", "claude_desktop_config.json"), []byte(`{"mcpServers":{}}`), 0o600)
	got := map[string]harnessConfigStatus{}
	for _, s := range detectHarnessConfigs(env) {
		got[s.Harness] = s
	}
	if !got["codex"].Points || !got["claude-desktop"].Found || got["claude-desktop"].Points || got["copilot"].Found {
		t.Fatalf("detect: %+v", got)
	}
}

func TestLocalDoctor(t *testing.T) {
	base := localDoctorDeps{
		goos:   "windows",
		runner: "headless",
		discover: func() (platform.BrowserInfo, error) {
			return platform.BrowserInfo{Path: `C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`, Kind: "edge"}, nil
		},
		launchTest: func() (string, error) { return "edge", nil },
		bindTest:   func() error { return nil },
		vault:      func() (bool, error) { return false, nil },
		owner:      func() error { return nil },
		audit:      func() (localaudit.VerifyResult, error) { return localaudit.VerifyResult{}, nil },
	}
	var out bytes.Buffer
	if err := runLocalDoctor(base, &out); err != nil {
		t.Fatalf("healthy doctor failed: %v\n%s", err, out.String())
	}
	for _, want := range []string{"Microsoft Edge at C:\\Program Files (x86)", "started hidden", "127.0.0.1", "no network calls", "citadel local config copilot"} {
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
	blocked.launchTest = func() (string, error) { return "", platform.ErrBrowserAutomationBlocked }
	out.Reset()
	if err := runLocalDoctor(blocked, &out); err == nil || !strings.Contains(out.String(), "per-user") {
		t.Fatalf("policy-blocked launch must name the fix:\n%s", out.String())
	}
}
