package memory

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return m
}

func TestMergeHook_ConcurrentUpdatesDoNotLoseEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, tc := range []struct{ event, command, marker string }{
		{"UserPromptSubmit", "citadel memory recall", RecallMarker},
		{"SessionEnd", "citadel memory capture", CaptureMarker},
	} {
		wg.Add(1)
		go func(event, command, marker string) {
			defer wg.Done()
			<-start
			if _, err := MergeHook(path, event, command, marker, 10); err != nil {
				t.Errorf("MergeHook(%s): %v", event, err)
			}
		}(tc.event, tc.command, tc.marker)
	}
	close(start)
	wg.Wait()
	hooks := readJSON(t, path)["hooks"].(map[string]any)
	if !hookMarkerPresent(hooks["UserPromptSubmit"].([]any), RecallMarker) ||
		!hookMarkerPresent(hooks["SessionEnd"].([]any), CaptureMarker) {
		t.Fatalf("concurrent update lost an event: %#v", hooks)
	}
}

func TestWriteMCPServer_CreatesAndPreserves(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude.json")

	// Pre-existing live config with unrelated keys + another MCP server.
	seed := map[string]any{
		"numStartups": float64(42),
		"theme":       "dark",
		"mcpServers": map[string]any{
			"posthog": map[string]any{"type": "http", "url": "https://mcp.posthog.com/mcp"},
		},
	}
	seedBytes, _ := json.MarshalIndent(seed, "", "  ")
	if err := os.WriteFile(path, seedBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	changed, err := WriteMCPServer(path, MCPServerName, "/opt/Citadel App/citadel", []string{"--no-auto-update", "mcp", "--memory-config"})
	if err != nil {
		t.Fatalf("WriteMCPServer: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true on first write")
	}

	got := readJSON(t, path)
	// Unrelated keys preserved.
	if got["theme"] != "dark" || got["numStartups"].(float64) != 42 {
		t.Fatalf("unrelated keys not preserved: %v", got)
	}
	servers := got["mcpServers"].(map[string]any)
	if _, ok := servers["posthog"]; !ok {
		t.Fatal("existing posthog MCP server was dropped")
	}
	entry := servers[MCPServerName].(map[string]any)
	if entry["type"] != "stdio" || entry["command"] != "/opt/Citadel App/citadel" {
		t.Fatalf("bad entry: %v", entry)
	}
	args := entry["args"].([]any)
	if len(args) != 3 || args[2] != "--memory-config" {
		t.Fatalf("bad args: %v", args)
	}
	encoded, _ := json.Marshal(entry)
	if strings.Contains(string(encoded), "act_") || strings.Contains(string(encoded), "Authorization") {
		t.Fatalf("MCP entry duplicated the bearer credential: %s", encoded)
	}

	// User-only perms (holds a bearer secret).
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("expected 0600 perms, got %o", info.Mode().Perm())
	}
}

func TestWriteMCPServer_Idempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude.json")

	if _, err := WriteMCPServer(path, MCPServerName, "/bin/citadel", []string{"--no-auto-update", "mcp", "--memory-config"}); err != nil {
		t.Fatal(err)
	}
	changed, err := WriteMCPServer(path, MCPServerName, "/bin/citadel", []string{"--no-auto-update", "mcp", "--memory-config"})
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("expected changed=false on identical re-write")
	}
}

func TestWriteMCPServer_PreservesLargeIntegerExactly(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".claude.json")
	const largeInteger = "9007199254740993"
	seed := []byte(`{"unrelatedLargeInteger":` + largeInteger + `}`)
	if err := os.WriteFile(path, seed, 0o600); err != nil {
		t.Fatal(err)
	}

	changed, err := WriteMCPServer(path, MCPServerName, "/bin/citadel", []string{"mcp", "--memory-config"})
	if err != nil || !changed {
		t.Fatalf("WriteMCPServer changed=%v err=%v", changed, err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `"unrelatedLargeInteger": `+largeInteger) {
		t.Fatalf("large integer was rounded during config rewrite: %s", got)
	}
}

func TestClaudeConfigMutation_RejectsSymlinkWithoutReplacingIt(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "managed.json")
	path := filepath.Join(dir, ".claude.json")
	before := []byte(`{"managed":true}`)
	if err := os.WriteFile(target, before, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	if _, err := WriteMCPServer(path, MCPServerName, "/bin/citadel", []string{"mcp"}); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("managed symlink was not rejected: %v", err)
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("managed symlink was severed: info=%v err=%v", info, err)
	}
	after, err := os.ReadFile(target)
	if err != nil || string(after) != string(before) {
		t.Fatalf("managed target changed: got=%q err=%v", after, err)
	}
}

func TestClaudeConfigMutation_RejectsOversizedAndSpecialFiles(t *testing.T) {
	t.Run("oversized", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), ".claude.json")
		const hostileClaudeConfigBytes = (8 << 20) + 1
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Truncate(hostileClaudeConfigBytes); err != nil {
			_ = f.Close()
			t.Fatal(err)
		}
		_ = f.Close()
		if _, err := WriteMCPServer(path, MCPServerName, "/bin/citadel", []string{"mcp"}); err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("oversized config was not rejected: %v", err)
		}
	})

	t.Run("directory", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), ".claude.json")
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := WriteMCPServer(path, MCPServerName, "/bin/citadel", []string{"mcp"}); err == nil || !strings.Contains(err.Error(), "regular file") {
			t.Fatalf("special config was not rejected: %v", err)
		}
	})
}

