package catalog

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"gopkg.in/yaml.v3"
)

// SourceKind classifies how a module source string should be resolved.
type SourceKind int

const (
	// KindCatalog is a plain service name resolved from the central catalog
	// (no slash, no scheme). The caller delegates to the existing catalog path.
	KindCatalog SourceKind = iota
	// KindGitHub is an "owner/repo" (optionally "owner/repo@ref") shorthand that
	// expands to https://github.com/owner/repo.git.
	KindGitHub
	// KindGitURL is a full git URL (https://…[.git][@ref|#ref] or scp-form
	// git@host:owner/repo.git).
	KindGitURL
)

// String renders a SourceKind for diagnostics.
func (k SourceKind) String() string {
	switch k {
	case KindCatalog:
		return "catalog"
	case KindGitHub:
		return "github"
	case KindGitURL:
		return "git-url"
	default:
		return "unknown"
	}
}

// Source is a parsed module source: where to find the module and at which ref.
type Source struct {
	// Kind is the resolution strategy.
	Kind SourceKind
	// Raw is the original, unmodified source string.
	Raw string
	// Name is the catalog service name (KindCatalog only).
	Name string
	// Owner / Repo are populated for KindGitHub (the "owner/repo" shorthand).
	Owner string
	Repo  string
	// CloneURL is the git clone URL for KindGitHub / KindGitURL.
	CloneURL string
	// Ref is an optional tag/branch/sha to check out (empty = default branch).
	Ref string
}

// ParseSource classifies a module source string into a Source. The classification
// order matters: scheme is checked first and a bare catalog name is the fallback.
//
//   - full git URL (https://…, http://…, ssh://…, or scp-form git@host:owner/repo.git)
//     → KindGitURL. Only the URL forms (not the scp form) accept a trailing
//     "@ref" or "#ref" suffix.
//   - "owner/repo" or "owner/repo@ref" (contains a slash, no scheme)
//     → KindGitHub, clone URL https://github.com/owner/repo.git.
//   - anything else (no slash, no scheme) → KindCatalog.
func ParseSource(s string) (Source, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return Source{}, fmt.Errorf("empty module source")
	}

	// 1. Full git URLs (scheme-based or scp-form).
	if hasGitScheme(raw) {
		url, ref := splitURLRef(raw)
		if url == "" {
			return Source{}, fmt.Errorf("invalid git URL %q", raw)
		}
		return Source{Kind: KindGitURL, Raw: raw, CloneURL: url, Ref: ref}, nil
	}
	if isSCPForm(raw) {
		// scp-form (git@host:owner/repo.git) contains '@' and ':' but no ref
		// suffix support -- splitting on '@' here would corrupt the user part.
		return Source{Kind: KindGitURL, Raw: raw, CloneURL: raw}, nil
	}

	// 2. "owner/repo" shorthand (contains a slash, no scheme).
	if strings.Contains(raw, "/") {
		repoPart, ref := splitAtRef(raw)
		parts := strings.Split(repoPart, "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return Source{}, fmt.Errorf("invalid owner/repo source %q (expected owner/repo[@ref])", raw)
		}
		owner, repo := parts[0], strings.TrimSuffix(parts[1], ".git")
		return Source{
			Kind:     KindGitHub,
			Raw:      raw,
			Owner:    owner,
			Repo:     repo,
			CloneURL: fmt.Sprintf("https://github.com/%s/%s.git", owner, repo),
			Ref:      ref,
		}, nil
	}

	// 3. Plain catalog name.
	if strings.ContainsAny(raw, "@:") {
		return Source{}, fmt.Errorf("invalid catalog name %q", raw)
	}
	return Source{Kind: KindCatalog, Raw: raw, Name: raw}, nil
}

// hasGitScheme reports whether s starts with a recognized URL scheme.
func hasGitScheme(s string) bool {
	for _, scheme := range []string{"https://", "http://", "ssh://", "git://"} {
		if strings.HasPrefix(s, scheme) {
			return true
		}
	}
	return false
}

