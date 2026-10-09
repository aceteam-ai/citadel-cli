// internal/jobs/embedding_handler.go
package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"sync"
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
// llm_inference.go. A var (not a const) so tests can shrink it rather than
// wait the full budget for the not-ready path.
var teiReadyTimeout = 60 * time.Second

// teiDefaultMaxClientBatchSize is the per-request input cap assumed when TEI's
// GET /info is unavailable or omits max_client_batch_size. It matches the stock
// TEI module's default (see services/compose/tei.yml). TEI rejects any request
// whose input array exceeds its max_client_batch_size with HTTP 413, so callers
// must sub-batch to this limit (see callTEIEmbeddings).
const teiDefaultMaxClientBatchSize = 32

// teiInfoTimeout bounds the best-effort GET /info probe used to learn a TEI
// backend's max_client_batch_size. Mirrors recordModelLicense's /info probe.
const teiInfoTimeout = 10 * time.Second

// errTEINotReady is returned (wrapped) by waitForTEIReady when the service does
// not become healthy within the timeout. The FILE_INDEX walk distinguishes this
// "service is gone" condition from a single file's embed failure: the former
// aborts the walk (nothing else can succeed), the latter is recorded and
// skipped so one bad file never stalls a large index.
var errTEINotReady = errors.New("TEI service not ready")

// teiBatchSizeMu guards teiBatchSizeCache.
var teiBatchSizeMu sync.Mutex

// teiBatchSizeCache memoizes each TEI base URL's resolved max_client_batch_size
// so the GET /info probe runs at most once per URL per process (the FILE_INDEX
// walk embeds many files through the same base URL). Keyed by base URL so
// distinct backends — including distinct httptest servers in tests — never share
// a cached limit.
var teiBatchSizeCache = map[string]int{}

// EmbeddingHandler handles JobTypeEmbedding ("embedding") jobs by routing them
// to the local TEI service's OpenAI-compatible /v1/embeddings endpoint.
//
// It implements the legacy jobs.JobHandler interface (Execute(JobContext,
// *nexus.Job) ([]byte, error)) so it can be wired into the live worker dispatch
// via worker.CreateLegacyHandlersWithOpts, exactly like VLLMInferenceHandler.
//
// IMPORTANT — payload contract: the live dispatch path
// (worker.LegacyHandlerAdapter) flattens the job payload map[string]any into
// map[string]string before the handler sees it. Since citadel-cli#462 the
// adapter json.Marshals nested []any/map values (not fmt.Sprint), so a raw JSON
// array of strings survives the flatten as `["a","b"]` too. The canonical,
// version-safe wire form is therefore a JSON-encoded array string for `input`
// (e.g. `["hello","world"]`), which the handler json.Unmarshals — the aceteam
// dispatch (run_fabric_embeddings) sends exactly this via json.dumps(texts). A
// bare scalar string is also accepted and treated as a single-element input for
// convenience. (Older adapters that fmt.Sprint'd a []string would mangle it to
// "[a b]"; sending a JSON string avoids that on every adapter version.)
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

