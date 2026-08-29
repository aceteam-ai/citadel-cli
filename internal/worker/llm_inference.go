// internal/worker/llm_inference.go
//
// llm_inference job handler (issue #590).
//
// The aceteam python-backend dispatches job_type="llm_inference" for ALL fabric
// inference (the OpenAI-compatible gateway, /fabric model deploys, mesh chat).
// The unified `citadel work` worker (internal/worker.JobHandler) previously had
// NO handler registered for it, so every fabric inference job failed with
// `unsupported job type "llm_inference": node X has no handler for it`.
//
// The routing logic already existed in internal/jobs/llm_inference.go, but that
// file implemented a DEAD interface — Execute(ctx, *redis.Client, *redis.Job) —
// and was registered nowhere (it streamed by calling client.PublishChunk /
// PublishEnd directly). This handler ports that logic into a NATIVE
// worker.JobHandler that streams via StreamWriter and is registered in
// cmd/nodejobs.go, so both `citadel work` and the control-center worker handle
// llm_inference.
//
// Streaming contract: the Runner calls stream.WriteStart before Execute and
// stream.WriteEnd(result.Output) on success (runner.go), so this handler emits
// tokens with stream.WriteChunk and returns the final {content, finish_reason,
// usage} as the JobResult.Output — it must NOT call WriteEnd itself (that would
// double-publish the terminal event). This mirrors the workflow handler, the
// closest streaming analog in this package.
package worker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/aep"
	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/nodeidentity"
	"github.com/aceteam-ai/citadel-cli/internal/status"
	"github.com/aceteam-ai/citadel-cli/internal/trust"
	"github.com/aceteam-ai/citadel-cli/internal/update"
	"github.com/aceteam-ai/citadel-cli/services"
)

// LLMInferenceHandler routes an llm_inference job to the node-local inference
// engine selected by payload.Backend (vllm / sglang / ollama / llamacpp /
// bonsai) and streams the reply back through a StreamWriter.
type LLMInferenceHandler struct {
	// baseURLs maps a backend name to its host-local base URL. Defaulted in the
	// constructor from the citadel port registry (services/ports.go); injectable
	// so tests can point a backend at an httptest server without a live engine.
	baseURLs map[string]string

	// httpClient issues the outbound engine requests. Defaults to
	// http.DefaultClient; overridable in tests.
	httpClient *http.Client

	// swapper, when non-nil, enables VRAM-aware on-demand model hotswap
	// (citadel-cli#632): before routing to the engine, an installed-but-not-
	// resident target is swapped in. Injected ONLY when CITADEL_MODEL_HOTSWAP is
	// on (cmd/nodejobs.go), so a nil swapper == today's behavior exactly.
	swapper modelSwapper

	// requestRecorder records a node-routed request against the resolved
	// backend name (citadel #691) -- the worker's half of the last_request_at
	// fix; the gateway's is internal/gateway.Server.requestRecorder. Defaults to
	// status.RecordEngineRequest in the constructor; overridable via
	// WithRequestRecorder so tests can inject a spy instead of touching the
	// shared process-wide log.
	requestRecorder func(engine string)

	// signer backs the signed AEP receipt (aceteam #8253, internal/aep).
	// Defaults to nodeidentity.Default() -- the node's persistent, unattended-
	// capable ECDSA P-256 key (see docs/design-node-identity-receipts.md
	// §1c/§1d for why that package, not internal/nodevault, backs this) --
	// in the constructor; overridable via WithSigner so tests never touch the
	// real host's platform.ConfigDir()/identity directory.
	//
	// KNOWN HAZARD, not fixed here: nodeidentity.Default() is rooted at
	// platform.ConfigDir(), which is INVOKER-scoped (see CLAUDE.md's
	// ConfigDir()/GetNodeConfigDir() section and citadel-cli#845/#726/#696/
	// #383) -- it can resolve to a DIFFERENT directory for `citadel init`
	// (which already calls nodeidentity.Default().GetOrCreateKey() via
	// cmd/init.go's ensureNodeIdentity) than for the `citadel work` process
	// that reaches this handler, e.g. a systemd-root worker vs. an
	// interactive non-root init. GetOrCreateKey does not error on a
	// directory mismatch -- it silently GENERATES A SECOND, DIFFERENT
	// KEYPAIR. Before this PR that was latent (the key had exactly one,
	// dormant consumer). This PR adds the second, live consumer, so the
	// hazard is now real: if a future Phase 2 registers the init-context
	// public key with the backend, a worker signing with a divergent
	// work-context key will produce signatures that never verify. Not fixed
	// here because migrating nodeidentity's storage location is out of this
	// PR's scope and could strand an already-registered key; WithSigner is
	// the seam a future fix can use to inject a network.GetNodeConfigDir()-
	// rooted store instead, without internal/worker needing to change again.
	signer aep.Signer

	// fabricNodeID resolves the AEP receipt's preferred node_id (aceteam
	// #8139's fabric node ID, via config.LoadDeviceCredsConverged --
	// inert/empty on every node until a backend echo point lands, see
	// docs/design-node-identity-receipts.md §2/§4). A func field (not a
	// direct call in buildAEPReceipt) so WithFabricNodeIDResolver lets tests
	// avoid reading the real host's device-config file.
	fabricNodeID func() string

	// aepLogf logs a non-fatal AEP receipt-signing failure (fail-open --
	// signing must never break inference). An injectable field, mirroring
	// swap_persist.go's persistLogf, rather than a bare log.Printf call:
	// this package otherwise imports no logger at all, and this keeps that
	// true for anything that doesn't opt into signing.
	aepLogf func(format string, args ...any)
}

// modelSwapper is the swap surface the handler needs. Satisfied by *SwapManager;
// an interface so the handler is unit-testable with a mock. See swap.go.
type modelSwapper interface {
	EnsureResident(ctx context.Context, backend, model string) (SwapOutcome, error)
}

