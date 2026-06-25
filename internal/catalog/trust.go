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
	// Publishers is the optional list of "verified publisher" trust entries: a
	// source pattern plus an expected cosign signing identity/key. A publisher
	// entry is itself a trust grant (it makes the matching source trusted) and,
	// when RequireSignature is set, also gates install on a successful signature
	// verification. Additive: an empty list leaves Patterns-based trust unchanged.
	Publishers []VerifiedPublisher `yaml:"publishers,omitempty"`
}

// VerifiedPublisher declares an expected signing identity (or public key) for a
// source pattern. The Pattern reuses the same matching semantics as Patterns
// (owner/repo, owner/*, or a bare host -- see matchTrust). When RequireSignature
// is true, the module's image signature must verify before install.
type VerifiedPublisher struct {
	// Pattern is a source pattern (owner/repo, owner/*, or a host).
	Pattern string `yaml:"pattern"`
	// RequireSignature gates install on a successful cosign verification.
	RequireSignature bool `yaml:"require_signature"`
	// Identity is the expected keyless OIDC identity (e.g. an email or a GitHub
	// Actions workflow URI). Used with Issuer for keyless verification.
	Identity string `yaml:"identity,omitempty"`
	// Issuer is the expected keyless OIDC issuer (e.g. https://token.actions.githubusercontent.com).
	Issuer string `yaml:"issuer,omitempty"`
	// Key is a path/URL/KMS URI to a cosign public key for keyful verification.
	Key string `yaml:"key,omitempty"`
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

// RemoveTrustedSource removes a pattern from the allowlist (both the plain
// Patterns list and any matching verified-publisher entry) and persists it.
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

	pubs := ts.Publishers[:0]
	for _, pub := range ts.Publishers {
		if pub.Pattern != pattern {
			pubs = append(pubs, pub)
		}
	}
	ts.Publishers = pubs
	return saveTrustedSources(ts)
}

// SetVerifiedPublisher adds or replaces a verified-publisher entry (keyed by
// Pattern) and persists it. The pattern is also added to the plain Patterns list
// so the source counts as trusted even before any verification runs. A blank
// pattern is rejected.
func SetVerifiedPublisher(pub VerifiedPublisher) error {
	pub.Pattern = strings.TrimSpace(pub.Pattern)
	if pub.Pattern == "" {
		return fmt.Errorf("empty publisher pattern")
	}
	ts, err := LoadTrustedSources()
	if err != nil {
		return err
	}
	replaced := false
	for i := range ts.Publishers {
		if ts.Publishers[i].Pattern == pub.Pattern {
			ts.Publishers[i] = pub
			replaced = true
			break
		}
	}
	if !replaced {
		ts.Publishers = append(ts.Publishers, pub)
	}
	// Ensure the pattern is also in the plain allowlist (idempotent).
	hasPattern := false
	for _, p := range ts.Patterns {
		if p == pub.Pattern {
			hasPattern = true
			break
		}
	}
	if !hasPattern {
		ts.Patterns = append(ts.Patterns, pub.Pattern)
		sort.Strings(ts.Patterns)
	}
	return saveTrustedSources(ts)
}

// MatchVerifiedPublisher returns the verified-publisher entry whose pattern
// matches src, plus true, or a zero value and false if none matches. Catalog
// (Tier 0) sources never match a publisher entry (they are first-party and
// exempt from signature gating).
func MatchVerifiedPublisher(src Source) (VerifiedPublisher, bool) {
	if src.Kind == KindCatalog {
		return VerifiedPublisher{}, false
	}
	ts, err := LoadTrustedSources()
	if err != nil {
		return VerifiedPublisher{}, false
	}
	return matchPublisher(ts.Publishers, src)
}

// matchPublisher is the pure core: returns the first publisher entry whose
// pattern trusts src (reusing matchTrust's owner/repo, owner/*, host semantics).
// Table-tested without IO.
func matchPublisher(pubs []VerifiedPublisher, src Source) (VerifiedPublisher, bool) {
	for _, pub := range pubs {
		if matchTrust([]string{pub.Pattern}, src) {
			return pub, true
		}
	}
	return VerifiedPublisher{}, false
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
	if matchTrust(ts.Patterns, src) {
		return true
	}
	// A verified-publisher entry is itself a trust grant.
	_, ok := matchPublisher(ts.Publishers, src)
	return ok
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
