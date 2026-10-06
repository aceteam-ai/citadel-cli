package memory

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
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

// WriteMCPServer registers (or updates) the local Citadel stdio bridge in
// Claude Code's user-scoped ~/.claude.json. The bridge reads memory.yaml at
// runtime, so the bearer credential has exactly one on-disk copy.
func WriteMCPServer(claudeJSONPath, name, command string, args []string) (bool, error) {
	return updateJSONObject(claudeJSONPath, func(root map[string]any) (bool, error) {
		rawServers, exists := root["mcpServers"]
		servers, ok := rawServers.(map[string]any)
		if exists && rawServers != nil && !ok {
			return false, fmt.Errorf("mcpServers is not a JSON object")
		}
		if servers == nil {
			servers = map[string]any{}
		}
		entry := map[string]any{"type": "stdio", "command": command, "args": args}
		if existing, ok := servers[name].(map[string]any); ok && jsonEqual(existing, entry) {
			return false, nil
		}
		servers[name] = entry
		root["mcpServers"] = servers
		return true, nil
	})
}

// RemoveMCPServer removes only Citadel's named server entry, preserving every
// unrelated Claude Code setting and MCP server.
func RemoveMCPServer(claudeJSONPath, name string) (bool, error) {
	return updateJSONObject(claudeJSONPath, func(root map[string]any) (bool, error) {
		rawServers, exists := root["mcpServers"]
		servers, ok := rawServers.(map[string]any)
		if exists && rawServers != nil && !ok {
			return false, fmt.Errorf("mcpServers is not a JSON object")
		}
		if servers == nil {
			return false, nil
		}
		if _, ok := servers[name]; !ok {
			return false, nil
		}
		delete(servers, name)
		root["mcpServers"] = servers
		return true, nil
	})
}

// hookGroup mirrors one entry of a hooks.<Event> array.
type hookGroup struct {
	Matcher string     `json:"matcher"`
	Hooks   []hookSpec `json:"hooks"`
}

// RemoveHook removes only command hooks containing marker from one event,
// retaining unrelated hooks even when they share the same group.
func RemoveHook(settingsPath, event, marker string) (bool, error) {
	return updateJSONObject(settingsPath, func(root map[string]any) (bool, error) {
		rawHooks, exists := root["hooks"]
		hooks, ok := rawHooks.(map[string]any)
		if exists && rawHooks != nil && !ok {
			return false, fmt.Errorf("hooks is not a JSON object")
		}
		if hooks == nil {
			return false, nil
		}
		rawGroups, exists := hooks[event]
		groups, ok := rawGroups.([]any)
		if exists && rawGroups != nil && !ok {
			return false, fmt.Errorf("hooks.%s is not a JSON array", event)
		}
		changed := false
		keptGroups := make([]any, 0, len(groups))
		for _, rawGroup := range groups {
			group, ok := rawGroup.(map[string]any)
			if !ok {
				keptGroups = append(keptGroups, rawGroup)
				continue
			}
			entries, ok := group["hooks"].([]any)
			if !ok {
				keptGroups = append(keptGroups, rawGroup)
				continue
			}
			keptEntries := make([]any, 0, len(entries))
			for _, rawEntry := range entries {
				entry, ok := rawEntry.(map[string]any)
				command, _ := entry["command"].(string)
				if ok && strings.Contains(command, marker) {
					changed = true
					continue
				}
				keptEntries = append(keptEntries, rawEntry)
			}
			if len(keptEntries) > 0 {
				group["hooks"] = keptEntries
				keptGroups = append(keptGroups, group)
			}
		}
		if !changed {
			return false, nil
		}
		if len(keptGroups) == 0 {
			delete(hooks, event)
		} else {
			hooks[event] = keptGroups
		}
		root["hooks"] = hooks
		return true, nil
	})
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
	return updateJSONObject(settingsPath, func(root map[string]any) (bool, error) {
		rawHooks, exists := root["hooks"]
		hooks, ok := rawHooks.(map[string]any)
		if exists && rawHooks != nil && !ok {
			return false, fmt.Errorf("hooks is not a JSON object")
		}
		if hooks == nil {
			hooks = map[string]any{}
		}

		// Existing groups for this event (as generic slice for preservation).
		var groups []any
		if raw, exists := hooks[event]; exists && raw != nil {
			var ok bool
			groups, ok = raw.([]any)
			if !ok {
				return false, fmt.Errorf("hooks.%s is not a JSON array", event)
			}
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

		return true, nil
	})
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
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("parse %s: multiple JSON values", path)
		}
		return nil, fmt.Errorf("parse %s trailing data: %w", path, err)
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
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return atomicWriteFile(path, data, perm)
}

func updateJSONObject(path string, mutate func(map[string]any) (bool, error)) (bool, error) {
	changed := false
	err := withFileLock(path, func() error {
		root, err := readJSONObject(path)
		if err != nil {
			return err
		}
		changed, err = mutate(root)
		if err != nil || !changed {
			return err
		}
		return writeJSONObject(path, root, 0o600)
	})
	return changed, err
}

func jsonEqual(a, b any) bool {
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return string(ab) == string(bb)
}
