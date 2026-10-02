package memory

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Claude Code integration constants. The recall/capture hook commands are
// matched by these markers for idempotency (re-running install must not add a
// duplicate hook), independent of the absolute binary path prefix.
const (
	// MCPServerName is the key under mcpServers in ~/.claude.json.
	MCPServerName = "aceteam-memory"
	// RecallMarker / CaptureMarker identify our hooks for dedup.
	RecallMarker  = "citadel memory recall"
	CaptureMarker = "citadel memory capture"
)

// WriteMCPServer registers (or updates) an HTTP MCP server entry pointing at
// the AceTeam memory endpoint with a bearer act_ key, in Claude Code's
// user-scoped ~/.claude.json. All other keys are preserved verbatim (the file
// is Claude Code's own live state). Returns changed=false when the entry is
// already present and identical. The file is written 0600 (it holds a secret).
func WriteMCPServer(claudeJSONPath, name, url, bearer string) (bool, error) {
	root, err := readJSONObject(claudeJSONPath)
	if err != nil {
		return false, err
	}

	servers, _ := root["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}

	entry := map[string]any{
		"type": "http",
		"url":  url,
		"headers": map[string]any{
			"Authorization": "Bearer " + bearer,
		},
	}

	if existing, ok := servers[name].(map[string]any); ok && jsonEqual(existing, entry) {
		return false, nil
	}

	servers[name] = entry
	root["mcpServers"] = servers
	if err := writeJSONObject(claudeJSONPath, root, 0o600); err != nil {
		return false, err
	}
	return true, nil
}

// hookGroup mirrors one entry of a hooks.<Event> array.
type hookGroup struct {
	Matcher string     `json:"matcher"`
	Hooks   []hookSpec `json:"hooks"`
}

type hookSpec struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"`
}

// MergeHook additively appends a command hook for the given event to Claude
// Code's ~/.claude/settings.json, preserving all existing hooks and settings.
// It is idempotent: if any existing hook command for that event already
// contains marker, nothing is written and changed=false is returned.
func MergeHook(settingsPath, event, command, marker string, timeout int) (bool, error) {
	root, err := readJSONObject(settingsPath)
	if err != nil {
		return false, err
	}

	hooks, _ := root["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}

	// Existing groups for this event (as generic slice for preservation).
	var groups []any
	if raw, ok := hooks[event].([]any); ok {
		groups = raw
	}

	// Idempotency: bail if marker already present in any command for this event.
	if hookMarkerPresent(groups, marker) {
		return false, nil
	}

	newGroup := map[string]any{
		"matcher": "",
		"hooks": []any{
			map[string]any{"type": "command", "command": command, "timeout": timeout},
		},
	}
	groups = append(groups, newGroup)
	hooks[event] = groups
	root["hooks"] = hooks

	if err := writeJSONObject(settingsPath, root, 0o644); err != nil {
		return false, err
	}
	return true, nil
}

// hookMarkerPresent reports whether any hook command in the event groups
// contains the marker substring.
func hookMarkerPresent(groups []any, marker string) bool {
	for _, g := range groups {
		gm, ok := g.(map[string]any)
		if !ok {
			continue
		}
		inner, ok := gm["hooks"].([]any)
		if !ok {
			continue
		}
		for _, h := range inner {
			hm, ok := h.(map[string]any)
			if !ok {
				continue
			}
			if cmd, ok := hm["command"].(string); ok && strings.Contains(cmd, marker) {
				return true
			}
		}
	}
	return false
}

// DetectClaudeCode reports whether Claude Code appears installed for the user
// (its config dir ~/.claude exists).
func DetectClaudeCode(homeDir string) bool {
	info, err := os.Stat(filepath.Join(homeDir, ".claude"))
	return err == nil && info.IsDir()
}

// ClaudeJSONPath / ClaudeSettingsPath resolve the user-scoped config files.
func ClaudeJSONPath(homeDir string) string {
	return filepath.Join(homeDir, ".claude.json")
}

func ClaudeSettingsPath(homeDir string) string {
	return filepath.Join(homeDir, ".claude", "settings.json")
}

// readJSONObject reads a JSON object file into a map, returning an empty map if
// the file does not exist.
func readJSONObject(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

// writeJSONObject writes the map as indented JSON, creating parent dirs. The
// write is atomic (temp file + rename) so a crash cannot truncate Claude Code's
// live config.
func writeJSONObject(path string, m map[string]any, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := path + ".citadel.tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	_ = os.Chmod(path, perm)
	return nil
}

func jsonEqual(a, b any) bool {
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return string(ab) == string(bb)
}
