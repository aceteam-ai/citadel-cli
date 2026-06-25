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
		if err := validateRef(ref); err != nil {
			return Source{}, fmt.Errorf("invalid ref in %q: %w", raw, err)
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
		if strings.HasPrefix(owner, "-") || strings.HasPrefix(repo, "-") {
			return Source{}, fmt.Errorf("invalid owner/repo source %q: components cannot begin with '-'", raw)
		}
		if err := validateRef(ref); err != nil {
			return Source{}, fmt.Errorf("invalid ref in %q: %w", raw, err)
		}
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

// validateRef guards against git argument injection: a ref is passed as a
// positional argument to `git clone --branch`/`git fetch origin <ref>`, so a
// value beginning with '-' would be parsed by git as an option (e.g.
// `--upload-pack=<cmd>` → arbitrary command execution). Real git refnames never
// begin with '-', so reject that (and embedded whitespace/control characters).
// An empty ref (default branch) is allowed.
//
// Exception: a semver-RANGE constraint (e.g. ">=1.0 <2.0") legitimately contains
// a space. Such a ref is never passed to git verbatim -- ResolveSource resolves
// it to a concrete tag first -- so the whitespace check is relaxed for
// constraint/channel refs. The injection guard (no leading '-', no control
// characters) still applies, since those would be dangerous even pre-resolution.
func validateRef(ref string) error {
	if ref == "" {
		return nil
	}
	if strings.HasPrefix(ref, "-") {
		return fmt.Errorf("a git ref cannot begin with '-'")
	}
	allowSpace := IsVersionConstraint(ref)
	for _, r := range ref {
		if r == '\n' || r == '\r' || r < 0x20 {
			return fmt.Errorf("a git ref cannot contain control characters")
		}
		if (r == ' ' || r == '\t') && !allowSpace {
			return fmt.Errorf("a git ref cannot contain whitespace")
		}
	}
	return nil
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

// ResolvedModule is the result of resolving an external module source: the
// parsed manifest, the compose path, plus provenance (cache dir, resolved git
// commit, and the image references parsed from the compose) used for the
// lockfile and the pre-install confirmation prompt.
type ResolvedModule struct {
	Manifest    *ServiceManifest
	ComposePath string
	CacheDir    string
	// Commit is the resolved git HEAD of the cache checkout (or "" if unknown).
	Commit string
	// ResolvedRef is the concrete tag a constraint/channel ref resolved to (e.g.
	// "^1.2" -> "v1.4.0"). Empty when the ref was already exact (tag/branch/SHA)
	// or unpinned. Recorded in the lockfile so an install is reproducible.
	ResolvedRef string
	// Images are the container image references parsed from the compose file.
	Images []string
}

// ResolveSource clones (or updates) the module repo for src into a per-source
// cache directory and loads its ServiceManifest + compose file path along with
// provenance (resolved commit + compose image refs). For a KindCatalog source it
// returns an error: the caller is expected to delegate catalog names to the
// existing catalog install path.
func ResolveSource(src Source) (*ResolvedModule, error) {
	if src.Kind == KindCatalog {
		return nil, fmt.Errorf("source %q is a catalog name; use the catalog path", src.Raw)
	}

	// If the ref is a semver constraint or a channel (stable/latest), resolve it
	// to a concrete tag against the repo's remote tag list FIRST. This rewrites
	// src.Ref to the resolved tag so the cache dir keys on the concrete tag and
	// the existing branch-checkout path (strategyBranch) handles it unchanged.
	// Exact tags/branches/SHAs are not constraints and are left untouched.
	var resolvedRef string
	if IsVersionConstraint(src.Ref) {
		tag, err := resolveConstraintRef(src)
		if err != nil {
			return nil, err
		}
		resolvedRef = tag
		src.Ref = tag
	}

	cacheDir := filepath.Join(ModulesCacheDir(), sanitizeCacheName(src))
	if err := os.MkdirAll(filepath.Dir(cacheDir), 0755); err != nil {
		return nil, fmt.Errorf("failed to create modules cache directory: %w", err)
	}

	if err := cloneOrUpdate(src, cacheDir); err != nil {
		return nil, err
	}

	manifest, composePath, err := loadModuleManifest(cacheDir)
	if err != nil {
		return nil, err
	}

	res := &ResolvedModule{
		Manifest:    manifest,
		ComposePath: composePath,
		CacheDir:    cacheDir,
		Commit:      gitHeadCommit(cacheDir),
		ResolvedRef: resolvedRef,
	}
	if data, rerr := os.ReadFile(composePath); rerr == nil {
		res.Images = parseComposeImages(string(data))
	}
	return res, nil
}

// cloneStrategy classifies how a source ref must be fetched. It is pure so the
// decision can be unit-tested without touching git.
type cloneStrategy int

const (
	// strategyPlain: no ref pinned -- shallow clone the default branch.
	strategyPlain cloneStrategy = iota
	// strategyBranch: a tag/branch ref -- shallow clone with --branch <ref>.
	strategyBranch
	// strategyFetchSHA: a raw commit SHA -- clone default branch, then fetch +
	// checkout the SHA (git clone --branch cannot take a raw SHA).
	strategyFetchSHA
)

// pickCloneStrategy returns the strategy for src's ref.
func pickCloneStrategy(src Source) cloneStrategy {
	if src.Ref == "" {
		return strategyPlain
	}
	if looksLikeSHA(src.Ref) {
		return strategyFetchSHA
	}
	return strategyBranch
}

// looksLikeSHA reports whether ref is a 7-40 char hex string (an abbreviated or
// full git commit SHA). Pure -- table-tested.
func looksLikeSHA(ref string) bool {
	if len(ref) < 7 || len(ref) > 40 {
		return false
	}
	for _, r := range ref {
		isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
		if !isHex {
			return false
		}
	}
	return true
}

// cloneOrUpdate performs a shallow clone of the source repo into dir (or updates
// it if dir is already a git repo) and checks out the requested ref. Mirrors the
// style of catalog.Update().
func cloneOrUpdate(src Source, dir string) error {
	strategy := pickCloneStrategy(src)

	if isGitRepo(dir) {
		// Existing checkout: a pinned ref (branch/tag/SHA) is fetched + checked
		// out; an unpinned source fast-forwards the current branch.
		if src.Ref != "" {
			return fetchAndCheckout(src, dir)
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
	if strategy == strategyBranch {
		// --branch accepts tags and branches (but not raw SHAs).
		args = append(args, "--branch", src.Ref)
	}
	args = append(args, src.CloneURL, dir)

	clone := exec.Command("git", args...)
	if out, err := clone.CombinedOutput(); err != nil {
		return cloneError(src, fmt.Sprintf("git clone failed: %s", strings.TrimSpace(string(out))))
	}

	// A raw SHA cannot be selected at clone time; fetch + checkout it now.
	if strategy == strategyFetchSHA {
		return fetchAndCheckout(src, dir)
	}
	return nil
}

// fetchAndCheckout fetches src.Ref into an existing checkout at dir and checks it
// out. It first tries a shallow fetch (works for branches/tags and, on most git
// servers, for a SHA); if that fails it falls back to a full fetch (needed when a
// server refuses to serve an arbitrary SHA shallowly).
func fetchAndCheckout(src Source, dir string) error {
	fetch := exec.Command("git", "-C", dir, "fetch", "--depth", "1", "origin", src.Ref)
	if out, err := fetch.CombinedOutput(); err != nil {
		// Fall back to a full fetch (drop --depth) before giving up.
		full := exec.Command("git", "-C", dir, "fetch", "origin", src.Ref)
		if out2, err2 := full.CombinedOutput(); err2 != nil {
			return cloneError(src, fmt.Sprintf("git fetch failed: %s",
				strings.TrimSpace(string(out))+"; "+strings.TrimSpace(string(out2))))
		}
	}
	checkout := exec.Command("git", "-C", dir, "checkout", "FETCH_HEAD")
	if out, err := checkout.CombinedOutput(); err != nil {
		return cloneError(src, fmt.Sprintf("git checkout failed: %s", strings.TrimSpace(string(out))))
	}
	return nil
}

// resolveConstraintRef resolves a semver-constraint or channel src.Ref to a
// concrete tag by enumerating the repo's tags with `git ls-remote --tags` (no
// clone needed -- a shallow clone would not fetch the tag list) and picking the
// best match via the pure ResolveVersion logic. Returns the matching tag name.
func resolveConstraintRef(src Source) (string, error) {
	tags, err := remoteTags(src.CloneURL)
	if err != nil {
		return "", err
	}
	if len(tags) == 0 {
		return "", fmt.Errorf("could not resolve %q: source %q has no tags to match against", src.Ref, src.Raw)
	}
	resolved, err := ResolveVersion(src.Ref, tags)
	if err != nil {
		return "", fmt.Errorf("could not resolve version %q for %q: %w", src.Ref, src.Raw, err)
	}
	return resolved, nil
}

// remoteTags lists the tags of a remote repo via `git ls-remote --tags <url>`,
// returning de-duplicated tag names (annotated-tag deref entries normalized).
func remoteTags(cloneURL string) ([]string, error) {
	out, err := exec.Command("git", "ls-remote", "--tags", cloneURL).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git ls-remote --tags failed for %s: %s",
			cloneURL, strings.TrimSpace(string(out)))
	}
	return parseLsRemoteTags(string(out)), nil
}

// gitHeadCommit returns the resolved HEAD commit of dir, or "" if it can't be
// determined (best-effort, for provenance).
func gitHeadCommit(dir string) string {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// SourceHost extracts the bare host (e.g. "github.com") from a parsed source's
// clone URL. It handles scheme URLs, scp-form remotes, and userinfo ("git@host")
// uniformly. It returns "" when no host can be determined (e.g. a KindCatalog
// source, which has no CloneURL).
//
// This is the canonical host-parsing routine; callers that need a host to key
// credentials by (see internal/source) MUST use this rather than re-deriving it,
// so the scp/userinfo edge cases stay handled in exactly one place.
func SourceHost(src Source) string {
	url := src.CloneURL
	// Strip scheme.
	for _, scheme := range []string{"https://", "http://", "ssh://", "git://"} {
		url = strings.TrimPrefix(url, scheme)
	}
	// Drop any userinfo ("git@" / "user@") that precedes the host. This covers
	// both scp-form "git@host:owner/repo.git" and "ssh://git@host/owner/repo".
	// Only strip an '@' that comes before the first path separator, so an '@' in
	// a later path segment is not mistaken for userinfo.
	sepAt := strings.IndexAny(url, "/:")
	if at := strings.Index(url, "@"); at >= 0 && (sepAt < 0 || at < sepAt) {
		url = url[at+1:]
	}
	// Take the host up to the first '/' or ':'.
	if i := strings.IndexAny(url, "/:"); i >= 0 {
		url = url[:i]
	}
	return url
}

// cloneErrorHost extracts a host label from a source's clone URL for the
// credential hint (e.g. "github.com"), falling back to a friendly phrase when
// the host is unknown. Pure -- table-tested.
func cloneErrorHost(src Source) string {
	if host := SourceHost(src); host != "" {
		return host
	}
	return "the source host"
}

// cloneError wraps a git failure with guidance about credentials, since a vanilla
// node has no auth and cannot clone private source repos.
func cloneError(src Source, detail string) error {
	host := cloneErrorHost(src)
	return fmt.Errorf("could not fetch module source %q: %s\n"+
		"   If this is a private repository, this node needs git credentials for %s.\n"+
		"   Fix one of: set a GITHUB_TOKEN env var, configure an SSH key for %s, or\n"+
		"   set up a git credential helper (e.g. `git credential-store` or `gh auth login`).",
		src.Raw, detail, host, host)
}

// parseComposeImages extracts container image references from a compose file's
// `image:` lines, in order, de-duplicated. Pure -- table-tested.
func parseComposeImages(compose string) []string {
	var images []string
	seen := make(map[string]bool)
	for _, line := range strings.Split(compose, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "image:") {
			continue
		}
		val := strings.TrimSpace(strings.TrimPrefix(trimmed, "image:"))
		val = strings.Trim(val, `"'`)
		if val == "" || seen[val] {
			continue
		}
		seen[val] = true
		images = append(images, val)
	}
	return images
}

// parseComposeContainerName extracts the first `container_name:` value from a
// compose file, or "" if none. Pure -- table-tested.
func parseComposeContainerName(compose string) string {
	for _, line := range strings.Split(compose, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "container_name:") {
			continue
		}
		val := strings.TrimSpace(strings.TrimPrefix(trimmed, "container_name:"))
		return strings.Trim(val, `"'`)
	}
	return ""
}

// SchemaWarning returns a forward-compat warning string if the manifest declares
// a schema_version newer than this CLI understands, or "" otherwise. Pure so the
// catalog package stays import-clean; the cmd layer prints it.
func SchemaWarning(manifest *ServiceManifest) string {
	if manifest != nil && manifest.SchemaVersion > CurrentSchemaVersion {
		return fmt.Sprintf("module declares schema_version %d but this CLI understands up to %d; "+
			"some fields may be ignored -- consider updating citadel.",
			manifest.SchemaVersion, CurrentSchemaVersion)
	}
	return ""
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
