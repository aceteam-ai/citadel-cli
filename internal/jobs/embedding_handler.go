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
	"strings"
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
// llm_inference.go.
const teiReadyTimeout = 60 * time.Second

// errTEINotReady identifies a failed operation-wide readiness probe, rather
// than a transient embedding failure for one file in a healthy operation.
var errTEINotReady = errors.New("TEI service not ready")

const (
	// teiDefaultClientBatchSize matches the stock TEI module's
	// max_client_batch_size and is the safe fallback when /info is unavailable
	// or does not provide a trustworthy limit.
	teiDefaultClientBatchSize = 32
	// teiMaxClientBatchSize prevents a malformed or compromised /info response
	// from disabling client-side batching with an implausibly large value.
	teiMaxClientBatchSize      = 1024
	teiInfoTimeout             = 5 * time.Second
	teiEmbeddingRequestTimeout = 2 * time.Minute
	// Embeddings are local calls, but a stalled socket must not hang a CLI
	// operation forever when its parent context has no deadline.
	teiHealthRequestTimeout   = 3 * time.Second
	teiEmbeddingResponseLimit = 16 << 20 // 16 MiB across at most one sub-batch
	teiErrorResponseLimit     = 4 << 10  // bounded drain; bodies never enter errors
)

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
	op := newTEIEmbeddingOperation(teiBaseURL())
	if err := op.initialize(ctx.Context()); err != nil {
		return nil, err
	}
	ctx.Log("info", "     - [Job %s] TEI ready. Embedding %d text(s) with model %q", job.ID, len(req.Input), req.Model)

	result, err := op.embed(ctx.Context(), req)
	if err != nil {
		return nil, err
	}

	out, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal embedding result: %w", err)
	}
	return out, nil
}

// teiEmbeddingOperation owns one logical embedding operation (one job or one
// index walk). It probes readiness and /info at most once, then applies the
// discovered batch limit to every request in that operation. Keeping this
// state operation-scoped avoids stale process-wide limits after TEI restarts or
// is reconfigured.
type teiEmbeddingOperation struct {
	baseURL        string
	client         *http.Client
	requestTimeout time.Duration
	readyTimeout   time.Duration

	initOnce  sync.Once
	initErr   error
	batchSize int
}

func newTEIEmbeddingOperation(baseURL string) *teiEmbeddingOperation {
	return &teiEmbeddingOperation{
		baseURL:        strings.TrimRight(baseURL, "/"),
		client:         &http.Client{Timeout: teiEmbeddingRequestTimeout},
		requestTimeout: teiEmbeddingRequestTimeout,
		readyTimeout:   teiReadyTimeout,
	}
}

// initialize waits for TEI and resolves its advertised client batch size once.
// /info is advisory: unavailable, malformed, or implausible values fall back to
// 32. Cancellation of the parent operation remains fatal and is never hidden by
// that fallback.
func (o *teiEmbeddingOperation) initialize(ctx context.Context) error {
	o.initOnce.Do(func() {
		if err := waitForTEIReady(ctx, o.client, o.baseURL, o.readyTimeout); err != nil {
			o.initErr = err
			return
		}
		o.batchSize, o.initErr = resolveTEIClientBatchSize(ctx, o.client, o.baseURL)
	})
	return o.initErr
}

type teiInfoResponse struct {
	MaxClientBatchSize int `json:"max_client_batch_size"`
}

func resolveTEIClientBatchSize(ctx context.Context, client *http.Client, baseURL string) (int, error) {
	fallback := func() (int, error) {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		return teiDefaultClientBatchSize, nil
	}
	infoCtx, cancel := context.WithTimeout(ctx, teiInfoTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(infoCtx, http.MethodGet, baseURL+"/info", nil)
	if err != nil {
		return fallback()
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return fallback()
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fallback()
	}

	const infoBodyLimit = 1 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, infoBodyLimit+1))
	if err != nil || len(body) > infoBodyLimit {
		return fallback()
	}
	var info teiInfoResponse
	if err := json.Unmarshal(body, &info); err != nil {
		return fallback()
	}
	if info.MaxClientBatchSize < 1 || info.MaxClientBatchSize > teiMaxClientBatchSize {
		return fallback()
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return info.MaxClientBatchSize, nil
}

