// Package rag exposes the node-local semantic index (aceteam#6087 /
// citadel-cli#589) as an on-box capability: index the files a Citadel node holds
// and query them locally, entirely on the node, with a self-hosted embedding
// model (the local TEI service, gte-multilingual-base, :8102).
//
// It is deliberately thin. The chunking, incremental content-hash indexing, TEI
// embedding, and brute-force cosine KNN already exist as the Redis-dispatched
// FILE_INDEX / FILE_SEMANTIC_SEARCH job handlers in internal/jobs, over the
// internal/nodeindex SQLite store. Those were only reachable via job dispatch
// from the backend; this package drives the SAME handlers in-process so a node
// (its CLI, its control-listener HTTP endpoints) can index/query/inspect its own
// index without a backend round-trip — and without duplicating the embed/chunk
// logic. There is a single source of truth for how a node embeds and stores.
package rag

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/nodeindex"
)

// Service drives node-local indexing and search over a single node-local index.
// The zero value is not usable; construct with New.
type Service struct {
	// workspaceDir roots path validation for indexing.
	workspaceDir string
	// dbPath is the resolved node-local index database path. It is resolved once
	// (via jobs.ResolveIndexDBPath) and passed to every handler so the CLI, the
	// HTTP endpoints, and a running worker all read/write the same index.db.
	dbPath string
	// model is the resolved embedding model (never empty after New), used for
	// provenance and passed to the handlers so search matches the index.
	model string
	// allowOutsideWorkspace lets Index walk a docs directory that lives outside
	// the sandbox workspace. It is opt-in per call site and defaults OFF: the
	// mesh-reachable HTTP surface keeps the same stricter workspace boundary the
	// FILE_INDEX dispatch handler enforces, so a remote caller cannot index
	// arbitrary node paths. The local `citadel rag` operator (who already has
	// shell access) opts in so pointing at a docs dir outside the workspace works.
	allowOutsideWorkspace bool
}

// New constructs a Service with the mesh-safe default (index paths confined to
// the workspace). workspaceDir is the node's workspace root; modelOverride is
// optional (empty => the same default the job handlers use). The index db path
// is resolved from CITADEL_INDEX_DB or the default beside the workspace,
// matching a running worker.
func New(workspaceDir, modelOverride string) *Service {
	return &Service{
		workspaceDir: workspaceDir,
		dbPath:       jobs.ResolveIndexDBPath("", workspaceDir),
		model:        jobs.ResolveEmbeddingModel(modelOverride),
	}
}

// NewLocal is New for a trusted local operator (the `citadel rag` CLI): it
// additionally permits indexing paths outside the workspace. Never use this for
// a network-reachable surface.
func NewLocal(workspaceDir, modelOverride string) *Service {
	s := New(workspaceDir, modelOverride)
	s.allowOutsideWorkspace = true
	return s
}

// Model returns the effective embedding model (for provenance reporting).
func (s *Service) Model() string { return s.model }

// DBPath returns the resolved node-local index database path.
func (s *Service) DBPath() string { return s.dbPath }

// Provenance is the local-origin string surfaced to users so a hit is clearly
// attributable to this node and this model, e.g.
// "indexed on this node via gte-multilingual-base".
func (s *Service) Provenance() string {
	return "indexed on this node via " + s.model
}

// IndexResult is the outcome of an Index call (mirrors the FILE_INDEX handler
// output, decoded into a typed struct).
type IndexResult struct {
	FilesIndexed   int    `json:"files_indexed"`
	FilesSkipped   int    `json:"files_skipped"`
	FilesRemoved   int    `json:"files_removed"`
	ChunksUpserted int    `json:"chunks_upserted"`
	ChunksEmbedded int    `json:"chunks_embedded"`
	Model          string `json:"model"`
	Dim            int    `json:"dim"`
}

