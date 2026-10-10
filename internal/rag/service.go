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
	"os"
	"path/filepath"

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
	// roots, when rootsMode is set, replaces the single-workspace boundary with
	// the authorized-roots allowlist (citadel-cli#617-619): Index validates the
	// target against jobs.ValidateWithinRoots, and Query FILTERS returned hits to
	// those under an authorized root so a de-authorized root's stale chunks never
	// leak. This mode powers the LOCAL `citadel search` / TUI / watcher surfaces;
	// the mesh HTTP surface deliberately keeps the stricter workspace confinement
	// (cmd/work.go constructs the Service with New(), not NewWithRoots()).
	roots     []string
	rootsMode bool
	// onProgress, when set, receives throttled progress updates during Index
	// (aceteam#10876 C1). nil for surfaces that do not render progress.
	onProgress func(map[string]any)
}

// SetProgressSink wires a throttled progress callback consumed by Index. The
// callback receives the final progress event map (stage/done/total/percent/
// current/rate/eta_seconds/counts). Optional; nil means no progress reporting.
func (s *Service) SetProgressSink(fn func(map[string]any)) { s.onProgress = fn }

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

// NewWithRoots constructs a Service whose index/search boundary is the
// authorized-roots allowlist rather than a single workspace (citadel-cli#617-619).
// It powers the local `citadel search` command, the TUI Search page, and the
// file watcher. workspaceForDB is used ONLY to locate the node-local index.db
// (the same file a running worker's FILE_INDEX writes) — NOT for authorization;
// authorization is the roots allowlist. Never expose this over the mesh.
func NewWithRoots(roots []string, workspaceForDB, modelOverride string) *Service {
	return &Service{
		workspaceDir:          workspaceForDB,
		dbPath:                jobs.ResolveIndexDBPath("", workspaceForDB),
		model:                 jobs.ResolveEmbeddingModel(modelOverride),
		allowOutsideWorkspace: true, // roots validation replaces the workspace check
		roots:                 roots,
		rootsMode:             true,
	}
}

// Roots returns the authorized roots this Service enforces (nil when not in
// roots mode).
func (s *Service) Roots() []string { return s.roots }

// WithIndexNamespace routes this Service at a per-org index namespace
// (`org_<id>/<name>`) under indexesDir instead of the default single DB
// (aceteam#10876 C2). It resolves and confines the namespace path
// (jobs.ResolveIndexNamespaceDBPath), creates the parent dir, and overrides the
// resolved dbPath so every subsequent Index/Query/Status call reads and writes
// that namespace's DB. indexesDir is resolved by the caller (cmd) from
// network.GetNodeConfigDir(); this package never resolves it itself. It is the
// local-surface analogue of the FILE_INDEX / FILE_SEMANTIC_SEARCH `index`
// payload field and applies the identical validation + path confinement.
func (s *Service) WithIndexNamespace(indexesDir, ns string) error {
	dbPath, err := jobs.ResolveIndexNamespaceDBPath(indexesDir, ns)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		return fmt.Errorf("create index namespace dir: %w", err)
	}
	s.dbPath = dbPath
	return nil
}

// NodeWorkspaceDir resolves the node's workspace directory the same way the
// worker's resolveWorkspaceDir does (CITADEL_WORKSPACE, else
// ~/citadel-node/workspace) WITHOUT the --workspace flag or directory creation.
// It exists so surfaces that cannot import the cmd package (the TUI Search page)
// can locate the SAME index.db a running worker uses. Returns "" only when the
// home directory cannot be resolved and no env override is set.
func NodeWorkspaceDir() string {
	if dir := os.Getenv("CITADEL_WORKSPACE"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "citadel-node", "workspace")
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
	FilesSeen    int `json:"files_seen"`
	FilesIndexed int `json:"files_indexed"`
	FilesSkipped int `json:"files_skipped"`
	FilesFailed  int `json:"files_failed"`
	FilesRemoved int `json:"files_removed"`
	// Large-file / chunk-cap accounting (aceteam#10876 C2): FilesSkippedTooLarge
	// = large files that could not be indexed at all (binary/empty head);
	// FilesTruncated = large text files whose head WAS indexed; FilesChunkCapped
	// = files that hit the per-file chunk cap. All reported, never silent.
	FilesSkippedTooLarge int    `json:"files_skipped_too_large"`
	FilesTruncated       int    `json:"files_truncated"`
	FilesChunkCapped     int    `json:"files_chunk_capped"`
	ChunksUpserted       int    `json:"chunks_upserted"`
	ChunksEmbedded       int    `json:"chunks_embedded"`
	Model                string `json:"model"`
	Dim                  int    `json:"dim"`
	// Status is "completed" on a clean walk or "cancelled" when a cooperative
	// cancel stopped it; Checkpoint is "complete" or "partial" (aceteam#10876 C1).
	Status     string `json:"status"`
	Checkpoint string `json:"checkpoint"`
}