// NewLLMInferenceHandler constructs the llm_inference handler with the default
// backend endpoints resolved from the citadel port registry (services/ports.go).
// It needs no workspace/config, so cmd/nodejobs.go registers it unconditionally
// (issue #590 — the backend dispatches llm_inference for all fabric inference).
func NewLLMInferenceHandler() *LLMInferenceHandler {
	return &LLMInferenceHandler{
		baseURLs: map[string]string{
			// vllm/llamacpp/bonsai host ports come from the citadel registry so a
			// per-node CITADEL_*_HOST_PORT override is honored (same source the
			// engines' compose publishes resolve).
			"vllm":     fmt.Sprintf("http://localhost:%d", services.VLLMHostPort),
			"llamacpp": fmt.Sprintf("http://localhost:%d", services.LlamacppHostPort),
			"bonsai":   fmt.Sprintf("http://localhost:%d", services.BonsaiHostPort),
			// unlimited-ocr (Baidu Unlimited-OCR) is served by vLLM on its own
			// registry host port; route inference there (multimodal image content
			// is carried by executeChatCompletionsAt via #625).
			"unlimited-ocr": fmt.Sprintf("http://localhost:%d", services.UnlimitedOCRHostPort),
			// sglang/ollama sit on their native, well-known host ports (not part of
			// the collision-managed 8200 block — see services/ports.go).
			"sglang": "http://localhost:30000",
			"ollama": "http://localhost:11434",
		},
		httpClient:      http.DefaultClient,
		requestRecorder: status.RecordEngineRequest,
		signer:          nodeidentity.Default(),
		fabricNodeID:    func() string { return config.LoadDeviceCredsConverged().FabricNodeID },
		aepLogf:         log.Printf,
	}
}

// WithSwapper attaches a model-hotswap swapper (citadel-cli#632). Called from
// cmd/nodejobs.go ONLY when CITADEL_MODEL_HOTSWAP is on; leaving it unset keeps
// the handler's behavior identical to before hotswap.
func (h *LLMInferenceHandler) WithSwapper(s modelSwapper) *LLMInferenceHandler {
	h.swapper = s
	return h
}

// WithRequestRecorder overrides the node-routed request recorder
// (citadel #691), primarily for tests that want to observe recorded calls
// without touching the shared process-wide status.RecordEngineRequest log.
// Passing nil disables recording entirely.
func (h *LLMInferenceHandler) WithRequestRecorder(recorder func(engine string)) *LLMInferenceHandler {
	h.requestRecorder = recorder
	return h
}

// WithSigner overrides the AEP receipt signer (aceteam #8253), primarily for
// tests that want a hermetic in-memory signer instead of nodeidentity.Default()
// (which reads/writes real key material under platform.ConfigDir()).
func (h *LLMInferenceHandler) WithSigner(signer aep.Signer) *LLMInferenceHandler {
	h.signer = signer
	return h
}

// WithFabricNodeIDResolver overrides how the AEP receipt resolves the
// preferred node_id (aceteam #8139), primarily for tests that want a
// hermetic, fixed value instead of config.LoadDeviceCredsConverged() reading
// the real host's device-config file.
func (h *LLMInferenceHandler) WithFabricNodeIDResolver(resolver func() string) *LLMInferenceHandler {
	h.fabricNodeID = resolver
	return h
}

// CanHandle reports whether this handler processes the given job type.
func (h *LLMInferenceHandler) CanHandle(jobType string) bool {
	return jobType == JobTypeLLMInference
}

// baseURL returns the configured base URL for a backend (empty if unknown).
func (h *LLMInferenceHandler) baseURL(backend string) string {
	return h.baseURLs[backend]
}

// Execute parses the payload and routes to the backend-specific path. The
// backend switch mirrors the original internal/jobs handler verbatim; only the
// output sink changed (StreamWriter + JobResult instead of Redis Publish*).
func (h *LLMInferenceHandler) Execute(ctx context.Context, job *Job, stream StreamWriter) (*JobResult, error) {
	payload, err := parseLLMInferencePayload(job.Payload)
	if err != nil {
		return h.failure(fmt.Errorf("invalid payload: %w", err)), nil
	}

	// Model hotswap (citadel-cli#632): when enabled (swapper injected), an
	// installed-but-not-resident target engine is swapped in before routing. If it
	// becomes ready within the wait budget (≤15s) we fall through and serve
	// normally; otherwise we return a structured model_warming result (a normal
	// success JobResult carrying no content) for the platform to relay + retry. A
	// nil swapper (flag off) skips this block entirely — unchanged behavior.
	if h.swapper != nil {
		outcome, swapErr := h.swapper.EnsureResident(ctx, payload.Backend, payload.Model)
		if swapErr != nil {
			// A node at its swap limit is refusing, not malfunctioning
			// (citadel-cli#687). It is still a job FAILURE — every consumer that
			// exists today renders a failure as a failure, whereas a new
			// success-shaped control status nothing parses would be relayed as an
			// empty reply, which is the thrash traded for silence. The reason rides
			// along machine-readably for a consumer that wants to special-case it.
			var rateErr *SwapRateLimitedError
			if errors.As(swapErr, &rateErr) {
				return h.unavailable(payload.Model, "swap_rate_limited", swapErr), nil
			}
			return h.failure(fmt.Errorf("model hotswap failed: %w", swapErr)), nil
		}
		if !outcome.Ready {
			return h.warming(payload.Model, outcome.ETASeconds, outcome.RetryAfterSeconds, outcome.WarmingFor), nil
		}
	}

	// Readiness gate (citadel-cli#680): residency is "the container is up", which
	// is NOT the same as "the engine is serving". A container that has bound its
	// port but is still loading weights used to be proxied into, and the caller
	// got a raw socket string. Probe every backend and answer with the typed
	// warming signal instead. See llm_readiness.go for why the probe asks "does
	// the API answer" rather than "does it list a model", and why the budgets
	// differ per engine.
	if err := h.ensureEngineReady(ctx, payload.Backend); err != nil {
		if errors.Is(err, errEngineWarming) {
			// This node's own start already targets payload.Model on this
			// backend (there is no "someone else's model" ambiguity here), so
			// the warmingFor discriminator is simply the requested model.
			return h.warming(payload.Model, engineWarmETA(payload.Backend), 0, payload.Model), nil
		}
		return h.failure(err), nil
	}

	// Record the dispatch against the resolved backend (citadel #691), now that
	// the engine has passed the readiness probe above and is actually about to
	// receive the request. This is what gives ollama/bonsai/llamacpp/
	// unlimited-ocr -- none of which expose a scrapeable request metric -- a
	// real last_request_at in the heartbeat instead of "never".
	if h.requestRecorder != nil {
		h.requestRecorder(payload.Backend)
	}

	switch payload.Backend {
	case "vllm":
		return h.executeVLLM(ctx, stream, payload, job.ID)
	case "sglang":
		return h.executeSGLang(ctx, stream, payload)
	case "ollama":
		return h.executeOllama(ctx, stream, payload)
	case "llamacpp":
		return h.executeLlamaCppAt(ctx, stream, payload, h.baseURL("llamacpp"), job.ID)
	case "bonsai":
		// Bonsai serves the identical llama.cpp-server API on its own host port
		// (PrismML fork). Reuse the llama.cpp request/stream path pointed at it.
		return h.executeLlamaCppAt(ctx, stream, payload, h.baseURL("bonsai"), job.ID)
	case "unlimited-ocr":
		// Baidu Unlimited-OCR is a vLLM OpenAI engine on its own host port; use the
		// chat-completions path so multimodal image_url content (#625) reaches it.
		return h.executeChatCompletionsAt(ctx, stream, payload, h.baseURL("unlimited-ocr"), job.ID)
	default:
		return h.failure(fmt.Errorf("unsupported backend: %s", payload.Backend)), nil
	}
}

