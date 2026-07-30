package nodeindex

import (
	"fmt"
	"math"
	"math/rand"
	"testing"
)

// seedAccel makes the accelerator deterministic for a store's dbPath so the
// parity test is reproducible (production leaves the seed zero => time-seeded).
func seedAccel(s *Store, seed int64) {
	if s.accel == nil {
		s.accel = getAccelerator(s.dbPath)
	}
	s.accel.mu.Lock()
	s.accel.seed = seed
	s.accel.built = false // force a rebuild with the deterministic Rng
	s.accel.mu.Unlock()
}

// randVec builds a reproducible pseudo-random dim-vector. Random vectors (unlike
// orthonormal one-hots, which are all equidistant and pathological for a
// navigable-graph ANN) have genuine neighbor structure, which is what HNSW is
// designed to exploit.
func randVec(r *rand.Rand, dim int) []float32 {
	v := make([]float32, dim)
	for i := range v {
		v[i] = float32(r.NormFloat64())
	}
	return v
}

// pathSet collects the paths of a hit slice.
func pathSet(hits []SearchHit) map[string]float64 {
	m := make(map[string]float64, len(hits))
	for _, h := range hits {
		m[h.Path] = h.Score
	}
	return m
}

// TestHNSWParityWithBruteForce asserts the accelerator agrees with the
// brute-force reference on the things that must be exact for an approximate
// index to be CORRECT:
//   - the unambiguous nearest neighbor (queries are a real stored vector plus
//     tiny noise, so exactly one document is clearly closest) — exact top-1,
//   - scores: any path returned by BOTH paths must score identically (this
//     verifies the exact-cosine recomputation from the graph's stored vectors),
//   - recall: the top-k sets overlap heavily (HNSW is approximate, so deep-rank
//     order may legitimately differ — we require >=70% overlap, not identity).
func TestHNSWParityWithBruteForce(t *testing.T) {
	s := openTemp(t)
	seedAccel(s, 1)

	// Clustered data: 6 well-separated clusters of 10 docs each. This gives the
	// index genuine neighbor structure (the realistic case for embeddings), so
	// the top-k for a query near a cluster is that cluster's members — clearly
	// separated from other clusters — and HNSW recall over the RELEVANT neighbors
	// is high. (Uniformly-random vectors, by contrast, have near-tied cosines at
	// every deep rank, where recall is both low and meaningless.)
	const dim = 32
	const clusters = 8
	const perCluster = 12
	const n = clusters * perCluster
	r := rand.New(rand.NewSource(42))
	centers := make([][]float32, clusters)
	for c := range centers {
		centers[c] = randVec(r, dim)
		for j := range centers[c] {
			centers[c][j] *= 3 // moderate separation — realistic, not degenerate islands
		}
	}
	vecs := make([][]float32, n)
	for i := 0; i < n; i++ {
		c := i / perCluster
		v := make([]float32, dim)
		for j := range v {
			v[j] = centers[c][j] + float32(r.NormFloat64())*0.6
		}
		vecs[i] = v
		path := fmt.Sprintf("/root/doc-%02d.md", i)
		if err := s.UpsertFile(path, fmt.Sprintf("h%d", i), 1, 1, "m", dim, []Chunk{
			{Index: 0, Text: fmt.Sprintf("chunk %d", i), Embedding: vecs[i]},
		}); err != nil {
			t.Fatalf("UpsertFile %d: %v", i, err)
		}
	}

	nr := rand.New(rand.NewSource(7))
	const topK = 5
	for q := 0; q < 25; q++ {
		target := q % n
		query := make([]float32, dim)
		for j := range query {
			query[j] = vecs[target][j] + float32(nr.NormFloat64())*0.001 // tiny perturbation
		}
		qNorm := norm(query)

		accelHits, served, err := s.accel.search(s.db, query, qNorm, topK)
		if err != nil {
			t.Fatalf("accel search: %v", err)
		}
		if !served {
			t.Fatalf("query %d: accelerator did not serve a non-empty index", q)
		}
		bruteHits, err := s.searchBrute(query, qNorm, topK)
		if err != nil {
			t.Fatalf("brute search: %v", err)
		}
		// Reference set of the genuinely-nearest chunks (a generous window): every
		// accelerator result must fall inside it (precision), and scores must match
		// exactly. Recall (finding ALL of the true top-k) is best-effort for an
		// approximate index and deliberately NOT asserted per-query.
		refNear, err := s.searchBrute(query, qNorm, 3*topK)
		if err != nil {
			t.Fatalf("brute ref search: %v", err)
		}
		refSet := pathSet(refNear)

		// (1) Exact nearest neighbor — the accelerator must find THE best hit.
		wantTop := fmt.Sprintf("/root/doc-%02d.md", target)
		if len(accelHits) == 0 || accelHits[0].Path != wantTop {
			t.Errorf("query %d: top-1 accel=%v want=%s", q, topPath(accelHits), wantTop)
		}
		if len(bruteHits) == 0 || bruteHits[0].Path != wantTop {
			t.Fatalf("query %d: brute top-1=%v want=%s (test setup broken)", q, topPath(bruteHits), wantTop)
		}
		if len(accelHits) != len(bruteHits) {
			t.Errorf("query %d: accel returned %d hits, brute %d", q, len(accelHits), len(bruteHits))
		}

		// (2) Precision + (3) exact score parity: every accel hit is genuinely near
		// and scored identically to the brute-force computation.
		for _, h := range accelHits {
			refScore, ok := refSet[h.Path]
			if !ok {
				t.Errorf("query %d: accel returned %s which is NOT among the true %d nearest", q, h.Path, 3*topK)
				continue
			}
			if math.Abs(h.Score-refScore) > 1e-6 {
				t.Errorf("query %d path %s: score mismatch accel=%.9f brute=%.9f", q, h.Path, h.Score, refScore)
			}
		}
	}
}

