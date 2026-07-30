package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"
)

// Roots is the authorized-roots allowlist for local semantic search
// (citadel-cli#617-619 desktop client). It is the security boundary for the
// `citadel search` surface: a node indexes and searches ONLY files that live
// under one of these authorized root directories, and nothing else. It is
// persisted as roots.yaml in platform.ConfigDir(), mirroring the energy.yaml
// pattern (LoadEnergy/SaveEnergy) exactly.
//
// The default is EMPTY on purpose: a fresh node authorizes nothing until the
// operator explicitly adds a root with `citadel search roots add <path>`. This
// is opt-in indexing — the node never walks the filesystem until told which
// directory it may see.
type Roots struct {
	// Roots is the list of absolute, cleaned root directories the node is
	// authorized to index and search. A path is permitted iff it resolves under
	// ANY entry here (see jobs.ValidateWithinRoots). Empty means "index nothing".
	Roots []string `yaml:"roots" json:"roots"`
}

const rootsFile = "roots.yaml"

// DefaultRoots returns an empty allowlist: the opt-in default. Nothing is
// indexable until an operator authorizes a root.
func DefaultRoots() *Roots {
	return &Roots{Roots: nil}
}

// LoadRoots reads the authorized-roots allowlist from the config directory. A
// missing file returns the empty default (indexes nothing). Partial files
// preserve defaults for absent keys, mirroring LoadEnergy.
func LoadRoots(configDir string) *Roots {
	r := DefaultRoots()

	data, err := os.ReadFile(filepath.Join(configDir, rootsFile))
	if err != nil {
		return r
	}
	_ = yaml.Unmarshal(data, r)
	return r
}

// SaveRoots writes the authorized-roots allowlist to the config directory,
// mirroring SaveEnergy.
func SaveRoots(configDir string, r *Roots) error {
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	data, err := yaml.Marshal(r)
	if err != nil {
		return fmt.Errorf("marshal roots: %w", err)
	}
	return os.WriteFile(filepath.Join(configDir, rootsFile), data, 0644)
}

// NormalizeRoot canonicalizes a user-supplied root path to the absolute,
// symlink-resolved, cleaned form stored in the allowlist. When the directory
// does not exist yet it falls back to an absolute+clean form (no symlink
// resolution). This keeps the persisted allowlist comparable and stable.
func NormalizeRoot(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("root path is empty")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", path, err)
	}
	abs = filepath.Clean(abs)
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return filepath.Clean(resolved), nil
	}
	return abs, nil
}

// Add authorizes a root, normalizing it first. It reports whether the root was
// newly added (false when it was already present). The list is kept sorted and
// de-duplicated.
func (r *Roots) Add(path string) (bool, error) {
	norm, err := NormalizeRoot(path)
	if err != nil {
		return false, err
	}
	for _, existing := range r.Roots {
		if existing == norm {
			return false, nil
		}
	}
	r.Roots = append(r.Roots, norm)
	sort.Strings(r.Roots)
	return true, nil
}

// Remove de-authorizes a root, normalizing the argument first. It reports
// whether an entry was actually removed.
func (r *Roots) Remove(path string) (bool, error) {
	norm, err := NormalizeRoot(path)
	if err != nil {
		return false, err
	}
	out := r.Roots[:0]
	removed := false
	for _, existing := range r.Roots {
		if existing == norm {
			removed = true
			continue
		}
		out = append(out, existing)
	}
	r.Roots = out
	return removed, nil
}