// parseLLMInferencePayload decodes the job payload into the shared payload
// struct (internal/jobs.LLMInferencePayload) and applies the same validation +
// backend default as the original handler.
func parseLLMInferencePayload(data map[string]any) (*jobs.LLMInferencePayload, error) {
	jsonBytes, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	var payload jobs.LLMInferencePayload
	if err := json.Unmarshal(jsonBytes, &payload); err != nil {
		return nil, err
	}
	if payload.Model == "" {
		return nil, fmt.Errorf("model is required")
	}
	if payload.Prompt == "" && len(payload.Messages) == 0 {
		return nil, fmt.Errorf("prompt or messages is required")
	}
	if payload.Backend == "" {
		payload.Backend = "vllm" // Default to vLLM
	}
	return &payload, nil
}

// executeVLLM handles inference via vLLM's OpenAI-compatible API.
func (h *LLMInferenceHandler) executeVLLM(ctx context.Context, stream StreamWriter, payload *jobs.LLMInferencePayload, jobID string) (*JobResult, error) {
	// Chat-style requests (gateway `messages`) use /v1/chat/completions so vLLM
	// applies the served model's chat template; the legacy /v1/completions prompt
	// path is kept for prompt-style jobs.
	if len(payload.Messages) > 0 {
		return h.executeChatCompletionsAt(ctx, stream, payload, h.baseURL("vllm"), jobID)
	}
	return h.executeCompletions(ctx, stream, payload, h.baseURL("vllm"), "vLLM")
}

// executeSGLang handles inference via SGLang's OpenAI-compatible API. SGLang
// exposes the same /v1/completions endpoint and response format as vLLM.
func (h *LLMInferenceHandler) executeSGLang(ctx context.Context, stream StreamWriter, payload *jobs.LLMInferencePayload) (*JobResult, error) {
	return h.executeCompletions(ctx, stream, payload, h.baseURL("sglang"), "SGLang")
}

// executeCompletions runs a prompt-style /v1/completions request (vLLM/SGLang
// share the OpenAI text-completions format).
func (h *LLMInferenceHandler) executeCompletions(ctx context.Context, stream StreamWriter, payload *jobs.LLMInferencePayload, baseURL, engine string) (*JobResult, error) {
	reqPayload := map[string]any{
		"model":       payload.Model,
		"prompt":      payload.Prompt,
		"max_tokens":  payload.MaxTokens,
		"temperature": payload.Temperature,
		"stream":      payload.Stream,
	}
	if payload.MaxTokens == 0 {
		reqPayload["max_tokens"] = 512
	}
	if len(payload.Stop) > 0 {
		reqPayload["stop"] = payload.Stop
	}

	resp, err := h.postJSON(ctx, baseURL+"/v1/completions", reqPayload)
	if err != nil {
		return h.engineRequestFailure(payload, err, "failed to connect to "+engine), nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return h.failure(fmt.Errorf("%s returned status %d: %s", engine, resp.StatusCode, string(body))), nil
	}

	if payload.Stream {
		return h.streamCompletions(stream, resp.Body)
	}
	return h.bufferedCompletions(stream, resp.Body, engine)
}

