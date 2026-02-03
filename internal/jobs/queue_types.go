// Package jobs contains job type definitions and handlers for the Redis queue system.
package jobs

// Job types that citadel can handle
const (
	// JobTypeLLMInference handles local LLM completion requests
	JobTypeLLMInference = "llm_inference"

	// JobTypeEmbedding handles local embedding generation (future)
	JobTypeEmbedding = "embedding"

	// JobTypeExtraction handles entity/relation extraction
	JobTypeExtraction = "EXTRACTION"
)

// Queue names following PR #1105 convention
const (
	QueueGPUGeneral = "jobs:v1:gpu-general"
	QueueCPUGeneral = "jobs:v1:cpu-general"
)

// LLMInferencePayload represents the payload for llm_inference jobs.
type LLMInferencePayload struct {
	// Model is the model identifier (e.g., "meta-llama/Llama-2-7b-chat-hf")
	Model string `json:"model"`

	// Prompt is the input text to send to the model
	Prompt string `json:"prompt"`

	// Messages is an alternative to Prompt for chat-style APIs
	Messages []ChatMessage `json:"messages,omitempty"`

	// MaxTokens is the maximum number of tokens to generate
	MaxTokens int `json:"max_tokens"`

	// Temperature controls randomness (0.0-2.0)
	Temperature float64 `json:"temperature"`

	// TopP is nucleus sampling parameter
	TopP float64 `json:"top_p,omitempty"`

	// Stream indicates whether to stream the response
	Stream bool `json:"stream"`

	// Backend specifies which inference engine to use
	Backend string `json:"backend"` // "vllm", "ollama", "llamacpp"

	// Stop sequences to end generation
	Stop []string `json:"stop,omitempty"`
}

// ChatMessage represents a message in chat-style APIs.
type ChatMessage struct {
	Role    string `json:"role"`    // "system", "user", "assistant"
	Content string `json:"content"`
}

// LLMInferenceResult represents the result of an llm_inference job.
type LLMInferenceResult struct {
	// Content is the generated text
	Content string `json:"content"`

	// FinishReason indicates why generation stopped
	FinishReason string `json:"finish_reason"` // "stop", "length", "error"

	// Usage contains token counts
	Usage UsageInfo `json:"usage"`

	// Model is the model that was used
	Model string `json:"model"`
}

// UsageInfo contains token usage information.
type UsageInfo struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// StreamChunk represents a single chunk in a streaming response.
type StreamChunk struct {
	Content      string `json:"content"`
	Index        int    `json:"index"`
	FinishReason string `json:"finish_reason,omitempty"`
}

// BaseJobPayload contains common fields for all job types (matches PR #1105).
type BaseJobPayload struct {
	Version        string `json:"version"`
	Type           string `json:"type"`
	JobID          string `json:"jobId"`
	UserID         string `json:"userId"`
	OrganizationID string `json:"organizationId"`
	CreatedAt      string `json:"createdAt"`
	Priority       int    `json:"priority"`
	MaxAttempts    int    `json:"maxAttempts"`
	TimeoutSeconds int    `json:"timeoutSeconds"`
}

// JobStatus represents the status of a job.
type JobStatus string

const (
	StatusEnqueued   JobStatus = "enqueued"
	StatusClaimed    JobStatus = "claimed"
	StatusProcessing JobStatus = "processing"
	StatusCompleted  JobStatus = "completed"
	StatusFailed     JobStatus = "failed"
)