// isSCPForm reports whether s is an scp-style git remote (git@host:owner/repo.git
// or user@host:path). It must contain '@' before a ':' and not be a scheme URL.
func isSCPForm(s string) bool {
	at := strings.Index(s, "@")
	colon := strings.Index(s, ":")
	return at > 0 && colon > at
}

// splitURLRef splits a scheme URL into its base URL and an optional ref. The ref
// may be appended as "#ref" (preferred for URLs) or "@ref". For "@ref" we only
// split on an '@' that appears after the host (i.e. after the "://"), so a
// userinfo "@" in the authority is preserved.
func splitURLRef(s string) (url, ref string) {
	// "#ref" suffix.
	if i := strings.LastIndex(s, "#"); i >= 0 {
		return s[:i], s[i+1:]
	}
	// "@ref" suffix: only consider an '@' that comes after the scheme's "://".
	schemeEnd := strings.Index(s, "://")
	if schemeEnd >= 0 {
		rest := s[schemeEnd+3:]
		if at := strings.LastIndex(rest, "@"); at >= 0 {
			// Treat as a ref only if it follows a path segment (contains '/'
			// before the '@'); otherwise it is userinfo and must be kept.
			if strings.Contains(rest[:at], "/") {
				return s[:schemeEnd+3+at], rest[at+1:]
			}
		}
	}
	return s, ""
}

// splitAtRef splits an "owner/repo@ref" into the repo part and the ref, splitting
// on the last '@'. The ref must not contain a '/' that would belong to the path.
func splitAtRef(s string) (repoPart, ref string) {
	if at := strings.LastIndex(s, "@"); at >= 0 {
		candidateRef := s[at+1:]
		// A ref never contains a slash; if it does, the '@' was not a ref marker.
		if candidateRef != "" && !strings.Contains(candidateRef, "/") {
			return s[:at], candidateRef
		}
	}
	return s, ""
}

// sanitizeCacheName turns a Source into a filesystem-safe cache directory name,
// e.g. "owner-repo@v1.2.0" for github sources or a slug derived from the URL.
func sanitizeCacheName(src Source) string {
	var base string
	switch src.Kind {
	case KindGitHub:
		base = src.Owner + "-" + src.Repo
	default:
		// Derive a slug from the clone URL: strip scheme, trailing .git, and
		// replace path separators.
		slug := src.CloneURL
		for _, scheme := range []string{"https://", "http://", "ssh://", "git://"} {
			slug = strings.TrimPrefix(slug, scheme)
		}
		slug = strings.TrimSuffix(slug, ".git")
		base = slug
	}
	if src.Ref != "" {
		base = base + "@" + src.Ref
	}
	return sanitizePathSegment(base)
}

