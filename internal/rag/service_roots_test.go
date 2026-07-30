package rag

import (
	"context"
	"path/filepath"
	"testing"
)

// newRootsTestService builds a roots-mode Service with TEI mocked and a temp db.
func newRootsTestService(t *testing.T, roots []string) *Service {
	t.Helper()
	tei := fakeTEI(t)
	t.Setenv("CITADEL_TEI_URL", tei.URL)
	t.Setenv("CITADEL_INDEX_DB", filepath.Join(t.TempDir(), "index.db"))
	t.Setenv("CITADEL_EMBEDDING_MODEL", "")
	return NewWithRoots(roots, t.TempDir(), "")
}

// TestRootsModeIndexRejectsOutsideRoots asserts indexing a path outside every
// authorized root is rejected — the core allowlist boundary.
func TestRootsModeIndexRejectsOutsideRoots(t *testing.T) {
	authorized := t.TempDir()
	outside := t.TempDir()
	writeFile(t, outside, "secret.md", "A database of secrets.")

	svc := newRootsTestService(t, []string{authorized})
	if _, err := svc.Index(context.Background(), outside, ""); err == nil {
		t.Fatal("indexing a path outside the authorized roots must be rejected")
	}
}

// TestRootsModeSearchFiltersDeauthorized proves that after a root is
// de-authorized, its already-indexed chunks no longer surface in search results
// (they remain in the DB until pruned, but the roots-mode surface filters them).
func TestRootsModeSearchFiltersDeauthorized(t *testing.T) {
	dbDir := filepath.Join(t.TempDir(), "index.db")
	rootA := t.TempDir()
	rootB := t.TempDir()
	writeFile(t, rootA, "catsA.md", "The cat sat on the mat. A kitten is a small feline.")
	writeFile(t, rootB, "catsB.md", "Another cat document about a feline kitten.")

	tei := fakeTEI(t)
	t.Setenv("CITADEL_TEI_URL", tei.URL)
	t.Setenv("CITADEL_INDEX_DB", dbDir)
	t.Setenv("CITADEL_EMBEDDING_MODEL", "")

	// Index both roots into the shared index.
	both := NewWithRoots([]string{rootA, rootB}, t.TempDir(), "")
	if _, err := both.Index(context.Background(), rootA, ""); err != nil {
		t.Fatalf("index A: %v", err)
	}
	if _, err := both.Index(context.Background(), rootB, ""); err != nil {
		t.Fatalf("index B: %v", err)
	}

	// With both authorized, a cat query can return hits from either root.
	res, err := both.Query(context.Background(), "kitten", 10)
	if err != nil {
		t.Fatalf("query both: %v", err)
	}
	if len(res.Hits) == 0 {
		t.Fatal("expected hits with both roots authorized")
	}

	// Now de-authorize rootB: the same index still contains catsB's chunks, but a
	// roots-mode Service over only rootA must NOT return them.
	onlyA := NewWithRoots([]string{rootA}, t.TempDir(), "")
	res, err = onlyA.Query(context.Background(), "kitten", 10)
	if err != nil {
		t.Fatalf("query onlyA: %v", err)
	}
	for _, h := range res.Hits {
		if filepath.Base(h.Path) == "catsB.md" {
			t.Errorf("de-authorized root's chunk leaked into search: %s", h.Path)
		}
	}
	// And catsA is still reachable.
	foundA := false
	for _, h := range res.Hits {
		if filepath.Base(h.Path) == "catsA.md" {
			foundA = true
		}
	}
	if !foundA {
		t.Errorf("authorized root's document should still be searchable; hits=%+v", res.Hits)
	}
}