// Index (re)indexes the files under path (a directory or single file),
// incrementally: unchanged files are skipped by content hash and files deleted
// on disk are pruned. filePattern optionally restricts filenames (e.g. "*.md").
// It requires the local TEI embedding service to be reachable; a clear error is
// returned when it is not.
func (s *Service) Index(ctx context.Context, path, filePattern string) (IndexResult, error) {
	if path == "" {
		return IndexResult{}, fmt.Errorf("index path is required")
	}
	h := jobs.NewFileIndexHandler(s.workspaceDir, s.dbPath)
	h.AllowOutsideWorkspace = s.allowOutsideWorkspace
	payload := map[string]string{"path": path, "model": s.model}
	if filePattern != "" {
		payload["file_pattern"] = filePattern
	}
	out, err := h.Execute(jobCtx(ctx), &nexus.Job{ID: "rag-index", Type: "FILE_INDEX", Payload: payload})
	if err != nil {
		return IndexResult{}, err
	}
	var res IndexResult
	if err := json.Unmarshal(out, &res); err != nil {
		return IndexResult{}, fmt.Errorf("decode index result: %w", err)
	}
	return res, nil
}

// Hit is one semantic-search result with its local provenance.
type Hit struct {
	Path       string  `json:"path"`
	ChunkIndex int     `json:"chunk_index"`
	Text       string  `json:"text"`
	Score      float64 `json:"score"`
}

// QueryResult is the outcome of a Query call.
type QueryResult struct {
	Hits       []Hit  `json:"hits"`
	Count      int    `json:"count"`
	Model      string `json:"model"`
	Provenance string `json:"provenance"`
}

// Query embeds the query with the local TEI service and returns the top-k
// cosine-nearest chunks from the node-local index. topK <= 0 uses the handler
// default.
func (s *Service) Query(ctx context.Context, query string, topK int) (QueryResult, error) {
	if query == "" {
		return QueryResult{}, fmt.Errorf("query is required")
	}
	h := jobs.NewFileSemanticSearchHandler(s.workspaceDir, s.dbPath)
	payload := map[string]string{"query": query, "model": s.model}
	if topK > 0 {
		payload["top_k"] = fmt.Sprintf("%d", topK)
	}
	out, err := h.Execute(jobCtx(ctx), &nexus.Job{ID: "rag-query", Type: "FILE_SEMANTIC_SEARCH", Payload: payload})
	if err != nil {
		return QueryResult{}, err
	}
	// The handler emits {hits, count, model}; decode and attach provenance.
	var raw struct {
		Hits  []Hit  `json:"hits"`
		Count int    `json:"count"`
		Model string `json:"model"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return QueryResult{}, fmt.Errorf("decode query result: %w", err)
	}
	return QueryResult{
		Hits:       raw.Hits,
		Count:      raw.Count,
		Model:      raw.Model,
		Provenance: s.Provenance(),
	}, nil
}

// Status reports the node-local index summary plus local provenance. It reads
// the index directly (no embedding), so it works even when TEI is down.
type Status struct {
	nodeindex.Status
	DBPath     string `json:"db_path"`
	Provenance string `json:"provenance"`
}

// Status opens the node-local index and returns its summary. An index that has
// never been built yields zero counts (not an error).
func (s *Service) Status() (Status, error) {
	store, err := nodeindex.Open(s.dbPath)
	if err != nil {
		return Status{}, fmt.Errorf("open node index: %w", err)
	}
	defer store.Close()
	st, err := store.Status()
	if err != nil {
		return Status{}, err
	}
	// Report the effective model even before anything is indexed, so `status`
	// on a fresh node still tells the operator which model queries will use.
	if st.Model == "" {
		st.Model = s.model
	}
	return Status{Status: st, DBPath: s.dbPath, Provenance: "indexed on this node via " + st.Model}, nil
}

// jobCtx builds the minimal jobs.JobContext the handlers need, threading the
// caller's context for cancellation and discarding handler log lines (the local
// surfaces do their own user-facing output).
func jobCtx(ctx context.Context) jobs.JobContext {
	return jobs.JobContext{Ctx: ctx, LogFn: func(string, string) {}}
}