// streamCompletions forwards an OpenAI text-completions SSE stream as chunks.
func (h *LLMInferenceHandler) streamCompletions(stream StreamWriter, body io.Reader) (*JobResult, error) {
	scanner := bufio.NewScanner(body)
	chunkIndex := 0
	var fullContent strings.Builder

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		var chunk struct {
			Choices []struct {
				Text         string `json:"text"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) > 0 {
			text := chunk.Choices[0].Text
			fullContent.WriteString(text)
			stream.WriteChunk(text, chunkIndex)
			chunkIndex++
			if chunk.Choices[0].FinishReason != "" {
				break
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return h.failure(err), nil
	}
	return h.success(map[string]any{
		"content":       fullContent.String(),
		"finish_reason": "stop",
	}), nil
}

// bufferedCompletions parses a non-streamed text-completions response.
func (h *LLMInferenceHandler) bufferedCompletions(stream StreamWriter, body io.Reader, engine string) (*JobResult, error) {
	bodyBytes, err := io.ReadAll(body)
	if err != nil {
		return h.failure(err), nil
	}
	var response struct {
		Choices []struct {
			Text         string `json:"text"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(bodyBytes, &response); err != nil {
		return h.failure(fmt.Errorf("failed to parse %s response: %w", engine, err)), nil
	}
	if len(response.Choices) == 0 {
		return h.failure(fmt.Errorf("%s returned no choices", engine)), nil
	}

	content := strings.TrimSpace(response.Choices[0].Text)
	// Emit a single chunk (parity with the streaming path) then the end.
	writeSingleChunk(stream, content)
	return h.success(map[string]any{
		"content":       content,
		"finish_reason": response.Choices[0].FinishReason,
		"usage": map[string]any{
			"prompt_tokens":     response.Usage.PromptTokens,
			"completion_tokens": response.Usage.CompletionTokens,
			"total_tokens":      response.Usage.TotalTokens,
		},
	}), nil
}

// executeOllama handles inference via Ollama's native /api/generate API.
func (h *LLMInferenceHandler) executeOllama(ctx context.Context, stream StreamWriter, payload *jobs.LLMInferencePayload) (*JobResult, error) {
	// Chat-style requests (the OpenAI gateway and MCP inference_chat send
	// `messages`, not `prompt`) route to Ollama's native /api/chat, which applies
	// the model's chat template and returns the reply in message.content.
	// /api/generate has no messages support: sending it an empty prompt made
	// Ollama merely load the model and return an empty `response`
	// (done_reason "load"), which surfaced as "No response content returned from
	// the inference node" on the platform (issue #6641). The /api/generate prompt
	// path is preserved for prompt-style jobs.
	if len(payload.Messages) > 0 {
		return h.executeOllamaChat(ctx, stream, payload)
	}

	reqPayload := map[string]any{
		"model":  payload.Model,
		"prompt": payload.Prompt,
		"stream": payload.Stream,
	}
	if payload.MaxTokens > 0 {
		reqPayload["options"] = map[string]any{"num_predict": payload.MaxTokens}
	}

	resp, err := h.postJSON(ctx, h.baseURL("ollama")+"/api/generate", reqPayload)
	if err != nil {
		return h.engineRequestFailure(payload, err, "failed to connect to Ollama"), nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return h.failure(fmt.Errorf("Ollama returned status %d: %s", resp.StatusCode, string(body))), nil
	}

	if payload.Stream {
		return h.streamOllama(stream, resp.Body)
	}
	return h.bufferedOllama(stream, resp.Body)
}

// streamOllama forwards Ollama's newline-delimited JSON stream as chunks.
func (h *LLMInferenceHandler) streamOllama(stream StreamWriter, body io.Reader) (*JobResult, error) {
	scanner := bufio.NewScanner(body)
	chunkIndex := 0
	var fullContent strings.Builder

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var chunk struct {
			Response string `json:"response"`
			Done     bool   `json:"done"`
		}
		if err := json.Unmarshal([]byte(line), &chunk); err != nil {
			continue
		}
		if chunk.Response != "" {
			fullContent.WriteString(chunk.Response)
			stream.WriteChunk(chunk.Response, chunkIndex)
			chunkIndex++
		}
		if chunk.Done {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return h.failure(err), nil
	}
	return h.success(map[string]any{
		"content":       fullContent.String(),
		"finish_reason": "stop",
	}), nil
}

// bufferedOllama parses a non-streamed Ollama response.
func (h *LLMInferenceHandler) bufferedOllama(stream StreamWriter, body io.Reader) (*JobResult, error) {
	bodyBytes, err := io.ReadAll(body)
	if err != nil {
		return h.failure(err), nil
	}
	var response struct {
		Response string `json:"response"`
	}
	if err := json.Unmarshal(bodyBytes, &response); err != nil {
		return h.failure(fmt.Errorf("failed to parse Ollama response: %w", err)), nil
	}
	writeSingleChunk(stream, response.Response)
	return h.success(map[string]any{
		"content":       response.Response,
		"finish_reason": "stop",
	}), nil
}

// executeOllamaChat handles chat-style inference via Ollama's native /api/chat
// API. Unlike /api/generate (prompt in, `response` out), /api/chat takes
// `messages` and returns the assistant turn in `message.content`, applying the
// served model's chat template. Used whenever an llm_inference job carries
// `messages` (the shape the OpenAI gateway and MCP inference_chat dispatch).
func (h *LLMInferenceHandler) executeOllamaChat(ctx context.Context, stream StreamWriter, payload *jobs.LLMInferencePayload) (*JobResult, error) {
	// Ollama's /api/chat takes text content (images ride a separate `images`
	// field we do not populate), so flatten any multimodal content to its text
	// parts. OCR/vision fabric models run on vLLM (executeChatCompletionsAt), not
	// this path.
	messages := make([]map[string]string, 0, len(payload.Messages))
	for _, m := range payload.Messages {
		messages = append(messages, map[string]string{"role": m.Role, "content": m.Text()})
	}

	reqPayload := map[string]any{
		"model":    payload.Model,
		"messages": messages,
		"stream":   payload.Stream,
	}
	// Ollama takes sampling controls under `options` (not top-level like the
	// OpenAI shape). Only send the ones the job set so an unset field keeps the
	// model default rather than pinning greedy/zero.
	options := map[string]any{}
	if payload.MaxTokens > 0 {
		options["num_predict"] = payload.MaxTokens
	}
	if payload.Temperature > 0 {
		options["temperature"] = payload.Temperature
	}
	if payload.TopP > 0 {
		options["top_p"] = payload.TopP
	}
	if len(payload.Stop) > 0 {
		options["stop"] = payload.Stop
	}
	if len(options) > 0 {
		reqPayload["options"] = options
	}

	resp, err := h.postJSON(ctx, h.baseURL("ollama")+"/api/chat", reqPayload)
	if err != nil {
		return h.engineRequestFailure(payload, err, "failed to connect to Ollama"), nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return h.failure(fmt.Errorf("Ollama returned status %d: %s", resp.StatusCode, string(body))), nil
	}

	if payload.Stream {
		return h.streamOllamaChat(stream, resp.Body)
	}
	return h.bufferedOllamaChat(stream, resp.Body)
}

// streamOllamaChat forwards Ollama's /api/chat newline-delimited JSON stream as
// chunks. Each frame carries an incremental message.content; the final frame has
// done=true plus token counts.
func (h *LLMInferenceHandler) streamOllamaChat(stream StreamWriter, body io.Reader) (*JobResult, error) {
	scanner := bufio.NewScanner(body)
	// Allow long JSON lines (large deltas) beyond bufio's default 64KiB cap.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	chunkIndex := 0
	var fullContent strings.Builder
	var promptTokens, completionTokens int

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var chunk struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			Done            bool `json:"done"`
			PromptEvalCount int  `json:"prompt_eval_count"`
			EvalCount       int  `json:"eval_count"`
		}
		if err := json.Unmarshal([]byte(line), &chunk); err != nil {
			continue
		}
		if chunk.Message.Content != "" {
			fullContent.WriteString(chunk.Message.Content)
			stream.WriteChunk(chunk.Message.Content, chunkIndex)
			chunkIndex++
		}
		if chunk.PromptEvalCount > 0 {
			promptTokens = chunk.PromptEvalCount
		}
		if chunk.EvalCount > 0 {
			completionTokens = chunk.EvalCount
		}
		if chunk.Done {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return h.failure(err), nil
	}
	return h.success(map[string]any{
		"content":       fullContent.String(),
		"finish_reason": "stop",
		"usage":         ollamaUsage(promptTokens, completionTokens),
	}), nil
}

// bufferedOllamaChat parses a non-streamed Ollama /api/chat response.
func (h *LLMInferenceHandler) bufferedOllamaChat(stream StreamWriter, body io.Reader) (*JobResult, error) {
	bodyBytes, err := io.ReadAll(body)
	if err != nil {
		return h.failure(err), nil
	}
	var response struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		PromptEvalCount int `json:"prompt_eval_count"`
		EvalCount       int `json:"eval_count"`
	}
	if err := json.Unmarshal(bodyBytes, &response); err != nil {
		return h.failure(fmt.Errorf("failed to parse Ollama response: %w", err)), nil
	}
	writeSingleChunk(stream, response.Message.Content)
	return h.success(map[string]any{
		"content":       response.Message.Content,
		"finish_reason": "stop",
		"usage":         ollamaUsage(response.PromptEvalCount, response.EvalCount),
	}), nil
}

