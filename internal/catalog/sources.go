// internal/catalog/sources.go
//
// Named catalog sources (#309). Out of the box the catalog has a single,
// hardcoded official source (aceteam-ai/citadel-services). This file adds the
// ability to register ADDITIONAL named sources -- community git repos laid out
// like the official catalog (a top-level registry.yaml and services/<name>/
// subdirs) -- so `catalog update/list/search/info/install` span every source.
//
// Persistence: the configured extra sources live in a small YAML file at
// <config>/catalog-sources.yaml. Each extra source is cloned into its own
// subdirectory under <config>/catalog-sources/<name>/. Both live OUTSIDE
// GetCatalogPath() on purpose: Update() does an os.RemoveAll(GetCatalogPath())
// when it re-clones the default catalog, so anything nested there would be
// silently wiped.
//
// The default source is always present, is named "default", and cannot be
// removed. Its cache stays at GetCatalogPath() so existing readers and the
// services/<name>/ on-disk layout are unchanged (no migration needed).
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

const (
	// DefaultSourceName is the reserved name of the built-in official source. It
	// is always present and cannot be added or removed.
	DefaultSourceName = "default"
	// sourcesFileName is the YAML file (under the config dir) that persists the
	// list of user-added catalog sources.
	sourcesFileName = "catalog-sources.yaml"
	// sourcesSubdir is the directory (under the config dir) that holds the cloned
	// cache of each user-added source, one subdir per source name.
	sourcesSubdir = "catalog-sources"
)

// CatalogSource is one configured catalog source: a unique name plus the git URL
// to clone it from. The default source is implicit and not stored here.
type CatalogSource struct {
	Name string `yaml:"name"`
	URL  string `yaml:"url"`
}

// sourcesFile is the on-disk shape of catalog-sources.yaml.
type sourcesFile struct {
	Version int             `yaml:"version"`
	Sources []CatalogSource `yaml:"sources"`
}

// resolvedSource is an internal pairing of a source's identity with the local
// cache directory its registry/manifests are read from. The default source's
// Path is GetCatalogPath(); each added source's Path is its subdir.
type resolvedSource struct {
	Name    string
	URL     string
	Path    string
	Default bool
}

// SourcesFilePath returns the path to the catalog-sources.yaml file.
func SourcesFilePath() string {
	return filepath.Join(platform.ConfigDir(), sourcesFileName)
}

// sourcesCacheDir returns the parent directory holding per-source clones.
func sourcesCacheDir() string {
	return filepath.Join(platform.ConfigDir(), sourcesSubdir)
}

// sourceCachePath returns the local clone directory for a named (non-default)
// source. The name is sanitized again here as defense-in-depth even though
// LoadSources rejects unsafe names on read.
func sourceCachePath(name string) string {
	return filepath.Join(sourcesCacheDir(), sanitizePathSegment(name))
}

// LoadSources reads the configured user-added sources from disk. A missing file
// yields an empty list and a nil error (the default source is always implied).
// Names/URLs are validated on read so a hand-edited file with an unsafe name can
// never reach a git clone or filesystem path.
func LoadSources() ([]CatalogSource, error) {
	path := SourcesFilePath()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read catalog sources %s: %w", path, err)
	}
	var sf sourcesFile
	if err := yaml.Unmarshal(data, &sf); err != nil {
		return nil, fmt.Errorf("failed to parse catalog sources %s: %w", path, err)
	}
	for _, s := range sf.Sources {
		if err := ValidateSourceName(s.Name); err != nil {
			return nil, fmt.Errorf("invalid source name in %s: %w", path, err)
		}
		if err := ValidateSourceURL(s.URL); err != nil {
			return nil, fmt.Errorf("invalid source URL for %q in %s: %w", s.Name, path, err)
		}
	}
	return sf.Sources, nil
}

// saveSources persists the given sources to catalog-sources.yaml.
func saveSources(sources []CatalogSource) error {
	path := SourcesFilePath()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}
	data, err := yaml.Marshal(sourcesFile{Version: 1, Sources: sources})
	if err != nil {
		return fmt.Errorf("failed to marshal catalog sources: %w", err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("failed to write catalog sources %s: %w", path, err)
	}
	return nil
}

// ValidateSourceName checks that name is a safe, non-reserved catalog source
// name. It must be non-empty, must not be the reserved default name, and must
// survive path-segment sanitization unchanged (which rejects path traversal,
// separators, and other unsafe characters since the name becomes a directory).
func ValidateSourceName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("source name cannot be empty")
	}
	if name == DefaultSourceName {
		return fmt.Errorf("%q is reserved for the built-in official source", DefaultSourceName)
	}
	// Reject dot-only names ("." / ".." / "...") explicitly: they survive
	// sanitizePathSegment unchanged (all chars are allowed) but are path-relative
	// specials. Used as a directory segment, ".." escapes the cache dir, so a
	// later RemoveSource(name) -> os.RemoveAll could target a parent directory.
	if strings.Trim(name, ".") == "" {
		return fmt.Errorf("source name %q is not allowed", name)
	}
	if sanitizePathSegment(name) != name {
		return fmt.Errorf("source name %q contains unsafe characters (use letters, digits, '-', '_', '.')", name)
	}
	return nil
}

