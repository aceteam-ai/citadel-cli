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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/aep"
	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/network"
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
	// Defaults to defaultAEPSigner() -- see that function's doc comment for
	// why it is NOT nodeidentity.Default() -- in the constructor; overridable
	// via WithSigner so tests never touch the real host's filesystem.
	//
	// MACHINE-CONVERGENT BY CONSTRUCTION, AND THE SAME KEY THE CA REGISTERED
	// (K-A, docs/design-trust-receipt-v2.md §4, aceteam#8253): unlike
	// nodeidentity.Default() (rooted at invoker-scoped platform.ConfigDir(),
	// see CLAUDE.md's ConfigDir()/GetNodeConfigDir() section and
	// citadel-cli#845/#726/#696/#383), nodeidentity.Convergent roots this key
	// at network.GetNodeConfigDir() -- the SAME machine-convergent directory
	// `citadel init`'s device-config write (#845) and #726's heartbeat marker
	// already use. A systemd-root `citadel work` and an interactive non-root
	// process therefore resolve the IDENTICAL signing key file.
	// TestDefaultAEPSigner_MachineConvergentAcrossInvocationContexts pins this.
	//
	// Crucially, Convergent is now ALSO what every CSR/enrollment site uses
	// (cmd/init.go:ensureNodeIdentity, cmd/device.go), so the signing key and
	// the CSR key whose SPKI the fabric CA signed into fabric_node_certs are
	// ONE key -- previously they were two (signer at GetNodeConfigDir, CSR at
	// Default()/ConfigDir), so a real receipt's public_key_fingerprint could
	// never match a fabric_node_certs row. Convergent's first-use read-through
	// adopts a key already registered under the legacy platform.ConfigDir()
	// location (byte-identical, so an issued leaf stays valid); see
	// nodeidentity.reconcileFromLegacy.
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
		signer:          defaultAEPSigner(),
		fabricNodeID:    func() string { return config.LoadDeviceCredsConverged().FabricNodeID },
		aepLogf:         log.Printf,
	}
}

// aepIdentitySubdir names the subdirectory (under the AEP signing store's
// root) the key files live in, mirroring nodeidentity's own "identity"
// convention (nodeidentity.dirName, unexported) so the on-disk layout is
// familiar -- just rooted at a different, machine-convergent parent.
const aepIdentitySubdir = "identity"

// aepSigningStoreDir returns the directory the AEP signing key is rooted at for
// a given machine-convergent node config dir: <nodeConfigDir>/identity.
//
// Retained as the TEST ORACLE for the signer's resolved path: defaultAEPSigner
// now goes through nodeidentity.Convergent (which appends nodeidentity.dirName,
// == "identity"), so this is an INDEPENDENT re-derivation the signer tests
// assert against (TestNewLLMInferenceHandler_SignerIsMachineConvergentNotInvokerScoped,
// TestAEPSigningStoreDir_PureFunctionOfNodeConfigDir) rather than production
// path resolution.
func aepSigningStoreDir(nodeConfigDir string) string {
	return filepath.Join(nodeConfigDir, aepIdentitySubdir)
}