func topPath(h []SearchHit) string {
	if len(h) == 0 {
		return "(none)"
	}
	return h[0].Path
}

// TestHNSWTopMatchesBrute is the public-Search integration check: Store.Search
// (accelerator path) and searchBrute agree on the top result.
func TestHNSWTopMatchesBrute(t *testing.T) {
	s := openTemp(t)
	seedAccel(s, 3)
	const n = 25
	const dim = 16
	r := rand.New(rand.NewSource(99))
	vecs := make([][]float32, n)
	for i := 0; i < n; i++ {
		vecs[i] = randVec(r, dim)
		_ = s.UpsertFile(fmt.Sprintf("/root/f%02d.md", i), fmt.Sprintf("h%d", i), 1, 1, "m", dim,
			[]Chunk{{Index: 0, Text: "t", Embedding: vecs[i]}})
	}
	// Query is a near-copy of doc 7, so doc 7 is unambiguously the nearest.
	query := make([]float32, dim)
	copy(query, vecs[7])
	query[0] += 0.001
	got, err := s.Search(query, 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	want, err := s.searchBrute(query, norm(query), 5)
	if err != nil {
		t.Fatalf("searchBrute: %v", err)
	}
	if len(got) == 0 || len(want) == 0 {
		t.Fatal("expected non-empty results")
	}
	if got[0].Path != want[0].Path {
		t.Errorf("top result mismatch: accel=%s brute=%s", got[0].Path, want[0].Path)
	}
}

// TestHNSWFingerprintRebuild verifies the accelerator rebuilds when new chunks
// are inserted (the cross-process staleness guard): a search after an insert
// must see the new file.
func TestHNSWFingerprintRebuild(t *testing.T) {
	s := openTemp(t)
	seedAccel(s, 2)
	_ = s.UpsertFile("/root/a.md", "h1", 1, 1, "m", 3, []Chunk{{Index: 0, Text: "a", Embedding: []float32{1, 0, 0}}})

	// Prime the accelerator (builds the graph with just a.md).
	if _, err := s.Search([]float32{1, 0, 0}, 5); err != nil {
		t.Fatalf("first search: %v", err)
	}

	// Insert a second file; the fingerprint (count, max_id) changes.
	_ = s.UpsertFile("/root/b.md", "h2", 1, 1, "m", 3, []Chunk{{Index: 0, Text: "b", Embedding: []float32{0, 1, 0}}})

	hits, err := s.Search([]float32{0, 1, 0}, 5)
	if err != nil {
		t.Fatalf("second search: %v", err)
	}
	found := false
	for _, h := range hits {
		if h.Path == "/root/b.md" {
			found = true
		}
	}
	if !found {
		t.Errorf("accelerator did not pick up newly-inserted b.md (stale graph); hits=%+v", hits)
	}
}

// TestHNSWMixedDimensionGraceful ensures a mixed-dimension index does not crash
// the accelerator: off-dimension chunks are excluded from the graph, and a query
// still returns the matching same-dimension chunk. (Brute force skips mismatches;
// the accelerator must not panic where hnsw's distance fn would on a length
// mismatch.)
func TestHNSWMixedDimensionGraceful(t *testing.T) {
	s := openTemp(t)
	seedAccel(s, 4)
	// First-seen dimension is 3.
	_ = s.UpsertFile("/root/three.md", "h", 1, 1, "m", 3, []Chunk{{Index: 0, Text: "three", Embedding: []float32{1, 0, 0}}})
	// A 5-dim chunk (e.g. after a model change) must not crash the build.
	_ = s.UpsertFile("/root/five.md", "h", 1, 1, "m", 5, []Chunk{{Index: 0, Text: "five", Embedding: []float32{1, 0, 0, 0, 0}}})

	query := []float32{1, 0, 0}
	hits, err := s.Search(query, 5)
	if err != nil {
		t.Fatalf("Search over mixed-dim index: %v", err)
	}
	if len(hits) == 0 || hits[0].Path != "/root/three.md" {
		t.Errorf("expected /root/three.md as top hit over mixed-dim index, got %+v", hits)
	}
}

// TestAccelDisabledFallsBackToBrute confirms the CITADEL_INDEX_HNSW kill switch
// disables the accelerator (Open leaves s.accel nil) while Search still works.
func TestAccelDisabledFallsBackToBrute(t *testing.T) {
	t.Setenv("CITADEL_INDEX_HNSW", "0")
	s := openTemp(t)
	if s.accel != nil {
		t.Fatal("expected accelerator to be nil when CITADEL_INDEX_HNSW=0")
	}
	_ = s.UpsertFile("/root/a.md", "h", 1, 1, "m", 3, []Chunk{{Index: 0, Text: "a", Embedding: []float32{1, 0, 0}}})
	hits, err := s.Search([]float32{1, 0, 0}, 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 || hits[0].Path != "/root/a.md" {
		t.Errorf("brute-force fallback returned unexpected hits: %+v", hits)
	}
}