// ValidateSourceURL checks that url is a plausible, safe git source. It reuses
// ParseSource (the module-source parser) to accept full git URLs and the
// owner/repo shorthand while rejecting empty/garbage input. A bare catalog name
// (KindCatalog) is rejected: a catalog source must be a clonable repo.
func ValidateSourceURL(url string) error {
	url = strings.TrimSpace(url)
	if url == "" {
		return fmt.Errorf("source URL cannot be empty")
	}
	src, err := ParseSource(url)
	if err != nil {
		return err
	}
	if src.Kind == KindCatalog {
		return fmt.Errorf("%q is not a git URL or owner/repo (a catalog source must be a git repository)", url)
	}
	return nil
}

// DefaultSourceNameFromURL derives a reasonable source name from a git URL or
// owner/repo shorthand (the repository's base name, minus a trailing ".git" and
// any ref suffix), sanitized for use as a directory. Returns "" if no usable
// name can be derived. Used as the `add` default when --name is not supplied.
func DefaultSourceNameFromURL(url string) string {
	src, err := ParseSource(strings.TrimSpace(url))
	if err != nil {
		return ""
	}
	if src.Kind == KindGitHub && src.Repo != "" {
		return sanitizePathSegment(src.Repo)
	}
	// Derive from the clone URL's last path segment.
	clone := src.CloneURL
	for _, scheme := range []string{"https://", "http://", "ssh://", "git://"} {
		clone = strings.TrimPrefix(clone, scheme)
	}
	clone = strings.TrimSuffix(clone, ".git")
	// scp-form "git@host:owner/repo" uses ':' before the path; normalize to '/'.
	clone = strings.ReplaceAll(clone, ":", "/")
	clone = strings.TrimRight(clone, "/")
	if i := strings.LastIndex(clone, "/"); i >= 0 {
		clone = clone[i+1:]
	}
	return sanitizePathSegment(clone)
}

// resolveSourceURL turns a user-provided source URL into the concrete git clone
// URL, expanding the owner/repo shorthand. ValidateSourceURL must have passed.
func resolveSourceURL(url string) (string, error) {
	src, err := ParseSource(strings.TrimSpace(url))
	if err != nil {
		return "", err
	}
	return src.CloneURL, nil
}

// AddSource registers a new named catalog source. It validates the name and URL,
// rejects the reserved default name, and rejects a duplicate name. It does not
// clone the repo -- the next `catalog update` pulls all sources.
func AddSource(name, url string) error {
	name = strings.TrimSpace(name)
	url = strings.TrimSpace(url)
	if err := ValidateSourceName(name); err != nil {
		return err
	}
	if err := ValidateSourceURL(url); err != nil {
		return err
	}
	sources, err := LoadSources()
	if err != nil {
		return err
	}
	for _, s := range sources {
		if s.Name == name {
			return fmt.Errorf("a catalog source named %q already exists", name)
		}
	}
	sources = append(sources, CatalogSource{Name: name, URL: url})
	return saveSources(sources)
}

// RemoveSource unregisters a named catalog source and removes its local clone.
// Removing the default source is an error.
func RemoveSource(name string) error {
	name = strings.TrimSpace(name)
	if name == DefaultSourceName {
		return fmt.Errorf("cannot remove the built-in %q catalog source", DefaultSourceName)
	}
	sources, err := LoadSources()
	if err != nil {
		return err
	}
	idx := -1
	for i, s := range sources {
		if s.Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("no catalog source named %q", name)
	}
	sources = append(sources[:idx], sources[idx+1:]...)
	if err := saveSources(sources); err != nil {
		return err
	}
	// Best-effort: drop the cached clone so a later re-add starts fresh.
	_ = os.RemoveAll(sourceCachePath(name))
	return nil
}

// ListSources returns every configured source including the implicit default,
// with the default first followed by user-added sources in registration order.
func ListSources() ([]CatalogSource, error) {
	added, err := LoadSources()
	if err != nil {
		return nil, err
	}
	out := make([]CatalogSource, 0, len(added)+1)
	out = append(out, CatalogSource{Name: DefaultSourceName, URL: DefaultCatalogURL})
	out = append(out, added...)
	return out, nil
}

