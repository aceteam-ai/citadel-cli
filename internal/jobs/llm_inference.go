// internal/jobs/llm_inference.go
package jobs

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	redisclient "github.com/aceteam-ai/citadel-cli/internal/redis"
	"github.com/aceteam-ai/citadel-cli/services"
)

// LLMInferenceHandler handles llm_inference jobs from the Redis queue.
type LLMInferenceHandler struct{}

// vllmBaseURL and llamacppBaseURL2 return host-local base URLs for the vLLM and
// llama.cpp engines using the citadel-owned host ports (services/ports.go)
// rather than hardcoded literals. (llamacppBaseURL is already defined in
// llamacpp_inference.go within this package; this file reuses that helper.)
func vllmBaseURL() string {
	return fmt.Sprintf("http://localhost:%d", services.VLLMHostPort)
}

// CanHandle returns true if this handler can process the given job type.
func (h *LLMInferenceHandler) CanHandle(jobType string) bool {
	return jobType == JobTypeLLMInference
}

// Execute processes an llm_inference job.
func (h *LLMInferenceHandler) Execute(ctx context.Context, client *redisclient.Client, job *redisclient.Job) error {
	// Parse payload
	payload, err := h.parsePayload(job.Payload)
	if err != nil {
		return fmt.Errorf("invalid payload: %w", err)
	}

	// Extract rayId for event tracing
	rayID := extractRayID(job)

	// Route to appropriate backend
	switch payload.Backend {
	case "vllm":
		return h.executeVLLM(ctx, client, job.JobID, rayID, payload)
	case "sglang":
		return h.executeSGLang(ctx, client, job.JobID, rayID, payload)
	case "ollama":
		return h.executeOllama(ctx, client, job.JobID, rayID, payload)
	case "llamacpp":
		return h.executeLlamaCpp(ctx, client, job.JobID, rayID, payload)
	case "bonsai":
		// Bonsai serves the same llama.cpp-server API as llamacpp, just on the
		// bonsai host port (built from the PrismML fork). Reuse the llama.cpp
		// request/stream path pointed at the bonsai endpoint.
		return h.executeLlamaCppAt(ctx, client, job.JobID, rayID, payload, bonsaiBaseURL())
	default:
		return fmt.Errorf("unsupported backend: %s", payload.Backend)
	}
}

// bonsaiBaseURL returns the host-local base URL for the bonsai engine using the
// citadel-owned host port (services/ports.go).
func bonsaiBaseURL() string {
	return fmt.Sprintf("http://localhost:%d", services.BonsaiHostPort)
}

// extractRayID gets the rayId from a redis.Job's RawData or Payload.
func extractRayID(job *redisclient.Job) string {
	if rayID, ok := job.RawData["rayId"].(string); ok && rayID != "" {
		return rayID
	}
	if job.Payload != nil {
		if rayID, ok := job.Payload["rayId"].(string); ok {
			return rayID
		}
	}
	return ""
}

func (h *LLMInferenceHandler) parsePayload(data map[string]any) (*LLMInferencePayload, error) {
	// Convert map to JSON then unmarshal to struct
	jsonBytes, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}

	var payload LLMInferencePayload
	if err := json.Unmarshal(jsonBytes, &payload); err != nil {
		return nil, err
	}

	// Validate required fields
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
func (h *LLMInferenceHandler) executeVLLM(ctx context.Context, client *redisclient.Client, jobID, rayID string, payload *LLMInferencePayload) error {
	// Wait for vLLM to be ready
	if err := h.waitForVLLMReady(ctx); err != nil {
		return err
	}

	// Chat-style requests (gateway `messages`) use the OpenAI-compatible
	// /v1/chat/completions so vLLM applies the served model's chat template;
	// the legacy /v1/completions prompt path is kept for prompt-style jobs.
	if len(payload.Messages) > 0 {
		return h.executeChatCompletionsAt(ctx, client, jobID, rayID, payload, vllmBaseURL())
	}

	vllmURL := vllmBaseURL() + "/v1/completions"

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

	reqBody, _ := json.Marshal(reqPayload)

	req, err := http.NewRequestWithContext(ctx, "POST", vllmURL, bytes.NewBuffer(reqBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to connect to vLLM: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("vLLM returned status %d: %s", resp.StatusCode, string(body))
	}

	if payload.Stream {
		return h.handleVLLMStream(ctx, client, jobID, rayID, resp.Body)
	}

	return h.handleVLLMNonStream(ctx, client, jobID, rayID, resp.Body)
}

func (h *LLMInferenceHandler) handleVLLMStream(ctx context.Context, client *redisclient.Client, jobID, rayID string, body io.Reader) error {
	scanner := bufio.NewScanner(body)
	chunkIndex := 0
	var fullContent strings.Builder

	for scanner.Scan() {
		line := scanner.Text()

		// SSE format: "data: {...}"
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

			// Publish chunk to Redis
			client.PublishChunk(ctx, jobID, rayID, text, chunkIndex)
			chunkIndex++

			if chunk.Choices[0].FinishReason != "" {
				break
			}
		}
	}

	// Publish end event
	client.PublishEnd(ctx, jobID, rayID, map[string]any{
		"content":       fullContent.String(),
		"finish_reason": "stop",
	})

	return scanner.Err()
}

