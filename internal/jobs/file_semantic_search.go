// internal/jobs/file_semantic_search.go
package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

// defaultSearchTopK is the number of hits FILE_SEMANTIC_SEARCH returns when the
// job does not specify top_k.
const defaultSearchTopK = 10

// maxSearchSnippetBytes bounds the chunk text returned per hit so a semantic
// search response stays small.
const maxSearchSnippetBytes = 500

// FileSemanticSearchHandler handles FILE_SEMANTIC_SEARCH jobs: it embeds the
// query with the node's TEI service, runs a brute-force cosine KNN over the
// node-local index, and returns the top chunk hits. This is the node half of
// aceteam#6087's federated search; the backend merges these hits with central
// pgvector hits by content_hash.
type FileSemanticSearchHandler struct {
	// WorkspaceDir is retained for symmetry with the other file handlers and to
	// resolve the default DB path; search itself does not read the workspace.
	WorkspaceDir string
	// DBPath is the node-local index database path. If empty, it is resolved from
	// CITADEL_INDEX_DB or a default beside the workspace.
	DBPath string
	// IndexesDir is the machine-convergent base dir (<node_config_dir>/indexes)
	// for per-org namespaced index DBs (aceteam#10876 C2). See
	// FileIndexHandler.IndexesDir. Empty means a payload `index` namespace cannot
	// be served (fails closed); the default (no `index`) path is unaffected.
	IndexesDir string
}

// NewFileSemanticSearchHandler creates a FileSemanticSearchHandler.
func NewFileSemanticSearchHandler(workspace, dbPath string) *FileSemanticSearchHandler {
	return &FileSemanticSearchHandler{WorkspaceDir: workspace, DBPath: dbPath}
}

// Execute embeds the query and returns the KNN hits.
//
// Payload fields:
//   - query: the search text. Required.
//   - top_k: max hits to return. Optional; defaults to defaultSearchTopK.
//   - model: TEI embedding model. Optional; must match the model the index was
//     built with for scores to be meaningful. Defaults to CITADEL_EMBEDDING_MODEL
//     or defaultEmbeddingModel.
//   - index: per-org index namespace (`org_<id>/<name>`). Optional; absent uses
//     the legacy single DB (aceteam#10876 C2).
//   - path_prefixes: optional JSON array or comma-separated list of path
//     prefixes; only chunks whose source file is under one of them are returned.
func (h *FileSemanticSearchHandler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
	query, ok := job.Payload["query"]
	if !ok || query == "" {
		return nil, fmt.Errorf("job payload missing 'query' field")
	}
	topK := atoiDefault(job.Payload["top_k"], defaultSearchTopK)
	model := job.Payload["model"]
	if model == "" {
		model = defaultEmbeddingModel
		if env := os.Getenv("CITADEL_EMBEDDING_MODEL"); env != "" {
			model = env
		}
	}
	keep, err := h.pathPrefixFilter(job.Payload["path_prefixes"])
	if err != nil {
		return nil, err
	}

	store, resolvedDB, err := openIndexStore(job.Payload["index"], h.IndexesDir, h.DBPath, h.WorkspaceDir)
	if err != nil {
		return nil, err
	}
	defer store.Close()

	ctx.Log("info", "     - [Job %s] FILE_SEMANTIC_SEARCH query=%q top_k=%d model=%q db=%q", job.ID, truncateLine(query, 80), topK, model, resolvedDB)

	op := newTEIEmbeddingOperation(teiBaseURL())
	vecs, err := embedTexts(ctx.Context(), op, model, []string{query})
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}
	if len(vecs) == 0 || len(vecs[0]) == 0 {
		return nil, fmt.Errorf("embedding service returned an empty query vector")
	}
	if err := ctx.Context().Err(); err != nil {
		return nil, fmt.Errorf("semantic search cancelled: %w", err)
	}

	// With a path_prefixes filter the KNN is filtered BEFORE the top-K trim
	// (store.SearchFiltered), so the K results are exactly the best-scoring chunks
	// under the prefixes — not an under-filled post-filter of a pre-trimmed set.
	hits, err := store.SearchFiltered(vecs[0], topK, keep)
	if err != nil {
		return nil, fmt.Errorf("index search: %w", err)
	}
	for i := range hits {
		hits[i].Text = truncateLine(hits[i].Text, maxSearchSnippetBytes)
	}

	out := map[string]any{
		"hits":  hits,
		"count": len(hits),
		"model": model,
	}
	return json.Marshal(out)
}

// pathPrefixFilter parses the optional path_prefixes payload field (a JSON array
// or comma-separated list, via the shared parsePatternField) into a keep
// predicate for store.SearchFiltered. A relative prefix is resolved against the
// handler's WorkspaceDir, because indexed chunk paths are absolute. Matching uses
// withinDir (filepath.Rel-based), so `/docs` matches `/docs/a.md` and `/docs`
// itself but never `/docs-evil`. Returns (nil, nil) when no prefixes are given
// (no filtering). An empty-after-parse value is treated as "no filter", not an
// error, so a caller sending path_prefixes="" behaves like omitting it.
func (h *FileSemanticSearchHandler) pathPrefixFilter(raw string) (func(path string) bool, error) {
	prefixes := parsePatternField(raw)
	if len(prefixes) == 0 {
		return nil, nil
	}
	abs := make([]string, 0, len(prefixes))
	for _, p := range prefixes {
		if !filepath.IsAbs(p) {
			if h.WorkspaceDir == "" {
				return nil, fmt.Errorf("relative path_prefix %q cannot be resolved without a workspace", p)
			}
			p = filepath.Join(h.WorkspaceDir, p)
		}
		abs = append(abs, filepath.Clean(p))
	}
	return func(path string) bool {
		clean := filepath.Clean(path)
		for _, prefix := range abs {
			if withinDir(prefix, clean) {
				return true
			}
		}
		return false
	}, nil
}

// embedTexts embeds inputs via the node's TEI service, waiting for readiness,
// and returns the vectors as float32 (the index's storage type). It reuses the
// same TEI call path as the EmbeddingHandler.
func embedTexts(ctx context.Context, op *teiEmbeddingOperation, model string, inputs []string) ([][]float32, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	res, err := op.embed(ctx, &EmbeddingRequest{Model: model, Input: inputs})
	if err != nil {
		return nil, err
	}
	if len(res.Embeddings) != len(inputs) {
		return nil, fmt.Errorf("embedding service returned %d vectors for %d inputs", len(res.Embeddings), len(inputs))
	}
	out := make([][]float32, len(res.Embeddings))
	for i, v := range res.Embeddings {
		f32 := make([]float32, len(v))
		for j, f := range v {
			if math.IsNaN(f) || math.IsInf(f, 0) || f > math.MaxFloat32 || f < -math.MaxFloat32 {
				return nil, fmt.Errorf("embedding service returned non-float32 value at vector %d dimension %d", i, j)
			}
			f32[j] = float32(f)
		}
		out[i] = f32
	}
	return out, nil
}

// Ensure FileSemanticSearchHandler implements JobHandler.
var _ JobHandler = (*FileSemanticSearchHandler)(nil)
