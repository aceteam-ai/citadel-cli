package catalog

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"gopkg.in/yaml.v3"
)

// TrustedSources is the persisted allowlist of module source patterns the
// operator has explicitly trusted. Patterns may be:
//   - exact "owner/repo"            (matches that GitHub shorthand)
//   - "owner/*"                     (any repo under that owner)
//   - a bare host "github.com"      (any source on that host)
type TrustedSources struct {
	Version  int      `yaml:"version"`
	Patterns []string `yaml:"patterns"`
}

// TrustedSourcesPath returns the path to the trusted-sources allowlist file.
func TrustedSourcesPath() string {
	return filepath.Join(platform.ConfigDir(), "trusted_sources.yaml")
}

// LoadTrustedSources reads the allowlist, returning an empty (version 1) list if
// the file does not yet exist.
func LoadTrustedSources() (*TrustedSources, error) {
	path := TrustedSourcesPath()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &TrustedSources{Version: 1}, nil
		}
		return nil, fmt.Errorf("failed to read trusted sources %s: %w", path, err)
	}
	var ts TrustedSources
	if err := yaml.Unmarshal(data, &ts); err != nil {
		return nil, fmt.Errorf("failed to parse trusted sources %s: %w", path, err)
	}
	if ts.Version == 0 {
		ts.Version = 1
	}
	return &ts, nil
}

// saveTrustedSources writes the allowlist back to disk.
func saveTrustedSources(ts *TrustedSources) error {
	path := TrustedSourcesPath()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}
	data, err := yaml.Marshal(ts)
	if err != nil {
		return fmt.Errorf("failed to marshal trusted sources: %w", err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("failed to write trusted sources %s: %w", path, err)
	}
	return nil
}

// AddTrustedSource adds a pattern to the allowlist (idempotent) and persists it.
func AddTrustedSource(pattern string) error {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return fmt.Errorf("empty trust pattern")
	}
	ts, err := LoadTrustedSources()
	if err != nil {
		return err
	}
	for _, p := range ts.Patterns {
		if p == pattern {
			return nil // already present
		}
	}
	ts.Patterns = append(ts.Patterns, pattern)
	sort.Strings(ts.Patterns)
	return saveTrustedSources(ts)
}

// RemoveTrustedSource removes a pattern from the allowlist and persists it.
// Removing a pattern that is not present is a no-op (no error).
func RemoveTrustedSource(pattern string) error {
	pattern = strings.TrimSpace(pattern)
	ts, err := LoadTrustedSources()
	if err != nil {
		return err
	}
	out := ts.Patterns[:0]
	for _, p := range ts.Patterns {
		if p != pattern {
			out = append(out, p)
		}
	}
	ts.Patterns = out
	return saveTrustedSources(ts)
}

// IsTrusted reports whether a parsed source is trusted. A catalog source
// (KindCatalog) is always trusted (Tier 0, first-party). External sources match
// against the persisted allowlist patterns.
func IsTrusted(src Source) bool {
	if src.Kind == KindCatalog {
		return true
	}
	ts, err := LoadTrustedSources()
	if err != nil {
		return false
	}
	return matchTrust(ts.Patterns, src)
}

// matchTrust is the pure matching core: reports whether any pattern trusts src.
// Table-tested without IO.
//
// Matching rules by kind:
//   - KindGitHub (owner/repo): matches exact "owner/repo", "owner/*", or the
//     host pattern "github.com" (GitHub shorthand is always on github.com).
//   - KindGitURL: host-level trust only -- matches a bare host pattern equal to
//     the URL's host. (owner-level matching on arbitrary git URLs is not
//     attempted; see note below.)
func matchTrust(patterns []string, src Source) bool {
	for _, raw := range patterns {
		p := strings.TrimSpace(raw)
		if p == "" {
			continue
		}
		switch src.Kind {
		case KindGitHub:
			if p == "github.com" {
				return true
			}
			if p == src.Owner+"/"+src.Repo {
				return true
			}
			if p == src.Owner+"/*" {
				return true
			}
		case KindGitURL:
			// Host-level trust for raw git URLs. owner/repo or owner/* patterns
			// are not matched here because a git URL's owner is not reliably
			// parseable across hosts -- a known limitation, host trust is the
			// realistic match.
			if !strings.Contains(p, "/") && p == cloneErrorHost(src) {
				return true
			}
		}
	}
	return false
}