// sanitizePathSegment replaces characters that are unsafe in a path segment.
func sanitizePathSegment(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-', r == '_', r == '.', r == '@':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}

// ModulesCacheDir returns the parent cache directory for external module sources.
func ModulesCacheDir() string {
	return filepath.Join(platform.ConfigDir(), "modules")
}

// ResolveSource clones (or updates) the module repo for src into a per-source
// cache directory and loads its ServiceManifest + compose file path. For a
// KindCatalog source it returns an error: the caller is expected to delegate
// catalog names to the existing catalog install path.
func ResolveSource(src Source) (manifest *ServiceManifest, composePath string, err error) {
	if src.Kind == KindCatalog {
		return nil, "", fmt.Errorf("source %q is a catalog name; use the catalog path", src.Raw)
	}

	cacheDir := filepath.Join(ModulesCacheDir(), sanitizeCacheName(src))
	if err := os.MkdirAll(filepath.Dir(cacheDir), 0755); err != nil {
		return nil, "", fmt.Errorf("failed to create modules cache directory: %w", err)
	}

	if err := cloneOrUpdate(src, cacheDir); err != nil {
		return nil, "", err
	}

	return loadModuleManifest(cacheDir)
}

// cloneOrUpdate performs a shallow clone of the source repo into dir, or updates
// it if dir is already a git repo. Mirrors the style of catalog.Update().
func cloneOrUpdate(src Source, dir string) error {
	if isGitRepo(dir) {
		// Existing checkout: fetch + reset to the requested ref (or fast-forward
		// the current branch when no ref is pinned).
		if src.Ref != "" {
			fetch := exec.Command("git", "-C", dir, "fetch", "--depth", "1", "origin", src.Ref)
			if out, err := fetch.CombinedOutput(); err != nil {
				return cloneError(src, fmt.Sprintf("git fetch failed: %s", strings.TrimSpace(string(out))))
			}
			checkout := exec.Command("git", "-C", dir, "checkout", "FETCH_HEAD")
			if out, err := checkout.CombinedOutput(); err != nil {
				return cloneError(src, fmt.Sprintf("git checkout failed: %s", strings.TrimSpace(string(out))))
			}
			return nil
		}
		pull := exec.Command("git", "-C", dir, "pull", "--ff-only")
		if out, err := pull.CombinedOutput(); err != nil {
			return cloneError(src, fmt.Sprintf("git pull failed: %s", strings.TrimSpace(string(out))))
		}
		return nil
	}

	// Fresh clone. Remove any leftover non-git directory first.
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("failed to clean module cache directory: %w", err)
	}

	args := []string{"clone", "--depth", "1"}
	if src.Ref != "" {
		// --branch accepts tags and branches (but not raw SHAs).
		args = append(args, "--branch", src.Ref)
	}
	args = append(args, src.CloneURL, dir)

	clone := exec.Command("git", args...)
	if out, err := clone.CombinedOutput(); err != nil {
		return cloneError(src, fmt.Sprintf("git clone failed: %s", strings.TrimSpace(string(out))))
	}
	return nil
}

// cloneError wraps a git failure with guidance about credentials, since a vanilla
// node has no GitHub auth and cannot clone private source repos.
func cloneError(src Source, detail string) error {
	return fmt.Errorf("could not fetch module source %q: %s\n"+
		"   If this is a private repository, this node needs git credentials to clone it:\n"+
		"   set a GITHUB_TOKEN, configure an SSH key, or set up a git credential helper.",
		src.Raw, detail)
}

// loadModuleManifest locates and parses the module's service.yaml and resolves
// its compose file path. It prefers the standardized "citadel/" subdirectory and
// falls back to the repo root.
func loadModuleManifest(dir string) (*ServiceManifest, string, error) {
	manifestPath := firstExisting(
		filepath.Join(dir, "citadel", "service.yaml"),
		filepath.Join(dir, "service.yaml"),
	)
	if manifestPath == "" {
		return nil, "", fmt.Errorf("no service.yaml found in module repo (looked in citadel/ and repo root); " +
			"a Citadel module must self-describe via citadel/service.yaml + citadel/compose.yml")
	}

	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, "", fmt.Errorf("failed to read module manifest %s: %w", manifestPath, err)
	}
	var manifest ServiceManifest
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		return nil, "", fmt.Errorf("failed to parse module manifest %s: %w", manifestPath, err)
	}
	if manifest.Name == "" {
		return nil, "", fmt.Errorf("module manifest %s has no 'name'", manifestPath)
	}

	composePath := firstExisting(
		filepath.Join(dir, "citadel", "compose.yml"),
		filepath.Join(dir, "compose.yml"),
	)
	if composePath == "" {
		return nil, "", fmt.Errorf("no compose.yml found in module repo (looked in citadel/ and repo root)")
	}

	return &manifest, composePath, nil
}

// firstExisting returns the first path that exists, or "".
func firstExisting(paths ...string) string {
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}
