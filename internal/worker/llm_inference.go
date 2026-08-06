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
	"net/http"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/jobs"
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
		httpClient: http.DefaultClient,
	}
}

// WithSwapper attaches a model-hotswap swapper (citadel-cli#632). Called from
// cmd/nodejobs.go ONLY when CITADEL_MODEL_HOTSWAP is on; leaving it unset keeps
// the handler's behavior identical to before hotswap.
func (h *LLMInferenceHandler) WithSwapper(s modelSwapper) *LLMInferenceHandler {
	h.swapper = s
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
			return h.failure(fmt.Errorf("model hotswap failed: %w", swapErr)), nil
		}
		if !outcome.Ready {
			return h.warming(payload.Model, outcome.ETASeconds, outcome.RetryAfterSeconds), nil
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
			return h.warming(payload.Model, engineWarmETA(payload.Backend), 0), nil
		}
		return h.failure(err), nil
	}

	switch payload.Backend {
	case "vllm":
		return h.executeVLLM(ctx, stream, payload)
	case "sglang":
		return h.executeSGLang(ctx, stream, payload)
	case "ollama":
		return h.executeOllama(ctx, stream, payload)
	case "llamacpp":
		return h.executeLlamaCppAt(ctx, stream, payload, h.baseURL("llamacpp"))
	case "bonsai":
		// Bonsai serves the identical llama.cpp-server API on its own host port
		// (PrismML fork). Reuse the llama.cpp request/stream path pointed at it.
		return h.executeLlamaCppAt(ctx, stream, payload, h.baseURL("bonsai"))
	case "unlimited-ocr":
		// Baidu Unlimited-OCR is a vLLM OpenAI engine on its own host port; use the
		// chat-completions path so multimodal image_url content (#625) reaches it.
		return h.executeChatCompletionsAt(ctx, stream, payload, h.baseURL("unlimited-ocr"))
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
func (h *LLMInferenceHandler) executeVLLM(ctx context.Context, stream StreamWriter, payload *jobs.LLMInferencePayload) (*JobResult, error) {
	// Chat-style requests (gateway `messages`) use /v1/chat/completions so vLLM
	// applies the served model's chat template; the legacy /v1/completions prompt
	// path is kept for prompt-style jobs.
	if len(payload.Messages) > 0 {
		return h.executeChatCompletionsAt(ctx, stream, payload, h.baseURL("vllm"))
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
func (h *LLMInferenceHandler) executeLlamaCppAt(ctx context.Context, stream StreamWriter, payload *jobs.LLMInferencePayload, baseURL string) (*JobResult, error) {
	// Chat-style requests (the OpenAI gateway sends `messages`, not `prompt`) go
	// to /v1/chat/completions so the engine applies the model's chat template.
	// This is required for chat/instruct models — and essential for thinking
	// models like Bonsai whose template emits the reasoning/answer split. The
	// legacy /completion path is kept for prompt-style jobs.
	if len(payload.Messages) > 0 {
		return h.executeChatCompletionsAt(ctx, stream, payload, baseURL)
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
func (h *LLMInferenceHandler) executeChatCompletionsAt(ctx context.Context, stream StreamWriter, payload *jobs.LLMInferencePayload, baseURL string) (*JobResult, error) {
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
	return h.bufferedChatCompletions(stream, resp.Body)
}

// bufferedChatCompletions parses a buffered OpenAI chat-completions response and
// emits the assistant's message content as a single chunk.
func (h *LLMInferenceHandler) bufferedChatCompletions(stream StreamWriter, body io.Reader) (*JobResult, error) {
	bodyBytes, err := io.ReadAll(body)
	if err != nil {
		return h.failure(err), nil
	}
	content, finishReason, usage, err := parseChatCompletionResponse(bodyBytes)
	if err != nil {
		return h.failure(err), nil
	}
	writeSingleChunk(stream, content)
	return h.success(map[string]any{
		"content":       content,
		"finish_reason": finishReason,
		"usage":         usage,
	}), nil
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
func (h *LLMInferenceHandler) warming(model string, etaSeconds, retryAfterSeconds int) *JobResult {
	if etaSeconds < 0 {
		etaSeconds = 0
	}
	if retryAfterSeconds <= 0 {
		retryAfterSeconds = warmingRetryAfter
	}
	return &JobResult{
		Status: JobStatusSuccess,
		Output: map[string]any{
			"status":      "model_warming",
			"model":       model,
			"eta_seconds": etaSeconds,
			"retry_after": retryAfterSeconds,
		},
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
		return h.warming(payload.Model, engineWarmETA(payload.Backend), 0)
	}
	// A refused connection here means the engine went away between the readiness
	// probe and the request. That is warming ONLY while a start this node issued
	// is still inside its load window; otherwise nothing is listening and no
	// amount of retrying will change that, so it stays a failure the caller can
	// act on (citadel-cli#705).
	if isConnectionRefused(err) && h.engineStartInFlight(payload.Backend) {
		return h.warming(payload.Model, engineWarmETA(payload.Backend), 0)
	}
	return h.failure(fmt.Errorf("%s: %w", wrap, err))
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
