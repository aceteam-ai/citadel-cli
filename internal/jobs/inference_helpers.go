// internal/jobs/inference_helpers.go
//
// Shared node-local inference helpers for the jobs package. These were extracted
// from the former internal/jobs/llm_inference.go when its dead Redis-interface
// handler was removed and re-implemented as a native worker.JobHandler
// (internal/worker/llm_inference.go, issue #590). Only the genuinely shared
// helpers survive here — they are still referenced by other files in this
// package (meeting_summary.go) and by tests (model_cache_pull_bonsai_test.go,
// llm_inference_chat_test.go).
package jobs

import (
	"encoding/json"
	"fmt"

	"github.com/aceteam-ai/citadel-cli/services"
)

// vllmBaseURL returns the host-local base URL for the vLLM engine using the
// citadel-owned host port (services/ports.go) rather than a hardcoded literal,
// honoring the CITADEL_VLLM_HOST_PORT override. Used by meeting_summary.go.
func vllmBaseURL() string {
	return fmt.Sprintf("http://localhost:%d", services.VLLMHostPort)
}

// bonsaiBaseURL returns the host-local base URL for the bonsai engine using the
// citadel-owned host port (services/ports.go).
func bonsaiBaseURL() string {
	return fmt.Sprintf("http://localhost:%d", services.BonsaiHostPort)
}

// parseChatCompletionResponse extracts the assistant content, finish reason, and
// usage from a buffered OpenAI chat-completions body. Content falls back to
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