func (h *LLMInferenceHandler) handleVLLMNonStream(ctx context.Context, client *redisclient.Client, jobID, rayID string, body io.Reader) error {
	bodyBytes, err := io.ReadAll(body)
	if err != nil {
		return err
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
		return fmt.Errorf("failed to parse vLLM response: %w", err)
	}

	if len(response.Choices) == 0 {
		return fmt.Errorf("vLLM returned no choices")
	}

	content := strings.TrimSpace(response.Choices[0].Text)

	// Publish single chunk then end
	client.PublishChunk(ctx, jobID, rayID, content, 0)
	client.PublishEnd(ctx, jobID, rayID, map[string]any{
		"content":       content,
		"finish_reason": response.Choices[0].FinishReason,
		"usage": map[string]any{
			"prompt_tokens":     response.Usage.PromptTokens,
			"completion_tokens": response.Usage.CompletionTokens,
			"total_tokens":      response.Usage.TotalTokens,
		},
	})

	return nil
}

func (h *LLMInferenceHandler) waitForVLLMReady(ctx context.Context) error {
	healthURL := vllmBaseURL() + "/health"
	maxWait := 60 * time.Second
	pollInterval := 1 * time.Second
	startTime := time.Now()

	for time.Since(startTime) < maxWait {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		req, _ := http.NewRequestWithContext(ctx, "GET", healthURL, nil)
		resp, err := http.DefaultClient.Do(req)
		if err == nil && resp.StatusCode == http.StatusOK {
			resp.Body.Close()
			return nil
		}
		if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(pollInterval)
	}
	return fmt.Errorf("vLLM did not become ready within %v", maxWait)
}

// executeSGLang handles inference via SGLang's OpenAI-compatible API.
// SGLang runs on services.SGLangHostPort and exposes the same /v1/completions endpoint as vLLM.
func (h *LLMInferenceHandler) executeSGLang(ctx context.Context, client *redisclient.Client, jobID, rayID string, payload *LLMInferencePayload) error {
	// Wait for SGLang to be ready
	if err := h.waitForSGLangReady(ctx); err != nil {
		return err
	}

	sglangURL := fmt.Sprintf("http://localhost:%d/v1/completions", services.SGLangHostPort)

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

	reqBody, _ := json.Marshal(reqPayload)

	req, err := http.NewRequestWithContext(ctx, "POST", sglangURL, bytes.NewBuffer(reqBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to connect to SGLang: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("SGLang returned status %d: %s", resp.StatusCode, string(body))
	}

	// SGLang uses the same OpenAI-compatible response format as vLLM
	if payload.Stream {
		return h.handleVLLMStream(ctx, client, jobID, rayID, resp.Body)
	}

	return h.handleVLLMNonStream(ctx, client, jobID, rayID, resp.Body)
}

func (h *LLMInferenceHandler) waitForSGLangReady(ctx context.Context) error {
	healthURL := fmt.Sprintf("http://localhost:%d/health", services.SGLangHostPort)
	maxWait := 60 * time.Second
	pollInterval := 1 * time.Second
	startTime := time.Now()

	for time.Since(startTime) < maxWait {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		req, _ := http.NewRequestWithContext(ctx, "GET", healthURL, nil)
		resp, err := http.DefaultClient.Do(req)
		if err == nil && resp.StatusCode == http.StatusOK {
			resp.Body.Close()
			return nil
		}
		if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(pollInterval)
	}
	return fmt.Errorf("SGLang did not become ready within %v", maxWait)
}

// executeOllama handles inference via Ollama's API.
func (h *LLMInferenceHandler) executeOllama(ctx context.Context, client *redisclient.Client, jobID, rayID string, payload *LLMInferencePayload) error {
	ollamaURL := "http://localhost:11434/api/generate"

	reqPayload := map[string]any{
		"model":  payload.Model,
		"prompt": payload.Prompt,
		"stream": payload.Stream,
	}

	if payload.MaxTokens > 0 {
		reqPayload["options"] = map[string]any{
			"num_predict": payload.MaxTokens,
		}
	}

	reqBody, _ := json.Marshal(reqPayload)

	req, err := http.NewRequestWithContext(ctx, "POST", ollamaURL, bytes.NewBuffer(reqBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to connect to Ollama: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("Ollama returned status %d: %s", resp.StatusCode, string(body))
	}

	if payload.Stream {
		return h.handleOllamaStream(ctx, client, jobID, rayID, resp.Body)
	}

	return h.handleOllamaNonStream(ctx, client, jobID, rayID, resp.Body)
}

func (h *LLMInferenceHandler) handleOllamaStream(ctx context.Context, client *redisclient.Client, jobID, rayID string, body io.Reader) error {
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
			client.PublishChunk(ctx, jobID, rayID, chunk.Response, chunkIndex)
			chunkIndex++
		}

		if chunk.Done {
			break
		}
	}

	client.PublishEnd(ctx, jobID, rayID, map[string]any{
		"content":       fullContent.String(),
		"finish_reason": "stop",
	})

	return scanner.Err()
}