// defaultAEPSigner returns the default signer for the AEP receipt (aceteam
// #8253): the machine-convergent nodeidentity.Store rooted at
// network.GetNodeConfigDir() (via nodeidentity.Convergent) -- NOT
// nodeidentity.Default(), which is rooted at invoker-scoped
// platform.ConfigDir() and would silently diverge between a systemd-root
// `citadel work` and an interactive `citadel init`. This is the SAME
// convergent store the CSR/enrollment sites now use (cmd/init.go,
// cmd/device.go), so the signing key IS the key the fabric CA registered
// (K-A, docs/design-trust-receipt-v2.md §4); see the `signer` field's doc
// comment on LLMInferenceHandler for the full reasoning.
func defaultAEPSigner() aep.Signer {
	return nodeidentity.Convergent(network.GetNodeConfigDir())
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
// tests that want a hermetic in-memory signer instead of the default
// nodeidentity.Convergent store (which reads/writes real key material under
// network.GetNodeConfigDir() and reads through the legacy platform.ConfigDir()).
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
			// A serveability preflight refusal (citadel-cli#956): the image was
			// GC'd, the weights were swept, or the disk is under pressure. Same
			// posture as the rate-limit branch above -- a named, machine-readable
			// reason, never a model_warming success that would just burn the
			// wait budget on a pull that was always going to fail.
			var preflightErr *SwapPreflightBlockedError
			if errors.As(swapErr, &preflightErr) {
				return h.unavailable(payload.Model, preflightErr.Reason, swapErr), nil
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

	// Single consolidated post-completion hook (citadel #1001, aceteam #8253
	// S1): every backend branch below funnels through THIS one result
	// variable before Execute returns, so applyTrustEngine runs exactly ONCE
	// per job regardless of which of the ten buffered/streaming engine
	// functions actually produced result.Output["content"] -- closing G6
	// (before this, only bufferedChatCompletions attached a verdict, so the
	// streaming agent-chat path and every llamacpp/ollama pair left the node
	// with no verdict at all). See applyTrustEngine's doc comment for the
	// exact no-op conditions (a failure/model_warming/model_unavailable
	// result, or the guardrail simply being off, leave Output untouched).
	var result *JobResult
	switch payload.Backend {
	case "vllm":
		result, err = h.executeVLLM(ctx, stream, payload, job.ID)
	case "sglang":
		result, err = h.executeSGLang(ctx, stream, payload)
	case "ollama":
		result, err = h.executeOllama(ctx, stream, payload)
	case "llamacpp":
		result, err = h.executeLlamaCppAt(ctx, stream, payload, h.baseURL("llamacpp"), job.ID)
	case "bonsai":
		// Bonsai serves the identical llama.cpp-server API on its own host port
		// (PrismML fork). Reuse the llama.cpp request/stream path pointed at it.
		result, err = h.executeLlamaCppAt(ctx, stream, payload, h.baseURL("bonsai"), job.ID)
	case "unlimited-ocr":
		// Baidu Unlimited-OCR is a vLLM OpenAI engine on its own host port; use the
		// chat-completions path so multimodal image_url content (#625) reaches it.
		result, err = h.executeChatCompletionsAt(ctx, stream, payload, h.baseURL("unlimited-ocr"), job.ID)
	default:
		return h.failure(fmt.Errorf("unsupported backend: %s", payload.Backend)), nil
	}
	h.applyTrustEngine(payload, job.ID, result)
	return result, err
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
//
// Tool calling (citadel-cli#603, the ollama half): Ollama's native /api/chat
// DOES support tools -- a `tools` array on the request (OpenAI-shaped
// function definitions, forwarded as-is) and `message.tool_calls` on the
// response. Two wire-shape differences from the OpenAI-compatible engines
// (executeChatCompletionsAt) are handled at the boundary rather than leaking
// into the rest of this file's OpenAI-shaped contract: Ollama returns
// `function.arguments` as a JSON OBJECT (OpenAI/this worker's contract is a
// JSON STRING containing serialized JSON -- see ollamaToolCallsToOpenAI), and
// Ollama assigns no `id` to a tool call (a synthetic call_<n> is generated on
// the way out; see the same function). tool_choice is deliberately NOT
// forwarded: Ollama's support for it is inconsistent across
// versions/models, unlike the OpenAI-compatible engines' native support.
func (h *LLMInferenceHandler) executeOllamaChat(ctx context.Context, stream StreamWriter, payload *jobs.LLMInferencePayload) (*JobResult, error) {
	// Ollama's /api/chat takes text content (images ride a separate `images`
	// field we do not populate), so flatten any multimodal content to its text
	// parts. OCR/vision fabric models run on vLLM (executeChatCompletionsAt), not
	// this path.
	messages := make([]map[string]any, 0, len(payload.Messages))
	for _, m := range payload.Messages {
		msg := map[string]any{"role": m.Role, "content": m.Text()}
		// Replay an assistant turn's prior tool_calls (OpenAI shape, as stored
		// on ChatMessage) converted to Ollama's request shape -- see
		// openAIToolCallsToOllama's doc comment for exactly what changes.
		// hasToolCalls (not a bare len() check) gates this so a `null`/`[]`
		// ToolCalls value on an ordinary turn is correctly treated as absent.
		if hasToolCalls(m.ToolCalls) {
			if converted := openAIToolCallsToOllama(m.ToolCalls); converted != nil {
				msg["tool_calls"] = converted
			}
		}
		// Ollama's tool-role message has no OpenAI-style tool_call_id
		// correlation field; forwarding it anyway is harmless (an unknown
		// JSON key Ollama ignores) and keeps this message shape uniform with
		// executeChatCompletionsAt's. `name` (the function name on a
		// role="tool" message) IS meaningful to Ollama.
		if m.ToolCallID != "" {
			msg["tool_call_id"] = m.ToolCallID
		}
		if m.Name != "" {
			msg["name"] = m.Name
		}
		messages = append(messages, msg)
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
	// Tool calling (citadel-cli#603): only attach `tools` when the payload
	// actually carries a non-empty array. hasToolCalls -- not a bare
	// len(payload.Tools) > 0 -- is the guard here specifically because a
	// literal `"tools": null` (4 bytes, len()>0) must read as absent, so a
	// text-only request's outbound body gains no new key.
	if hasToolCalls(payload.Tools) {
		reqPayload["tools"] = payload.Tools
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
//
// Tool-call handling (citadel-cli#603): unlike OpenAI's streaming delta.
// tool_calls (which arrive character-by-character across many frames and need
// toolCallAccumulator to merge), Ollama emits a tool call already complete in
// a single frame's message.tool_calls -- there is no partial/incremental
// tool_calls shape in Ollama's protocol. So the latest non-empty sighting
// across frames IS the final answer; no accumulator is needed here.
func (h *LLMInferenceHandler) streamOllamaChat(stream StreamWriter, body io.Reader) (*JobResult, error) {
	scanner := bufio.NewScanner(body)
	// Allow long JSON lines (large deltas) beyond bufio's default 64KiB cap.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	chunkIndex := 0
	var fullContent strings.Builder
	var promptTokens, completionTokens int
	var toolCalls json.RawMessage

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var chunk struct {
			Message struct {
				Content   string           `json:"content"`
				ToolCalls []ollamaToolCall `json:"tool_calls"`
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
		if len(chunk.Message.ToolCalls) > 0 {
			if converted := ollamaToolCallsToOpenAI(chunk.Message.ToolCalls); converted != nil {
				toolCalls = converted
			}
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
	finishReason := "stop"
	output := map[string]any{
		"content":       fullContent.String(),
		"finish_reason": finishReason,
		"usage":         ollamaUsage(promptTokens, completionTokens),
	}
	if len(toolCalls) > 0 {
		output["finish_reason"] = "tool_calls"
		output["tool_calls"] = toolCalls
	}
	return h.success(output), nil
}

// bufferedOllamaChat parses a non-streamed Ollama /api/chat response,
// including message.tool_calls (citadel-cli#603) when present.
func (h *LLMInferenceHandler) bufferedOllamaChat(stream StreamWriter, body io.Reader) (*JobResult, error) {
	bodyBytes, err := io.ReadAll(body)
	if err != nil {
		return h.failure(err), nil
	}
	var response struct {
		Message struct {
			Content   string           `json:"content"`
			ToolCalls []ollamaToolCall `json:"tool_calls"`
		} `json:"message"`
		PromptEvalCount int `json:"prompt_eval_count"`
		EvalCount       int `json:"eval_count"`
	}
	if err := json.Unmarshal(bodyBytes, &response); err != nil {
		return h.failure(fmt.Errorf("failed to parse Ollama response: %w", err)), nil
	}
	toolCalls := ollamaToolCallsToOpenAI(response.Message.ToolCalls)
	// A tool_calls-only reply has nothing worth publishing as a chunk --
	// mirrors bufferedChatCompletions' identical rule for the OpenAI-
	// compatible engines.
	if response.Message.Content != "" || len(toolCalls) == 0 {
		writeSingleChunk(stream, response.Message.Content)
	}
	output := map[string]any{
		"content":       response.Message.Content,
		"finish_reason": "stop",
		"usage":         ollamaUsage(response.PromptEvalCount, response.EvalCount),
	}
	if len(toolCalls) > 0 {
		output["finish_reason"] = "tool_calls"
		output["tool_calls"] = toolCalls
	}
	return h.success(output), nil
}

// ollamaToolCall is one entry in Ollama's native message.tool_calls array
// (citadel-cli#603). Unlike OpenAI's shape, Ollama assigns no `id`/`type` and
// returns `function.arguments` as a JSON OBJECT rather than a JSON-encoded
// string -- see ollamaToolCallsToOpenAI for the conversion this drives.
type ollamaToolCall struct {
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

// ollamaToolCallsToOpenAI converts Ollama's native tool_calls into the OpenAI
// shape every other engine path in this file returns (id, type: "function",
// function.arguments as a JSON STRING) -- the shape the aceteam consumer
// (agents/fabric_client.py) and #603's OpenAI-compatible-engine path both
// already rely on. Ollama assigns no id, so a stable synthetic one
// (call_<index>) is generated; it only needs to be unique within THIS
// response for a follow-up tool-role message to reference, which Ollama
// itself does not validate (unlike OpenAI). Returns nil for no calls, so a
// text-only reply's output map gains no new key.
func ollamaToolCallsToOpenAI(calls []ollamaToolCall) json.RawMessage {
	if len(calls) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(calls))
	for i, c := range calls {
		args := "{}"
		if len(c.Function.Arguments) > 0 && string(c.Function.Arguments) != "null" {
			args = string(c.Function.Arguments)
		}
		out = append(out, map[string]any{
			"id":   fmt.Sprintf("call_%d", i),
			"type": "function",
			"function": map[string]any{
				"name":      c.Function.Name,
				"arguments": args,
			},
		})
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil
	}
	return b
}

// openAIToolCallsToOllama converts an OpenAI-shaped tool_calls array (as
// stored on ChatMessage.ToolCalls -- id, type, function.arguments as a JSON
// STRING) into Ollama's native request shape (function.arguments as a JSON
// OBJECT, no id/type -- Ollama does not echo one back and does not require
// one on replayed history). Used when forwarding a prior assistant turn's
// tool_calls to Ollama as conversation history. Returns nil when raw has no
// parseable entries; the call site (hasToolCalls) has already gated presence,
// so a parse failure here degrades to "omit tool_calls from this replayed
// turn" rather than failing the whole request.
func openAIToolCallsToOllama(raw json.RawMessage) []map[string]any {
	var calls []struct {
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &calls); err != nil {
		return nil
	}
	out := make([]map[string]any, 0, len(calls))
	for _, c := range calls {
		var args any
		switch {
		case c.Function.Arguments == "":
			args = map[string]any{}
		default:
			if err := json.Unmarshal([]byte(c.Function.Arguments), &args); err != nil {
				// Not valid JSON (shouldn't happen for a well-formed OpenAI
				// tool call) -- forward the raw string rather than dropping
				// the argument entirely.
				args = c.Function.Arguments
			}
		}
		out = append(out, map[string]any{
			"function": map[string]any{
				"name":      c.Function.Name,
				"arguments": args,
			},
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
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
	//
	// tool_calls/tool_call_id/name (citadel-cli#603) are attached ONLY when the
	// message actually carries them, so a plain user/assistant/system turn's
	// wire shape is byte-for-byte what it was before #603 — no new keys, no
	// engine that validates message shape strictly ever sees them.
	messages := make([]map[string]any, 0, len(payload.Messages))
	for _, m := range payload.Messages {
		msg := map[string]any{"role": m.Role, "content": m.ContentJSON()}
		if hasToolCalls(m.ToolCalls) {
			msg["tool_calls"] = m.ToolCalls
		}
		if m.ToolCallID != "" {
			msg["tool_call_id"] = m.ToolCallID
		}
		if m.Name != "" {
			msg["name"] = m.Name
		}
		messages = append(messages, msg)
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
	// Tool calling (citadel-cli#603, aceteam #6555, citadel-cli#933): only
	// attach `tools`/`tool_choice` when the job payload actually carries them,
	// so a text-only request's outbound body gains no new keys --
	// vLLM/llama.cpp/bonsai's OpenAI-compatible /v1/chat/completions already
	// accept both natively. hasToolCalls -- not a bare len(payload.Tools) > 0
	// -- gates `tools` for the same reason it gates m.ToolCalls above: a
	// literal `"tools": null` (4 bytes) or `"tools": []` (2 bytes) both pass a
	// bare length check despite meaning "no tools", so a payload carrying
	// either forwarded a `"tools": null`/`"tools": []` key to the engine
	// instead of omitting it (citadel-cli#933; the ollama path already got
	// this right at its own hasToolCalls(payload.Tools) call above). `tools`
	// is an OpenAI array like tool_calls, so hasToolCalls' array-unmarshal
	// check applies directly. `tool_choice` is NOT an array (OpenAI's shape is
	// a string -- "auto"/"none"/"required" -- or an object), so it uses
	// hasJSONValue instead, which only rejects an explicit null/empty value
	// rather than requiring array shape.
	if hasToolCalls(payload.Tools) {
		reqPayload["tools"] = payload.Tools
		if hasJSONValue(payload.ToolChoice) {
			reqPayload["tool_choice"] = payload.ToolChoice
		}
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
	return h.bufferedChatCompletions(stream, resp.Body)
}

// bufferedChatCompletions parses a buffered OpenAI chat-completions response and
// emits the assistant's message content as a single chunk.
//
// Before citadel #1001 (aceteam #8253 S1) this was the ONE wired integration
// point for the on-node grounding guardrail/signed-receipt hook: it was the
// only place where the full request input and the full model output both
// already existed as Go strings before the result left the node. That hook
// now lives at Execute's single dispatch point instead (applyTrustEngine),
// so every buffered AND streaming engine path gets it uniformly -- this
// function only parses the response and shapes "content"/"finish_reason"/
// "usage"/"tool_calls" now, same as every other buffered-response parser in
// this file.
func (h *LLMInferenceHandler) bufferedChatCompletions(stream StreamWriter, body io.Reader) (*JobResult, error) {
	bodyBytes, err := io.ReadAll(body)
	if err != nil {
		return h.failure(err), nil
	}
	content, finishReason, toolCalls, usage, err := parseChatCompletionResponse(bodyBytes)
	if err != nil {
		return h.failure(err), nil
	}
	// A tool_calls-only reply (content=="" alongside a populated tool_calls
	// array — the common shape for a model that decided to call a tool rather
	// than answer in text) has nothing worth publishing as a chunk; the
	// pre-#603 single-parity-chunk behavior for a genuine text reply (even an
	// empty one) is unchanged.
	if content != "" || len(toolCalls) == 0 {
		writeSingleChunk(stream, content)
	}
	output := map[string]any{
		"content":       content,
		"finish_reason": finishReason,
		"usage":         usage,
	}
	if len(toolCalls) > 0 {
		output["tool_calls"] = toolCalls
	}
	return h.success(output), nil
}

// buildAEPReceiptV2 resolves node_id (aceteam #8139's fabric node ID when
// known, else the signer's own public-key fingerprint -- internal/aep.
// ResolveNodeID's phasing fallback) and signs the v2 AEP receipt with h.signer.
// The content/verdict fields (input/output/policy digests, action, verdict_hash)
// are passed in via aep.V2Inputs -- they were computed once in applyTrustEngine
// so the signed verdict_hash matches the unsigned trust_verdict.verdict_hash.
func (h *LLMInferenceHandler) buildAEPReceiptV2(jobID string, payload *jobs.LLMInferencePayload, result trust.GroundingResult, in aep.V2Inputs) (*aep.AEPReceiptV2, error) {
	var fabricNodeID string
	if h.fabricNodeID != nil {
		fabricNodeID = h.fabricNodeID()
	}
	nodeID, err := aep.ResolveNodeID(h.signer, fabricNodeID)
	if err != nil {
		return nil, err
	}
	return aep.BuildSignedReceiptV2(h.signer, nodeID, jobID, payload.Backend, payload.Model, in, result, time.Now())
}

// applyTrustEngine is the single, consolidated post-completion Trust Engine
// hook (citadel #1001, aceteam #8253 S1). Execute calls it exactly ONCE,
// after its backend switch has produced a result from whichever of the ten
// buffered/streaming engine functions actually served the request (vLLM/
// SGLang text-completions, ollama /api/generate, ollama /api/chat, llama.cpp/
// bonsai /completion, and the shared OpenAI-compatible /v1/chat/completions
// path -- each buffered AND streaming) -- rather than duplicating this call
// inside each of those ten functions. Every one of them already sets
// result.Output["content"] before returning, so hooking here closes #8253's
// G6 gap (before this, only bufferedChatCompletions attached anything, so
// the streaming agent-chat path and every llamacpp/ollama pair left the node
// with no verdict at all) with one call site instead of ten.
//
// A no-op (result.Output left byte-identical) whenever:
//   - result is nil, or result.Status != JobStatusSuccess, or result.Output
//     is nil -- a failure/model_unavailable result carries no content worth
//     checking, and mutating an error result's Output here would be an
//     unrelated side effect on a code path this hook has no business
//     touching.
//   - result.Output has no "content" key as a string -- a model_warming
//     success result carries status/model/eta_seconds instead of content,
//     and there is nothing to check yet (the actual reply has not been
//     produced).
//   - CITADEL_GROUNDING_GUARDRAIL is off (the default) -- matching this
//     repo's advisory-signal opt-in convention exactly as before this
//     change; a disabled node's Output is unchanged from before #1001.
//
// Deliberately a POST-completion hook, never a gate: by the time this runs,
// a streamed reply is already fully on the wire (tokens went out via
// stream.WriteChunk before Execute ever sees the final Output), so it can
// only flag, never withhold -- see the (preserved) rationale on
// groundingGuardrailEnabled/bufferedChatCompletions' old doc comment. The
// signed receipt is the fifteen-field v2 shape (aep.AEPReceiptV2, aceteam
// #8253 S3): it binds the input/output/policy digests and the trust verdict
// (buildAEPReceiptV2 below). The v1 primitive (aep.AEPReceiptV1) is retained
// but no longer emitted.
func (h *LLMInferenceHandler) applyTrustEngine(payload *jobs.LLMInferencePayload, jobID string, result *JobResult) {
	if result == nil || result.Status != JobStatusSuccess || result.Output == nil {
		return
	}
	content, ok := result.Output["content"].(string)
	if !ok {
		return
	}
	if !groundingGuardrailEnabled() {
		return
	}
	input := promptTextFromPayload(payload)
	grounding := trust.CheckGrounding(input, content)

	// Build the whole Trust Engine verdict ONCE and reuse it for both the
	// unsigned trust_verdict map and (when signing) the signed v2 receipt, so
	// the two verdict_hash values can never diverge (aceteam #8253 S3, design
	// §7). The three content-binding digests are also computed once here:
	//   - output_sha256: sha256:hex(content) -- the final content string every
	//     engine path already set (matches the backend's _expected_output_sha256).
	//   - input_sha256: sha256:hex(input). For a bare-prompt payload this is
	//     byte-identical to the backend's _expected_input_sha256 (a JSON string
	//     round-trips to the same Go string). For a MESSAGES payload it is the
	//     node's own \n-joined view of the checked input and does NOT match the
	//     backend's json.dumps(messages) -- content_bound stays false for
	//     messages until the raw-payload retention decision lands (design §2.2
	//     option I-A / open question §11.4). This does not affect the signature
	//     (input_sha256 is opaque in the canon) and both content gates are OFF
	//     by default.
	//   - policy_hash: aep.EmptyPolicyHash (hash of the empty policy {}), the
	//     interim value until S4/S5 policy delivery lands (design §2.3).
	outputSHA := sha256Hex(content)
	inputSHA := sha256Hex(input)
	policyHash := aep.EmptyPolicyHash
	verdict := trust.BuildVerdict(input, content, grounding, trust.DefaultDetectors(), policyHash)

	result.Output["grounding"] = groundingReceiptMap(grounding)
	result.Output["trust_verdict"] = trustVerdictMap(verdict, grounding, inputSHA, outputSHA, policyHash)

	// Signed AEP receipt (aceteam #8253 S3, internal/aep -- the signing half
	// deferred at citadel#847's merge). Nested INSIDE the grounding-guardrail
	// gate deliberately: the receipt signs THIS verdict, so signing it when the
	// guardrail itself is off would mean signing a check that was never
	// surfaced anywhere else. A second, independent opt-in (signAEPReceiptsEnabled)
	// gates signing on top of that -- default OFF, so a guardrail-on-but-signing-off
	// node's output carries "grounding"/"trust_verdict" but no "aep_receipt".
	if !signAEPReceiptsEnabled() {
		return
	}
	receipt, err := h.buildAEPReceiptV2(jobID, payload, grounding, aep.V2Inputs{
		InputSHA256:  inputSHA,
		OutputSHA256: outputSHA,
		PolicyHash:   policyHash,
		Action:       verdict.Action,
		VerdictHash:  verdict.VerdictHash,
	})
	if err != nil {
		// Fail open: signing must never break inference. Mirrors
		// internal/nodeidentity's own fail-open convention for its other
		// consumer (the mTLS CSR/leaf flow, cmd/init.go's ensureNodeIdentity)
		// -- a node whose key is unavailable, OR a receipt whose free-string
		// fields collide with the canonical delimiter (BuildSignedReceiptV2's
		// refuse-to-sign guard), simply serves without a signed receipt.
		h.aepLogf("[aep] failed to build signed receipt for job %s (non-fatal): %v", jobID, err)
		return
	}
	receiptMap, err := receipt.ToMap()
	if err != nil {
		// Attaching *aep.AEPReceiptV2 directly would be the only typed Go
		// pointer in this map -- see ToMap's doc comment for why that's
		// unsafe across this map's eventual wire serialization. This branch
		// should be unreachable (the struct is always JSON-marshalable) but
		// is handled the same fail-open way regardless.
		h.aepLogf("[aep] failed to shape signed receipt for job %s (non-fatal): %v", jobID, err)
		return
	}
	result.Output["aep_receipt"] = receiptMap
}

// trustVerdictMap shapes the unsigned, human-readable "trust_verdict" map
// (aceteam #8253's Trust Engine naming; distinct from the legacy "grounding"
// map, which is kept verbatim alongside it for continuity with pre-#1001
// consumers). It takes an ALREADY-BUILT verdict (assembled once in
// applyTrustEngine via trust.BuildVerdict) plus the three content-binding
// digests, so this unsigned map and the signed v2 receipt carry the identical
// verdict_hash / input_sha256 / output_sha256 / policy_hash -- they must never
// diverge (#8253 S3, design §7).
//
// The digests are all UNSIGNED convenience copies here (the signed originals
// live in the v2 aep_receipt). input_sha256/output_sha256/policy_hash are added
// alongside verdict_hash (design §7.2) so the unsigned verdict is
// self-describing and content-recomputable without the signed receipt.
func trustVerdictMap(v trust.Verdict, grounding trust.GroundingResult, inputSHA, outputSHA, policyHash string) map[string]any {
	return map[string]any{
		"action":        v.Action,
		"input_sha256":  inputSHA,
		"output_sha256": outputSHA,
		"policy_hash":   policyHash,
		"verdict_hash":  v.VerdictHash,
		"checks":        v.CheckMaps(),
		"grounding":     groundingReceiptMap(grounding),
	}
}

// sha256Hex returns the "sha256:<hex>" digest of s, matching the digest
// format aceteam's provenance_receipts.py already uses (see #8253's delta
// DoR, G9) so a future cross-repo comparison needs no reformatting.
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// streamChatCompletions translates an OpenAI chat-completions SSE stream into
// chunks. Each `data:` frame carries a choices[].delta. The answer is streamed
// from delta.content (matching the buffered path and standard OpenAI clients).
// Thinking models like Bonsai emit the chain-of-thought in delta.reasoning_content
// and the answer in delta.content; the reasoning is accumulated but only surfaced
// if the stream ends with NO answer AND no tool call (token budget spent
// mid-reasoning), so a reply is never silently blank while staying consistent
// with the non-stream content-only result. A thinking model that decided to
// call a tool instead (citadel-cli#603) has real reasoning_content but nothing
// there is the visible reply, so it must not be promoted to `content` there
// either -- see parseChatCompletionResponse's doc comment for the identical
// buffered-path reasoning.
//
// Tool-call deltas (choices[].delta.tool_calls) are accumulated across chunks
// via toolCallAccumulator and surfaced ONLY in the returned JobResult.Output
// (the Runner's terminal WriteEnd, per this file's package doc comment) --
// deliberately NOT emitted as per-chunk deltas via stream.WriteChunk, because
// StreamWriter.WriteChunk(content string, index int) has no field for
// structured data; widening it would ripple through RedisStreamWriter/
// APIStreamWriter/NoOpStreamWriter, internal/redis and internal/redisapi's
// PublishChunk, and every existing caller, for a payload the consumer does not
// need: aceteam/python-backend's FabricInferenceClient._event_to_chunk (the
// real consumer, agents/fabric_client.py) already reads tool_calls from the
// "end" event's `data.result` -- exactly what this function returns here --
// and its "chunk" event branch drops a delta entirely when it carries neither
// text nor tool_calls, so a node that never emits per-chunk deltas is not a
// regression for that consumer today. Mirrors the citadel-cli#717 "a guessed
// field is worse than no field" precedent for exactly this kind of interface
// widening with no verified consumer.
func (h *LLMInferenceHandler) streamChatCompletions(stream StreamWriter, body io.Reader) (*JobResult, error) {
	scanner := bufio.NewScanner(body)
	// Allow long SSE lines (large deltas) beyond bufio's default 64KiB cap.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	chunkIndex := 0
	var answer strings.Builder
	var reasoning strings.Builder
	finishReason := "stop"
	toolCalls := newToolCallAccumulator()

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
					Content          string          `json:"content"`
					ReasoningContent string          `json:"reasoning_content"`
					ToolCalls        []toolCallDelta `json:"tool_calls"`
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
		if len(chunk.Choices[0].Delta.ToolCalls) > 0 {
			toolCalls.merge(chunk.Choices[0].Delta.ToolCalls)
		}

		if fr := chunk.Choices[0].FinishReason; fr != "" {
			// Only "tool_calls" overrides the pre-#603 hardcoded "stop" --
			// this streaming path never surfaced any other finish_reason
			// (e.g. "length") before this change, so widening the override to
			// every finish_reason would change output for a text-only stream
			// that previously always reported "stop", violating the
			// no-tools-⇒-unchanged-behavior contract. "tool_calls" cannot
			// have occurred before #603 (tools were never forwarded), so this
			// branch is unreachable pre-#603 and only activates in the new
			// tool-calling path.
			if fr == "tool_calls" {
				finishReason = fr
			}
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return h.failure(err), nil
	}

	final := answer.String()
	if final == "" && toolCalls.empty() {
		// No answer and no tool call produced (thinking model ran out of budget
		// mid-reason); surface the reasoning so the reply is not blank, mirroring
		// the buffered path's reasoning_content fallback.
		if final = reasoning.String(); final != "" {
			stream.WriteChunk(final, chunkIndex)
		}
	}
	output := map[string]any{
		"content":       final,
		"finish_reason": finishReason,
	}
	if tc := toolCalls.json(); len(tc) > 0 {
		output["tool_calls"] = tc
	}
	return h.success(output), nil
}

// hasToolCalls reports whether raw is a MEANINGFUL, non-empty OpenAI
// tool_calls array -- i.e. whether it should be treated as "tool calls are
// present", as opposed to absent. json.RawMessage's byte length alone is NOT
// a meaningful presence check: the literal JSON `null` is 4 bytes and `[]` is
// 2 bytes, both len(raw) > 0 despite meaning "no tool calls". A Pydantic- (or
// any strictly-typed-response-model-) backed engine can plausibly serialize
// `"tool_calls": null` on an ORDINARY non-tool reply, so `len(raw) > 0` alone
// would wrongly treat that reply as tool-calls-present -- on the buffered
// response path this wrongly suppresses the reasoning_content->content
// fallback for a thinking model (e.g. bonsai) that ran out of budget
// mid-reasoning and also happened to carry an explicit null/empty tool_calls
// field, so the caller gets an empty reply instead of the reasoning. This
// unmarshals into a slice and counts elements instead of trusting the byte
// length, so `null`/`""`/`[]`/malformed JSON all correctly read as "absent".
func hasToolCalls(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return false
	}
	return len(arr) > 0
}

// hasJSONValue reports whether raw carries a meaningful JSON value, as
// opposed to being absent or an explicit `null`. Unlike hasToolCalls, it does
// NOT require an array shape -- it exists for fields like tool_choice, whose
// OpenAI shape is a string ("auto"/"none"/"required") or an object (a
// specific tool selection), never an array, so hasToolCalls' array-unmarshal
// check would (incorrectly) reject every legitimate value. A bare
// len(raw) > 0 check is still not enough on its own: the literal JSON `null`
// is 4 bytes, so it would pass and forward a `"tool_choice": null` key the
// same way the bare len() check on `tools` did before citadel-cli#933.
func hasJSONValue(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	trimmed := bytes.TrimSpace(raw)
	return string(trimmed) != "null"
}

// toolCallDelta is one entry in an OpenAI streaming delta.tool_calls array
// (citadel-cli#603). Deltas arrive incrementally by index -- id/type/
// function.name typically land on the FIRST delta for that index, and
// function.arguments accumulates piecemeal across many subsequent deltas
// (potentially character-by-character), so a single delta is never a complete
// tool call on its own.
type toolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// toolCallAccumulator merges streaming tool_calls deltas (indexed, partial)
// into a final OpenAI-shaped tool_calls array. Deliberately NOT a generic
// json.RawMessage merge: `arguments` must be concatenated as TEXT (a delta may
// split the JSON string mid-token), and id/type/name follow "last non-empty
// wins" (merge only overwrites on a non-empty delta field, never blanks a
// value an earlier delta already set) rather than being locked after the
// first sighting -- in the common shape (id+name on the first delta for an
// index, bare argument fragments after) those two behave identically, but
// they are not the same rule, so don't describe this as "latched from the
// first delta".
type toolCallAccumulator struct {
	order []int
	byIdx map[int]*accumulatedToolCall
}

type accumulatedToolCall struct {
	id        string
	typ       string
	name      string
	arguments strings.Builder
}

func newToolCallAccumulator() *toolCallAccumulator {
	return &toolCallAccumulator{byIdx: make(map[int]*accumulatedToolCall)}
}

// merge folds one delta frame's tool_calls entries into the accumulator,
// preserving first-seen order by index.
func (a *toolCallAccumulator) merge(deltas []toolCallDelta) {
	for _, d := range deltas {
		tc, ok := a.byIdx[d.Index]
		if !ok {
			tc = &accumulatedToolCall{}
			a.byIdx[d.Index] = tc
			a.order = append(a.order, d.Index)
		}
		if d.ID != "" {
			tc.id = d.ID
		}
		if d.Type != "" {
			tc.typ = d.Type
		}
		if d.Function.Name != "" {
			tc.name = d.Function.Name
		}
		tc.arguments.WriteString(d.Function.Arguments)
	}
}

// empty reports whether any tool-call delta has been seen at all.
func (a *toolCallAccumulator) empty() bool {
	return len(a.order) == 0
}

// json renders the merged tool_calls as a final OpenAI-shaped array (nil when
// no tool-call deltas were seen, or on the never-expected marshal failure --
// either way a text-only stream's output map gains no new key).
func (a *toolCallAccumulator) json() json.RawMessage {
	if a.empty() {
		return nil
	}
	out := make([]map[string]any, 0, len(a.order))
	for _, idx := range a.order {
		tc := a.byIdx[idx]
		typ := tc.typ
		if typ == "" {
			typ = "function"
		}
		out = append(out, map[string]any{
			"id":   tc.id,
			"type": typ,
			"function": map[string]any{
				"name":      tc.name,
				"arguments": tc.arguments.String(),
			},
		})
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil
	}
	return b
}

// parseChatCompletionResponse extracts the assistant content, finish reason,
// tool_calls, and usage from a buffered OpenAI chat-completions body. Content
// falls back to reasoning_content when the answer field is empty (thinking
// models like Bonsai whose token budget was spent mid-reasoning), so a caller
// never gets a blank reply while tokens were clearly generated -- EXCEPT when
// the model instead emitted tool_calls (citadel-cli#603): a thinking model
// that decided to call a tool has real chain-of-thought in reasoning_content
// but nothing there is meant to be the assistant's visible reply, so surfacing
// it as `content` would render as a bogus text answer ALONGSIDE the tool call.
// (Ported from internal/jobs; kept unexported here to keep the worker handler
// self-contained.)
func parseChatCompletionResponse(bodyBytes []byte) (content, finishReason string, toolCalls json.RawMessage, usage map[string]any, err error) {
	var response struct {
		Choices []struct {
			Message struct {
				Content          string          `json:"content"`
				ReasoningContent string          `json:"reasoning_content"`
				ToolCalls        json.RawMessage `json:"tool_calls,omitempty"`
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
		return "", "", nil, nil, fmt.Errorf("failed to parse chat completions response: %w", err)
	}

	finishReason = "stop"
	if len(response.Choices) > 0 {
		msg := response.Choices[0].Message
		content = msg.Content
		if hasToolCalls(msg.ToolCalls) {
			toolCalls = msg.ToolCalls
		} else if content == "" {
			content = msg.ReasoningContent
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
	return content, finishReason, toolCalls, usage, nil
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
// Messages-first matches what every dispatch site that populates Messages
// actually sent to the engine (see Execute/executeVLLM/executeOllama/
// executeLlamaCppAt/executeChatCompletionsAt, all of which route on
// `len(payload.Messages) > 0`): whichever backend function served the
// request built its outbound body from Messages alone once that branch was
// taken, never Prompt. A payload that happened to carry a stray Prompt
// alongside Messages must not have the guardrail compare the output against
// text the model never saw. Called from applyTrustEngine (citadel #1001),
// Execute's single consolidated post-completion hook, so this now runs for
// every backend/engine path, not just the OpenAI-chat-completions one.
// ChatMessage.Text() already strips multimodal content parts down to their
// text; messages are joined in order so the guardrail sees the full
// conversation context, not just the latest turn.
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

// signAEPReceiptsEnabled reports whether applyTrustEngine should
// additionally sign the grounding receipt into a verifiable AEP receipt
// (internal/aep.AEPReceiptV2). Default OFF, matching this codebase's
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
// signing addition) so applyTrustEngine can compute it ONCE and reuse
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
