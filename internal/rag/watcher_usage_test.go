package rag

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/jobs"
)

// TestWatcherServiceOriginIsWatcher pins that a watcher re-index is attributed
// as origin=watcher (not local_cli): NewWatcher stamps jobs.OriginWatcher onto
// the Service the watcher drives, so the emitted usage object and every record
// the sink writes carry the watcher origin.
func TestWatcherServiceOriginIsWatcher(t *testing.T) {
	t.Setenv("CITADEL_INDEX_DB", filepath.Join(t.TempDir(), "index.db"))
	w, err := NewWatcher([]string{t.TempDir()}, t.TempDir(), "")
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	defer w.Close()
	if w.svc.origin != jobs.OriginWatcher {
		t.Fatalf("watcher service origin = %q, want %q", w.svc.origin, jobs.OriginWatcher)
	}
}

// TestWatcherReindexDirtyRecordsPerRoot proves the acceptance: a change under a
// bound root re-indexes THAT root and emits exactly one usage record for it
// (origin=watcher is the sink's responsibility, pinned in the cmd test), while a
// root with no pending change — and anything outside all roots — produces no
// record. Hermetic: temp dirs, an injected sink, and a mocked TEI; no fsnotify
// watch on a real node dir.
func TestWatcherReindexDirtyRecordsPerRoot(t *testing.T) {
	tei := fakeTEI(t)
	t.Setenv("CITADEL_TEI_URL", tei.URL)
	t.Setenv("CITADEL_INDEX_DB", filepath.Join(t.TempDir(), "index.db"))
	t.Setenv("CITADEL_EMBEDDING_MODEL", "")

	rootA := t.TempDir()
	rootB := t.TempDir()
	writeFile(t, rootA, "cats.md", "The cat sat on the mat. A kitten is a small feline.")

	w, err := NewWatcher([]string{rootA, rootB}, t.TempDir(), "")
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	defer w.Close()

	var mu sync.Mutex
	captured := map[string]IndexResult{}
	w.SetUsageSink(func(root string, res IndexResult, _ time.Time) {
		mu.Lock()
		captured[root] = res
		mu.Unlock()
	})

	// Seed the dirty set with ONLY rootA (as a change under rootA would), then
	// fire the debounced re-index directly — no timer, fully deterministic.
	w.dirtyMu.Lock()
	w.dirty[rootA] = struct{}{}
	w.dirtyMu.Unlock()
	w.reindexDirty()

	mu.Lock()
	defer mu.Unlock()
	if _, ok := captured[rootA]; !ok {
		t.Fatalf("expected a usage record for the changed root %q, got %v", rootA, captured)
	}
	if captured[rootA].FilesIndexed < 1 {
		t.Errorf("expected the changed root's file to be indexed, got %+v", captured[rootA])
	}
	if _, ok := captured[rootB]; ok {
		t.Errorf("rootB had no pending change and must not produce a record; got %v", captured)
	}
	if len(captured) != 1 {
		t.Errorf("expected exactly one re-indexed root, got %v", captured)
	}
}

// TestRootForPath pins the pure root-attribution helper: a path under a bound
// root resolves to that root (lexical containment, NOT a string prefix, so a
// sibling like rootEVIL is never matched), and a path outside all roots resolves
// to "" — the "a change outside all roots does not produce a record" guarantee,
// tested without a sleep-based negative.
func TestRootForPath(t *testing.T) {
	base := t.TempDir()
	rootA := filepath.Join(base, "A")
	rootB := filepath.Join(base, "B")
	evil := filepath.Join(base, "AEVIL") // sibling with a shared string prefix of rootA
	roots := []string{rootA, rootB}

	if got := rootForPath(roots, filepath.Join(rootA, "sub", "x.md")); got != rootA {
		t.Errorf("path under rootA resolved to %q, want %q", got, rootA)
	}
	if got := rootForPath(roots, rootB); got != rootB {
		t.Errorf("rootB itself should resolve to rootB, got %q", got)
	}
	if got := rootForPath(roots, filepath.Join(evil, "x.md")); got != "" {
		t.Errorf("sibling %q must not be attributed to any root (not a prefix match), got %q", evil, got)
	}
	if got := rootForPath(roots, filepath.Join(base, "elsewhere", "y.md")); got != "" {
		t.Errorf("path outside all roots must resolve to \"\", got %q", got)
	}
}

// TestWatcherIgnorePathStopsSelfTrigger pins that IgnorePath adds a file and its
// SQLite siblings to the self-trigger ignore set, so the watcher's own usage.db
// writes under an authorized root cannot drive an endless re-index loop.
func TestWatcherIgnorePathStopsSelfTrigger(t *testing.T) {
	t.Setenv("CITADEL_INDEX_DB", filepath.Join(t.TempDir(), "index.db"))
	w, err := NewWatcher([]string{t.TempDir()}, t.TempDir(), "")
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	defer w.Close()
	usageDB := filepath.Join(t.TempDir(), "usage.db")
	w.IgnorePath(usageDB)
	for _, p := range []string{usageDB, usageDB + "-wal", usageDB + "-shm", usageDB + "-journal"} {
		if _, ok := w.dbPaths[p]; !ok {
			t.Errorf("IgnorePath should have added %s to the ignore set", p)
		}
	}
}