func TestClaudeConfigMutation_RefusesWrongShapeWithoutClobbering(t *testing.T) {
	for _, tc := range []struct {
		name string
		seed string
		run  func(string) error
	}{
		{
			name: "mcpServers is array",
			seed: `{"theme":"dark","mcpServers":[{"future":"shape"}]}`,
			run: func(path string) error {
				_, err := WriteMCPServer(path, MCPServerName, "/bin/citadel", []string{"mcp"})
				return err
			},
		},
		{
			name: "hooks is array",
			seed: `{"theme":"dark","hooks":[{"future":"shape"}]}`,
			run: func(path string) error {
				_, err := MergeHook(path, "SessionEnd", "/bin/citadel memory capture", CaptureMarker, 15)
				return err
			},
		},
		{
			name: "event is object",
			seed: `{"theme":"dark","hooks":{"SessionEnd":{"future":"shape"}}}`,
			run: func(path string) error {
				_, err := RemoveHook(path, "SessionEnd", CaptureMarker)
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			before := []byte(tc.seed)
			if err := os.WriteFile(path, before, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := tc.run(path); err == nil {
				t.Fatal("wrong-shaped Claude config was accepted")
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Fatalf("config changed on refusal: before=%s after=%s", before, after)
			}
		})
	}
}

func TestRemoveMCPServer_PreservesUnrelatedServers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude.json")
	seed := map[string]any{"mcpServers": map[string]any{
		MCPServerName: map[string]any{"type": "stdio", "command": "citadel"},
		"other":       map[string]any{"type": "http", "url": "https://example.test/mcp"},
	}}
	data, _ := json.Marshal(seed)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := RemoveMCPServer(path, MCPServerName)
	if err != nil || !changed {
		t.Fatalf("RemoveMCPServer changed=%v err=%v", changed, err)
	}
	servers := readJSON(t, path)["mcpServers"].(map[string]any)
	if _, ok := servers[MCPServerName]; ok {
		t.Fatal("Citadel server still present")
	}
	if _, ok := servers["other"]; !ok {
		t.Fatal("unrelated server was removed")
	}
}

func TestMergeHook_AdditiveAndIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	// Seed with an existing unrelated hook on the same event.
	seed := map[string]any{
		"theme": "dark",
		"hooks": map[string]any{
			"UserPromptSubmit": []any{
				map[string]any{
					"matcher": "",
					"hooks": []any{
						map[string]any{"type": "command", "command": "bash existing.sh", "timeout": float64(5)},
					},
				},
			},
		},
	}
	seedBytes, _ := json.MarshalIndent(seed, "", "  ")
	if err := os.WriteFile(path, seedBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	changed, err := MergeHook(path, "UserPromptSubmit", "/usr/local/bin/citadel memory recall", RecallMarker, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected changed=true adding recall hook")
	}

	got := readJSON(t, path)
	if got["theme"] != "dark" {
		t.Fatal("unrelated settings dropped")
	}
	groups := got["hooks"].(map[string]any)["UserPromptSubmit"].([]any)
	if len(groups) != 2 {
		t.Fatalf("expected 2 hook groups (existing + ours), got %d", len(groups))
	}
	// Existing hook still present.
	if !hookMarkerPresent(groups, "bash existing.sh") {
		t.Fatal("existing hook was clobbered")
	}
	if !hookMarkerPresent(groups, RecallMarker) {
		t.Fatal("recall hook not added")
	}

	// Idempotency: re-merge with a DIFFERENT binary path but same marker.
	changed, err = MergeHook(path, "UserPromptSubmit", "/opt/citadel memory recall", RecallMarker, 10)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("expected changed=false; marker already present")
	}
	got = readJSON(t, path)
	groups = got["hooks"].(map[string]any)["UserPromptSubmit"].([]any)
	if len(groups) != 2 {
		t.Fatalf("re-merge duplicated hook: got %d groups", len(groups))
	}
}

func TestRemoveHook_PreservesOtherEntriesInGroup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	seed := map[string]any{"hooks": map[string]any{"SessionEnd": []any{map[string]any{
		"matcher": "",
		"hooks": []any{
			map[string]any{"type": "command", "command": "/bin/citadel memory capture --no-auto-update"},
			map[string]any{"type": "command", "command": "other cleanup"},
		},
	}}}}
	data, _ := json.Marshal(seed)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := RemoveHook(path, "SessionEnd", CaptureMarker)
	if err != nil || !changed {
		t.Fatalf("RemoveHook changed=%v err=%v", changed, err)
	}
	groups := readJSON(t, path)["hooks"].(map[string]any)["SessionEnd"].([]any)
	if hookMarkerPresent(groups, CaptureMarker) || !hookMarkerPresent(groups, "other cleanup") {
		t.Fatalf("unexpected remaining groups: %#v", groups)
	}
}

func TestMergeHook_CreatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "settings.json") // parent dir absent

	changed, err := MergeHook(path, "SessionEnd", "/bin/citadel memory capture", CaptureMarker, 15)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected changed=true")
	}
	got := readJSON(t, path)
	groups := got["hooks"].(map[string]any)["SessionEnd"].([]any)
	if !hookMarkerPresent(groups, CaptureMarker) {
		t.Fatal("capture hook not written")
	}
}

func TestDetectClaudeCode(t *testing.T) {
	home := t.TempDir()
	if DetectClaudeCode(home) {
		t.Fatal("should be false without ~/.claude")
	}
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !DetectClaudeCode(home) {
		t.Fatal("should be true with ~/.claude")
	}
}
