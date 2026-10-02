package memory

import (
	"encoding/json"
	"os"
	"path/filepath"
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

	changed, err := WriteMCPServer(path, MCPServerName, "https://aceteam.ai/api/mcp/aceteam/mcp", "act_secret123")
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
	if entry["type"] != "http" || entry["url"] != "https://aceteam.ai/api/mcp/aceteam/mcp" {
		t.Fatalf("bad entry: %v", entry)
	}
	headers := entry["headers"].(map[string]any)
	if headers["Authorization"] != "Bearer act_secret123" {
		t.Fatalf("bad auth header: %v", headers)
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

	if _, err := WriteMCPServer(path, MCPServerName, "https://x/mcp", "act_k"); err != nil {
		t.Fatal(err)
	}
	changed, err := WriteMCPServer(path, MCPServerName, "https://x/mcp", "act_k")
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("expected changed=false on identical re-write")
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
