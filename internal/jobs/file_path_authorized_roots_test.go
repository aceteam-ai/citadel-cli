package jobs

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestValidateReadPathWithRoots_EmptyRootsMatchesTodayExactly pins the
// back-compat guarantee (aceteam#10876 C6): with no authorized roots, the new
// validator is byte-for-byte identical to ValidateReadPath(workspace, path,
// false) — same accepts, same rejects, same returned path. The table includes
// FAILURE cases, not just the happy path, because the whole point is that the
// empty-roots branch must not change any decision or error.
func TestValidateReadPathWithRoots_EmptyRootsMatchesTodayExactly(t *testing.T) {
	ws := t.TempDir()
	in := filepath.Join(ws, "sub", "file.md")
	if err := os.MkdirAll(filepath.Dir(in), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(in, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []string{
		in,                 // absolute inside workspace
		"sub/file.md",      // relative inside workspace
		"../../etc/passwd", // relative traversal escaping workspace (reject)
		"/etc/passwd",      // absolute outside workspace (reject)
		"",                 // empty (reject)
	}
	for _, p := range cases {
		wantPath, wantErr := ValidateReadPath(ws, p, false)
		gotPath, gotErr := ValidateReadPathWithRoots(ws, nil, p, false)
		if (wantErr == nil) != (gotErr == nil) {
			t.Errorf("path %q: err mismatch: today=%v withRoots=%v", p, wantErr, gotErr)
			continue
		}
		if wantErr == nil && gotPath != wantPath {
			t.Errorf("path %q: path mismatch: today=%q withRoots=%q", p, wantPath, gotPath)
		}
		if wantErr != nil && gotErr != nil && wantErr.Error() != gotErr.Error() {
			t.Errorf("path %q: error string mismatch: today=%q withRoots=%q", p, wantErr.Error(), gotErr.Error())
		}
	}
}

// TestValidateReadPathWithRoots_WorkspaceAndRootAllowed covers the two accept
// branches: a path under the workspace, and a path under a bound authorized root
// that lives OUTSIDE the workspace.
func TestValidateReadPathWithRoots_WorkspaceAndRootAllowed(t *testing.T) {
	ws := t.TempDir()
	root := t.TempDir() // a bound authorized root, disjoint from the workspace

	inWS := filepath.Join(ws, "a.md")
	if err := os.WriteFile(inWS, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	inRoot := filepath.Join(root, "docs", "b.md")
	if err := os.MkdirAll(filepath.Dir(inRoot), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inRoot, []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}

	roots := []string{root}
	if _, err := ValidateReadPathWithRoots(ws, roots, inWS, false); err != nil {
		t.Errorf("path under workspace must be allowed: %v", err)
	}
	if _, err := ValidateReadPathWithRoots(ws, roots, inRoot, false); err != nil {
		t.Errorf("path under an authorized root must be allowed: %v", err)
	}
	// A relative path still anchors to the workspace, never a root.
	if _, err := ValidateReadPathWithRoots(ws, roots, "a.md", false); err != nil {
		t.Errorf("relative path under workspace must be allowed: %v", err)
	}
}

// TestValidateReadPathWithRoots_OutsideAllRefusedWithRemediation covers the
// refusal path: a path under neither the workspace nor any root is refused with
// a *PathNotAuthorizedError whose message names the remediation.
func TestValidateReadPathWithRoots_OutsideAllRefusedWithRemediation(t *testing.T) {
	ws := t.TempDir()
	root := t.TempDir()
	outside := t.TempDir()

	req := filepath.Join(outside, "secret.md")
	if err := os.WriteFile(req, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := ValidateReadPathWithRoots(ws, []string{root}, req, false)
	if err == nil {
		t.Fatal("path outside workspace and all roots must be refused")
	}
	var perr *PathNotAuthorizedError
	if !errors.As(err, &perr) {
		t.Fatalf("want *PathNotAuthorizedError, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "citadel search roots add") {
		t.Errorf("remediation not named in error: %q", err.Error())
	}
	// The refusal echoes the requested path but must NOT leak the workspace or
	// the bound root (node-internal layout).
	if strings.Contains(err.Error(), ws) || strings.Contains(err.Error(), root) {
		t.Errorf("refusal leaks node-internal layout: %q", err.Error())
	}
}

// TestValidateReadPathWithRoots_DotDotEscapeOutOfRootRefused pins that a `..`
// traversal out of an authorized root is refused (the cleaned path resolves
// outside the root, so no root matches).
func TestValidateReadPathWithRoots_DotDotEscapeOutOfRootRefused(t *testing.T) {
	ws := t.TempDir()
	root := t.TempDir()
	// An absolute path that uses .. to climb out of the root.
	escaped := filepath.Join(root, "..", "elsewhere", "secret.md")
	if _, err := ValidateReadPathWithRoots(ws, []string{root}, escaped, false); err == nil {
		t.Fatal("a .. traversal out of an authorized root must be refused")
	}
}

// TestValidateReadPathWithRoots_SymlinkEscapeOutOfRootRefused pins the core
// security property: a symlink INSIDE an authorized root pointing OUTSIDE it
// does not grant access to the target (symlink-resolved confinement, not
// lexical).
func TestValidateReadPathWithRoots_SymlinkEscapeOutOfRootRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is unreliable without privileges on Windows")
	}
	ws := t.TempDir()
	root := t.TempDir()
	secretDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(secretDir, "secret.txt"), []byte("top secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "escape")
	if err := os.Symlink(secretDir, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	escaped := filepath.Join(link, "secret.txt") // root/escape/secret.txt -> secretDir/secret.txt
	if _, err := ValidateReadPathWithRoots(ws, []string{root}, escaped, false); err == nil {
		t.Fatal("a symlink escaping the authorized root must be refused")
	}
}

// TestValidateReadPathWithRoots_SiblingPrefixNotMatched proves the boundary is
// NOT a lexical string prefix: an authorized root /data/root must not authorize
// a sibling /data/rootEVIL. (withinDir uses filepath.Rel, which handles this;
// this pins it against a regression to HasPrefix.)
func TestValidateReadPathWithRoots_SiblingPrefixNotMatched(t *testing.T) {
	base := t.TempDir()
	ws := t.TempDir()
	root := filepath.Join(base, "root")
	evil := filepath.Join(base, "rootEVIL")
	for _, d := range []string{root, evil} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	req := filepath.Join(evil, "x.md")
	if err := os.WriteFile(req, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateReadPathWithRoots(ws, []string{root}, req, false); err == nil {
		t.Fatalf("sibling %q must not be authorized by root %q (not a lexical prefix)", evil, root)
	}
}

// TestValidateReadPathWithRoots_SymlinkIntoWorkspaceAllowed proves the rule is
// "the resolved location is authorized", not "no symlinks": a symlink inside an
// authorized root whose target lands INSIDE the workspace is allowed.
func TestValidateReadPathWithRoots_SymlinkIntoWorkspaceAllowed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is unreliable without privileges on Windows")
	}
	ws := t.TempDir()
	root := t.TempDir()
	target := filepath.Join(ws, "real.md")
	if err := os.WriteFile(target, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "into-ws.md")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	// The link lives under the root (so it validates under the root boundary),
	// and its resolved target is under the workspace — either boundary accepting
	// is enough.
	if _, err := ValidateReadPathWithRoots(ws, []string{root}, link, false); err != nil {
		t.Errorf("symlink under a root resolving into the workspace must be allowed: %v", err)
	}
}

// TestValidateReadPathWithRoots_AllowOutsideBypassesRoots pins that the
// --allow-read-outside-workspace escape hatch is unchanged: with allowOutside
// true, an arbitrary absolute path is accepted (relaxed cleaning), independent
// of the roots list — roots are additive permission, never a tightening.
func TestValidateReadPathWithRoots_AllowOutsideBypassesRoots(t *testing.T) {
	ws := t.TempDir()
	outside := t.TempDir()
	req := filepath.Join(outside, "anything.md")
	got, err := ValidateReadPathWithRoots(ws, nil, req, true)
	if err != nil {
		t.Fatalf("allowOutside must accept an outside path: %v", err)
	}
	if got != filepath.Clean(req) {
		t.Errorf("allowOutside path = %q, want %q", got, filepath.Clean(req))
	}
	// Same with a non-empty roots list that does not contain the path: still
	// accepted, because roots are not consulted on the relaxed path.
	if _, err := ValidateReadPathWithRoots(ws, []string{t.TempDir()}, req, true); err != nil {
		t.Errorf("allowOutside must not be tightened by a roots list: %v", err)
	}
}
