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

	// The real boundary: the fully symlink-resolved target must sit inside the
	// symlink-resolved workspace. Both sides are in RESOLVED space, so this is the
	// check that actually catches a symlink escape.
	if !withinDir(resolvedWorkspace, resolved) {
		return "", fmt.Errorf("path %q resolves outside workspace", requestedPath)
	}

	// There used to be a SECOND, lexical check here comparing cleanTarget against
	// resolvedWorkspace. It was removed because it was both WRONG and redundant:
	//
	//   Wrong: cleanTarget lives in UNRESOLVED space (built from the caller's
	//   workspace argument, or given absolute by the caller) while
	//   resolvedWorkspace has been through EvalSymlinks. Comparing the two
	//   rejected a workspace's own contents whenever the workspace sat behind a
	//   symlink. macOS puts /tmp and /var behind /private, so every temp-dir
	//   workspace failed with "resolves outside workspace" -- which broke
	//   `go test ./...` and therefore blocked cutting any release from a Mac. It
	//   equally breaks a node whose authorized root is reached via a symlink.
	//
	//   Redundant: its stated job was catching ".." in a not-yet-existing leaf,
	//   but resolveNearestAncestor rebuilds the full path INCLUDING that tail, and
	//   filepath.Rel (inside withinDir) Cleans both operands -- so "ws/../../etc"
	//   collapses to "/etc" and the check above already rejects it. Covered by
	//   TestValidatePathRejectsDotDotInNonExistentTail.
	//
	// The returned value stays in the caller's spelling: cleanTarget and resolved
	// denote the same file, and callers pass this straight back to os.* which
	// follows symlinks anyway.
	return filepath.Clean(target), nil
}

// withinDir reports whether target is dir itself or lexically beneath it. It
// uses filepath.Rel rather than a string prefix so /workspace does not match
// /workspaceEVIL, and treats an un-relatable pair (different volumes on Windows)
// as outside -- failing closed.
func withinDir(dir, target string) bool {
	rel, err := filepath.Rel(dir, target)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// ValidateWithinRoots generalizes the single-root ValidatePath boundary to an
// allowlist: requestedPath is valid iff it resolves under ANY authorized root.
// It returns the cleaned absolute path on success. This is the security core of
// the local semantic-search surface (citadel-cli#617-619): a path outside every
// authorized root is REJECTED, and symlink escapes are caught because each
// candidate is run through ValidatePath (which EvalSymlinks the target before
// the boundary check).
//
// An empty allowlist is a hard error (nothing is authorized), so every caller
// fails closed on a fresh node that has authorized no roots.
func ValidateWithinRoots(roots []string, requestedPath string) (string, error) {
	if len(roots) == 0 {
		return "", fmt.Errorf("no authorized roots configured (authorize one with 'citadel search roots add <path>')")
	}
	if requestedPath == "" {
		return "", fmt.Errorf("path is empty")
	}
	for _, root := range roots {
		if root == "" {
			continue
		}
		// Reuse the airtight single-root symlink boundary check per root. The
		// first root the path validates under wins.
		if cleaned, err := ValidatePath(root, requestedPath); err == nil {
			return cleaned, nil
		}
	}
	return "", fmt.Errorf("path %q resolves outside all authorized roots", requestedPath)
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

// PathNotAuthorizedError is returned by ValidateReadPathWithRoots when a
// requested read path resolves under NEITHER the workspace NOR any authorized
// root. It is a typed error so a handler/caller can branch on it (errors.As);
// its message names the remediation — binding an authorized root — WITHOUT
// disclosing the node's workspace or the bound roots to a remote caller (this
// message crosses the wire in a dispatched job's error output, so it echoes only
// the caller's own requested path, never node-internal layout).
type PathNotAuthorizedError struct {
	// Path is the requested path, echoed back so the operator knows what was
	// refused. It is the caller's own input, not node-internal layout.
	Path string
}

func (e *PathNotAuthorizedError) Error() string {
	return fmt.Sprintf("path %q is outside the workspace and all authorized roots; "+
		"authorize a directory containing it on the node with "+
		"'citadel search roots add <dir>'", e.Path)
}

// resolveAuthorizedRoots invokes an authorized-roots resolver, returning nil
// when the resolver is nil. Read handlers hold the resolver as a func (live, not
// a boot snapshot) and call this once per dispatch to get the current list.
func resolveAuthorizedRoots(fn func() []string) []string {
	if fn == nil {
		return nil
	}
	return fn()
}

// ValidateReadPathWithRoots is the authorized-roots read sandbox (aceteam#10876
// C6, citadel-cli#1273). A path is permitted iff it resolves (after symlink
// resolution) under the workspace OR under one of the authorized roots — the
// operator-bound directories a dispatched read job may read even when they live
// outside the workspace sandbox.
//
// Three branches, all preserving back-compat:
//   - allowOutside == true: the --allow-read-outside-workspace escape hatch is
//     unchanged (ValidateReadPath's relaxed, unconfined cleaning). Authorized
//     roots are an ADDITIVE permission, never a tightening, so they are not
//     consulted on this path.
//   - roots is empty: reduces to ValidateReadPath(workspace, path, false) EXACTLY
//     — today's workspace-only sandbox, byte-for-byte (same resolution, same
//     error strings). This is the back-compat guarantee.
//   - roots is non-empty: try the workspace first (this also anchors a RELATIVE
//     path), then each authorized root for an ABSOLUTE path. The first boundary
//     the symlink-resolved path validates under wins.
//
// A relative path is only ever resolved against the workspace, never joined to a
// root: joining a relative path to whichever root happens to contain it would be
// a surprising new resolution rule, and a relative traversal escaping the
// workspace (e.g. "../../etc") is refused rather than silently retried against a
// root. A path under neither boundary is refused with a *PathNotAuthorizedError
// naming the remediation.
//
// Each per-root check reuses ValidatePath, so the symlink-escape boundary
// (resolveNearestAncestor + withinDir, compared in resolved space) is identical
// to the single-workspace sandbox: a `..` traversal out of a root, or a symlink
// inside a root pointing outside it, is refused; a symlink whose target lands
// back inside an authorized boundary is allowed.
func ValidateReadPathWithRoots(workspace string, roots []string, requestedPath string, allowOutside bool) (string, error) {
	if allowOutside {
		return ValidateReadPath(workspace, requestedPath, true)
	}
	if len(roots) == 0 {
		// Byte-for-byte today's behavior: the workspace-only sandbox with its
		// existing resolution and error strings.
		return ValidateReadPath(workspace, requestedPath, false)
	}
	if requestedPath == "" {
		return "", fmt.Errorf("path is empty")
	}
	// Workspace first — this is also what anchors a relative requestedPath.
	if workspace != "" {
		if cleaned, err := ValidatePath(workspace, requestedPath); err == nil {
			return cleaned, nil
		}
	}
	// Authorized roots only ever match an ABSOLUTE path (see the doc comment).
	if filepath.IsAbs(requestedPath) {
		for _, root := range roots {
			if root == "" {
				continue
			}
			if cleaned, err := ValidatePath(root, requestedPath); err == nil {
				return cleaned, nil
			}
		}
	}
	return "", &PathNotAuthorizedError{Path: requestedPath}
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
