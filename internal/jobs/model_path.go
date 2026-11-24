// internal/jobs/model_path.go
package jobs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// ErrUnsafeModelPath is returned when a model job's payload names a path
// component that is not a single plain name, or when the resulting path
// does not stay inside the model cache. The message is fixed and never
// echoes the payload value.
var ErrUnsafeModelPath = errors.New("refusing model job: path fields must each be a single plain name that stays inside the model cache")

// validModelPathComponent reports whether s is safe to use as ONE path
// component under a cache root. The rules are the same on every OS, so a
// Windows-style value (backslash, drive letter, UNC prefix, reserved device
// name) is refused on Linux too.
func validModelPathComponent(s string) bool {
	if s == "" || len(s) > 255 {
		return false
	}
	// ".", "..", "..." and any other all-dot name.
	if strings.Trim(s, ".") == "" {
		return false
	}
	// Windows silently strips trailing dots and spaces, which turns
	// "name." or "name " into an alias for "name".
	if strings.TrimSpace(s) != s || strings.HasSuffix(s, ".") {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
		switch r {
		case '/', '\\', ':', '<', '>', '"', '|', '?', '*':
			return false
		}
	}
	if isWindowsReservedName(s) {
		return false
	}
	return filepath.IsLocal(s) && !filepath.IsAbs(s) && filepath.VolumeName(s) == ""
}

// isWindowsReservedName reports whether s is a Windows device name (CON,
// NUL, COM1, ...), with or without an extension, compared case-insensitively.
func isWindowsReservedName(s string) bool {
	base := s
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	base = strings.ToUpper(strings.TrimRight(base, " "))
	switch base {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return true
	}
	if len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) {
		return base[3] >= '0' && base[3] <= '9'
	}
	return false
}

// validHFRepoID reports whether id is a HuggingFace repo id of the form
// "name" or "org/name" whose parts are plain names. A leading '-' or '.' is
// refused so the value can never be read as a CLI option or a hidden path.
func validHFRepoID(id string) bool {
	parts := strings.Split(id, "/")
	if len(parts) > 2 {
		return false
	}
	for _, p := range parts {
		if !validModelPathComponent(p) || p[0] == '-' || p[0] == '.' {
			return false
		}
	}
	return true
}

// strictlyWithin reports whether target is inside dir and is not dir itself.
func strictlyWithin(dir, target string) bool {
	rel, err := filepath.Rel(dir, target)
	if err != nil || rel == "." {
		return false
	}
	return withinDir(dir, target)
}

// resolveModelCacheDest validates subdir and name, creates root/subdir, and
// returns the directory and file path a download may write to. It proves the
// destination is strictly inside root lexically, and again with symlinks on
// the existing prefix resolved, both before and after creating the
// directory. A symlinked root is allowed (an operator may place the whole
// cache on another disk); a subdirectory or leaf that resolves outside it is
// not. The leaf must be absent or a regular file; exists reports which.
func resolveModelCacheDest(root, subdir, name string) (destDir, destPath string, exists bool, err error) {
	if !validModelPathComponent(subdir) || !validModelPathComponent(name) {
		return "", "", false, ErrUnsafeModelPath
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", "", false, ErrUnsafeModelPath
	}
	destDir = filepath.Join(absRoot, subdir)
	destPath = filepath.Join(destDir, name)
	if !strictlyWithin(absRoot, destDir) || filepath.Dir(destPath) != destDir {
		return "", "", false, ErrUnsafeModelPath
	}

	resolvedRoot, err := resolveNearestAncestor(absRoot)
	if err != nil {
		return "", "", false, ErrUnsafeModelPath
	}
	resolvedDir, err := resolveNearestAncestor(destDir)
	if err != nil || !strictlyWithin(resolvedRoot, resolvedDir) {
		return "", "", false, ErrUnsafeModelPath
	}

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", "", false, errors.New("failed to create model cache directory")
	}

	resolvedRoot, err = filepath.EvalSymlinks(absRoot)
	if err != nil {
		return "", "", false, ErrUnsafeModelPath
	}
	resolvedDir, err = filepath.EvalSymlinks(destDir)
	if err != nil || !strictlyWithin(resolvedRoot, resolvedDir) {
		return "", "", false, ErrUnsafeModelPath
	}

	fi, err := os.Lstat(destPath)
	switch {
	case err == nil && fi.Mode().IsRegular():
		return destDir, destPath, true, nil
	case err == nil:
		// A symlink, directory or device at the leaf would redirect the write.
		return "", "", false, ErrUnsafeModelPath
	case errors.Is(err, os.ErrNotExist):
		return destDir, destPath, false, nil
	default:
		return "", "", false, ErrUnsafeModelPath
	}
}

// ErrUnsafeModelArgument is returned when a model job's payload value would
// reach a model tool's command line in a form it could read as an option
// (a leading '-') or contains control characters. The message is fixed and
// never echoes the payload value.
var ErrUnsafeModelArgument = errors.New("refusing model job: model names and patterns must not start with '-' or contain control characters")

// validModelArgument reports whether s may be passed as a positional or
// option value to a model tool (ollama, the HuggingFace CLI).
func validModelArgument(s string) bool {
	if s == "" || s[0] == '-' {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// validModelCachePullPatterns reports whether every allow/ignore pattern in a
// MODEL_CACHE_PULL payload is a safe CLI value. A bad pattern refuses the
// whole job: dropping it would silently widen or unfilter the pull.
func validModelCachePullPatterns(payload map[string]string) bool {
	allow, ignore := parseModelCachePullPatterns(payload)
	for _, p := range append(allow, ignore...) {
		if !validModelArgument(p) {
			return false
		}
	}
	return true
}
