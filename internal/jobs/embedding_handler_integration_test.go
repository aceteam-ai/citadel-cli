//go:build integration

// Live-TEI integration test for the embedding handler (issue #351, code merged
// in #357). Unlike embedding_handler_test.go, which mocks TEI with httptest,
// this exercises EmbeddingHandler.Execute against a REAL running Text-
// Embeddings-Inference container (gte-multilingual-base, 768-d), proving the
// handler<->live-TEI leg end to end.
//
// Build-tagged `integration` so it never runs in normal CI. It is also gated to
// t.Skip when the TEI service is unreachable, so a developer without TEI up does
// not see spurious failures.
//
// Run it with TEI listening on the URL given by CITADEL_TEI_URL, e.g.:
//
//	CITADEL_TEI_URL=http://localhost:8085 \
//	  go test -tags integration -run TestEmbeddingHandlerLiveTEI ./internal/jobs/ -v
package jobs

import (
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

// liveTEIURL resolves the TEI base URL the handler will use, honoring
// CITADEL_TEI_URL (the same override teiBaseURL() reads) and falling back to the
// local container's port.
func liveTEIURL() string {
	if v := os.Getenv("CITADEL_TEI_URL"); v != "" {
		return v
	}
	return "http://localhost:8085"
}

// requireLiveTEI skips the test unless a real TEI /health responds 200 quickly.
// It uses a short timeout so an absent service yields an immediate t.Skip rather
// than the handler's 60s readiness poll.
func requireLiveTEI(t *testing.T) string {
	t.Helper()
	base := liveTEIURL()
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(base + "/health")
	if err != nil {
		t.Skipf("live TEI not reachable at %s (%v); skipping integration test", base, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Skipf("live TEI health at %s returned %d; skipping integration test", base, resp.StatusCode)
	}
	return base
}

// TestEmbeddingHandlerLiveTEI drives EmbeddingHandler.Execute against a real TEI
// service. It covers the canonical JSON-array input path (incl. multilingual
// text) and the scalar-string fallback path.
func TestEmbeddingHandlerLiveTEI(t *testing.T) {
	base := requireLiveTEI(t)

	// Point the handler at the live service. t.Setenv auto-restores on cleanup
	// (and forbids t.Parallel, which we intentionally do not use).
	t.Setenv("CITADEL_TEI_URL", base)

	t.Run("json_array_multilingual", func(t *testing.T) {
		job := &nexus.Job{
			ID:   "live-embed-array",
			Type: "embedding",
			Payload: map[string]string{
				"model": "gte",
				// `input` arrives as a JSON-array string, matching the flattened
				// map[string]string contract of the live dispatch path.
				"input": `["hello world","你好世界"]`,
			},
		}

		out, err := (&EmbeddingHandler{}).Execute(JobContext{}, job)
		if err != nil {
			t.Fatalf("Execute against live TEI failed: %v", err)
		}

		var res EmbeddingResult
		if err := json.Unmarshal(out, &res); err != nil {
			t.Fatalf("result did not unmarshal to EmbeddingResult: %v\nraw: %s", err, string(out))
		}

		if len(res.Embeddings) != 2 {
			t.Fatalf("got %d embeddings, want 2", len(res.Embeddings))
		}
		// gte-multilingual-base is 768-d; the task pins this dimensionality.
		const wantDim = 768
		if res.Dimensions != wantDim {
			t.Errorf("Dimensions = %d, want %d", res.Dimensions, wantDim)
		}
		for i, emb := range res.Embeddings {
			if len(emb) == 0 {
				t.Errorf("embedding[%d] is empty", i)
				continue
			}
			if len(emb) != wantDim {
				t.Errorf("embedding[%d] has dim %d, want %d", i, len(emb), wantDim)
			}
		}
		if res.Usage.TotalTokens <= 0 {
			t.Errorf("Usage.TotalTokens = %d, want > 0", res.Usage.TotalTokens)
		}
		if res.Usage.PromptTokens <= 0 {
			t.Errorf("Usage.PromptTokens = %d, want > 0", res.Usage.PromptTokens)
		}

		t.Logf("live TEI: model=%q embeddings=%d dim=%d usage{prompt=%d total=%d} emb0[0:3]=%v",
			res.Model, len(res.Embeddings), res.Dimensions,
			res.Usage.PromptTokens, res.Usage.TotalTokens, head3(res.Embeddings[0]))
	})

	t.Run("scalar_fallback", func(t *testing.T) {
		job := &nexus.Job{
			ID:   "live-embed-scalar",
			Type: "embedding",
			Payload: map[string]string{
				"model": "gte",
				// A bare, non-JSON string: json.Unmarshal fails and the handler
				// falls back to a single-element input. Proves the scalar path
				// against live TEI.
				"input": "hello world",
			},
		}

		out, err := (&EmbeddingHandler{}).Execute(JobContext{}, job)
		if err != nil {
			t.Fatalf("Execute (scalar input) against live TEI failed: %v", err)
		}

		var res EmbeddingResult
		if err := json.Unmarshal(out, &res); err != nil {
			t.Fatalf("result did not unmarshal to EmbeddingResult: %v\nraw: %s", err, string(out))
		}

		if len(res.Embeddings) != 1 {
			t.Fatalf("got %d embeddings, want 1 (scalar fallback)", len(res.Embeddings))
		}
		if len(res.Embeddings[0]) == 0 {
			t.Fatalf("scalar embedding is empty")
		}
		if res.Dimensions != len(res.Embeddings[0]) {
			t.Errorf("Dimensions = %d, but embedding has %d values", res.Dimensions, len(res.Embeddings[0]))
		}
		if res.Usage.TotalTokens <= 0 {
			t.Errorf("Usage.TotalTokens = %d, want > 0", res.Usage.TotalTokens)
		}

		t.Logf("live TEI (scalar): model=%q embeddings=%d dim=%d usage{prompt=%d total=%d}",
			res.Model, len(res.Embeddings), res.Dimensions,
			res.Usage.PromptTokens, res.Usage.TotalTokens)
	})
}

// head3 returns up to the first three values of v, for compact logging.
func head3(v []float64) []float64 {
	if len(v) > 3 {
		return v[:3]
	}
	return v
}
