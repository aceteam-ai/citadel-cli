// Package nodeindex provides a node-local semantic index over the files a
// Citadel node hosts. It is the node tier of the two-tier (central + node-local)
// index described in aceteam#6087: the drive indexes what's on the drive, the
// node indexes what's on the node, and search fans out across both.
//
// Storage is a pure-Go SQLite database (modernc.org/sqlite, the same engine the
// usage store uses) at, by default, ~/citadel-node/index.db. Embeddings are
// stored as little-endian float32 BLOBs and KNN is a brute-force cosine scan in
// Go.
//
// Why not sqlite-vec: sqlite-vec is a C loadable extension. Release builds are
// CGO_ENABLED=0 (see build.sh) and the SQLite driver is the cgo-free modernc
// engine, which cannot load C extensions. A node hosts thousands, not millions,
// of chunks, so a brute-force cosine scan is adequate for the foundation. When a
// node's chunk count justifies it, a sqlite-vec acceleration can be added behind
// a build tag without changing this package's API (aceteam#6087, Phase 3).
package nodeindex

import (
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"sort"
	"time"

	_ "modernc.org/sqlite"
)

// schema is the node-local index schema. This SQLite database is a node-local
// cache, NOT the central (sacred) DB; it may be recreated at any time by
// re-running FILE_INDEX.
const schema = `
CREATE TABLE IF NOT EXISTS indexed_files (
    path          TEXT PRIMARY KEY,
    content_hash  TEXT NOT NULL,
    mtime         INTEGER NOT NULL DEFAULT 0,
    size          INTEGER NOT NULL DEFAULT 0,
    model         TEXT NOT NULL DEFAULT '',
    dim           INTEGER NOT NULL DEFAULT 0,
    chunk_count   INTEGER NOT NULL DEFAULT 0,
    indexed_at    TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE TABLE IF NOT EXISTS chunks (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    path         TEXT NOT NULL,
    chunk_index  INTEGER NOT NULL,
    text         TEXT NOT NULL,
    embedding    BLOB NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_chunks_path ON chunks(path);
`

// Store is a node-local semantic index backed by SQLite.
type Store struct {
	db *sql.DB
}

// Chunk is one embedded unit of a file: a slice of text and its vector.
type Chunk struct {
	Index     int
	Text      string
	Embedding []float32
}

// SearchHit is one KNN result: the source file, which chunk, the chunk text,
// and the cosine similarity score in [-1, 1].
type SearchHit struct {
	Path       string  `json:"path"`
	ChunkIndex int     `json:"chunk_index"`
	Text       string  `json:"text"`
	Score      float64 `json:"score"`
}

// Open opens (or creates) the node-local index database at dbPath and runs
// migrations. WAL mode lets a FILE_SEMANTIC_SEARCH read proceed while a
// FILE_INDEX write is in flight.
func Open(dbPath string) (*Store, error) {
	if dbPath == "" {
		return nil, fmt.Errorf("index db path is empty")
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open index db: %w", err)
	}
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable WAL: %w", err)
	}
	// A FILE_INDEX write and a FILE_SEMANTIC_SEARCH read (or two indexers) can
	// open the same db concurrently; wait rather than fail immediately on a lock.
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set busy_timeout: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("run migrations: %w", err)
	}
	return &Store{db: db}, nil
}

// Close releases the underlying database handle.
func (s *Store) Close() error {
	return s.db.Close()
}

// FileHash returns the content hash recorded for path and whether the path is
// currently indexed. It lets FILE_INDEX skip files whose content is unchanged.
func (s *Store) FileHash(path string) (hash string, indexed bool, err error) {
	row := s.db.QueryRow(`SELECT content_hash FROM indexed_files WHERE path = ?`, path)
	switch err := row.Scan(&hash); err {
	case nil:
		return hash, true, nil
	case sql.ErrNoRows:
		return "", false, nil
	default:
		return "", false, fmt.Errorf("query file hash: %w", err)
	}
}

// IndexedPaths returns the set of currently indexed file paths, so a re-index of
// a directory can prune entries whose files were deleted on disk.
func (s *Store) IndexedPaths() (map[string]struct{}, error) {
	rows, err := s.db.Query(`SELECT path FROM indexed_files`)
	if err != nil {
		return nil, fmt.Errorf("query indexed paths: %w", err)
	}
	defer rows.Close()
	out := make(map[string]struct{})
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("scan path: %w", err)
		}
		out[p] = struct{}{}
	}
	return out, rows.Err()
}