// ollamaUsage maps Ollama's prompt_eval_count/eval_count onto the platform's
// usage shape (prompt_tokens/completion_tokens/total_tokens) so the token footer
// works for the ollama chat path like the vLLM/llama.cpp chat path.
func ollamaUsage(promptTokens, completionTokens int) map[string]any {
	return map[string]any{
		"prompt_tokens":     promptTokens,
		"completion_tokens": completionTokens,
		"total_tokens":      promptTokens + completionTokens,
	}
}

// executeLlamaCppAt runs a llama.cpp-server inference against an explicit base
// URL. Shared by the llamacpp and bonsai backends (bonsai is the PrismML
// llama.cpp fork serving the identical API on its own host port).
func (h *LLMInferenceHandler) executeLlamaCppAt(ctx context.Context, stream StreamWriter, payload *jobs.LLMInferencePayload, baseURL string, jobID string) (*JobResult, error) {
	// Chat-style requests (the OpenAI gateway sends `messages`, not `prompt`) go
	// to /v1/chat/completions so the engine applies the model's chat template.
	// This is required for chat/instruct models — and essential for thinking
	// models like Bonsai whose template emits the reasoning/answer split. The
	// legacy /completion path is kept for prompt-style jobs.
	if len(payload.Messages) > 0 {
		return h.executeChatCompletionsAt(ctx, stream, payload, baseURL, jobID)
	}

	reqPayload := map[string]any{
		"prompt":      payload.Prompt,
		"n_predict":   payload.MaxTokens,
		"temperature": payload.Temperature,
		"stream":      payload.Stream,
	}
	if payload.MaxTokens == 0 {
		reqPayload["n_predict"] = 512
	}
	if len(payload.Stop) > 0 {
		reqPayload["stop"] = payload.Stop
	}

	resp, err := h.postJSON(ctx, baseURL+"/completion", reqPayload)
	if err != nil {
		return h.engineRequestFailure(payload, err, "failed to connect to llama.cpp"), nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return h.failure(fmt.Errorf("llama.cpp returned status %d: %s", resp.StatusCode, string(body))), nil
	}

	if payload.Stream {
		return h.streamLlamaCpp(stream, resp.Body)
	}
	return h.bufferedLlamaCpp(stream, resp.Body)
}

