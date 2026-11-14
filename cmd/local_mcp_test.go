package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

// cmdFakeBrowser is a minimal localcobrowse.Browser for bridge tests.
type cmdFakeBrowser struct {
	mu     sync.Mutex
	clicks int
	n      int
	human  map[string]bool
	live   map[string]bool
	// landing and title, when set, are what PageState reports (a redirect
	// and a page title).
	landing, title string
}

func newCmdFakeBrowser() *cmdFakeBrowser {
	return &cmdFakeBrowser{human: map[string]bool{}, live: map[string]bool{}}
}

func (f *cmdFakeBrowser) st(id string) platform.CobrowseSessionStatus {
	d := platform.DriverAI
	if f.human[id] {
		d = platform.DriverHuman
	}
	return platform.CobrowseSessionStatus{ID: id, State: platform.SessionRunning, Driver: d, Browser: "edge", URL: "http://fixture.test/"}
}
func (f *cmdFakeBrowser) gate(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.live[id] {
		return fmt.Errorf("no such browser session")
	}
	if f.human[id] {
		return platform.ErrHandedOff
	}
	return nil
}
func (f *cmdFakeBrowser) StartSession(string) (platform.CobrowseSessionStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	id := fmt.Sprintf("cb-%d", f.n)
	f.live[id] = true
	return f.st(id), nil
}
func (f *cmdFakeBrowser) SessionStatus(id string) (platform.CobrowseSessionStatus, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.st(id), f.live[id]
}
func (f *cmdFakeBrowser) Stop(id string) error {
	f.mu.Lock()
	delete(f.live, id)
	f.mu.Unlock()
	return nil
}
func (f *cmdFakeBrowser) StopAll() error {
	f.mu.Lock()
	f.live = map[string]bool{}
	f.mu.Unlock()
	return nil
}
func (f *cmdFakeBrowser) ok(id string) (platform.CobrowseSessionStatus, error) {
	if err := f.gate(id); err != nil {
		return platform.CobrowseSessionStatus{}, err
	}
	return f.st(id), nil
}
func (f *cmdFakeBrowser) Navigate(id, _ string) (platform.CobrowseSessionStatus, error) {
	return f.ok(id)
}
func (f *cmdFakeBrowser) NavigateAsHuman(id, _ string) (platform.CobrowseSessionStatus, error) {
	return f.st(id), nil
}
func (f *cmdFakeBrowser) Click(id, _ string, _, _ *float64) (platform.CobrowseSessionStatus, error) {
	st, err := f.ok(id)
	if err == nil {
		f.mu.Lock()
		f.clicks++
		f.mu.Unlock()
	}
	return st, err
}
func (f *cmdFakeBrowser) Type(id, _ string) (platform.CobrowseSessionStatus, error) { return f.ok(id) }
func (f *cmdFakeBrowser) Select(id, _, v, l string) (platform.SelectResult, error) {
	return platform.SelectResult{Value: v, Label: l}, f.gate(id)
}
func (f *cmdFakeBrowser) Key(id, _ string) (platform.CobrowseSessionStatus, error) { return f.ok(id) }
func (f *cmdFakeBrowser) Scroll(id, _ string, _, _ *float64, _, _ float64) (platform.CobrowseSessionStatus, error) {
	return f.ok(id)
}
func (f *cmdFakeBrowser) ExtractBounded(id, _ string, _ []string, _ int) (platform.ExtractResult, error) {
	return platform.ExtractResult{Text: "fixture"}, f.gate(id)
}
func (f *cmdFakeBrowser) Screenshot(id string) (string, error) {
	return "iVBORw0KGgoAAAANSUhEUg==", f.gate(id)
}
func (f *cmdFakeBrowser) PageState(id string) (platform.PageState, error) {
	u := "http://fixture.test/"
	if f.landing != "" {
		u = f.landing
	}
	return platform.PageState{URL: u, ReadyState: "complete", Title: f.title}, f.gate(id)
}
func (f *cmdFakeBrowser) InspectTarget(id string, _ platform.TargetProbe) (platform.TargetInfo, error) {
	return platform.TargetInfo{Resolved: true, Tag: "div", PageURL: "http://fixture.test/"}, f.gate(id)
}
func (f *cmdFakeBrowser) Handoff(id string) (platform.CobrowseSessionStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.human[id] = true
	return f.st(id), nil
}
func (f *cmdFakeBrowser) Resume(id string) (platform.CobrowseSessionStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.human[id] = false
	return f.st(id), nil
}

type recordingTransport struct{ calls atomic.Int64 }

func (r *recordingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	r.calls.Add(1)
	return nil, fmt.Errorf("outbound request attempted")
}

func allowAllURLs(context.Context, string) error { return nil }

// runLocalBridge feeds lines to a local-only bridge and returns responses by id.
func runLocalBridge(t *testing.T, rt *localCobrowseRuntime, transport http.RoundTripper, lines ...string) map[string]map[string]any {
	t.Helper()
	tools := newLocalMCPTools(localMCPDeps{cobrowse: rt, workspaceDir: t.TempDir(), egressRelayConfigDir: t.TempDir})
	var out bytes.Buffer
	b := newLocalOnlyBridge(tools, rt, strings.NewReader(strings.Join(lines, "\n")+"\n"), &out, transport)
	if err := b.run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	got := map[string]map[string]any{}
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("bad response line %q: %v", l, err)
		}
		got[fmt.Sprint(m["id"])] = m
	}
	return got
}

func call(id int, tool string, args map[string]any) string {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args}})
	return string(b)
}

func toolResult(t *testing.T, resp map[string]any) (content []any, isError bool) {
	t.Helper()
	res, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result in %v", resp)
	}
	content, _ = res["content"].([]any)
	isError, _ = res["isError"].(bool)
	return content, isError
}

