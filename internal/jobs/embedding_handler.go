// internal/jobs/embedding_handler.go
package jobs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

// teiDefaultURL is the local TEI (HF Text-Embeddings-Inference) service address.
// TEI serves an OpenAI-compatible /v1/embeddings endpoint (and a native /embed)
// on host port 8102 (see services/compose/tei.yml from #343 PR A). Override via
// the CITADEL_TEI_URL environment variable for non-default deployments.
const teiDefaultURL = "http://localhost:8102"

// teiReadyTimeout bounds how long the handler waits for TEI to report healthy
// before giving up. Mirrors the vLLM/SGLang readiness budget in
// llm_inference.go.
const teiReadyTimeout = 60 * time.Second

// EmbeddingHandler handles JobTypeEmbedding ("embedding") jobs by routing them
// to the local TEI service's OpenAI-compatible /v1/embeddings endpoint.
//
// It implements the legacy jobs.JobHandler interface (Execute(JobContext,
// *nexus.Job) ([]byte, error)) so it can be wired into the live worker dispatch
// via worker.CreateLegacyHandlersWithOpts, exactly like VLLMInferenceHandler.
//
// IMPORTANT — payload contract: the live dispatch path
// (worker.LegacyHandlerAdapter) flattens the job payload map[string]any into
// map[string]string via fmt.Sprint before the handler sees it. A raw []string
// would be stringified to "[a b]" and become unrecoverable. The handler
// therefore expects `input` to arrive as a JSON-encoded array string, e.g.
// `["hello","world"]`, which it json.Unmarshals. A bare scalar string is also
// accepted and treated as a single-element input for convenience.
//
// This differs from llm_inference.go's structure-preserving signature; that
// handler is not wired into the live worker path (see PR notes). We mirror its
// TEI-call style while conforming to the interface the live dispatch actually
// uses.
type EmbeddingHandler struct{}

// EmbeddingRequest is the parsed embedding job payload.
type EmbeddingRequest struct {
	// Model is the embedding model identifier (e.g. "gte-multilingual-base").
	Model string
	// Input is the list of texts to embed.
	Input []string
	// Dimensions optionally requests Matryoshka-truncated output dimensions.
	// Zero means "use the model's native dimensionality".
	Dimensions int
}

// teiEmbeddingResponse mirrors the OpenAI-compatible response TEI returns from
// /v1/embeddings. Note: unlike chat/completions, embeddings usage carries only
// prompt_tokens and total_tokens (no completion_tokens).
type teiEmbeddingResponse struct {
	Object string `json:"object"`
	Data   []struct {
		Object    string    `json:"object"`
		Embedding []float64 `json:"embedding"`
		Index     int       `json:"index"`
	} `json:"data"`
	Model string `json:"model"`
	Usage struct {
		PromptTokens int `json:"prompt_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
}

// EmbeddingResult is the handler's output (JSON-serialized into the job result).
type EmbeddingResult struct {
	Model      string      `json:"model"`
	Embeddings [][]float64 `json:"embeddings"`
	Dimensions int         `json:"dimensions"`
	Usage      struct {
		PromptTokens int `json:"prompt_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
}

// teiBaseURL returns the TEI service base URL, honoring CITADEL_TEI_URL.
func teiBaseURL() string {
	if v := os.Getenv("CITADEL_TEI_URL"); v != "" {
		return v
	}
	return teiDefaultURL
}

// Execute parses the embedding job payload, waits for TEI readiness, calls the
// OpenAI-compatible /v1/embeddings endpoint, and returns the embeddings + usage
// as a JSON blob.
func (h *EmbeddingHandler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
	req, err := parseEmbeddingPayload(job.Payload)
	if err != nil {
		return nil, fmt.Errorf("invalid embedding payload: %w", err)
	}

	ctx.Log("info", "     - [Job %s] Waiting for TEI embedding service to become ready...", job.ID)
	if err := waitForTEIReady(teiBaseURL(), teiReadyTimeout); err != nil {
		return nil, err
	}
	ctx.Log("info", "     - [Job %s] TEI ready. Embedding %d text(s) with model %q", job.ID, len(req.Input), req.Model)

	result, err := callTEIEmbeddings(teiBaseURL(), req)
	if err != nil {
		return nil, err
	}

	out, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal embedding result: %w", err)
	}
	return out, nil
}

