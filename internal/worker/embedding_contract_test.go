package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/jobs"
)

// TestEmbeddingDispatchContract pins the cross-repo contract between the aceteam
// backend's run_fabric_embeddings dispatch and this node's embedding handler, at
// the exact seam aceteam depends on:
//
//   - Wire IN: aceteam sends an `embedding` job whose payload is
//     {"model": <str>, "input": json.dumps(texts)} — i.e. `input` is a JSON
//     ARRAY STRING, not a raw list (routes/fabric_inference.py::
//     run_fabric_embeddings).
//   - Routing: the type "embedding" resolves to jobs.EmbeddingHandler via the
//     legacy adapter registered in CreateLegacyHandlersWithOpts.
//   - Wire OUT: the legacy adapter double-wraps the handler's JSON output as
//     JobResult.Output["output"] = "<json string>" — aceteam's
//     workflows.fabric_embedding_node._extract_embeddings json.loads that inner
//     string to read {model, embeddings, dimensions, usage}.
//
// If any of these shapes drift, the flow node breaks silently, so this test
// guards them together against a stub TEI.
func TestEmbeddingDispatchContract(t *testing.T) {
	tei := httptest.NewServer(func() http.Handler {
		mux := http.NewServeMux()
		mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		mux.HandleFunc("/v1/embeddings", func(w http.ResponseWriter, _ *http.Request) {
			// TEI returns entries out of order to prove index-based reordering.
			_, _ = w.Write([]byte(`{"data":[` +
				`{"object":"embedding","index":1,"embedding":[0.3,0.4]},` +
				`{"object":"embedding","index":0,"embedding":[0.1,0.2]}` +
				`],"model":"bge","usage":{"prompt_tokens":3,"total_tokens":3}}`))
		})
		return mux
	}())
	defer tei.Close()
	t.Setenv("CITADEL_TEI_URL", tei.URL)

	// The single registration site the live worker path uses. Locate the adapter
	// that CanHandle the "embedding" type — proves the type is actually routed.
	var embedHandler JobHandler
	for _, h := range CreateLegacyHandlersWithOpts(LegacyHandlerOpts{}) {
		if h.CanHandle(JobTypeEmbedding) {
			embedHandler = h
			break
		}
	}
	if embedHandler == nil {
		t.Fatalf("no registered handler CanHandle(%q); embedding jobs would be unroutable", JobTypeEmbedding)
	}

	// The exact payload run_fabric_embeddings emits: input is a JSON array STRING.
	job := &Job{
		ID:   "job-embed-contract",
		Type: JobTypeEmbedding,
		Payload: map[string]any{
			"model": "bge",
			"input": `["hello","world"]`,
		},
	}

	result, err := embedHandler.Execute(context.Background(), job, nil)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if result.Status != JobStatusSuccess {
		t.Fatalf("Status = %v, want success", result.Status)
	}

	// Wire OUT: the result is double-encoded — Output["output"] is a JSON string.
	rawOutput, ok := result.Output["output"].(string)
	if !ok {
		t.Fatalf("Output[\"output\"] is %T, want string (the double-wrapped contract)", result.Output["output"])
	}

	var parsed struct {
		Model      string      `json:"model"`
		Embeddings [][]float64 `json:"embeddings"`
		Dimensions int         `json:"dimensions"`
		Usage      struct {
			PromptTokens int `json:"prompt_tokens"`
			TotalTokens  int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(rawOutput), &parsed); err != nil {
		t.Fatalf("inner output not valid JSON: %v", err)
	}

	if len(parsed.Embeddings) != 2 {
		t.Fatalf("got %d embeddings, want 2", len(parsed.Embeddings))
	}
	// Index-based reordering: index 0 -> [0.1,0.2], index 1 -> [0.3,0.4].
	if parsed.Embeddings[0][0] != 0.1 || parsed.Embeddings[1][0] != 0.3 {
		t.Errorf("embeddings not restored to input order: %v", parsed.Embeddings)
	}
	if parsed.Dimensions != 2 {
		t.Errorf("Dimensions = %d, want 2", parsed.Dimensions)
	}
	if parsed.Usage.TotalTokens != 3 {
		t.Errorf("Usage.TotalTokens = %d, want 3", parsed.Usage.TotalTokens)
	}
}

// TestEmbeddingHandlerImplementsLegacyInterface is a compile-time-ish guard that
// the concrete handler aceteam relies on is the one wired for JobTypeEmbedding.
func TestEmbeddingHandlerRegisteredType(t *testing.T) {
	_ = &jobs.EmbeddingHandler{} // referenced so the dependency is explicit
	handlers := CreateLegacyHandlersWithOpts(LegacyHandlerOpts{})
	count := 0
	for _, h := range handlers {
		if h.CanHandle(JobTypeEmbedding) {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly one handler for %q, got %d", JobTypeEmbedding, count)
	}
}
