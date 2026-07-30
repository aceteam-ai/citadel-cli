package nodeindex

import (
	"database/sql"
	"fmt"
	"math"
	"math/rand"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/coder/hnsw"
)

// This file adds a pure-Go HNSW (Hierarchical Navigable Small World) query
// accelerator over the SQLite source of truth. SQLite still owns every chunk +
// vector durably; the HNSW graph is an in-memory ANN index built from those rows
// so a query does an approximate-nearest-neighbor descent instead of a full
// brute-force cosine scan of every chunk.
//
// Why coder/hnsw: it is pure Go (no cgo — verified: no `import "C"` anywhere in
// the module), supports both Add and Delete, and its search returns each node's
// stored vector so we can recompute an EXACT cosine score (matching the
// brute-force path bit-for-bit) rather than trusting the graph's internal
// distance. This keeps release builds CGO_ENABLED=0 (see build.sh) — the same
// constraint that rules out the C sqlite-vec extension.
//
// Cross-process correctness: `citadel work` holds a built graph in memory while
// a SEPARATE `citadel search index` process writes the same index.db. A stale
// in-memory graph would silently omit newly-indexed files. To catch that, the
// accelerator stores a cheap fingerprint (chunk COUNT + MAX(id)) of the SQLite
// state it was built from and re-checks it on every Search, rebuilding when it
// differs. Because chunk ids are AUTOINCREMENT (monotonic, never reused), any
// insert bumps MAX(id) and any delete lowers COUNT, so the (count, max_id) pair
// changes on any write — including a same-count re-index of a file, whose new
// chunks get higher ids. This is simpler and more correct across processes than
// threading incremental add/delete through the write transactions.

// accelDisabled reports whether the HNSW accelerator is turned off via the
// CITADEL_INDEX_HNSW kill switch (default: enabled). When disabled, Search falls
// back to the brute-force scan.
func accelDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CITADEL_INDEX_HNSW"))) {
	case "0", "false", "off", "no":
		return true
	default:
		return false
	}
}

// accelEfSearch is the graph's search breadth. Higher improves recall at the
// cost of a little more work per query. 64 keeps recall high for the small
// (thousands-of-chunks) indexes a node holds while staying cheap.
const accelEfSearch = 64

// accelerator wraps a coder/hnsw graph keyed by chunk id, plus the fingerprint
// of the SQLite state it was built from. It is registered per-db-path so a
// long-running process (worker, TUI) builds the graph once and reuses it across
// queries, while a change on disk (even from another process) triggers a lazy
// rebuild on the next Search.
type accelerator struct {
	mu      sync.Mutex
	graph   *hnsw.Graph[int64]
	dim     int   // vector dimension the graph was built with; mismatches are skipped
	fpCount int   // chunk COUNT at build time
	fpMaxID int64 // chunk MAX(id) at build time
	built   bool
	// seed, when non-zero, makes graph level-generation deterministic (tests
	// only). Production leaves it zero so NewGraph time-seeds its own Rng.
	seed int64
}

var (
	accelRegistryMu sync.Mutex
	accelRegistry   = map[string]*accelerator{}
)

// getAccelerator returns the process-global accelerator for dbPath, creating it
// on first use. Keying by dbPath lets every Store opened against the same index
// share one in-memory graph.
func getAccelerator(dbPath string) *accelerator {
	accelRegistryMu.Lock()
	defer accelRegistryMu.Unlock()
	a := accelRegistry[dbPath]
	if a == nil {
		a = &accelerator{}
		accelRegistry[dbPath] = a
	}
	return a
}

// fingerprint returns the cheap (count, max_id) signature of the chunks table.
func fingerprint(db *sql.DB) (count int, maxID int64, err error) {
	row := db.QueryRow(`SELECT COUNT(*), COALESCE(MAX(id), 0) FROM chunks`)
	if err := row.Scan(&count, &maxID); err != nil {
		return 0, 0, fmt.Errorf("fingerprint chunks: %w", err)
	}
	return count, maxID, nil
}

// ensureFreshLocked (re)builds the graph if it has never been built or the
// SQLite fingerprint has changed since the last build. Caller holds a.mu.
func (a *accelerator) ensureFreshLocked(db *sql.DB) error {
	count, maxID, err := fingerprint(db)
	if err != nil {
		return err
	}
	if a.built && count == a.fpCount && maxID == a.fpMaxID {
		return nil
	}
	return a.rebuildLocked(db, count, maxID)
}

