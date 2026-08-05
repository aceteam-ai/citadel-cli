package jobs

import (
	"os"
	"path/filepath"
	"testing"
)

// symlinkedWorkspace returns (linkPath, realPath) where linkPath is a symlink to
// a real directory — the shape macOS gives every test for free (/var -> /private/var,
// /tmp -> /private/tmp) and the shape any node has when an authorized root is
// reached through a symlink.
func symlinkedWorkspace(t *testing.T) (string, string) {
	t.Helper()
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(real, 0755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	return link, real
}

// The regression: ValidatePath compared the symlink-RESOLVED workspace against
// the UNRESOLVED target, so a workspace reached through a symlink rejected its
// own contents with "resolves outside workspace". On macOS that broke every
// test using t.TempDir() and, worse, blocked `go test ./...` — meaning no
// release could be cut from a Mac at all.
func TestValidatePathAcceptsContentsOfSymlinkedWorkspace(t *testing.T) {
	link, _ := symlinkedWorkspace(t)
	if err := os.WriteFile(filepath.Join(link, "notes.md"), []byte("hi"), 0644); err != nil {
		t.Fatal(err)
	}

	for _, req := range []string{
		"notes.md",                            // relative
		filepath.Join(link, "notes.md"),       // absolute, via the symlink
		filepath.Join(link, "does-not-exist"), // non-existent leaf (write target)
		link,                                  // the workspace itself
	} {
		if _, err := ValidatePath(link, req); err != nil {
			t.Errorf("ValidatePath(%q) = %v, want accepted", req, err)
		}
	}
}

// The same path spelled in resolved form must also be accepted — a caller that
// canonicalized the path first is not an attacker.
func TestValidatePathAcceptsResolvedSpelling(t *testing.T) {
	link, real := symlinkedWorkspace(t)
	if err := os.WriteFile(filepath.Join(real, "notes.md"), []byte("hi"), 0644); err != nil {
		t.Fatal(err)
	}

	if _, err := ValidatePath(link, filepath.Join(real, "notes.md")); err != nil {
		t.Errorf("resolved spelling rejected: %v", err)
	}
}

// SECURITY: the boundary must still hold. These are the cases the relaxed
// lexical check must NOT let through.
func TestValidatePathStillRejectsEscapes(t *testing.T) {
	link, _ := symlinkedWorkspace(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("s"), 0644); err != nil {
		t.Fatal(err)
	}

	// A symlink INSIDE the workspace pointing out of it — the escape the
	// resolved-space check exists for.
	escape := filepath.Join(link, "escape")
	if err := os.Symlink(outside, escape); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	cases := []struct {
		name string
		req  string
	}{
		{"parent traversal", filepath.Join(link, "..", "..", "etc", "passwd")},
		{"absolute outside", filepath.Join(outside, "secret")},
		{"symlink escape", filepath.Join(escape, "secret")},
		{"relative traversal", filepath.Join("..", "..", "etc", "passwd")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := ValidatePath(link, tc.req); err == nil {
				t.Errorf("ValidatePath(%q) = %q with no error; escape must be rejected", tc.req, got)
			}
		})
	}
}

// The removed lexical check nominally guarded ".." inside a NOT-YET-EXISTING
// tail (a write target). Prove the remaining resolved-space check catches it:
// resolveNearestAncestor rebuilds the full path including that tail, and
// filepath.Rel Cleans both operands, so the traversal collapses and is rejected.
func TestValidatePathRejectsDotDotInNonExistentTail(t *testing.T) {
	link, _ := symlinkedWorkspace(t)

	// CRITICAL: build these by STRING CONCATENATION, not filepath.Join. Join
	// collapses ".." before the value ever reaches ValidatePath, so a Join-built
	// case would prove nothing about how the validator handles a live traversal.
	// These keep the ".." intact all the way in.
	//
	// Neither "newdir" nor anything under it exists, so resolution falls back to
	// resolveNearestAncestor — the exact branch the removed lexical check covered.
	for _, req := range []string{
		link + "/newdir/../../escaped.txt",
		link + "/a/b/../../../escaped.txt",
		link + "/../escaped.txt",
		link + "/newdir/../../../../../../../../etc/passwd",
	} {
		if got, err := ValidatePath(link, req); err == nil {
			t.Errorf("ValidatePath(%q) = %q with no error; a .. traversal through a non-existent tail must be rejected", req, got)
		}
	}

	// ...while a plain non-existent write target inside the workspace is fine.
	if _, err := ValidatePath(link, filepath.Join(link, "newdir", "file.txt")); err != nil {
		t.Errorf("legitimate non-existent write target rejected: %v", err)
	}
}

// A sibling whose name merely PREFIXES the workspace must not pass — the
// /workspace vs /workspaceEVIL case the Rel-based check exists to prevent.
func TestValidatePathRejectsPrefixSibling(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "workspace")
	evil := filepath.Join(base, "workspaceEVIL")
	for _, d := range []string{ws, evil} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(evil, "secret"), []byte("s"), 0644); err != nil {
		t.Fatal(err)
	}

	if got, err := ValidatePath(ws, filepath.Join(evil, "secret")); err == nil {
		t.Errorf("ValidatePath = %q with no error; a name-prefix sibling is outside the workspace", got)
	}
}

// ValidateWithinRoots layers on ValidatePath, so a symlinked authorized root
// must work too — this is what the local semantic-search surface actually calls.
func TestValidateWithinRootsAcceptsSymlinkedRoot(t *testing.T) {
	link, _ := symlinkedWorkspace(t)
	if err := os.WriteFile(filepath.Join(link, "doc.md"), []byte("hi"), 0644); err != nil {
		t.Fatal(err)
	}

	if _, err := ValidateWithinRoots([]string{link}, filepath.Join(link, "doc.md")); err != nil {
		t.Errorf("ValidateWithinRoots with a symlinked root: %v", err)
	}
	// And still fails closed outside every root.
	if _, err := ValidateWithinRoots([]string{link}, filepath.Join(t.TempDir(), "elsewhere")); err == nil {
		t.Error("a path outside every authorized root was accepted")
	}
}