// callTEIEmbeddings embeds all of req.Input via TEI's /v1/embeddings, sub-batching
// to the backend's max_client_batch_size so a request larger than that limit does
// not fail with HTTP 413 ("batch size N > maximum allowed batch size M"). The
// per-batch responses are concatenated IN ORDER, so the returned vectors line up
// one-to-one with req.Input regardless of how many batches were needed or how the
// engine ordered each response. Usage counts are summed across batches; the model
// name and dimensionality are taken from the first batch.
//
// A single input (the FILE_SEMANTIC_SEARCH query path) collapses to one batch and
// produces a request byte-identical to the pre-sub-batching behavior.
func callTEIEmbeddings(baseURL string, req *EmbeddingRequest) (*EmbeddingResult, error) {
	limit := teiMaxClientBatchSize(baseURL)
	if limit < 1 {
		limit = teiDefaultMaxClientBatchSize
	}

	result := &EmbeddingResult{
		Model:      req.Model,
		Embeddings: make([][]float64, 0, len(req.Input)),
	}
	first := true
	for start := 0; start < len(req.Input); start += limit {
		end := start + limit
		if end > len(req.Input) {
			end = len(req.Input)
		}
		batch := &EmbeddingRequest{
			Model:      req.Model,
			Input:      req.Input[start:end],
			Dimensions: req.Dimensions,
		}
		batchRes, err := postTEIEmbeddingsBatch(baseURL, batch)
		if err != nil {
			return nil, err
		}
		result.Embeddings = append(result.Embeddings, batchRes.Embeddings...)
		result.Usage.PromptTokens += batchRes.Usage.PromptTokens
		result.Usage.TotalTokens += batchRes.Usage.TotalTokens
		if first {
			result.Model = batchRes.Model
			first = false
		}
	}
	if len(result.Embeddings) > 0 {
		result.Dimensions = len(result.Embeddings[0])
	}
	return result, nil
}

// postTEIEmbeddingsBatch POSTs a single (already size-bounded) batch to TEI's
// /v1/embeddings and returns the parsed result sized to exactly len(req.Input).
// It rejects a response whose entry count differs from the input count, or that
// leaves any input position without a vector, so a short/garbled response errors
// rather than silently storing a nil embedding downstream.
func postTEIEmbeddingsBatch(baseURL string, req *EmbeddingRequest) (*EmbeddingResult, error) {
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
	if len(teiResp.Data) != len(req.Input) {
		return nil, fmt.Errorf("TEI returned %d embeddings for %d inputs", len(teiResp.Data), len(req.Input))
	}

	result := &EmbeddingResult{
		Model:      teiResp.Model,
		Embeddings: make([][]float64, len(req.Input)),
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
	for i, v := range result.Embeddings {
		if v == nil {
			return nil, fmt.Errorf("TEI response missing embedding for input index %d", i)
		}
	}
	if len(result.Embeddings) > 0 {
		result.Dimensions = len(result.Embeddings[0])
	}
	result.Usage.PromptTokens = teiResp.Usage.PromptTokens
	result.Usage.TotalTokens = teiResp.Usage.TotalTokens

	return result, nil
}

// teiMaxClientBatchSize returns the backend's max_client_batch_size, fetched from
// GET /info at most once per base URL (memoized). Any failure resolves to
// teiDefaultMaxClientBatchSize, which is also cached so a down /info endpoint is
// probed only once.
func teiMaxClientBatchSize(baseURL string) int {
	teiBatchSizeMu.Lock()
	defer teiBatchSizeMu.Unlock()
	if v, ok := teiBatchSizeCache[baseURL]; ok {
		return v
	}
	limit := fetchTEIMaxClientBatchSize(baseURL)
	teiBatchSizeCache[baseURL] = limit
	return limit
}

// fetchTEIMaxClientBatchSize performs a best-effort GET <baseURL>/info and returns
// the reported max_client_batch_size. It falls back to teiDefaultMaxClientBatchSize
// on any failure: unreachable, non-200, unparsable body, a missing/zero/negative
// field. Mirrors recordModelLicense's defensive /info probe.
func fetchTEIMaxClientBatchSize(baseURL string) int {
	ctx, cancel := context.WithTimeout(context.Background(), teiInfoTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/info", nil)
	if err != nil {
		return teiDefaultMaxClientBatchSize
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return teiDefaultMaxClientBatchSize
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return teiDefaultMaxClientBatchSize
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return teiDefaultMaxClientBatchSize
	}
	var info struct {
		MaxClientBatchSize int `json:"max_client_batch_size"`
	}
	if err := json.Unmarshal(body, &info); err != nil || info.MaxClientBatchSize <= 0 {
		return teiDefaultMaxClientBatchSize
	}
	return info.MaxClientBatchSize
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
	return fmt.Errorf("TEI service did not become ready within %v: %w", timeout, errTEINotReady)
}
