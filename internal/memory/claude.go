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

// Claude Code integration constants. RecallMarker and CaptureMarker are exact
// inert CLI ownership flags appended to commands installed by Citadel.
const (
	// MCPServerName is the key under mcpServers in ~/.claude.json.
	MCPServerName = "aceteam-memory"
	RecallMarker  = "--citadel-memory-hook-owner=recall-v1"
	CaptureMarker = "--citadel-memory-hook-owner=capture-v1"
)

var managedMCPArgs = []string{"--no-auto-update", "mcp", "--memory-config"}

// WriteMCPServer registers (or updates) the local Citadel stdio bridge in
// Claude Code's user-scoped ~/.claude.json. The bridge reads memory.yaml at
// runtime, so the bearer credential has exactly one on-disk copy.
func WriteMCPServer(claudeJSONPath, name, command string, args []string) (bool, error) {
	entry := map[string]any{"type": "stdio", "command": command, "args": args}
	if !isManagedMCPServer(entry) {
		return false, fmt.Errorf("refusing non-Citadel MCP server shape")
	}
	return updateJSONObject(claudeJSONPath, func(root map[string]any) (bool, error) {
		rawServers, exists := root["mcpServers"]
		servers, ok := rawServers.(map[string]any)
		if exists && rawServers != nil && !ok {
			return false, fmt.Errorf("mcpServers is not a JSON object")
		}
		if servers == nil {
			servers = map[string]any{}
		}
		if existingRaw, present := servers[name]; present {
			existing, ok := existingRaw.(map[string]any)
			if !ok || !isManagedMCPServer(existing) {
				return false, fmt.Errorf("mcpServers.%s already exists and is not owned by Citadel", name)
			}
			if jsonEqual(existing, entry) {
				return false, nil
			}
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
		existingRaw, present := servers[name]
		if !present {
			return false, nil
		}
		existing, ok := existingRaw.(map[string]any)
		if !ok || !isManagedMCPServer(existing) {
			return false, fmt.Errorf("mcpServers.%s is not owned by Citadel", name)
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

// RemoveHook removes only hooks carrying Citadel's exact ownership tag and
// exact generated entry shape, retaining unrelated hooks in the same group.
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
			matcher, matcherOK := group["matcher"].(string)
			if !ok || !matcherOK || matcher != "" {
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
				if ok && isManagedHookEntry(entry, marker) {
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
// It is idempotent and updates only an entry carrying the exact ownership tag;
// prose or unrelated commands containing similar words are never claimed.
func MergeHook(settingsPath, event, command, marker string, timeout int) (bool, error) {
	if marker != RecallMarker && marker != CaptureMarker {
		return false, fmt.Errorf("unknown Citadel hook marker")
	}
	managedCommand := strings.TrimSpace(command) + " " + marker
	desired := map[string]any{"type": "command", "command": managedCommand, "timeout": timeout}
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

		// Update only a provably Citadel-owned entry. This supports binary path
		// changes without ever overwriting a user's same-name hook.
		for _, rawGroup := range groups {
			group, ok := rawGroup.(map[string]any)
			matcher, matcherOK := group["matcher"].(string)
			if !ok || !matcherOK || matcher != "" {
				continue
			}
			entries, ok := group["hooks"].([]any)
			if !ok {
				continue
			}
			for i, rawEntry := range entries {
				entry, ok := rawEntry.(map[string]any)
				if !ok || !isManagedHookEntry(entry, marker) {
					continue
				}
				if jsonEqual(entry, desired) {
					return false, nil
				}
				entries[i] = desired
				group["hooks"] = entries
				hooks[event] = groups
				root["hooks"] = hooks
				return true, nil
			}
		}

		newGroup := map[string]any{
			"matcher": "",
			"hooks": []any{
				desired,
			},
		}
		groups = append(groups, newGroup)
		hooks[event] = groups
		root["hooks"] = hooks

		return true, nil
	})
}

// hookMarkerPresent reports whether an exact Citadel-owned hook is present.
func hookMarkerPresent(groups []any, marker string) bool {
	for _, g := range groups {
		gm, ok := g.(map[string]any)
		if !ok {
			continue
		}
		matcher, matcherOK := gm["matcher"].(string)
		inner, ok := gm["hooks"].([]any)
		if !ok || !matcherOK || matcher != "" {
			continue
		}
		for _, h := range inner {
			hm, ok := h.(map[string]any)
			if !ok {
				continue
			}
			if isManagedHookEntry(hm, marker) {
				return true
			}
		}
	}
	return false
}

func isManagedHookEntry(entry map[string]any, marker string) bool {
	if marker != RecallMarker && marker != CaptureMarker || len(entry) != 3 {
		return false
	}
	typeValue, typeOK := entry["type"].(string)
	command, commandOK := entry["command"].(string)
	if !typeOK || typeValue != "command" || !commandOK ||
		!strings.HasSuffix(command, " "+marker) || strings.TrimSuffix(command, " "+marker) == "" {
		return false
	}
	switch timeout := entry["timeout"].(type) {
	case json.Number:
		value, err := timeout.Int64()
		return err == nil && value > 0
	case int:
		return timeout > 0
	case float64:
		return timeout > 0 && timeout == float64(int64(timeout))
	default:
		return false
	}
}

func isManagedMCPServer(entry map[string]any) bool {
	if len(entry) != 3 || entry["type"] != "stdio" {
		return false
	}
	command, ok := entry["command"].(string)
	if !ok || command == "" {
		return false
	}
	base := strings.ToLower(filepath.Base(command))
	if base != "citadel" && base != "citadel.exe" {
		return false
	}
	rawArgs, ok := entry["args"]
	if !ok {
		return false
	}
	return jsonEqual(rawArgs, managedMCPArgs)
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
	data, err := readClaudeConfigFile(path)
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

const maxClaudeConfigBytes = 8 << 20

// readClaudeConfigFile refuses links and special files before opening, then
// verifies that the opened descriptor still names the same regular path. This
// prevents a FIFO from blocking configuration updates and avoids silently
// severing a dotfile manager's symlink during the later atomic rename. Reads
// are bounded so a corrupt file cannot cause an unbounded allocation.
func readClaudeConfigFile(path string) ([]byte, error) {
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.Mode().IsRegular() {
		return nil, fmt.Errorf("Claude config is not a regular file")
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	openedInfo, err := f.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() {
		return nil, fmt.Errorf("Claude config is not a regular file")
	}
	currentInfo, err := os.Lstat(path)
	if err != nil || currentInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(openedInfo, currentInfo) {
		return nil, fmt.Errorf("Claude config changed while opening")
	}

	data, err := io.ReadAll(io.LimitReader(f, maxClaudeConfigBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxClaudeConfigBytes {
		return nil, fmt.Errorf("Claude config exceeds %d bytes", maxClaudeConfigBytes)
	}
	return data, nil
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
