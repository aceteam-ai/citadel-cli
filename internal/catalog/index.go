// internal/catalog/index.go
//
// Curated central index for module discovery (#347). The index is a YAML file
// mapping a module name to its source repo, a description, and tags. It lets
// `citadel module search <query>` surface community/curated modules that live
// outside the embedded service catalog.
//
// Source: by default the index lives as `index.yaml` at the root of the existing
// citadel-services catalog repo, reusing the catalog clone/cache machinery
// (GetCatalogPath / Update). The location is configurable via the
// CITADEL_MODULE_INDEX env var (a path to a local YAML file).
//
// Fail-soft by design: if the catalog has not been cloned, or index.yaml does
// not exist yet, the index plumbing returns a friendly empty result (never a
// hard error) so it no-ops gracefully until an index is published.
package catalog

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// ModuleIndexEnv is the env var that overrides the index file location (a path
// to a local YAML file). When unset, the index is read from index.yaml at the
// catalog cache root.
const ModuleIndexEnv = "CITADEL_MODULE_INDEX"

// indexFileName is the curated index file name within the catalog repo root.
const indexFileName = "index.yaml"

// ModuleIndex is the parsed curated index.
type ModuleIndex struct {
	Version int                `yaml:"version"`
	Modules []ModuleIndexEntry `yaml:"modules"`
}

// ModuleIndexEntry is one curated module: a display name, the source repo to
// install from (owner/repo or a git URL), a description, and free-form tags.
type ModuleIndexEntry struct {
	Name        string   `yaml:"name"`
	Source      string   `yaml:"source"`
	Description string   `yaml:"description"`
	Tags        []string `yaml:"tags,omitempty"`
}

// ModuleIndexPath returns the resolved path to the index file: the env override
// if set, else index.yaml at the catalog cache root.
func ModuleIndexPath() string {
	if p := strings.TrimSpace(os.Getenv(ModuleIndexEnv)); p != "" {
		return p
	}
	return filepath.Join(GetCatalogPath(), indexFileName)
}

// LoadModuleIndex reads and parses the curated index. It fails soft: a missing
// index file (catalog not cloned, or no index published yet) yields an empty
// index and a nil error. Only a present-but-malformed index returns an error.
func LoadModuleIndex() (*ModuleIndex, error) {
	path := ModuleIndexPath()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &ModuleIndex{Version: 1}, nil
		}
		return nil, fmt.Errorf("failed to read module index %s: %w", path, err)
	}
	idx, err := ParseModuleIndex(data)
	if err != nil {
		return nil, fmt.Errorf("failed to parse module index %s: %w", path, err)
	}
	return idx, nil
}

// ParseModuleIndex parses index YAML bytes into a ModuleIndex. Pure --
// table-tested with literals, no network/IO.
func ParseModuleIndex(data []byte) (*ModuleIndex, error) {
	var idx ModuleIndex
	if err := yaml.Unmarshal(data, &idx); err != nil {
		return nil, err
	}
	if idx.Version == 0 {
		idx.Version = 1
	}
	return &idx, nil
}

// SearchModuleIndex returns the index entries matching query (case-insensitive)
// against name, description, and tags. An empty query returns all entries. Pure
// -- table-tested.
func SearchModuleIndex(idx *ModuleIndex, query string) []ModuleIndexEntry {
	if idx == nil {
		return nil
	}
	q := strings.ToLower(strings.TrimSpace(query))
	var out []ModuleIndexEntry
	for _, e := range idx.Modules {
		if q == "" || moduleIndexMatches(e, q) {
			out = append(out, e)
		}
	}
	return out
}

// moduleIndexMatches reports whether entry matches an already-lowercased query.
// Pure -- table-tested.
func moduleIndexMatches(e ModuleIndexEntry, q string) bool {
	if strings.Contains(strings.ToLower(e.Name), q) {
		return true
	}
	if strings.Contains(strings.ToLower(e.Description), q) {
		return true
	}
	if strings.Contains(strings.ToLower(e.Source), q) {
		return true
	}
	for _, t := range e.Tags {
		if strings.Contains(strings.ToLower(t), q) {
			return true
		}
	}
	return false
}