func (h *LLMInferenceHandler) handleOllamaNonStream(ctx context.Context, client *redisclient.Client, jobID, rayID string, body io.Reader) error {
	bodyBytes, err := io.ReadAll(body)
	if err != nil {
		return err
	}

	var response struct {
		Response string `json:"response"`
	}

	if err := json.Unmarshal(bodyBytes, &response); err != nil {
		return fmt.Errorf("failed to parse Ollama response: %w", err)
	}

	client.PublishChunk(ctx, jobID, rayID, response.Response, 0)
	client.PublishEnd(ctx, jobID, rayID, map[string]any{
		"content":       response.Response,
		"finish_reason": "stop",
	})

	return nil
}

// executeLlamaCpp handles inference via llama.cpp server API.
func (h *LLMInferenceHandler) executeLlamaCpp(ctx context.Context, client *redisclient.Client, jobID, rayID string, payload *LLMInferencePayload) error {
	return h.executeLlamaCppAt(ctx, client, jobID, rayID, payload, llamacppBaseURL())
}

// executeLlamaCppAt runs a llama.cpp-server inference against an explicit base
// URL. Shared by the llamacpp and bonsai backends (bonsai is the PrismML
// llama.cpp fork serving the identical API on its own host port).
func (h *LLMInferenceHandler) executeLlamaCppAt(ctx context.Context, client *redisclient.Client, jobID, rayID string, payload *LLMInferencePayload, baseURL string) error {
	// Chat-style requests (the OpenAI gateway sends `messages`, not `prompt`)
	// go to the server's OpenAI-compatible /v1/chat/completions so the engine
	// applies the model's chat template. This is required for chat/instruct
	// models — and essential for thinking models like Bonsai whose template
	// emits the reasoning/answer split. The legacy /completion path below is
	// kept for prompt-style jobs.
	if len(payload.Messages) > 0 {
		return h.executeChatCompletionsAt(ctx, client, jobID, rayID, payload, baseURL)
	}

	llamaCppURL := baseURL + "/completion"

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

	reqBody, _ := json.Marshal(reqPayload)

	req, err := http.NewRequestWithContext(ctx, "POST", llamaCppURL, bytes.NewBuffer(reqBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to connect to llama.cpp: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("llama.cpp returned status %d: %s", resp.StatusCode, string(body))
	}

	if payload.Stream {
		return h.handleLlamaCppStream(ctx, client, jobID, rayID, resp.Body)
	}

	return h.handleLlamaCppNonStream(ctx, client, jobID, rayID, resp.Body)
}

func (h *LLMInferenceHandler) handleLlamaCppStream(ctx context.Context, client *redisclient.Client, jobID, rayID string, body io.Reader) error {
	scanner := bufio.NewScanner(body)
	chunkIndex := 0
	var fullContent strings.Builder

	for scanner.Scan() {
		line := scanner.Text()

		// SSE format: "data: {...}"
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
			client.PublishChunk(ctx, jobID, rayID, chunk.Content, chunkIndex)
			chunkIndex++
		}

		if chunk.Stop {
			break
		}
	}

	client.PublishEnd(ctx, jobID, rayID, map[string]any{
		"content":       fullContent.String(),
		"finish_reason": "stop",
	})

	return scanner.Err()
}