// streamLlamaCpp forwards a llama.cpp /completion SSE stream as chunks.
func (h *LLMInferenceHandler) streamLlamaCpp(stream StreamWriter, body io.Reader) (*JobResult, error) {
	scanner := bufio.NewScanner(body)
	chunkIndex := 0
	var fullContent strings.Builder

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")

		var chunk struct {
			Content string `json:"content"`
			Stop    bool   `json:"stop"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if chunk.Content != "" {
			fullContent.WriteString(chunk.Content)
			stream.WriteChunk(chunk.Content, chunkIndex)
			chunkIndex++
		}
		if chunk.Stop {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return h.failure(err), nil
	}
	return h.success(map[string]any{
		"content":       fullContent.String(),
		"finish_reason": "stop",
	}), nil
}

// bufferedLlamaCpp parses a non-streamed llama.cpp /completion response.
func (h *LLMInferenceHandler) bufferedLlamaCpp(stream StreamWriter, body io.Reader) (*JobResult, error) {
	bodyBytes, err := io.ReadAll(body)
	if err != nil {
		return h.failure(err), nil
	}
	var response struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(bodyBytes, &response); err != nil {
		return h.failure(fmt.Errorf("failed to parse llama.cpp response: %w", err)), nil
	}
	writeSingleChunk(stream, response.Content)
	return h.success(map[string]any{
		"content":       response.Content,
		"finish_reason": "stop",
	}), nil
}

// executeChatCompletionsAt runs a chat-style inference against an OpenAI-
// compatible /v1/chat/completions endpoint. vLLM, llama.cpp, and the bonsai
// llama.cpp fork all expose it identically. Sending `messages` (rather than a
// flattened prompt) lets the engine apply the served model's chat template,
// which is required for instruct/chat models and essential for thinking models
// like Bonsai. Used whenever an llm_inference job carries `messages` (the shape
// the OpenAI inference gateway dispatches).
func (h *LLMInferenceHandler) executeChatCompletionsAt(ctx context.Context, stream StreamWriter, payload *jobs.LLMInferencePayload, baseURL string, jobID string) (*JobResult, error) {
	// Forward content verbatim (map[string]any, not map[string]string) so the
	// OpenAI multimodal "content parts" array — e.g. an image_url for a vision/OCR
	// model like baidu/Unlimited-OCR (#625) — reaches the engine intact. A plain
	// string content marshals back to a string unchanged.
	messages := make([]map[string]any, 0, len(payload.Messages))
	for _, m := range payload.Messages {
		messages = append(messages, map[string]any{"role": m.Role, "content": m.ContentJSON()})
	}

	reqPayload := map[string]any{
		"model":       payload.Model,
		"messages":    messages,
		"max_tokens":  payload.MaxTokens,
		"temperature": payload.Temperature,
		"stream":      payload.Stream,
	}
	if payload.MaxTokens == 0 {
		reqPayload["max_tokens"] = 512
	}
	if payload.TopP > 0 {
		reqPayload["top_p"] = payload.TopP
	}
	if len(payload.Stop) > 0 {
		reqPayload["stop"] = payload.Stop
	}

	resp, err := h.postJSON(ctx, baseURL+"/v1/chat/completions", reqPayload)
	if err != nil {
		return h.engineRequestFailure(payload, err, "failed to connect to chat endpoint"), nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return h.failure(fmt.Errorf("chat endpoint returned status %d: %s", resp.StatusCode, string(body))), nil
	}

	if payload.Stream {
		return h.streamChatCompletions(stream, resp.Body)
	}
	return h.bufferedChatCompletions(stream, resp.Body, payload, jobID)
}

// bufferedChatCompletions parses a buffered OpenAI chat-completions response and
// emits the assistant's message content as a single chunk.
//
// This is the wired integration point for the on-node grounding guardrail
// (internal/trust, citadel #8253 guardrail half): it is the one call site
// where the full request input and the full model output both already exist
// as Go strings before the result leaves the node, non-streaming so there is
// no possibility of gating a response already sent. The other chat/completions
// paths in this file (streamChatCompletions and the llamacpp/ollama
// buffered/stream pairs) are documented, not-yet-wired hook points — same
// shape, not done here to keep this change to ONE clear integration point per
// #8253's scope.
//
// The guardrail is opt-in (see groundingGuardrailEnabled): `llm_inference`
// serves general chat, code generation, and vision/OCR traffic through this
// SAME function, not just grounded-extraction tasks, and "a number in the
// output not present in the input" is the NORMAL case for those (a code
// answer citing "port 8080", a model doing arithmetic, a fact recalled from
// training data). Attaching the receipt unconditionally would flag most of
// that traffic, so it stays off until a caller opts in, matching this repo's
// default-OFF convention for advisory signals (CITADEL_ENERGY_SAMPLING,
// SERVICE_AUTO_STOP_WHEN_IDLE). Disabled, the output map is byte-identical to
// before this change — no new key, nothing for a downstream consumer to
// notice.
func (h *LLMInferenceHandler) bufferedChatCompletions(stream StreamWriter, body io.Reader, payload *jobs.LLMInferencePayload, jobID string) (*JobResult, error) {
	bodyBytes, err := io.ReadAll(body)
	if err != nil {
		return h.failure(err), nil
	}
	content, finishReason, usage, err := parseChatCompletionResponse(bodyBytes)
	if err != nil {
		return h.failure(err), nil
	}
	writeSingleChunk(stream, content)
	output := map[string]any{
		"content":       content,
		"finish_reason": finishReason,
		"usage":         usage,
	}
	if groundingGuardrailEnabled() {
		result := trust.CheckGrounding(promptTextFromPayload(payload), content)
		output["grounding"] = groundingReceiptMap(result)

		// Signed AEP receipt (aceteam #8253, the signing half deferred at
		// citadel#847's merge -- see internal/aep's package doc and
		// docs/design-node-identity-receipts.md §3). Nested INSIDE the
		// grounding-guardrail gate deliberately: the receipt signs THIS
		// GroundingResult, so signing it when the guardrail itself is off
		// would mean signing a check that was never surfaced anywhere else.
		// A second, independent opt-in (signAEPReceiptsEnabled) gates
		// signing on top of that -- default OFF, so a
		// guardrail-on-but-signing-off node's output is unchanged from
		// before this feature existed (only "grounding" attaches, exactly
		// as citadel#847 shipped it).
		if signAEPReceiptsEnabled() {
			receipt, err := h.buildAEPReceipt(jobID, payload, result)
			if err != nil {
				// Fail open: signing must never break inference. Mirrors
				// internal/nodeidentity's own fail-open convention for its
				// other consumer (the mTLS CSR/leaf flow, cmd/init.go's
				// ensureNodeIdentity) -- a node whose key is unavailable
				// simply serves without a signed receipt.
				h.aepLogf("[aep] failed to build signed receipt for job %s (non-fatal): %v", jobID, err)
			} else if receiptMap, err := receipt.ToMap(); err != nil {
				// Attaching *aep.AEPReceiptV1 directly would be the only typed
				// Go pointer in this map -- see ToMap's doc comment for why
				// that's unsafe across this map's eventual wire
				// serialization. This branch should be unreachable (the
				// struct is always JSON-marshalable) but is handled the same
				// fail-open way regardless.
				h.aepLogf("[aep] failed to shape signed receipt for job %s (non-fatal): %v", jobID, err)
			} else {
				output["aep_receipt"] = receiptMap
			}
		}
	}
	return h.success(output), nil
}

// buildAEPReceipt resolves node_id (aceteam #8139's fabric node ID when
// known, else the signer's own public-key fingerprint -- internal/aep.
// ResolveNodeID's phasing fallback) and signs the AEP receipt with h.signer.
func (h *LLMInferenceHandler) buildAEPReceipt(jobID string, payload *jobs.LLMInferencePayload, result trust.GroundingResult) (*aep.AEPReceiptV1, error) {
	var fabricNodeID string
	if h.fabricNodeID != nil {
		fabricNodeID = h.fabricNodeID()
	}
	nodeID, err := aep.ResolveNodeID(h.signer, fabricNodeID)
	if err != nil {
		return nil, err
	}
	return aep.BuildSignedReceipt(h.signer, nodeID, jobID, payload.Backend, payload.Model, result, time.Now())
}

// streamChatCompletions translates an OpenAI chat-completions SSE stream into
// chunks. Each `data:` frame carries a choices[].delta. The answer is streamed
// from delta.content (matching the buffered path and standard OpenAI clients).
// Thinking models like Bonsai emit the chain-of-thought in delta.reasoning_content
// and the answer in delta.content; the reasoning is accumulated but only surfaced
// if the stream ends with NO answer (token budget spent mid-reasoning), so a
// reply is never silently blank while staying consistent with the non-stream
// content-only result.
func (h *LLMInferenceHandler) streamChatCompletions(stream StreamWriter, body io.Reader) (*JobResult, error) {
	scanner := bufio.NewScanner(body)
	// Allow long SSE lines (large deltas) beyond bufio's default 64KiB cap.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	chunkIndex := 0
	var answer strings.Builder
	var reasoning strings.Builder

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) == 0 {
			continue
		}

		if text := chunk.Choices[0].Delta.Content; text != "" {
			answer.WriteString(text)
			stream.WriteChunk(text, chunkIndex)
			chunkIndex++
		} else if rc := chunk.Choices[0].Delta.ReasoningContent; rc != "" {
			reasoning.WriteString(rc)
		}

		if chunk.Choices[0].FinishReason != "" {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return h.failure(err), nil
	}

	final := answer.String()
	if final == "" {
		// No answer produced (thinking model ran out of budget mid-reason); surface
		// the reasoning so the reply is not blank, mirroring the buffered path's
		// reasoning_content fallback.
		if final = reasoning.String(); final != "" {
			stream.WriteChunk(final, chunkIndex)
		}
	}
	return h.success(map[string]any{
		"content":       final,
		"finish_reason": "stop",
	}), nil
}

// parseChatCompletionResponse extracts the assistant content, finish reason, and
// usage from a buffered OpenAI chat-completions body. Content falls back to
// reasoning_content when the answer field is empty (thinking models like Bonsai
// whose token budget was spent mid-reasoning), so a caller never gets a blank
// reply while tokens were clearly generated. (Ported from internal/jobs; kept
// unexported here to keep the worker handler self-contained.)
func parseChatCompletionResponse(bodyBytes []byte) (content, finishReason string, usage map[string]any, err error) {
	var response struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(bodyBytes, &response); err != nil {
		return "", "", nil, fmt.Errorf("failed to parse chat completions response: %w", err)
	}

	finishReason = "stop"
	if len(response.Choices) > 0 {
		content = response.Choices[0].Message.Content
		if content == "" {
			content = response.Choices[0].Message.ReasoningContent
		}
		if response.Choices[0].FinishReason != "" {
			finishReason = response.Choices[0].FinishReason
		}
	}

	usage = map[string]any{
		"prompt_tokens":     response.Usage.PromptTokens,
		"completion_tokens": response.Usage.CompletionTokens,
		"total_tokens":      response.Usage.TotalTokens,
	}
	return content, finishReason, usage, nil
}

// postJSON issues a ctx-bound POST with a JSON body so a per-job deadline
// cancels the outbound request (issue #548 watchdog).
func (h *LLMInferenceHandler) postJSON(ctx context.Context, url string, payload map[string]any) (*http.Response, error) {
	reqBody, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return h.client().Do(req)
}

func (h *LLMInferenceHandler) client() *http.Client {
	if h.httpClient != nil {
		return h.httpClient
	}
	return http.DefaultClient
}

// writeSingleChunk emits one chunk at index 0 when a stream is present (the
// non-streaming paths still emit a single chunk for parity with streaming, so a
// pub/sub subscriber sees content before the terminal event). A nil stream is a
// no-op (used by unit tests exercising the buffered parse directly).
func writeSingleChunk(stream StreamWriter, content string) {
	if stream != nil {
		stream.WriteChunk(content, 0)
	}
}

func (h *LLMInferenceHandler) success(output map[string]any) *JobResult {
	return &JobResult{Status: JobStatusSuccess, Output: output}
}

// promptTextFromPayload builds the plain-text "input" side of a grounding
// check from an inference payload: Messages when present, else Prompt.
// Messages-first matches what this handler's ONLY caller of this function
// (bufferedChatCompletions, reached exclusively through
// executeChatCompletionsAt) actually sent to the engine — every dispatch
// site that routes there does so specifically because `len(payload.Messages)
// > 0` (see Execute/executeVLLM/executeLlamaCppAt), and
// executeChatCompletionsAt builds its outbound request body from Messages
// alone, never Prompt. A payload that happened to carry a stray Prompt
// alongside Messages must not have the guardrail compare the output against
// text the model never saw. ChatMessage.Text() already strips multimodal
// content parts down to their text; messages are joined in order so the
// guardrail sees the full conversation context, not just the latest turn.
func promptTextFromPayload(payload *jobs.LLMInferencePayload) string {
	if payload == nil {
		return ""
	}
	if len(payload.Messages) > 0 {
		parts := make([]string, 0, len(payload.Messages))
		for _, m := range payload.Messages {
			if t := m.Text(); t != "" {
				parts = append(parts, t)
			}
		}
		return strings.Join(parts, "\n")
	}
	return payload.Prompt
}

// groundingGuardrailEnvVar opts a node into attaching the grounding-guardrail
// receipt (see groundingGuardrailEnabled). Named distinctly from the
// job-payload-level toggles elsewhere in this file because it is a NODE
// setting, not something the platform sends per job.
const groundingGuardrailEnvVar = "CITADEL_GROUNDING_GUARDRAIL"

// groundingGuardrailEnabled reports whether bufferedChatCompletions should run
// and attach the on-node grounding guardrail (citadel #8253). Default OFF,
// like every other advisory-signal toggle in this codebase
// (CITADEL_ENERGY_SAMPLING, SERVICE_AUTO_STOP_WHEN_IDLE) — see the doc comment
// on bufferedChatCompletions for why unconditional attachment would be noisy
// for this handler's non-extraction traffic.
func groundingGuardrailEnabled() bool {
	return update.IsTruthy(os.Getenv(groundingGuardrailEnvVar))
}

// signAEPReceiptsEnvVar opts a node into SIGNING the grounding receipt
// (aceteam #8253's deferred signing half, internal/aep) with the node's
// internal/nodeidentity ECDSA key. A separate toggle from
// groundingGuardrailEnvVar, per docs/design-node-identity-receipts.md §5
// Phase 3 — a node can run the (cheap, local, non-cryptographic) grounding
// check without ever touching a private key, and only opts into signing
// deliberately.
const signAEPReceiptsEnvVar = "CITADEL_SIGN_AEP_RECEIPTS"

// signAEPReceiptsEnabled reports whether bufferedChatCompletions should
// additionally sign the grounding receipt into a verifiable AEP receipt
// (internal/aep.AEPReceiptV1). Default OFF, matching this codebase's
// advisory-signal convention. Inert today in the sense that matters to a
// verifier: the backend does not yet hold this node's public key to check
// the signature against (design doc §3/§4, Phase 2, not done here) — see
// this file's doc comment on the call site for what "inert" does and does
// not mean here.
func signAEPReceiptsEnabled() bool {
	return update.IsTruthy(os.Getenv(signAEPReceiptsEnvVar))
}

// groundingReceiptMap shapes an already-computed trust.GroundingResult
// (citadel #8253 guardrail half) as an advisory receipt attached alongside
// the primary "content" field — mirroring the synthesizeReceiptFromHeaders
// precedent (internal/jobs/synthesize_speech.go): the guardrail's job is to
// FLAG, not block, so a result never fails or withholds content because of
// what this returns. Policy is PolicyFlag (default, never gates) here;
// gating is a documented follow-up for a caller that wants HITL review of
// ungrounded results.
//
// Takes the GroundingResult directly (rather than running
// trust.CheckGrounding itself, as this function did before the aep receipt
// signing addition) so bufferedChatCompletions can compute it ONCE and reuse
// it for both this map and, when CITADEL_SIGN_AEP_RECEIPTS is also on, the
// signed receipt — trust.CheckGrounding is a pure function of the same
// (input, output) pair either way, so this is a refactor, not a behavior
// change.
//
// claims_checked (the eligible-claim denominator behind score) is included
// deliberately: without it, a claim-free prose reply and a reply with ten
// verified statistics both report {grounded: true, score: 1.0} and are
// indistinguishable to a consumer — see GroundingResult.Grounded's doc
// comment in internal/trust for why score alone must not be read as
// "verified true".
func groundingReceiptMap(result trust.GroundingResult) map[string]any {
	flagged := make([]map[string]any, 0, len(result.Flagged))
	for _, c := range result.Flagged {
		flagged = append(flagged, map[string]any{
			"value":  c.Value,
			"kind":   string(c.Kind),
			"reason": c.Reason,
		})
	}
	return map[string]any{
		"grounded":       result.Grounded,
		"score":          result.Score,
		"claims_checked": result.ClaimsChecked,
		"flagged":        flagged,
	}
}

// warming returns the structured model_warming result (citadel-cli#632) for an
// engine that is not serving yet: a swap that did not become ready within the
// wait budget, an engine that is up but still loading (citadel-cli#680), or one
// that dropped the connection mid-handshake. It is a SUCCESS result (so the
// runner WriteEnds + Acks it) carrying a control payload rather than assistant
// content — deliberately no WriteChunk, so the platform relays the warming
// signal instead of streaming it as a reply. The platform inspects
// output.status == "model_warming" and retries after retry_after seconds.
//
// retryAfterSeconds <= 0 falls back to the standard hint. A caller that knows
// better (e.g. a swap for a DIFFERENT model is holding the single-flight slot,
// so this model's load has not even started) passes its own, so the platform
// does not busy-retry against a node that is not working on its request.
//
// warmingFor names the model actually loading right now (citadel-cli#681),
// which may differ from `model` — the discriminator that lets a caller tell
// "loading yours" from "busy with someone else's, yours not started" apart,
// rather than both rendering as the same warming response. Additive:
// warmingFor == "" omits `warming_for` from the output entirely, so a caller
// parsing the pre-#681 contract sees no change.
func (h *LLMInferenceHandler) warming(model string, etaSeconds, retryAfterSeconds int, warmingFor string) *JobResult {
	if etaSeconds < 0 {
		etaSeconds = 0
	}
	if retryAfterSeconds <= 0 {
		retryAfterSeconds = warmingRetryAfter
	}
	output := map[string]any{
		"status":      "model_warming",
		"model":       model,
		"eta_seconds": etaSeconds,
		"retry_after": retryAfterSeconds,
	}
	if warmingFor != "" {
		output["warming_for"] = warmingFor
	}
	return &JobResult{
		Status: JobStatusSuccess,
		Output: output,
	}
}

// engineRequestFailure maps an outbound engine request error to a job result. A
// transport-level error means the engine was not listening or dropped the
// connection, which on a loading engine is warming, not a fault: it is answered
// with the typed signal so `use of closed network connection` never reaches a
// caller (citadel-cli#680). Anything else stays a genuine failure.
func (h *LLMInferenceHandler) engineRequestFailure(
	payload *jobs.LLMInferencePayload,
	err error,
	wrap string,
) *JobResult {
	if isEngineNotServing(err) {
		// Same-model case, as in the readiness-gate warming above.
		return h.warming(payload.Model, engineWarmETA(payload.Backend), 0, payload.Model)
	}
	// A refused connection here means the engine went away between the readiness
	// probe and the request. That is warming ONLY while a start this node issued
	// is still inside its load window; otherwise nothing is listening and no
	// amount of retrying will change that, so it stays a failure the caller can
	// act on (citadel-cli#705).
	if isConnectionRefused(err) && h.engineStartInFlight(payload.Backend) {
		return h.warming(payload.Model, engineWarmETA(payload.Backend), 0, payload.Model)
	}
	return h.failure(fmt.Errorf("%s: %w", wrap, err))
}

// unavailable reports that the node is deliberately declining to serve this
// model right now, for a named reason — as opposed to warming (it is coming) or
// a plain failure (something broke). It is a FAILURE result carrying
// `status: "model_unavailable"` and a `reason` code alongside the human message.
//
// Why a failure and not a success-shaped control payload like warming: the
// platform branches on `output.status == "model_warming"` and has no branch for
// anything else, so an unrecognized control status returned as a success is
// relayed as a successful empty reply — the user asks a question and gets
// nothing, with no error recorded anywhere. A failure is legible to every
// consumer that exists today; the structured `reason` is additive on top, for a
// consumer that later wants to render "this node is at its swap limit" specially.
func (h *LLMInferenceHandler) unavailable(model, reason string, err error) *JobResult {
	return &JobResult{
		Status: JobStatusFailure,
		Error:  err,
		Output: map[string]any{
			"status": "model_unavailable",
			"reason": reason,
			"model":  model,
			"error":  err.Error(),
		},
	}
}

func (h *LLMInferenceHandler) failure(err error) *JobResult {
	return &JobResult{
		Status: JobStatusFailure,
		Error:  err,
		Output: map[string]any{"error": err.Error()},
	}
}

// Ensure LLMInferenceHandler implements JobHandler.
var _ JobHandler = (*LLMInferenceHandler)(nil)