// UpsertFile replaces the indexed representation of a single file: it deletes any
// prior chunks for path and inserts the file row plus its chunks in one
// transaction, so a re-index never leaves duplicate or orphaned chunks.
func (s *Store) UpsertFile(path, contentHash string, mtime, size int64, model string, dim int, chunks []Chunk) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	if _, err := tx.Exec(`DELETE FROM chunks WHERE path = ?`, path); err != nil {
		return fmt.Errorf("delete old chunks: %w", err)
	}
	if _, err := tx.Exec(`
		INSERT INTO indexed_files (path, content_hash, mtime, size, model, dim, chunk_count, indexed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
			content_hash=excluded.content_hash,
			mtime=excluded.mtime,
			size=excluded.size,
			model=excluded.model,
			dim=excluded.dim,
			chunk_count=excluded.chunk_count,
			indexed_at=excluded.indexed_at`,
		path, contentHash, mtime, size, model, dim, len(chunks),
		time.Now().UTC().Format(time.RFC3339)); err != nil {
		return fmt.Errorf("upsert file row: %w", err)
	}
	stmt, err := tx.Prepare(`INSERT INTO chunks (path, chunk_index, text, embedding) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare chunk insert: %w", err)
	}
	defer stmt.Close()
	for _, c := range chunks {
		if _, err := stmt.Exec(path, c.Index, c.Text, encodeVector(c.Embedding)); err != nil {
			return fmt.Errorf("insert chunk %d: %w", c.Index, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// DeleteFile removes a file and its chunks from the index (used when a file that
// was previously indexed no longer exists on disk).
func (s *Store) DeleteFile(path string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.Exec(`DELETE FROM chunks WHERE path = ?`, path); err != nil {
		return fmt.Errorf("delete chunks: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM indexed_files WHERE path = ?`, path); err != nil {
		return fmt.Errorf("delete file row: %w", err)
	}
	return tx.Commit()
}

// Search embeds are compared by cosine similarity against every stored chunk
// (brute force) and the topK highest-scoring hits are returned, best first. A
// query vector with zero magnitude, or an empty index, yields no hits.
func (s *Store) Search(query []float32, topK int) ([]SearchHit, error) {
	if topK <= 0 {
		topK = 10
	}
	qNorm := norm(query)
	if qNorm == 0 {
		return nil, nil
	}
	rows, err := s.db.Query(`SELECT path, chunk_index, text, embedding FROM chunks`)
	if err != nil {
		return nil, fmt.Errorf("scan chunks: %w", err)
	}
	defer rows.Close()

	var hits []SearchHit
	for rows.Next() {
		var (
			path string
			idx  int
			text string
			blob []byte
		)
		if err := rows.Scan(&path, &idx, &text, &blob); err != nil {
			return nil, fmt.Errorf("scan chunk row: %w", err)
		}
		vec := decodeVector(blob)
		score := cosinePrenorm(query, qNorm, vec)
		if math.IsNaN(score) {
			continue
		}
		hits = append(hits, SearchHit{Path: path, ChunkIndex: idx, Text: text, Score: score})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate chunks: %w", err)
	}

	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if len(hits) > topK {
		hits = hits[:topK]
	}
	return hits, nil
}

// Stats reports how many files and chunks are indexed (used by tests and
// diagnostics).
func (s *Store) Stats() (files, chunks int, err error) {
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM indexed_files`).Scan(&files); err != nil {
		return 0, 0, fmt.Errorf("count files: %w", err)
	}
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM chunks`).Scan(&chunks); err != nil {
		return 0, 0, fmt.Errorf("count chunks: %w", err)
	}
	return files, chunks, nil
}

// encodeVector serializes a float32 vector to a little-endian byte blob.
func encodeVector(v []float32) []byte {
	buf := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
	}
	return buf
}

// decodeVector deserializes a little-endian byte blob into a float32 vector.
// Trailing bytes that do not form a full float32 are ignored.
func decodeVector(b []byte) []float32 {
	n := len(b) / 4
	out := make([]float32, n)
	for i := 0; i < n; i++ {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return out
}

// norm returns the Euclidean length of v.
func norm(v []float32) float64 {
	var sum float64
	for _, f := range v {
		sum += float64(f) * float64(f)
	}
	return math.Sqrt(sum)
}

// cosinePrenorm computes cosine similarity between query (whose norm is
// precomputed as qNorm) and vec. Returns NaN when the vectors have mismatched
// lengths or vec has zero magnitude, so the caller can skip the hit.
func cosinePrenorm(query []float32, qNorm float64, vec []float32) float64 {
	if len(vec) != len(query) || qNorm == 0 {
		return math.NaN()
	}
	var dot, vSum float64
	for i := range query {
		dot += float64(query[i]) * float64(vec[i])
		vSum += float64(vec[i]) * float64(vec[i])
	}
	vNorm := math.Sqrt(vSum)
	if vNorm == 0 {
		return math.NaN()
	}
	return dot / (qNorm * vNorm)
}