func firstText(content []any) string {
	if len(content) == 0 {
		return ""
	}
	m, _ := content[0].(map[string]any)
	s, _ := m["text"].(string)
	return s
}

func TestLocalMCPStdioMakesNoOutboundCalls(t *testing.T) {
	ownerDir := t.TempDir()
	rt := newLocalCobrowseRuntime(localCobrowseOptions{ownerDir: func() string { return ownerDir }, browser: newCmdFakeBrowser(), policy: allowAllURLs})
	defer rt.close()
	tr := &recordingTransport{}

	first := runLocalBridge(t, rt, tr,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","clientInfo":{"name":"Test Harness","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":4,"method":"resources/list"}`,
		call(5, "fabric_list_nodes", nil),
		call(6, "local_cobrowse_start", map[string]any{"url": "http://fixture.test/", "goal": "demo"}),
	)
	second := runLocalBridge(t, rt, tr,
		call(7, "local_cobrowse_screenshot", map[string]any{"session_id": "cb-1"}),
		call(8, "local_cobrowse_click", map[string]any{"session_id": "cb-1", "selector": "#next"}),
		call(9, "local_cobrowse_status", nil),
	)
	if n := tr.calls.Load(); n != 0 {
		t.Fatalf("local mode attempted %d outbound HTTP request(s)", n)
	}

	init := first["1"]["result"].(map[string]any)
	if init["serverInfo"].(map[string]any)["name"] != "citadel-local" {
		t.Errorf("initialize: %v", init)
	}
	tools := first["2"]["result"].(map[string]any)["tools"].([]any)
	for _, raw := range tools {
		name := raw.(map[string]any)["name"].(string)
		if !strings.HasPrefix(name, "local_") {
			t.Errorf("local mode lists a non-local tool %s", name)
		}
	}
	for _, id := range []string{"4", "5"} {
		e, _ := first[id]["error"].(map[string]any)
		if e == nil || e["code"].(float64) != -32601 {
			t.Errorf("id %s: want -32601 (no backend fallthrough), got %v", id, first[id])
		}
	}
	if content, isErr := toolResult(t, first["6"]); isErr || !strings.Contains(firstText(content), `"session_id": "cb-1"`) {
		t.Fatalf("start: %v", first["6"])
	}

	content, isErr := toolResult(t, second["7"])
	if isErr || len(content) != 1 {
		t.Fatalf("screenshot: %v", second["7"])
	}
	img := content[0].(map[string]any)
	if img["type"] != "image" || img["mimeType"] != "image/png" || img["data"] == "" {
		t.Fatalf("screenshot must be an MCP image block, got %v", img)
	}
	if content, isErr := toolResult(t, second["8"]); !isErr || !strings.Contains(firstText(content), "approval_required") {
		t.Fatalf("click without a grant must be refused: %v", second["8"])
	}
	if content, _ := toolResult(t, second["9"]); !strings.Contains(firstText(content), "Test Harness") {
		t.Errorf("status should name the connected agent: %v", second["9"])
	}

	data, err := os.ReadFile(filepath.Join(ownerDir, "local-cobrowse-owner.json"))
	if err != nil || !strings.Contains(string(data), "Test Harness") {
		t.Errorf("owner marker should name the client: %s %v", data, err)
	}
}

func TestLocalMCPSecondInstanceGetsOwnerError(t *testing.T) {
	ownerDir := t.TempDir()
	dir := func() string { return ownerDir }
	a := newLocalCobrowseRuntime(localCobrowseOptions{ownerDir: dir, browser: newCmdFakeBrowser(), policy: allowAllURLs})
	defer a.close()
	b := newLocalCobrowseRuntime(localCobrowseOptions{ownerDir: dir, browser: newCmdFakeBrowser(), policy: allowAllURLs})
	defer b.close()

	ra := runLocalBridge(t, a, nil,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"clientInfo":{"name":"First Harness"}}}`,
		call(2, "local_cobrowse_start", nil))
	if _, isErr := toolResult(t, ra["2"]); isErr {
		t.Fatalf("first start: %v", ra["2"])
	}
	rb := runLocalBridge(t, b, nil,
		call(1, "local_cobrowse_start", nil),
		call(2, "local_list_files", nil))
	content, isErr := toolResult(t, rb["1"])
	if !isErr || !strings.Contains(firstText(content), "already owns co-browse") || !strings.Contains(firstText(content), "First Harness") {
		t.Fatalf("second instance should get the owner error naming the first: %v", rb["1"])
	}
	if _, ok := rb["2"]["result"]; !ok {
		t.Fatalf("second instance must still serve other local tools: %v", rb["2"])
	}
}

func TestLocalModeSkipsCATrustAndUpdateCheck(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	origCheck := checkForUpdateOnStartupFn
	origNode := nodeConfigDirFn
	t.Cleanup(func() { checkForUpdateOnStartupFn = origCheck; nodeConfigDirFn = origNode })
	checked := false
	checkForUpdateOnStartupFn = func(bool) { checked = true }
	caPrepared := false
	nodeConfigDirFn = func() string { caPrepared = true; return t.TempDir() }

	if !isLocalModeCommand(localMCPCmd) || !isLocalModeCommand(localCmd) || isLocalModeCommand(mcpCmd) {
		t.Fatal("isLocalModeCommand classification is wrong")
	}
	rootCmd.PersistentPreRun(localMCPCmd, nil)
	if checked {
		t.Error("citadel local mcp must not run the update check")
	}
	if caPrepared {
		t.Error("citadel local mcp must not prepare private CA trust")
	}
	if !debugToStderr {
		t.Error("local mcp must route debug output to stderr")
	}
}
