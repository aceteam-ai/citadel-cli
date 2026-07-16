// internal/compose/envfile.go
//
// Resolution of the install-time config env for service compose invocations.
//
// `citadel service catalog install <name>` writes resolved config values to
// <servicesDir>/<name>.env, next to the installed <name>.yml compose file. But
// docker compose only auto-loads a file literally named ".env" (in the project
// directory), so that sibling is invisible unless passed explicitly with
// --env-file. Any compose file that guards its config with ${VAR:?...}
// interpolation (claudecode's ACETEAM_*/ANTHROPIC_*, livekit's LIVEKIT_*) then
// fails EVERY compose invocation — up, ps, restart, logs — even though the
// config exists on disk. The whatsapp command worked around this locally
// (cmd/whatsapp.go); this helper is the shared fix for the catalog services.
package compose

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// EnvFileArgs returns the "--env-file <path>" arguments for a compose
// invocation against an installed service compose file, when the install-time
// sibling env file (<name>.env next to <name>.yml) exists. Returns nil when
// there is no sibling, so call sites can append unconditionally.
//
// Derive from the ORIGINAL installed compose path, not a platform-stripped
// temp copy — the sibling lives next to the original (same rule as the
// sandbox override).
func EnvFileArgs(composePath string) []string {
	if composePath == "" {
		return nil
	}
	envPath := SiblingEnvPath(composePath)
	if fi, err := os.Stat(envPath); err != nil || fi.IsDir() {
		return nil
	}
	return []string{"--env-file", envPath}
}

// SiblingEnvPath returns the path of the install-time config env that sits
// next to a service compose file: <dir>/<name>.env for <dir>/<name>.yml. It is
// the single derivation shared by EnvFileArgs (read side) and the writers
// (catalog install, SERVICE_START model persistence #530), so a value written
// by one path is guaranteed to be the file the other passes to --env-file.
func SiblingEnvPath(composePath string) string {
	return strings.TrimSuffix(composePath, filepath.Ext(composePath)) + ".env"
}

// UpsertEnvVar sets key=value in the env file at envPath, creating the file
// (0600, parent dirs included) if absent and preserving every other line —
// comments, blank lines, and unrelated KEY=VALUE entries — untouched. If the
// key already exists (first occurrence wins), its value is replaced in place;
// later duplicate lines are dropped so the file cannot accumulate conflicting
// definitions. Used by the SERVICE_START model persistence (#530) to record
// the chosen model next to the compose file without clobbering install-time
// config the catalog wrote to the same file.
func UpsertEnvVar(envPath, key, value string) error {
	if envPath == "" || key == "" {
		return fmt.Errorf("env file path and key must be non-empty")
	}
	var lines []string
	if data, err := os.ReadFile(envPath); err == nil {
		lines = strings.Split(strings.TrimRight(string(data), "\n"), "\n")
		// A fresh/empty file splits to one empty line; treat as no lines.
		if len(lines) == 1 && lines[0] == "" {
			lines = nil
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	newLine := key + "=" + value
	replaced := false
	out := lines[:0]
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if k, _, found := strings.Cut(trimmed, "="); found &&
			!strings.HasPrefix(trimmed, "#") && strings.TrimSpace(k) == key {
			if replaced {
				continue // drop duplicate definitions of the key
			}
			out = append(out, newLine)
			replaced = true
			continue
		}
		out = append(out, line)
	}
	if !replaced {
		out = append(out, newLine)
	}

	if err := os.MkdirAll(filepath.Dir(envPath), 0755); err != nil {
		return err
	}
	// 0600: catalog installs write secrets (API keys) to the same file.
	return os.WriteFile(envPath, []byte(strings.Join(out, "\n")+"\n"), 0600)
}

// ReadEnvVar returns the value of key in the env file at envPath and whether it
// was present. A missing file reads as absent. Comment lines are ignored; the
// first matching definition wins (mirroring UpsertEnvVar's replacement rule).
func ReadEnvVar(envPath, key string) (string, bool) {
	data, err := os.ReadFile(envPath)
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if k, v, found := strings.Cut(trimmed, "="); found && strings.TrimSpace(k) == key {
			return strings.TrimSpace(v), true
		}
	}
	return "", false
}
