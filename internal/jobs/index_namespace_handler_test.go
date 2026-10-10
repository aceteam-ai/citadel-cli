package jobs

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/nodeindex"
)

// TestFileIndexNamespaceIsolation pins that two orgs' namespaced FILE_INDEX jobs
// land in SEPARATE confined DBs under the indexes dir — the cross-org isolation
// #1263 exists for — and that FILE_SEMANTIC_SEARCH reads the matching namespace.
func TestFileIndexNamespaceIsolation(t *testing.T) {
	tei := fakeTEI(t)
	t.Setenv("CITADEL_TEI_URL", tei.URL)
	t.Setenv("CITADEL_INDEX_DB", "")
	t.Setenv("CITADEL_INDEX_HNSW", "false")

	indexesDir := t.TempDir()
	wsA := t.TempDir()
	wsB := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsA, "a.md"), []byte("The cat sat on the mat."), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wsB, "b.md"), []byte("A database stores rows."), 0o644); err != nil {
		t.Fatal(err)
	}

	idxA := NewFileIndexHandler(wsA, "")
	idxA.IndexesDir = indexesDir
	idxA.AllowOutsideWorkspace = true
	if _, err := idxA.Execute(JobContext{}, &nexus.Job{ID: "a", Type: "FILE_INDEX", Payload: map[string]string{"path": wsA, "index": "org_1/docs"}}); err != nil {
		t.Fatalf("index org_1: %v", err)
	}
	idxB := NewFileIndexHandler(wsB, "")
	idxB.IndexesDir = indexesDir
	idxB.AllowOutsideWorkspace = true
	if _, err := idxB.Execute(JobContext{}, &nexus.Job{ID: "b", Type: "FILE_INDEX", Payload: map[string]string{"path": wsB, "index": "org_2/docs"}}); err != nil {
		t.Fatalf("index org_2: %v", err)
	}

	// Each namespace has its own DB file with only its own file.
	for _, tc := range []struct{ ns, wantFile string }{
		{"org_1/docs", "a.md"},
		{"org_2/docs", "b.md"},
	} {
		dbPath := filepath.Join(indexesDir, filepath.FromSlash(tc.ns)+".db")
		store, err := nodeindex.Open(dbPath)
		if err != nil {
			t.Fatalf("open %s db: %v", tc.ns, err)
		}
		files, _, err := store.Stats()
		paths, _ := store.IndexedPaths()
		_ = store.Close()
		if err != nil {
			t.Fatalf("%s stats: %v", tc.ns, err)
		}
		if files != 1 {
			t.Fatalf("%s indexed %d files, want 1 (namespace isolation)", tc.ns, files)
		}
		found := false
		for p := range paths {
			if strings.HasSuffix(p, tc.wantFile) {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s db does not contain %s; paths=%v", tc.ns, tc.wantFile, paths)
		}
	}

	// Search in org_1 returns a.md; the org_1 DB has no knowledge of b.md.
	search := NewFileSemanticSearchHandler(wsA, "")
	search.IndexesDir = indexesDir
	out, err := search.Execute(JobContext{}, &nexus.Job{ID: "s", Type: "FILE_SEMANTIC_SEARCH", Payload: map[string]string{"query": "tell me about the cat", "index": "org_1/docs"}})
	if err != nil {
		t.Fatalf("search org_1: %v", err)
	}
	var sres struct {
		Hits []struct {
			Path string `json:"path"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(out, &sres); err != nil {
		t.Fatal(err)
	}
	if len(sres.Hits) == 0 {
		t.Fatal("org_1 search returned no hits")
	}
	for _, h := range sres.Hits {
		if !strings.HasSuffix(h.Path, "a.md") {
			t.Fatalf("org_1 search leaked a non-org_1 path %q", h.Path)
		}
	}
}

func TestFileIndexNamespaceTraversalRefusedInExecute(t *testing.T) {
	h := NewFileIndexHandler(t.TempDir(), "")
	h.IndexesDir = t.TempDir()
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "f.md"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := h.Execute(JobContext{}, &nexus.Job{ID: "t", Type: "FILE_INDEX", Payload: map[string]string{"path": ws, "index": "../escape"}})
	if err == nil {
		t.Fatal("FILE_INDEX with traversal namespace succeeded; want refusal")
	}
}

// TestFileIndexNamespaceWithoutIndexesDirFailsClosed pins that a namespaced
// request on a handler with no indexes dir errors rather than silently writing
// into the shared legacy DB.
func TestFileIndexNamespaceWithoutIndexesDirFailsClosed(t *testing.T) {
	h := NewFileIndexHandler(t.TempDir(), "")
	// IndexesDir intentionally empty.
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "f.md"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := h.Execute(JobContext{}, &nexus.Job{ID: "t", Type: "FILE_INDEX", Payload: map[string]string{"path": ws, "index": "org_1/docs"}})
	if err == nil {
		t.Fatal("namespaced FILE_INDEX with no indexes dir succeeded; want fail-closed error")
	}
}

// TestFileIndexLargeFileTruncatedNotSkipped pins that a file larger than the
// configurable read budget is stream-read and its head indexed (reported as
// truncated), instead of the old silent >1 MiB skip.
func TestFileIndexLargeFileTruncatedNotSkipped(t *testing.T) {
	tei := fakeTEI(t)
	t.Setenv("CITADEL_TEI_URL", tei.URL)
	t.Setenv("CITADEL_INDEX_HNSW", "false")

	ws := t.TempDir()
	// A file comfortably larger than the configured 50-byte budget, text, with a
	// head that fakeTEI maps to a non-zero vector.
	big := "the cat " + strings.Repeat("feline ", 100)
	if err := os.WriteFile(filepath.Join(ws, "big.md"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	h := NewFileIndexHandler(ws, filepath.Join(t.TempDir(), "index.db"))
	out, err := h.Execute(JobContext{}, &nexus.Job{ID: "big", Type: "FILE_INDEX", Payload: map[string]string{"path": ws, "max_file_bytes": "50"}})
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	var res struct {
		FilesIndexed         int `json:"files_indexed"`
		FilesSkipped         int `json:"files_skipped"`
		FilesSkippedTooLarge int `json:"files_skipped_too_large"`
		FilesTruncated       int `json:"files_truncated"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatal(err)
	}
	if res.FilesIndexed != 1 || res.FilesTruncated != 1 {
		t.Fatalf("result = %+v, want indexed=1 truncated=1 (large text file head indexed, not dropped)", res)
	}
	if res.FilesSkipped != 0 || res.FilesSkippedTooLarge != 0 {
		t.Fatalf("result = %+v, want skipped=0 skipped_too_large=0 (a text file is never silently skipped for size)", res)
	}
}

// TestFileIndexLargeBinaryHeadSkippedTooLarge pins that a large file whose head
// is binary is reported explicitly in files_skipped_too_large, never silently.
func TestFileIndexLargeBinaryHeadSkippedTooLarge(t *testing.T) {
	tei := fakeTEI(t)
	t.Setenv("CITADEL_TEI_URL", tei.URL)

	ws := t.TempDir()
	blob := make([]byte, 200) // all-NUL head, larger than the 50-byte budget
	if err := os.WriteFile(filepath.Join(ws, "blob.bin"), blob, 0o644); err != nil {
		t.Fatal(err)
	}
	h := NewFileIndexHandler(ws, filepath.Join(t.TempDir(), "index.db"))
	out, err := h.Execute(JobContext{}, &nexus.Job{ID: "blob", Type: "FILE_INDEX", Payload: map[string]string{"path": ws, "max_file_bytes": "50"}})
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	var res struct {
		FilesIndexed         int `json:"files_indexed"`
		FilesSkipped         int `json:"files_skipped"`
		FilesSkippedTooLarge int `json:"files_skipped_too_large"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatal(err)
	}
	if res.FilesIndexed != 0 || res.FilesSkippedTooLarge != 1 || res.FilesSkipped != 0 {
		t.Fatalf("result = %+v, want indexed=0 skipped_too_large=1 skipped=0", res)
	}
}

// TestFileIndexChunkCapReported pins that hitting the per-file chunk cap is
// reported in files_chunk_capped rather than silently dropping the tail.
func TestFileIndexChunkCapReported(t *testing.T) {
	tei := fakeTEI(t)
	t.Setenv("CITADEL_TEI_URL", tei.URL)
	t.Setenv("CITADEL_INDEX_HNSW", "false")

	ws := t.TempDir()
	var paras []string
	for i := 0; i < 6; i++ {
		paras = append(paras, fmt.Sprintf("%04d-%s", i, strings.Repeat("y", chunkTargetBytes)))
	}
	if err := os.WriteFile(filepath.Join(ws, "many.md"), []byte(strings.Join(paras, "\n\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	h := NewFileIndexHandler(ws, filepath.Join(t.TempDir(), "index.db"))
	out, err := h.Execute(JobContext{}, &nexus.Job{ID: "cap", Type: "FILE_INDEX", Payload: map[string]string{"path": ws, "max_chunks_per_file": "3"}})
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	var res struct {
		FilesIndexed     int `json:"files_indexed"`
		FilesChunkCapped int `json:"files_chunk_capped"`
		ChunksUpserted   int `json:"chunks_upserted"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatal(err)
	}
	if res.FilesIndexed != 1 || res.FilesChunkCapped != 1 || res.ChunksUpserted != 3 {
		t.Fatalf("result = %+v, want indexed=1 chunk_capped=1 chunks=3", res)
	}
}

// TestFileSemanticSearchPathPrefixesFilter pins that path_prefixes restricts hits
// to files under a given prefix, filtered before the top-K trim.
func TestFileSemanticSearchPathPrefixesFilter(t *testing.T) {
	tei := fakeTEI(t)
	t.Setenv("CITADEL_TEI_URL", tei.URL)
	t.Setenv("CITADEL_INDEX_DB", "")
	// Leave the HNSW accelerator ENABLED (default) so this exercises the
	// keep != nil -> accelerator-bypass branch of Store.search with the
	// accelerator actually present, not just the brute-force fallback.

	ws := t.TempDir()
	subA := filepath.Join(ws, "subA")
	subB := filepath.Join(ws, "subB")
	if err := os.MkdirAll(subA, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(subB, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subA, "cat1.md"), []byte("the cat is a feline"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subB, "cat2.md"), []byte("another cat is a kitten"), 0o644); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "index.db")
	idx := NewFileIndexHandler(ws, dbPath)
	if _, err := idx.Execute(JobContext{}, &nexus.Job{ID: "i", Type: "FILE_INDEX", Payload: map[string]string{"path": ws}}); err != nil {
		t.Fatalf("index: %v", err)
	}

	search := NewFileSemanticSearchHandler(ws, dbPath)
	// Unfiltered: both cat files are candidates.
	outAll, err := search.Execute(JobContext{}, &nexus.Job{ID: "all", Type: "FILE_SEMANTIC_SEARCH", Payload: map[string]string{"query": "cat"}})
	if err != nil {
		t.Fatalf("search all: %v", err)
	}
	var all struct {
		Count int `json:"count"`
	}
	_ = json.Unmarshal(outAll, &all)
	if all.Count < 2 {
		t.Fatalf("unfiltered search found %d hits, want >= 2", all.Count)
	}

	// Filtered to subA: only subA hits.
	prefixes, _ := json.Marshal([]string{subA})
	out, err := search.Execute(JobContext{}, &nexus.Job{ID: "f", Type: "FILE_SEMANTIC_SEARCH", Payload: map[string]string{"query": "cat", "path_prefixes": string(prefixes)}})
	if err != nil {
		t.Fatalf("search filtered: %v", err)
	}
	var res struct {
		Hits []struct {
			Path string `json:"path"`
		} `json:"hits"`
		Count int `json:"count"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatal(err)
	}
	if res.Count == 0 {
		t.Fatal("filtered search returned no hits; want the subA hit")
	}
	for _, h := range res.Hits {
		if !strings.HasPrefix(h.Path, subA) {
			t.Fatalf("path_prefixes filter leaked a hit outside subA: %q", h.Path)
		}
	}
}
