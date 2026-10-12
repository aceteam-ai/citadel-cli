package jobs

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// TestFileRead_AuthorizedRoot_Allowed proves the handler wiring: with
// AllowOutsideWorkspace=false and an AuthorizedRoots resolver naming a disjoint
// root, a dispatched FILE_READ of a file under that root succeeds, while a file
// under neither the workspace nor the root is refused with the remediation.
func TestFileRead_AuthorizedRoot_Allowed(t *testing.T) {
	ws := setupWorkspace(t)
	root := t.TempDir()
	resolvedRoot, _ := filepath.EvalSymlinks(root)
	underRoot := writeTestFile(t, resolvedRoot, "doc.txt", "allowed content\n")

	h := NewFileReadHandler(ws)
	h.AuthorizedRoots = func() []string { return []string{resolvedRoot} }

	out, err := h.Execute(JobContext{}, makeJob(map[string]string{"path": underRoot}))
	if err != nil {
		t.Fatalf("read under authorized root should succeed: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("json: %v", err)
	}
	if !strings.Contains(result["content"].(string), "allowed content") {
		t.Errorf("content wrong: %v", result["content"])
	}
}

// TestFileRead_OutsideWorkspaceAndRoots_RefusedWithRemediation proves a path
// under neither the workspace nor any authorized root is refused, and the error
// (which crosses the wire) names the remediation.
func TestFileRead_OutsideWorkspaceAndRoots_RefusedWithRemediation(t *testing.T) {
	ws := setupWorkspace(t)
	root := t.TempDir()
	outside := t.TempDir()
	resolvedOut, _ := filepath.EvalSymlinks(outside)
	target := writeTestFile(t, resolvedOut, "secret.txt", "nope")

	h := NewFileReadHandler(ws)
	h.AuthorizedRoots = func() []string { return []string{root} }

	_, err := h.Execute(JobContext{}, makeJob(map[string]string{"path": target}))
	if err == nil {
		t.Fatal("read outside workspace and all authorized roots must be refused")
	}
	var perr *PathNotAuthorizedError
	if !errors.As(err, &perr) {
		t.Fatalf("want *PathNotAuthorizedError in chain, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "citadel search roots add") {
		t.Errorf("remediation not named: %q", err.Error())
	}
}

// TestFileIndex_AuthorizedRoot_Allowed proves the DRIVING adopter (FILE_INDEX)
// honors authorized roots: indexing a root outside the workspace is permitted
// when it is authorized, and refused when it is not. TEI is never reached — an
// empty root indexes zero files and returns without embedding.
func TestFileIndex_AuthorizedRoot_Allowed(t *testing.T) {
	ws := setupWorkspace(t)
	root := t.TempDir()
	resolvedRoot, _ := filepath.EvalSymlinks(root)

	h := NewFileIndexHandler(ws, filepath.Join(t.TempDir(), "index.db"))
	h.AuthorizedRoots = func() []string { return []string{resolvedRoot} }

	// Authorized root (empty) → index succeeds without TEI.
	if _, err := h.Execute(JobContext{}, makeJob(map[string]string{"path": resolvedRoot})); err != nil {
		t.Fatalf("FILE_INDEX of an authorized root should succeed: %v", err)
	}

	// Unauthorized outside dir → refused before any walk.
	outside := t.TempDir()
	resolvedOut, _ := filepath.EvalSymlinks(outside)
	_, err := h.Execute(JobContext{}, makeJob(map[string]string{"path": resolvedOut}))
	if err == nil {
		t.Fatal("FILE_INDEX of an unauthorized out-of-workspace path must be refused")
	}
	if !strings.Contains(err.Error(), "citadel search roots add") {
		t.Errorf("FILE_INDEX refusal should name the remediation: %q", err.Error())
	}
}

// TestFileRead_NilAuthorizedRoots_BackCompat proves that with no authorized
// roots resolver (the pre-#1273 default), behavior is unchanged: an outside path
// is refused exactly as before (workspace-only).
func TestFileRead_NilAuthorizedRoots_BackCompat(t *testing.T) {
	ws := setupWorkspace(t)
	outside := t.TempDir()
	resolvedOut, _ := filepath.EvalSymlinks(outside)
	target := writeTestFile(t, resolvedOut, "x.txt", "data")

	h := NewFileReadHandler(ws) // AuthorizedRoots nil, AllowOutsideWorkspace false
	if _, err := h.Execute(JobContext{}, makeJob(map[string]string{"path": target})); err == nil {
		t.Fatal("with no roots and workspace-only, an outside path must be refused")
	}
}
