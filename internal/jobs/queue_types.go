// Package jobs contains job type definitions and handlers for the Redis queue system.
package jobs

import (
	"encoding/json"
	"strings"
)

// Job types that citadel can handle
const (
	// JobTypeLLMInference handles local LLM completion requests
	JobTypeLLMInference = "llm_inference"

	// JobTypeEmbedding handles local embedding generation (future)
	JobTypeEmbedding = "embedding"

	// JobTypeExtraction handles entity/relation extraction
	JobTypeExtraction = "EXTRACTION"

	// File operation job types for agent workspace access
	JobTypeFileRead      = "FILE_READ"
	JobTypeFileReadBytes = "FILE_READ_BYTES"
	JobTypeFileWrite     = "FILE_WRITE"
	JobTypeFileEdit      = "FILE_EDIT"
	JobTypeFileList      = "FILE_LIST"
	JobTypeFileSearch    = "FILE_SEARCH"

	// Model cache management job types
	JobTypeModelCachePull  = "MODEL_CACHE_PULL"
	JobTypeModelCacheEvict = "MODEL_CACHE_EVICT"

	// Mobile build job types (issue #140 Phase 2). iOS and gomobile-iOS builds
	// require a macOS node with Xcode; Android builds require an Android SDK.
	JobTypeIOSBuild      = "IOS_BUILD"
	JobTypeAndroidBuild  = "ANDROID_BUILD"
	JobTypeGomobileBuild = "GOMOBILE_BUILD"

	// JobTypeReconcile is the push-nudge for the node reconcile loop (issue
	// #353, epic #352). A `reconcile` job on the node's queue triggers an
	// immediate desired-state reconcile pass so /fabric actions feel instant
	// (push accelerates; the periodic pull remains the source of truth).
	//
	// The handler is INERT unless the reconcile feature is explicitly enabled
	// (reconcile.Config.Enabled, DISABLED BY DEFAULT) — an existing node that
	// has not opted into remote management ignores this job. The worker dispatch
	// is intentionally NOT wired in this increment to avoid touching the live
	// worker path (see internal/reconcile.HandleReconcileJob).
	JobTypeReconcile = "RECONCILE"
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
	Backend string `json:"backend"` // "vllm", "sglang", "ollama", "llamacpp", "bonsai"

	// Stop sequences to end generation
	Stop []string `json:"stop,omitempty"`

	// Tools carries the OpenAI `tools` array (function/tool definitions) for
	// tool-calling requests (citadel-cli#603, aceteam #6555). Kept as raw JSON
	// -- like ChatMessage.Content -- rather than a typed Go struct so an
	// engine-specific JSON Schema shape in `function.parameters` is never
	// lossily re-typed. nil/absent means no tools (the pre-#603 text-only
	// path), so a text-only request's payload is structurally unchanged.
	Tools json.RawMessage `json:"tools,omitempty"`

	// ToolChoice carries the OpenAI `tool_choice` field, which is EITHER a
	// bare string ("auto"/"none"/"required") OR an object naming a specific
	// function -- forwarded raw for the same reason as Tools.
	ToolChoice json.RawMessage `json:"tool_choice,omitempty"`
}

// ChatMessage represents a message in chat-style APIs.
//
// Content is stored as raw JSON because the OpenAI chat schema allows it to be
// EITHER a plain string (the common case) OR the multimodal "content parts"
// array — e.g. `[{"type":"text",...},{"type":"image_url","image_url":{"url":
// "data:image/png;base64,..."}}]` — which a vision/OCR model like
// baidu/Unlimited-OCR needs (#625). Keeping it raw lets the node forward either
// shape verbatim to an OpenAI-compatible engine without lossy re-encoding; use
// Text() when a plain-text view is required.
type ChatMessage struct {
	Role    string          `json:"role"` // "system", "user", "assistant"
	Content json.RawMessage `json:"content,omitempty"`

	// ToolCalls carries the OpenAI `tool_calls` array on an assistant message
	// that requested one or more tool invocations (citadel-cli#603). Raw JSON,
	// forwarded verbatim -- the same reasoning as Content. nil on a plain text
	// turn, so a non-tool conversation's messages are unaffected.
	ToolCalls json.RawMessage `json:"tool_calls,omitempty"`

	// ToolCallID correlates a role="tool" message's result back to the
	// assistant tool_calls entry that requested it (OpenAI convention).
	ToolCallID string `json:"tool_call_id,omitempty"`

	// Name is the function name on a role="tool" message.
	Name string `json:"name,omitempty"`
}

// Text returns the message content as plain text. Content may be a JSON string
// (returned as-is) or the OpenAI multimodal array (its "text" parts are
// concatenated; non-text parts such as image_url are ignored). Empty or
// unparseable content yields "".
func (m ChatMessage) Text() string {
	if len(m.Content) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(m.Content, &parts); err == nil {
		var b strings.Builder
		for _, p := range parts {
			b.WriteString(p.Text)
		}
		return b.String()
	}
	return ""
}

// ContentJSON returns the raw content for forwarding verbatim to an
// OpenAI-compatible engine, preserving multimodal parts. Empty content becomes
// an empty JSON string so the outgoing request is always valid.
func (m ChatMessage) ContentJSON() json.RawMessage {
	if len(m.Content) == 0 {
		return json.RawMessage(`""`)
	}
	return m.Content
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
