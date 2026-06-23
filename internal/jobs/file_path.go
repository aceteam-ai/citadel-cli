// internal/jobs/file_path.go
package jobs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ValidatePath resolves requestedPath within the workspace sandbox and returns
// the cleaned absolute path. It rejects any path that escapes the workspace
// after symlink resolution. For paths that do not yet exist (e.g. write
// targets), the nearest existing ancestor is resolved and validated instead.
func ValidatePath(workspace, requestedPath string) (string, error) {
	if workspace == "" {
		return "", fmt.Errorf("workspace directory is not configured")
	}
	if requestedPath == "" {
		return "", fmt.Errorf("path is empty")
	}

	// Resolve the workspace root itself (it may be under a symlinked tmpdir).
	resolvedWorkspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return "", fmt.Errorf("cannot resolve workspace %q: %w", workspace, err)
	}
	resolvedWorkspace = filepath.Clean(resolvedWorkspace)

	// If requestedPath is relative, join it to the workspace.
	target := requestedPath
	if !filepath.IsAbs(target) {
		target = filepath.Join(resolvedWorkspace, target)
	}
	target = filepath.Clean(target)

	// Try to resolve the full path via symlinks.
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		// Path doesn't exist yet — walk up to find the nearest existing ancestor.
		resolved, err = resolveNearestAncestor(target)
		if err != nil {
			return "", fmt.Errorf("cannot resolve path %q: %w", requestedPath, err)
		}
		// The ancestor resolved successfully. Now reconstruct the full target
		// using the resolved ancestor + the relative remainder.
		// This ensures that the non-existing suffix is still lexically clean.
	}

	// Boundary-safe prefix check: use filepath.Rel to avoid /workspace matching
	// /workspaceEVIL. Rel returns a path starting with ".." if the target is
	// outside the workspace.
	rel, err := filepath.Rel(resolvedWorkspace, resolved)
	if err != nil {
		return "", fmt.Errorf("path %q is outside workspace: %w", requestedPath, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q resolves outside workspace", requestedPath)
	}

	// Also verify the full (possibly non-existent) target lexically.
	cleanTarget := filepath.Clean(target)
	relTarget, err := filepath.Rel(resolvedWorkspace, cleanTarget)
	if err != nil {
		return "", fmt.Errorf("path %q is outside workspace: %w", requestedPath, err)
	}
	if relTarget == ".." || strings.HasPrefix(relTarget, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q resolves outside workspace", requestedPath)
	}

	return cleanTarget, nil
}

// resolveNearestAncestor walks up from target until it finds a directory that
// exists, resolves it via EvalSymlinks, then re-appends the tail. Returns the
// fully resolved path even though the leaf may not exist.
func resolveNearestAncestor(target string) (string, error) {
	current := target
	var tail []string

	for {
		info, err := os.Lstat(current)
		if err == nil {
			// Found an existing path — resolve symlinks on it.
			if info.Mode()&os.ModeSymlink != 0 || info.IsDir() {
				resolved, err := filepath.EvalSymlinks(current)
				if err != nil {
					return "", err
				}
				// Re-attach the tail components.
				parts := append([]string{resolved}, tail...)
				return filepath.Join(parts...), nil
			}
			// It exists but is a regular file and we still have tail — error.
			if len(tail) > 0 {
				return "", fmt.Errorf("path component %q is not a directory", current)
			}
			resolved, err := filepath.EvalSymlinks(current)
			if err != nil {
				return "", err
			}
			return resolved, nil
		}

		parent := filepath.Dir(current)
		if parent == current {
			// Reached filesystem root without finding anything — give up.
			return "", fmt.Errorf("no existing ancestor for %q", target)
		}
		tail = append([]string{filepath.Base(current)}, tail...)
		current = parent
	}
}

// ValidateReadPath resolves requestedPath for read-only operations. When
// allowOutside is false it delegates to ValidatePath (full workspace sandbox).
// When allowOutside is true it performs basic cleaning and returns the absolute
// path without a workspace boundary check, relying on OS file permissions and
// the handler's own size caps for safety.
func ValidateReadPath(workspace, requestedPath string, allowOutside bool) (string, error) {
	if !allowOutside {
		return ValidatePath(workspace, requestedPath)
	}

	if requestedPath == "" {
		return "", fmt.Errorf("path is empty")
	}

	// If relative, anchor to workspace (when available) so relative paths
	// still work in relaxed mode.
	target := requestedPath
	if !filepath.IsAbs(target) {
		if workspace == "" {
			return "", fmt.Errorf("workspace directory is not configured and path is relative")
		}
		resolvedWorkspace, err := filepath.EvalSymlinks(workspace)
		if err != nil {
			return "", fmt.Errorf("cannot resolve workspace %q: %w", workspace, err)
		}
		target = filepath.Join(resolvedWorkspace, target)
	}
	return filepath.Clean(target), nil
}

// isBinaryContent checks the first n bytes for NUL bytes, which indicate
// binary content.
func isBinaryContent(data []byte) bool {
	for _, b := range data {
		if b == 0 {
			return true
		}
	}
	return false
}
