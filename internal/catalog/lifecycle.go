package catalog

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// Module lifecycle helpers (update / outdated / gc / rollback). The decision
// logic here is kept PURE where possible so it is testable under the only test
// path this repo allows (./internal/catalog/...). The cmd layer (module_update.go)
// is thin wiring around these.

// ExpectedCacheDir returns the absolute cache directory ResolveSource would use
// for src (after any constraint/channel ref has been rewritten to a concrete
// tag). gc uses this to map lockfile entries back to their on-disk cache dirs.
func ExpectedCacheDir(src Source) string {
	return filepath.Join(ModulesCacheDir(), sanitizeCacheName(src))
}

// UpdateDecision classifies the result of re-resolving an installed module.
type UpdateDecision int

const (
	// UpdateUnchanged: the re-resolved commit matches the locked commit.
	UpdateUnchanged UpdateDecision = iota
	// UpdateChanged: the re-resolved commit differs from the locked commit.
	UpdateChanged
	// UpdateUnknown: the locked or re-resolved commit is empty, so we cannot tell.
	UpdateUnknown
)

func (d UpdateDecision) String() string {
	switch d {
	case UpdateUnchanged:
		return "up-to-date"
	case UpdateChanged:
		return "outdated"
	case UpdateUnknown:
		return "unknown"
	default:
		return "invalid"
	}
}

// CompareCommits decides whether a module is outdated by comparing the commit
// recorded in the lockfile against a freshly resolved commit. Pure -- table-tested.
func CompareCommits(lockedCommit, resolvedCommit string) UpdateDecision {
	if lockedCommit == "" || resolvedCommit == "" {
		return UpdateUnknown
	}
	if lockedCommit == resolvedCommit {
		return UpdateUnchanged
	}
	return UpdateChanged
}

// GCCandidates returns the subset of present cache directory names that are NOT
// in the referenced set -- i.e. cache dirs safe to remove because no lockfile
// entry (and thus no installed module) points at them. Inputs are bare directory
// names (not full paths). The result is sorted for stable output. Pure.
func GCCandidates(present []string, referenced map[string]bool) []string {
	var out []string
	for _, name := range present {
		if name == "" {
			continue
		}
		if !referenced[name] {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// ReferencedCacheDirs builds the set of cache-directory names (bare names, not
// paths) that the lockfile entries map to. An entry whose source fails to parse
// is skipped (it cannot be mapped, so its dir -- if any -- is left for manual
// cleanup rather than risking deleting an unrelated dir). The entry's recorded
// Ref/ResolvedRef is used to reconstruct the same dir ResolveSource wrote.
func ReferencedCacheDirs(entries []LockEntry) map[string]bool {
	ref := make(map[string]bool)
	for _, e := range entries {
		src, err := ParseSource(e.Source)
		if err != nil || src.Kind == KindCatalog {
			continue
		}
		// A constraint/channel ref is rewritten to its resolved tag before the
		// cache dir is keyed, so reconstruct with the resolved tag when present.
		if e.ResolvedRef != "" {
			src.Ref = e.ResolvedRef
		}
		ref[sanitizeCacheName(src)] = true
	}
	return ref
}

// ListCacheDirs returns the bare names of the immediate subdirectories of the
// modules cache root. Missing root is not an error (returns empty).
func ListCacheDirs() ([]string, error) {
	root := ModulesCacheDir()
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read modules cache %s: %w", root, err)
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	return dirs, nil
}

// PruneCache removes the named cache subdirectories from the modules cache root
// and returns the list actually removed. Names are validated to be plain single
// path segments (no separators, no "..") so a malformed lockfile cannot cause a
// deletion outside the cache root.
func PruneCache(names []string) ([]string, error) {
	root := ModulesCacheDir()
	var removed []string
	for _, name := range names {
		if !isSafeCacheName(name) {
			continue
		}
		path := filepath.Join(root, name)
		if err := os.RemoveAll(path); err != nil {
			return removed, fmt.Errorf("failed to remove cache dir %s: %w", path, err)
		}
		removed = append(removed, name)
	}
	return removed, nil
}

// SourceFromLock reconstructs the Source for a lock entry so it can be
// re-resolved. The entry's recorded Ref (the originally-requested ref, which may
// be a constraint/channel) is honored so an `update` re-resolves the constraint
// against the latest tags -- not pinned to the old resolved tag.
func SourceFromLock(e LockEntry) (Source, error) {
	src, err := ParseSource(e.Source)
	if err != nil {
		return Source{}, err
	}
	if e.Ref != "" {
		src.Ref = e.Ref
	}
	return src, nil
}

// SourceAtCommit reconstructs a Source pinned to an exact commit, for rollback to
// a previously-locked version. The commit must look like a git SHA; otherwise an
// error is returned (we will not roll back to an unverifiable ref).
func SourceAtCommit(e LockEntry, commit string) (Source, error) {
	src, err := ParseSource(e.Source)
	if err != nil {
		return Source{}, err
	}
	if !looksLikeSHA(commit) {
		return Source{}, fmt.Errorf("cannot roll back %q: locked commit %q is not a usable git SHA", e.Name, commit)
	}
	src.Ref = commit
	return src, nil
}

// isSafeCacheName reports whether name is a single, non-traversing path segment
// safe to join under the cache root and delete. Pure -- table-tested.
func isSafeCacheName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	if name != filepath.Base(name) {
		return false
	}
	for _, r := range name {
		if r == '/' || r == '\\' || r == 0 {
			return false
		}
	}
	return true
}