// resolvedSources returns each source paired with the local cache path its
// registry/manifests are read from, default first. It does not touch the
// network; an absent clone is simply skipped by the readers.
func resolvedSources() ([]resolvedSource, error) {
	added, err := LoadSources()
	if err != nil {
		return nil, err
	}
	out := make([]resolvedSource, 0, len(added)+1)
	out = append(out, resolvedSource{
		Name:    DefaultSourceName,
		URL:     DefaultCatalogURL,
		Path:    GetCatalogPath(),
		Default: true,
	})
	for _, s := range added {
		out = append(out, resolvedSource{
			Name: s.Name,
			URL:  s.URL,
			Path: sourceCachePath(s.Name),
		})
	}
	return out, nil
}

// ResolvedCatalogService is the atomic resolution of a catalog service name to a
// single owning source: its manifest, compose path, and source name all come
// from the SAME source. Resolving these together (rather than via three
// independent cross-source searches) is a security invariant: the trust decision
// (SourceName) must key the very compose that gets installed, so a community
// source cannot shadow a default service's name to install its own compose under
// the default's trusted privileges.
type ResolvedCatalogService struct {
	// Manifest is the parsed service.yaml from the owning source.
	Manifest *ServiceManifest
	// ComposePath is the compose.yml path in the owning source, or "" when the
	// service is host-provisioned (no compose.yml) in that source.
	ComposePath string
	// SourceName is the owning source (drives the trust decision).
	SourceName string
}

// ResolveCatalogService resolves a service name to a single owning source,
// following the standard precedence (default first, then user-added sources in
// registration order). The FIRST source that contains the service's service.yaml
// wins; the manifest AND compose are read from that same source so trust and
// compose can never come from different sources. A source whose service.yaml is
// present but unparseable is a hard error (it is the winner -- we do not silently
// fall through to a lower-precedence source, which would reintroduce the shadow).
func ResolveCatalogService(name string) (*ResolvedCatalogService, error) {
	sources, err := resolvedSources()
	if err != nil {
		return nil, err
	}
	for _, src := range sources {
		manifestPath := filepath.Join(src.Path, servicesSubdir, name, "service.yaml")
		if _, statErr := os.Stat(manifestPath); statErr != nil {
			if os.IsNotExist(statErr) {
				continue
			}
			return nil, fmt.Errorf("failed to access service manifest for '%s': %w", name, statErr)
		}
		manifest, mErr := loadManifestFromPath(src.Path, name)
		if mErr != nil {
			return nil, mErr
		}
		composePath := ""
		cp := filepath.Join(src.Path, servicesSubdir, name, "compose.yml")
		if _, cErr := os.Stat(cp); cErr == nil {
			composePath = cp
		}
		return &ResolvedCatalogService{
			Manifest:    manifest,
			ComposePath: composePath,
			SourceName:  src.Name,
		}, nil
	}
	return nil, fmt.Errorf("service '%s' not found in catalog", name)
}

// SourceOf returns the name of the catalog source that owns service `name`,
// following the same precedence the readers use (default first, then user-added
// sources in registration order). It returns "" if the service is not found in
// any cloned source. Used by the install path to decide a service's trust level:
// only the built-in default source is first-party (Tier 0); community sources
// are untrusted (Tier 2) and get the least-privilege sandbox + privilege gate.
func SourceOf(name string) string {
	sources, err := resolvedSources()
	if err != nil {
		return ""
	}
	for _, src := range sources {
		manifestPath := filepath.Join(src.Path, servicesSubdir, name, "service.yaml")
		if _, err := os.Stat(manifestPath); err == nil {
			return src.Name
		}
	}
	return ""
}

// IsDefaultSource reports whether sourceName is the built-in (trusted) default
// source. An empty name is treated as the default for backward compatibility
// (legacy single-source installs).
func IsDefaultSource(sourceName string) bool {
	return sourceName == "" || sourceName == DefaultSourceName
}

// sourceRegistry pairs a source name with the registry loaded from its cache.
type sourceRegistry struct {
	Source   string
	Services []RegistryEntry
}

// mergeRegistries flattens per-source registries into a single list, deduping by
// service name. Precedence on a name collision: the first source in the input
// order wins (callers pass the default source first, then user-added sources in
// registration order). Each returned entry has its Source field stamped with the
// owning source's name. Pure -- table-tested, no IO.
func mergeRegistries(regs []sourceRegistry) []RegistryEntry {
	seen := make(map[string]bool)
	var out []RegistryEntry
	for _, sr := range regs {
		for _, entry := range sr.Services {
			if seen[entry.Name] {
				continue
			}
			seen[entry.Name] = true
			entry.Source = sr.Source
			out = append(out, entry)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Name < out[j].Name
	})
	return out
}
