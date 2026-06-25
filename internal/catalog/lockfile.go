package catalog

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"gopkg.in/yaml.v3"
)

// LockEntry records the provenance of one installed module: where it came from,
// the resolved git commit, and the image reference(s) (with best-effort digest)
// it deploys. This makes nodes reproducible and tamper-evident.
type LockEntry struct {
	Name        string      `yaml:"name"`
	Source      string      `yaml:"source"`                 // normalized source string
	Ref         string      `yaml:"ref,omitempty"`          // requested ref (tag/branch/sha/constraint/channel), if any
	ResolvedRef string      `yaml:"resolved_ref,omitempty"` // concrete tag a constraint/channel resolved to (e.g. "^1.2" -> "v1.4.0")
	Commit      string      `yaml:"commit,omitempty"`       // resolved git HEAD commit
	Images      []LockImage `yaml:"images,omitempty"`
	// Sandboxed records that a least-privilege hardening override was generated
	// for this (untrusted/Tier-2) module at install time. Absent/false means the
	// module runs without an override (trusted/curated, or pre-sandbox installs).
	Sandboxed bool `yaml:"sandboxed,omitempty"`
}

// LockImage is a single image reference plus an optional resolved digest.
type LockImage struct {
	Ref    string `yaml:"ref"`
	Digest string `yaml:"digest,omitempty"` // sha256:... if resolvable, else ""
	// Verified records that this image's signature was verified by cosign at
	// install time (against a verified-publisher trust entry). Absent/false means
	// no signature was required or none was verified.
	Verified bool `yaml:"verified,omitempty"`
}

// Lockfile is the top-level modules.lock structure.
type Lockfile struct {
	Version int         `yaml:"version"`
	Modules []LockEntry `yaml:"modules"`
}

// LockfilePath returns the path to the modules lockfile.
func LockfilePath() string {
	return filepath.Join(platform.ConfigDir(), "modules.lock")
}

// LoadLockfile reads the modules lockfile, returning an empty (version 1)
// lockfile if it does not yet exist.
func LoadLockfile() (*Lockfile, error) {
	path := LockfilePath()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Lockfile{Version: 1}, nil
		}
		return nil, fmt.Errorf("failed to read lockfile %s: %w", path, err)
	}
	var lf Lockfile
	if err := yaml.Unmarshal(data, &lf); err != nil {
		return nil, fmt.Errorf("failed to parse lockfile %s: %w", path, err)
	}
	if lf.Version == 0 {
		lf.Version = 1
	}
	return &lf, nil
}

// LookupLock returns the lock entry for a module name, or (zero, false).
func (lf *Lockfile) LookupLock(name string) (LockEntry, bool) {
	for _, e := range lf.Modules {
		if e.Name == name {
			return e, true
		}
	}
	return LockEntry{}, false
}

// UpsertLockEntry records (or replaces) a module's provenance in the lockfile and
// writes it back. It is a read-modify-write upsert keyed by name; other entries
// are preserved. Best-effort: a write failure is returned but callers should not
// fail the install over it.
func UpsertLockEntry(entry LockEntry) error {
	lf, err := LoadLockfile()
	if err != nil {
		return err
	}
	replaced := false
	for i := range lf.Modules {
		if lf.Modules[i].Name == entry.Name {
			lf.Modules[i] = entry
			replaced = true
			break
		}
	}
	if !replaced {
		lf.Modules = append(lf.Modules, entry)
	}

	path := LockfilePath()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("failed to create lockfile directory: %w", err)
	}
	data, err := yaml.Marshal(lf)
	if err != nil {
		return fmt.Errorf("failed to marshal lockfile: %w", err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("failed to write lockfile %s: %w", path, err)
	}
	return nil
}

// BuildLockImages turns image references into LockImage entries, attempting a
// best-effort digest lookup via `docker`. Any digest failure (no docker, no
// auth, image not pulled, timeout) just omits the digest -- never an error.
func BuildLockImages(imageRefs []string) []LockImage {
	out := make([]LockImage, 0, len(imageRefs))
	for _, ref := range imageRefs {
		out = append(out, LockImage{Ref: ref, Digest: resolveImageDigest(ref)})
	}
	return out
}

// resolveImageDigest tries to resolve a sha256 digest for an image reference via
// docker. It is best-effort and time-bounded: on any failure it returns "".
func resolveImageDigest(ref string) string {
	if _, err := exec.LookPath("docker"); err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// Prefer a local inspect (works once the image is pulled, no registry auth).
	local := exec.CommandContext(ctx, "docker", "image", "inspect", ref,
		"--format", "{{index .RepoDigests 0}}")
	if out, err := local.Output(); err == nil {
		if d := digestFromRepoDigest(strings.TrimSpace(string(out))); d != "" {
			return d
		}
	}

	// Fall back to a registry manifest inspect (needs the image to be reachable;
	// may need auth for private registries -- omitted on failure).
	remote := exec.CommandContext(ctx, "docker", "manifest", "inspect", ref,
		"--format", "{{.Descriptor.Digest}}")
	if out, err := remote.Output(); err == nil {
		d := strings.TrimSpace(string(out))
		if strings.HasPrefix(d, "sha256:") {
			return d
		}
	}
	return ""
}

// digestFromRepoDigest extracts the "sha256:..." part from a "repo@sha256:..."
// RepoDigests entry, or "" if absent.
func digestFromRepoDigest(repoDigest string) string {
	if i := strings.Index(repoDigest, "@"); i >= 0 {
		d := repoDigest[i+1:]
		if strings.HasPrefix(d, "sha256:") {
			return d
		}
	}
	return ""
}

// ContainerNameConflict reports whether a container with the given name already
// exists on this host (running or stopped). Best-effort: returns false if docker
// is unavailable or the query fails, so it never blocks an install spuriously.
// An empty name always returns false.
func ContainerNameConflict(name string) bool {
	if name == "" {
		return false
	}
	if _, err := exec.LookPath("docker"); err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "ps", "-a", "--format", "{{.Names}}").Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == name {
			return true
		}
	}
	return false
}