func (h *LLMInferenceHandler) handleLlamaCppNonStream(ctx context.Context, client *redisclient.Client, jobID, rayID string, body io.Reader) error {
	bodyBytes, err := io.ReadAll(body)
	if err != nil {
		return err
	}

	var response struct {
		Content string `json:"content"`
	}

	if err := json.Unmarshal(bodyBytes, &response); err != nil {
		return fmt.Errorf("failed to parse llama.cpp response: %w", err)
	}

	client.PublishChunk(ctx, jobID, rayID, response.Content, 0)
	client.PublishEnd(ctx, jobID, rayID, map[string]any{
		"content":       response.Content,
		"finish_reason": "stop",
	})

	return nil
}

// executeChatCompletionsAt runs a chat-style inference against an OpenAI-
// compatible /v1/chat/completions endpoint. vLLM, llama.cpp, and the bonsai
// llama.cpp fork all expose it identically. Sending `messages` (rather than a
// flattened prompt) lets the engine apply the served model's chat template,
// which is required for instruct/chat models and essential for thinking models
// like Bonsai. Used whenever an llm_inference job carries `messages` (the shape
// the OpenAI inference gateway dispatches).
func (h *LLMInferenceHandler) executeChatCompletionsAt(ctx context.Context, client *redisclient.Client, jobID, rayID string, payload *LLMInferencePayload, baseURL string) error {
	chatURL := baseURL + "/v1/chat/completions"

	messages := make([]map[string]string, 0, len(payload.Messages))
	for _, m := range payload.Messages {
		messages = append(messages, map[string]string{"role": m.Role, "content": m.Content})
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

	reqBody, _ := json.Marshal(reqPayload)
	req, err := http.NewRequestWithContext(ctx, "POST", chatURL, bytes.NewBuffer(reqBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to connect to chat endpoint: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("chat endpoint returned status %d: %s", resp.StatusCode, string(body))
	}

	if payload.Stream {
		return h.handleChatCompletionsStream(ctx, client, jobID, rayID, resp.Body)
	}
	return h.handleChatCompletionsNonStream(ctx, client, jobID, rayID, resp.Body)
}

// handleChatCompletionsNonStream parses a buffered OpenAI chat-completions
// response and publishes the assistant's message content.
func (h *LLMInferenceHandler) handleChatCompletionsNonStream(ctx context.Context, client *redisclient.Client, jobID, rayID string, body io.Reader) error {
	bodyBytes, err := io.ReadAll(body)
	if err != nil {
		return err
	}

	content, finishReason, usage, err := parseChatCompletionResponse(bodyBytes)
	if err != nil {
		return err
	}

	client.PublishChunk(ctx, jobID, rayID, content, 0)
	client.PublishEnd(ctx, jobID, rayID, map[string]any{
		"content":       content,
		"finish_reason": finishReason,
		"usage":         usage,
	})
	return nil
}

// parseChatCompletionResponse extracts the assistant content, finish reason,
// and usage from a buffered OpenAI chat-completions body. Content falls back to
// reasoning_content when the answer field is empty (thinking models like Bonsai
// whose token budget was spent mid-reasoning), so a caller never gets a blank
// reply while tokens were clearly generated.
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

// handleChatCompletionsStream translates an OpenAI chat-completions SSE stream
// into Redis chunks. Each `data:` frame carries a choices[].delta. The answer
// is streamed from delta.content (matching the buffered path and standard
// OpenAI clients). Thinking models like Bonsai emit the chain-of-thought in
// delta.reasoning_content and the answer in delta.content; the reasoning is
// accumulated but only surfaced if the stream ends with NO answer (token budget
// spent mid-reasoning), so a reply is never silently blank while staying
// consistent with the non-stream content-only result.
func (h *LLMInferenceHandler) handleChatCompletionsStream(ctx context.Context, client *redisclient.Client, jobID, rayID string, body io.Reader) error {
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
			client.PublishChunk(ctx, jobID, rayID, text, chunkIndex)
			chunkIndex++
		} else if rc := chunk.Choices[0].Delta.ReasoningContent; rc != "" {
			reasoning.WriteString(rc)
		}

		if chunk.Choices[0].FinishReason != "" {
			break
		}
	}

	final := answer.String()
	if final == "" {
		// No answer was produced (thinking model ran out of budget mid-reason);
		// surface the reasoning so the reply is not blank, mirroring the
		// buffered path's reasoning_content fallback.
		if final = reasoning.String(); final != "" {
			client.PublishChunk(ctx, jobID, rayID, final, chunkIndex)
		}
	}

	client.PublishEnd(ctx, jobID, rayID, map[string]any{
		"content":       final,
		"finish_reason": "stop",
	})
	return scanner.Err()
}