// rebuildLocked builds a fresh graph from all chunk rows. Caller holds a.mu.
func (a *accelerator) rebuildLocked(db *sql.DB, count int, maxID int64) error {
	g := hnsw.NewGraph[int64]()
	g.EfSearch = accelEfSearch
	if a.seed != 0 {
		g.Rng = rand.New(rand.NewSource(a.seed))
	}

	rows, err := db.Query(`SELECT id, embedding FROM chunks`)
	if err != nil {
		return fmt.Errorf("scan chunks for hnsw build: %w", err)
	}
	defer rows.Close()

	dim := 0
	for rows.Next() {
		var (
			id   int64
			blob []byte
		)
		if err := rows.Scan(&id, &blob); err != nil {
			return fmt.Errorf("scan chunk row for hnsw: %w", err)
		}
		vec := decodeVector(blob)
		if len(vec) == 0 {
			continue
		}
		if dim == 0 {
			dim = len(vec)
		}
		// A mixed-dimension index (e.g. after a --model / CITADEL_EMBEDDING_MODEL
		// change) would make hnsw's distance function panic on length mismatch,
		// whereas the brute-force path skips such rows. Preserve that graceful
		// behavior: only the majority (first-seen) dimension enters the graph;
		// off-dimension chunks are searchable only via the brute-force fallback.
		if len(vec) != dim {
			continue
		}
		g.Add(hnsw.MakeNode(id, vec))
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate chunks for hnsw: %w", err)
	}

	a.graph = g
	a.dim = dim
	a.fpCount = count
	a.fpMaxID = maxID
	a.built = true
	return nil
}

// search runs an ANN query. It returns (hits, true, nil) when the accelerator
// served the query, or (nil, false, nil) to signal the caller should fall back
// to brute force (empty graph, or a query whose dimension does not match the
// graph). Scores are EXACT cosine similarities recomputed from each returned
// node's stored vector, so they match the brute-force path.
func (a *accelerator) search(db *sql.DB, query []float32, qNorm float64, topK int) ([]SearchHit, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if err := a.ensureFreshLocked(db); err != nil {
		return nil, false, err
	}
	if !a.built || a.graph.Len() == 0 {
		return nil, false, nil // empty index — let brute force return nil cleanly
	}
	// Dimension mismatch: defer to brute force, which skips mismatched rows and
	// still scores the compatible ones.
	if a.dim != 0 && len(query) != a.dim {
		return nil, false, nil
	}

	nodes := a.graph.Search(query, topK)
	if len(nodes) == 0 {
		return nil, false, nil
	}

	type scored struct {
		id    int64
		score float64
	}
	scoredHits := make([]scored, 0, len(nodes))
	ids := make([]int64, 0, len(nodes))
	for _, n := range nodes {
		score := cosinePrenorm(query, qNorm, n.Value)
		if math.IsNaN(score) {
			continue
		}
		scoredHits = append(scoredHits, scored{id: n.Key, score: score})
		ids = append(ids, n.Key)
	}
	if len(ids) == 0 {
		return nil, true, nil
	}

	meta, err := fetchChunkMeta(db, ids)
	if err != nil {
		return nil, false, err
	}

	hits := make([]SearchHit, 0, len(scoredHits))
	for _, sh := range scoredHits {
		m, ok := meta[sh.id]
		if !ok {
			continue // row vanished between search and fetch; skip
		}
		hits = append(hits, SearchHit{Path: m.path, ChunkIndex: m.chunkIndex, Text: m.text, Score: sh.score})
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if len(hits) > topK {
		hits = hits[:topK]
	}
	return hits, true, nil
}

type chunkMeta struct {
	path       string
	chunkIndex int
	text       string
}

// fetchChunkMeta loads (path, chunk_index, text) for the given chunk ids in one
// query, returning a map keyed by id.
func fetchChunkMeta(db *sql.DB, ids []int64) (map[int64]chunkMeta, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	q := "SELECT id, path, chunk_index, text FROM chunks WHERE id IN (" + strings.Join(placeholders, ",") + ")"
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("fetch chunk metadata: %w", err)
	}
	defer rows.Close()
	out := make(map[int64]chunkMeta, len(ids))
	for rows.Next() {
		var (
			id int64
			m  chunkMeta
		)
		if err := rows.Scan(&id, &m.path, &m.chunkIndex, &m.text); err != nil {
			return nil, fmt.Errorf("scan chunk metadata: %w", err)
		}
		out[id] = m
	}
	return out, rows.Err()
}
