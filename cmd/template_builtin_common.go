package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/jobs"
)

// decodeBuiltinParams gives compiled-in runners an exact-key decoder. The
// approved schema gate is the first line of defence; this second line makes a
// builtin safe even if a future schema feature accidentally accepts an
// unconstrained key. It deliberately does not use encoding/json struct binding,
// whose case-insensitive field matching motivated the #1161 gate.
func decodeBuiltinParams(raw json.RawMessage, allowed ...string) (map[string]any, error) {
	v, err := jobs.DecodeTemplateJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("decode params: %w", err)
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("params must be an object")
	}
	allow := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		allow[key] = struct{}{}
	}
	for key := range m {
		if _, ok := allow[key]; !ok {
			return nil, fmt.Errorf("unknown or noncanonical param key %q", key)
		}
	}
	return m, nil
}

// removeStaleBuiltinOutput prevents an earlier failed or retried run from
// satisfying the post-command non-empty-file check. It also removes a stale
// symlink before a fixed output path is handed to an external encoder.
func removeStaleBuiltinOutput(path string) error {
	err := os.Remove(path)
	if err == nil || os.IsNotExist(err) {
		return nil
	}
	return fmt.Errorf("remove stale output %q: %w", path, err)
}

func withEnvOverrides(base []string, overrides ...string) []string {
	keys := make([]string, 0, len(overrides))
	for _, entry := range overrides {
		key, _, _ := strings.Cut(entry, "=")
		keys = append(keys, key)
	}
	out := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
		key, _, _ := strings.Cut(entry, "=")
		replaced := false
		for _, overrideKey := range keys {
			if strings.EqualFold(key, overrideKey) {
				replaced = true
				break
			}
		}
		if !replaced {
			out = append(out, entry)
		}
	}
	return append(out, overrides...)
}
