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
)

// LLMInferenceHandler handles llm_inference jobs from the Redis queue.
type LLMInferenceHandler struct{}

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

	// Route to appropriate backend
	switch payload.Backend {
	case "vllm":
		return h.executeVLLM(ctx, client, job.JobID, payload)
	case "ollama":
		return h.executeOllama(ctx, client, job.JobID, payload)
	case "llamacpp":
		return h.executeLlamaCpp(ctx, client, job.JobID, payload)
	default:
		return fmt.Errorf("unsupported backend: %s", payload.Backend)
	}
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
func (h *LLMInferenceHandler) executeVLLM(ctx context.Context, client *redisclient.Client, jobID string, payload *LLMInferencePayload) error {
	// Wait for vLLM to be ready
	if err := h.waitForVLLMReady(ctx); err != nil {
		return err
	}

	vllmURL := "http://localhost:8000/v1/completions"

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
		return h.handleVLLMStream(ctx, client, jobID, resp.Body)
	}

	return h.handleVLLMNonStream(ctx, client, jobID, resp.Body)
}

func (h *LLMInferenceHandler) handleVLLMStream(ctx context.Context, client *redisclient.Client, jobID string, body io.Reader) error {
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
			client.PublishChunk(ctx, jobID, text, chunkIndex)
			chunkIndex++

			if chunk.Choices[0].FinishReason != "" {
				break
			}
		}
	}

	// Publish end event
	client.PublishEnd(ctx, jobID, map[string]any{
		"content":       fullContent.String(),
		"finish_reason": "stop",
	})

	return scanner.Err()
}

func (h *LLMInferenceHandler) handleVLLMNonStream(ctx context.Context, client *redisclient.Client, jobID string, body io.Reader) error {
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
	client.PublishChunk(ctx, jobID, content, 0)
	client.PublishEnd(ctx, jobID, map[string]any{
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
	healthURL := "http://localhost:8000/health"
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

// executeOllama handles inference via Ollama's API.
func (h *LLMInferenceHandler) executeOllama(ctx context.Context, client *redisclient.Client, jobID string, payload *LLMInferencePayload) error {
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
		return h.handleOllamaStream(ctx, client, jobID, resp.Body)
	}

	return h.handleOllamaNonStream(ctx, client, jobID, resp.Body)
}

func (h *LLMInferenceHandler) handleOllamaStream(ctx context.Context, client *redisclient.Client, jobID string, body io.Reader) error {
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
			client.PublishChunk(ctx, jobID, chunk.Response, chunkIndex)
			chunkIndex++
		}

		if chunk.Done {
			break
		}
	}

	client.PublishEnd(ctx, jobID, map[string]any{
		"content":       fullContent.String(),
		"finish_reason": "stop",
	})

	return scanner.Err()
}

func (h *LLMInferenceHandler) handleOllamaNonStream(ctx context.Context, client *redisclient.Client, jobID string, body io.Reader) error {
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

	client.PublishChunk(ctx, jobID, response.Response, 0)
	client.PublishEnd(ctx, jobID, map[string]any{
		"content":       response.Response,
		"finish_reason": "stop",
	})

	return nil
}

// executeLlamaCpp handles inference via llama.cpp server API.
func (h *LLMInferenceHandler) executeLlamaCpp(ctx context.Context, client *redisclient.Client, jobID string, payload *LLMInferencePayload) error {
	llamaCppURL := "http://localhost:8080/completion"

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
		return h.handleLlamaCppStream(ctx, client, jobID, resp.Body)
	}

	return h.handleLlamaCppNonStream(ctx, client, jobID, resp.Body)
}

func (h *LLMInferenceHandler) handleLlamaCppStream(ctx context.Context, client *redisclient.Client, jobID string, body io.Reader) error {
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
			client.PublishChunk(ctx, jobID, chunk.Content, chunkIndex)
			chunkIndex++
		}

		if chunk.Stop {
			break
		}
	}

	client.PublishEnd(ctx, jobID, map[string]any{
		"content":       fullContent.String(),
		"finish_reason": "stop",
	})

	return scanner.Err()
}

func (h *LLMInferenceHandler) handleLlamaCppNonStream(ctx context.Context, client *redisclient.Client, jobID string, body io.Reader) error {
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

	client.PublishChunk(ctx, jobID, response.Content, 0)
	client.PublishEnd(ctx, jobID, map[string]any{
		"content":       response.Content,
		"finish_reason": "stop",
	})

	return nil
}