// Index (re)indexes the files under path (a directory or single file),
// incrementally: unchanged files are skipped by content hash and files deleted
// on disk are pruned. filePattern optionally restricts filenames (e.g. "*.md").
// Per-file embedding failures against a ready local TEI service are counted in
// FilesFailed while the walk continues. Operation-wide readiness failure,
// cancellation, root-walk failures, and index-storage failures remain fatal.
func (s *Service) Index(ctx context.Context, path, filePattern string) (IndexResult, error) {
	return s.index(ctx, path, filePattern, nil)
}

// IndexCooperatively requests a graceful stop at the next file boundary when
// cancelRequested becomes true. ctx remains the independent hard authority.
func (s *Service) IndexCooperatively(ctx context.Context, path, filePattern string, cancelRequested func() bool) (IndexResult, error) {
	return s.index(ctx, path, filePattern, cancelRequested)
}

func (s *Service) index(ctx context.Context, path, filePattern string, cancelRequested func() bool) (IndexResult, error) {
	if path == "" {
		return IndexResult{}, fmt.Errorf("index path is required")
	}
	// In roots mode the authorized-roots allowlist is the boundary: reject any
	// path that does not resolve under an authorized root, then hand the handler
	// the validated absolute path (allowOutsideWorkspace is already true).
	if s.rootsMode {
		validated, err := jobs.ValidateWithinRoots(s.roots, path)
		if err != nil {
			return IndexResult{}, err
		}
		path = validated
	}
	h := jobs.NewFileIndexHandler(s.workspaceDir, s.dbPath)
	h.AllowOutsideWorkspace = s.allowOutsideWorkspace
	payload := map[string]string{"path": path, "model": s.model}
	if filePattern != "" {
		payload["file_pattern"] = filePattern
	}

	jc := jobs.JobContext{
		Ctx:             ctx,
		LogFn:           func(string, string) {},
		CancelRequested: cancelRequested,
		Progress:        jobs.NewThrottledProgress(s.onProgress, nil),
	}
	out, err := h.Execute(jc, &nexus.Job{ID: "rag-index", Type: "FILE_INDEX", Payload: payload})
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
	return s.QueryWithPrefixes(ctx, query, topK, nil)
}

// QueryWithPrefixes is Query restricted to chunks whose source file is under one
// of pathPrefixes (aceteam#10876 C2's path_prefixes filter). A relative prefix is
// resolved against the workspace by the handler. nil/empty prefixes behave
// exactly like Query.
func (s *Service) QueryWithPrefixes(ctx context.Context, query string, topK int, pathPrefixes []string) (QueryResult, error) {
	if query == "" {
		return QueryResult{}, fmt.Errorf("query is required")
	}
	h := jobs.NewFileSemanticSearchHandler(s.workspaceDir, s.dbPath)
	payload := map[string]string{"query": query, "model": s.model}
	if len(pathPrefixes) > 0 {
		encoded, err := json.Marshal(pathPrefixes)
		if err != nil {
			return QueryResult{}, fmt.Errorf("encode path_prefixes: %w", err)
		}
		payload["path_prefixes"] = string(encoded)
	}
	// In roots mode we filter returned hits to authorized roots, so over-fetch to
	// compensate for hits dropped by the filter (result count may still be < topK
	// when many hits fall outside the roots).
	fetchK := topK
	if s.rootsMode && topK > 0 {
		fetchK = topK*4 + 20
	}
	if fetchK > 0 {
		payload["top_k"] = fmt.Sprintf("%d", fetchK)
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
	hits := raw.Hits
	if s.rootsMode {
		hits = s.filterHitsToRoots(hits, topK)
	}
	return QueryResult{
		Hits:       hits,
		Count:      len(hits),
		Model:      raw.Model,
		Provenance: s.Provenance(),
	}, nil
}

// filterHitsToRoots drops any hit whose path does not resolve under an
// authorized root and trims to topK. This keeps a de-authorized root's stale
// index chunks from leaking through search: even though they remain in the DB
// until pruned, the roots-mode surface never returns them.
func (s *Service) filterHitsToRoots(hits []Hit, topK int) []Hit {
	out := make([]Hit, 0, len(hits))
	for _, h := range hits {
		if _, err := jobs.ValidateWithinRoots(s.roots, h.Path); err != nil {
			continue
		}
		out = append(out, h)
		if topK > 0 && len(out) >= topK {
			break
		}
	}
	return out
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
