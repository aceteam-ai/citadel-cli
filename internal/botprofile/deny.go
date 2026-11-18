package botprofile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// ErrDenied is returned for any path that is, or lies under, a bot-profile
// store. It names no path.
var ErrDenied = errors.New("path is not available to file jobs")

// ConfigDirs returns the node config directories whose "bot-profiles" child is
// protected. internal/jobs sets it at init (it can import network and
// platform; this leaf cannot). Tests replace it. A nil value makes DenyPath
// fail closed, so every package that relies on it must be linked with
// internal/jobs (the worker binary always is) or set a resolver in its tests.
var ConfigDirs func() []string

// IsStoreName reports whether a single path component names the store. The
// match is case-insensitive so a case-folding filesystem (APFS, NTFS) cannot
// be used to reach the store under another spelling.
func IsStoreName(name string) bool { return strings.EqualFold(name, DirName) }

// DenyPath refuses p when it is the store or beneath it, however it is
// reached. The decision is identity-based, not only lexical: it walks the
// ancestors of the symlink-resolved candidate and refuses when a component
// named like the store (case-insensitive) sits directly under a directory that
// is the same file (os.SameFile) as a protected config dir. That holds when
// the store does not exist yet, when the config dir is reached through a
// symlink, and for os.Root based handlers, which must call this with the
// absolute path they are about to open or create. A lexical comparison against
// the cleaned and resolved config dir paths runs as well, for a config dir
// that does not exist.
func DenyPath(p string) error {
	if p == "" {
		return nil
	}
	if ConfigDirs == nil {
		// Fail closed: with no resolver wired (internal/jobs not linked) nothing
		// can be proven outside the store. Callers that mean "no protected
		// dirs" set a resolver returning nil.
		return ErrDenied
	}
	cands := []string{filepath.Clean(p)}
	// A symlink leaf that cannot be resolved (dangling) could be created
	// through by an os.Root write; refuse it.
	if fi, err := os.Lstat(cands[0]); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		if _, err := filepath.EvalSymlinks(cands[0]); err != nil {
			return ErrDenied
		}
	}
	if r, ok := resolveNearest(cands[0]); ok && r != cands[0] {
		cands = append(cands, r)
	}
	for _, cfg := range ConfigDirs() {
		if cfg == "" {
			continue
		}
		cfg = filepath.Clean(cfg)
		cfgInfo, _ := os.Stat(cfg)
		bases := []string{cfg}
		if r, ok := resolveNearest(cfg); ok && r != cfg {
			bases = append(bases, r)
		}
		for _, c := range cands {
			for _, b := range bases {
				if underStore(b, c) {
					return ErrDenied
				}
			}
			if cfgInfo != nil && sameFileAncestor(cfgInfo, c) {
				return ErrDenied
			}
		}
	}
	return nil
}

// underStore: c is base/<store>[/...], lexically, case-insensitively.
func underStore(base, c string) bool {
	rel, err := filepath.Rel(base, c)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	first, _, _ := strings.Cut(rel, string(filepath.Separator))
	return IsStoreName(first)
}

// sameFileAncestor walks c's ancestors: a store-named component whose parent
// is the same file as cfg is the store.
func sameFileAncestor(cfg os.FileInfo, c string) bool {
	for cur := c; ; {
		parent := filepath.Dir(cur)
		if parent == cur {
			return false
		}
		if IsStoreName(filepath.Base(cur)) {
			if pi, err := os.Stat(parent); err == nil && os.SameFile(pi, cfg) {
				return true
			}
		}
		cur = parent
	}
}

// resolveNearest resolves symlinks on the longest existing prefix of p and
// re-appends the rest, so a path that does not exist yet (a write target) still
// resolves through symlinked parents.
func resolveNearest(p string) (string, bool) {
	cur := p
	var tail []string
	for {
		if _, err := os.Lstat(cur); err == nil {
			r, err := filepath.EvalSymlinks(cur)
			if err != nil {
				return "", false
			}
			return filepath.Join(append([]string{r}, tail...)...), true
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", false
		}
		tail = append([]string{filepath.Base(cur)}, tail...)
		cur = parent
	}
}
