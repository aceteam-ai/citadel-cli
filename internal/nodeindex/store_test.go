package nodeindex

import (
	"math"
	"path/filepath"
	"strings"
	"testing"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	db := filepath.Join(t.TempDir(), "index.db")
	s, err := Open(db)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestVectorCodecRoundTrip(t *testing.T) {
	cases := [][]float32{
		{},
		{0},
		{1, -1, 0.5, math.MaxFloat32, -math.MaxFloat32},
		{0.0001, 123456.7, -0.0},
	}
	for _, v := range cases {
		got := decodeVector(encodeVector(v))
		if len(got) != len(v) {
			t.Fatalf("len mismatch: got %d want %d", len(got), len(v))
		}
		for i := range v {
			if got[i] != v[i] {
				t.Fatalf("elem %d: got %v want %v", i, got[i], v[i])
			}
		}
	}
}

func TestUpsertAndSearchRoundTrip(t *testing.T) {
	s := openTemp(t)

	// Two orthogonal-ish vectors on distinct files.
	err := s.UpsertFile("/ws/a.md", "hashA", 1, 10, "m", 3, []Chunk{
		{Index: 0, Text: "alpha doc", Embedding: []float32{1, 0, 0}},
	})
	if err != nil {
		t.Fatalf("UpsertFile a: %v", err)
	}
	err = s.UpsertFile("/ws/b.md", "hashB", 2, 10, "m", 3, []Chunk{
		{Index: 0, Text: "beta doc", Embedding: []float32{0, 1, 0}},
	})
	if err != nil {
		t.Fatalf("UpsertFile b: %v", err)
	}

	files, chunks, err := s.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if files != 2 || chunks != 2 {
		t.Fatalf("stats: files=%d chunks=%d want 2/2", files, chunks)
	}

	// Query aligned with a.md's vector.
	hits, err := s.Search([]float32{1, 0, 0}, 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("want 2 hits, got %d", len(hits))
	}
	if hits[0].Path != "/ws/a.md" {
		t.Fatalf("best hit should be a.md, got %s (score %v)", hits[0].Path, hits[0].Score)
	}
	if math.Abs(hits[0].Score-1.0) > 1e-6 {
		t.Fatalf("identical vector should score ~1.0, got %v", hits[0].Score)
	}
	if math.Abs(hits[1].Score) > 1e-6 {
		t.Fatalf("orthogonal vector should score ~0, got %v", hits[1].Score)
	}
}

func TestReUpsertReplacesChunks(t *testing.T) {
	s := openTemp(t)
	_ = s.UpsertFile("/ws/a.md", "h1", 1, 10, "m", 2, []Chunk{
		{Index: 0, Text: "one", Embedding: []float32{1, 0}},
		{Index: 1, Text: "two", Embedding: []float32{0, 1}},
	})
	// Re-index the same path with fewer chunks; must not leave the stale one.
	_ = s.UpsertFile("/ws/a.md", "h2", 2, 10, "m", 2, []Chunk{
		{Index: 0, Text: "only", Embedding: []float32{1, 1}},
	})
	files, chunks, err := s.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if files != 1 || chunks != 1 {
		t.Fatalf("re-upsert should leave 1 file / 1 chunk, got %d/%d", files, chunks)
	}
	hash, indexed, err := s.FileHash("/ws/a.md")
	if err != nil || !indexed {
		t.Fatalf("FileHash: hash=%q indexed=%v err=%v", hash, indexed, err)
	}
	if hash != "h2" {
		t.Fatalf("hash not updated: got %q want h2", hash)
	}
}

func TestDeleteFileAndIndexedPaths(t *testing.T) {
	s := openTemp(t)
	_ = s.UpsertFile("/ws/a.md", "h", 1, 1, "m", 1, []Chunk{{Index: 0, Text: "x", Embedding: []float32{1}}})
	_ = s.UpsertFile("/ws/b.md", "h", 1, 1, "m", 1, []Chunk{{Index: 0, Text: "y", Embedding: []float32{1}}})

	paths, err := s.IndexedPaths()
	if err != nil {
		t.Fatalf("IndexedPaths: %v", err)
	}
	if _, ok := paths["/ws/a.md"]; !ok {
		t.Fatalf("a.md missing from indexed paths")
	}
	if len(paths) != 2 {
		t.Fatalf("want 2 indexed paths, got %d", len(paths))
	}

	if err := s.DeleteFile("/ws/a.md"); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	files, chunks, _ := s.Stats()
	if files != 1 || chunks != 1 {
		t.Fatalf("after delete want 1/1, got %d/%d", files, chunks)
	}
}

func TestSearchEmptyIndexAndZeroQuery(t *testing.T) {
	s := openTemp(t)
	hits, err := s.Search([]float32{1, 0, 0}, 5)
	if err != nil {
		t.Fatalf("Search empty: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("empty index should yield no hits, got %d", len(hits))
	}

	_ = s.UpsertFile("/ws/a.md", "h", 1, 1, "m", 3, []Chunk{{Index: 0, Text: "x", Embedding: []float32{1, 0, 0}}})
	// Zero-magnitude query vector: no meaningful direction, expect no hits.
	hits, err = s.Search([]float32{0, 0, 0}, 5)
	if err != nil {
		t.Fatalf("Search zero query: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("zero query should yield no hits, got %d", len(hits))
	}
}

func TestSearchMismatchedDimSkipped(t *testing.T) {
	s := openTemp(t)
	// Index a 3-dim vector, then query with a 2-dim vector: dimensions differ,
	// the hit must be skipped (NaN score), not crash or produce a bogus score.
	_ = s.UpsertFile("/ws/a.md", "h", 1, 1, "m", 3, []Chunk{{Index: 0, Text: "x", Embedding: []float32{1, 0, 0}}})
	hits, err := s.Search([]float32{1, 0}, 5)
	if err != nil {
		t.Fatalf("Search mismatched dim: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("mismatched-dim hit should be skipped, got %d", len(hits))
	}
}

func TestSearchTopKCap(t *testing.T) {
	s := openTemp(t)
	for i := 0; i < 5; i++ {
		p := "/ws/f" + string(rune('a'+i)) + ".md"
		_ = s.UpsertFile(p, "h", 1, 1, "m", 2, []Chunk{{Index: 0, Text: "t", Embedding: []float32{float32(i + 1), 1}}})
	}
	hits, err := s.Search([]float32{1, 1}, 3)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 3 {
		t.Fatalf("topK=3 should cap at 3, got %d", len(hits))
	}
	// Scores must be sorted descending.
	for i := 1; i < len(hits); i++ {
		if hits[i-1].Score < hits[i].Score {
			t.Fatalf("hits not sorted desc: %v", hits)
		}
	}
}

func TestLongTextChunkStored(t *testing.T) {
	s := openTemp(t)
	long := strings.Repeat("word ", 5000) // ~25 KB single chunk text
	if err := s.UpsertFile("/ws/big.md", "h", 1, int64(len(long)), "m", 2, []Chunk{
		{Index: 0, Text: long, Embedding: []float32{1, 0}},
	}); err != nil {
		t.Fatalf("UpsertFile long: %v", err)
	}
	hits, err := s.Search([]float32{1, 0}, 1)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 || len(hits[0].Text) != len(long) {
		t.Fatalf("long text not round-tripped intact")
	}
}