// embed splits req.Input into server-sized batches and returns one atomic,
// input-ordered result. A later batch failure returns no partial result.
func (o *teiEmbeddingOperation) embed(ctx context.Context, req *EmbeddingRequest) (*EmbeddingResult, error) {
	if req == nil || len(req.Input) == 0 {
		return nil, fmt.Errorf("embedding input must contain at least one text")
	}
	if err := o.initialize(ctx); err != nil {
		return nil, err
	}

	result := &EmbeddingResult{
		Model:      req.Model,
		Embeddings: make([][]float64, len(req.Input)),
	}
	firstBatch := true
	for start := 0; start < len(req.Input); start += o.batchSize {
		end := start + o.batchSize
		if end > len(req.Input) {
			end = len(req.Input)
		}
		batch, err := callTEIEmbeddingBatch(ctx, o.client, o.baseURL, o.requestTimeout, &EmbeddingRequest{
			Model:      req.Model,
			Input:      req.Input[start:end],
			Dimensions: req.Dimensions,
		})
		if err != nil {
			return nil, fmt.Errorf("embedding batch %d-%d: %w", start, end-1, err)
		}
		if result.Dimensions == 0 {
			result.Dimensions = batch.Dimensions
		} else if batch.Dimensions != result.Dimensions {
			return nil, fmt.Errorf("TEI returned inconsistent embedding dimensions: batch %d-%d has %d, want %d", start, end-1, batch.Dimensions, result.Dimensions)
		}
		if batch.Model != "" {
			if !firstBatch && batch.Model != result.Model {
				return nil, fmt.Errorf("TEI returned inconsistent models across embedding batches at batch %d-%d", start, end-1)
			}
			result.Model = batch.Model
		}
		firstBatch = false
		copy(result.Embeddings[start:end], batch.Embeddings)
		result.Usage.PromptTokens += batch.Usage.PromptTokens
		result.Usage.TotalTokens += batch.Usage.TotalTokens
	}
	return result, nil
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

// callTEIEmbeddings is retained as a test-facing convenience for one complete
// operation. Production call paths keep and reuse a teiEmbeddingOperation.
func callTEIEmbeddings(baseURL string, req *EmbeddingRequest) (*EmbeddingResult, error) {
	return newTEIEmbeddingOperation(baseURL).embed(context.Background(), req)
}

// callTEIEmbeddingBatch POSTs one already-bounded embedding request to TEI's
// /v1/embeddings endpoint and strictly validates cardinality, indices, and
// vector dimensions before returning it to the operation-level aggregator.
func callTEIEmbeddingBatch(ctx context.Context, client *http.Client, baseURL string, timeout time.Duration, req *EmbeddingRequest) (*EmbeddingResult, error) {
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

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

	httpReq, err := http.NewRequestWithContext(requestCtx, http.MethodPost, baseURL+"/v1/embeddings", bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create TEI request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to TEI service: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Drain only a bounded prefix for connection reuse, but never place an
		// upstream body in an error: a local server may echo indexed content.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, teiErrorResponseLimit))
		return nil, &teiHTTPError{StatusCode: resp.StatusCode}
	}
	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, teiEmbeddingResponseLimit+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read TEI response: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(bodyBytes) > teiEmbeddingResponseLimit {
		return nil, fmt.Errorf("TEI response exceeds %d bytes", teiEmbeddingResponseLimit)
	}

	var teiResp teiEmbeddingResponse
	if err := json.Unmarshal(bodyBytes, &teiResp); err != nil {
		return nil, errors.New("failed to parse TEI response")
	}
	if len(teiResp.Data) != len(req.Input) {
		return nil, fmt.Errorf("TEI returned %d embeddings for %d inputs", len(teiResp.Data), len(req.Input))
	}

	result := &EmbeddingResult{
		Model:      teiResp.Model,
		Embeddings: make([][]float64, len(teiResp.Data)),
	}
	if result.Model == "" {
		result.Model = req.Model
	}
	seen := make([]bool, len(result.Embeddings))
	dim := 0
	for _, d := range teiResp.Data {
		// TEI returns data entries with explicit indices; place each vector at
		// its index so output order matches the input order regardless of how
		// the engine ordered the response.
		if d.Index < 0 || d.Index >= len(result.Embeddings) {
			return nil, fmt.Errorf("TEI returned out-of-range embedding index %d", d.Index)
		}
		if seen[d.Index] {
			return nil, fmt.Errorf("TEI returned duplicate embedding index %d", d.Index)
		}
		if len(d.Embedding) == 0 {
			return nil, fmt.Errorf("TEI returned empty embedding at index %d", d.Index)
		}
		if dim == 0 {
			dim = len(d.Embedding)
		} else if len(d.Embedding) != dim {
			return nil, fmt.Errorf("TEI returned embedding dimension %d at index %d, want %d", len(d.Embedding), d.Index, dim)
		}
		seen[d.Index] = true
		result.Embeddings[d.Index] = d.Embedding
	}
	for i, present := range seen {
		if !present {
			return nil, fmt.Errorf("TEI response missing embedding index %d", i)
		}
	}
	if req.Dimensions > 0 && dim != req.Dimensions {
		return nil, fmt.Errorf("TEI returned embedding dimension %d, requested %d", dim, req.Dimensions)
	}
	result.Dimensions = dim
	result.Usage.PromptTokens = teiResp.Usage.PromptTokens
	result.Usage.TotalTokens = teiResp.Usage.TotalTokens
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	return result, nil
}

type teiHTTPError struct {
	StatusCode int
}

func (e *teiHTTPError) Error() string {
	return fmt.Sprintf("TEI returned status %d", e.StatusCode)
}

// teiFailureCategory deliberately excludes upstream response bodies. FILE_INDEX
// continues after a per-file embedding failure, so logging the complete error
// there would turn a server that echoes input into a persistent content leak.
func teiFailureCategory(err error) string {
	var httpErr *teiHTTPError
	if errors.As(err, &httpErr) {
		return fmt.Sprintf("TEI HTTP %d", httpErr.StatusCode)
	}
	return "TEI request failed"
}

// waitForTEIReady polls TEI's /health endpoint until it reports ready or the
// timeout elapses. Mirrors waitForVLLMReady in llm_inference.go.
func waitForTEIReady(ctx context.Context, client *http.Client, baseURL string, timeout time.Duration) error {
	healthURL := baseURL + "/health"
	pollInterval := 1 * time.Second
	readyCtx, readyCancel := context.WithTimeout(ctx, timeout)
	defer readyCancel()

	for {
		healthCtx, cancel := context.WithTimeout(readyCtx, teiHealthRequestTimeout)
		req, err := http.NewRequestWithContext(healthCtx, http.MethodGet, healthURL, nil)
		if err != nil {
			cancel()
			return fmt.Errorf("create TEI health request: %w", err)
		}
		resp, err := client.Do(req)
		cancel()
		if err == nil && resp.StatusCode == http.StatusOK {
			resp.Body.Close()
			return nil
		}
		if resp != nil {
			resp.Body.Close()
		}
		select {
		case <-readyCtx.Done():
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("%w within %v", errTEINotReady, timeout)
		case <-time.After(pollInterval):
		}
	}
}
