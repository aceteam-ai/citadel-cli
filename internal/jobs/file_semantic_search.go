// internal/jobs/file_semantic_search.go
package jobs

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/nodeindex"
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

	store, err := nodeindex.Open(resolveIndexDBPath(h.DBPath, h.WorkspaceDir))
	if err != nil {
		return nil, fmt.Errorf("open node index: %w", err)
	}
	defer store.Close()

	ctx.Log("info", "     - [Job %s] FILE_SEMANTIC_SEARCH query=%q top_k=%d model=%q", job.ID, truncateLine(query, 80), topK, model)

	vecs, err := embedTexts(model, []string{query})
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}
	if len(vecs) == 0 || len(vecs[0]) == 0 {
		return nil, fmt.Errorf("embedding service returned an empty query vector")
	}

	hits, err := store.Search(vecs[0], topK)
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

// embedTexts embeds inputs via the node's TEI service, waiting for readiness,
// and returns the vectors as float32 (the index's storage type). It reuses the
// same TEI call path as the EmbeddingHandler.
func embedTexts(model string, inputs []string) ([][]float32, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	base := teiBaseURL()
	if err := waitForTEIReady(base, teiReadyTimeout); err != nil {
		return nil, err
	}
	res, err := callTEIEmbeddings(base, &EmbeddingRequest{Model: model, Input: inputs})
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
			f32[j] = float32(f)
		}
		out[i] = f32
	}
	return out, nil
}

// Ensure FileSemanticSearchHandler implements JobHandler.
var _ JobHandler = (*FileSemanticSearchHandler)(nil)