// parseEmbeddingPayload extracts and validates the embedding request from the
// flattened (map[string]string) job payload produced by the legacy adapter.
//
// `input` may be either a JSON-encoded array (e.g. `["a","b"]`) or a bare
// scalar string (treated as a single input). `model` is required. `dimensions`
// is optional.
func parseEmbeddingPayload(payload map[string]string) (*EmbeddingRequest, error) {
	model := payload["model"]
	if model == "" {
		return nil, fmt.Errorf("model is required")
	}

	rawInput, ok := payload["input"]
	if !ok || rawInput == "" {
		return nil, fmt.Errorf("input is required")
	}

	var input []string
	// Prefer JSON array decoding (the canonical wire form for []string).
	if err := json.Unmarshal([]byte(rawInput), &input); err != nil {
		// Fall back: treat the whole value as a single text to embed.
		input = []string{rawInput}
	}
	if len(input) == 0 {
		return nil, fmt.Errorf("input must contain at least one text")
	}

	req := &EmbeddingRequest{
		Model: model,
		Input: input,
	}

	if dimStr, ok := payload["dimensions"]; ok && dimStr != "" {
		dim, err := strconv.Atoi(dimStr)
		if err != nil {
			return nil, fmt.Errorf("dimensions must be an integer: %w", err)
		}
		if dim < 0 {
			return nil, fmt.Errorf("dimensions must be non-negative")
		}
		req.Dimensions = dim
	}

	return req, nil
}

// callTEIEmbeddings POSTs the embedding request to TEI's /v1/embeddings and
// returns the parsed result.
func callTEIEmbeddings(baseURL string, req *EmbeddingRequest) (*EmbeddingResult, error) {
	reqPayload := map[string]any{
		"model": req.Model,
		"input": req.Input,
	}
	// Matryoshka: forward `dimensions` only when explicitly requested, otherwise
	// let TEI return the model's native dimensionality.
	if req.Dimensions > 0 {
		reqPayload["dimensions"] = req.Dimensions
	}

	reqBody, err := json.Marshal(reqPayload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal TEI request: %w", err)
	}

	resp, err := http.Post(baseURL+"/v1/embeddings", "application/json", bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, fmt.Errorf("failed to connect to TEI service: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("TEI returned status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var teiResp teiEmbeddingResponse
	if err := json.Unmarshal(bodyBytes, &teiResp); err != nil {
		return nil, fmt.Errorf("failed to parse TEI response: %w", err)
	}
	if len(teiResp.Data) == 0 {
		return nil, fmt.Errorf("TEI returned no embeddings")
	}

	result := &EmbeddingResult{
		Model:      teiResp.Model,
		Embeddings: make([][]float64, len(teiResp.Data)),
	}
	if result.Model == "" {
		result.Model = req.Model
	}
	for _, d := range teiResp.Data {
		// TEI returns data entries with explicit indices; place each vector at
		// its index so output order matches the input order regardless of how
		// the engine ordered the response.
		if d.Index < 0 || d.Index >= len(result.Embeddings) {
			return nil, fmt.Errorf("TEI returned out-of-range embedding index %d", d.Index)
		}
		result.Embeddings[d.Index] = d.Embedding
	}
	if len(result.Embeddings) > 0 {
		result.Dimensions = len(result.Embeddings[0])
	}
	result.Usage.PromptTokens = teiResp.Usage.PromptTokens
	result.Usage.TotalTokens = teiResp.Usage.TotalTokens

	return result, nil
}

// waitForTEIReady polls TEI's /health endpoint until it reports ready or the
// timeout elapses. Mirrors waitForVLLMReady in llm_inference.go.
func waitForTEIReady(baseURL string, timeout time.Duration) error {
	healthURL := baseURL + "/health"
	pollInterval := 1 * time.Second
	startTime := time.Now()

	for time.Since(startTime) < timeout {
		resp, err := http.Get(healthURL)
		if err == nil && resp.StatusCode == http.StatusOK {
			resp.Body.Close()
			return nil
		}
		if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(pollInterval)
	}
	return fmt.Errorf("TEI service did not become ready within %v", timeout)
}
