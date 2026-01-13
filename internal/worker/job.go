// Package worker provides a unified job processing framework for Citadel.
//
// This package abstracts the differences between job sources (Nexus HTTP, Redis Streams)
// and provides a common interface for job handlers. It enables a single `citadel run`
// command that can operate in different modes.
//
// Architecture:
//
//	JobSource (nexus/redis) → Runner → JobHandler → StreamWriter (optional)
//
// The Runner orchestrates the job processing loop:
//  1. Connect to job source
//  2. Fetch next job (blocking)
//  3. Dispatch to appropriate handler
//  4. Ack/Nack based on result
//  5. Repeat
package worker

import "time"

// Job represents a unit of work to be processed.
// This is the common job format used internally, regardless of source.
type Job struct {
	// ID uniquely identifies this job
	ID string

	// Type determines which handler processes this job
	Type string

	// Payload contains job-specific data
	Payload map[string]any

	// Source identifies where this job came from (for logging/debugging)
	Source string

	// MessageID is the source-specific message identifier (for ack/nack)
	MessageID string

	// Metadata contains additional source-specific information
	Metadata JobMetadata
}

// JobMetadata contains optional job metadata.
type JobMetadata struct {
	// CreatedAt is when the job was created
	CreatedAt time.Time

	// Attempts is the number of times this job has been attempted
	Attempts int

	// MaxAttempts is the maximum retry count before DLQ
	MaxAttempts int

	// Priority is the job priority (source-specific interpretation)
	Priority int

	// Tags are arbitrary labels for routing/filtering
	Tags []string
}

// JobResult contains the outcome of job processing.
type JobResult struct {
	// Status is the job outcome (success, failure, retry)
	Status JobStatus

	// Output is the result data (handler-specific)
	Output map[string]any

	// Error contains error details if status is not success
	Error error

	// Duration is how long the job took to process
	Duration time.Duration
}

// JobStatus represents the outcome of job processing.
type JobStatus string

const (
	// JobStatusSuccess indicates the job completed successfully
	JobStatusSuccess JobStatus = "success"

	// JobStatusFailure indicates the job failed (will not retry)
	JobStatusFailure JobStatus = "failure"

	// JobStatusRetry indicates the job should be retried
	JobStatusRetry JobStatus = "retry"
)

// Common job types used across sources.
const (
	JobTypeShellCommand      = "SHELL_COMMAND"
	JobTypeDownloadModel     = "DOWNLOAD_MODEL"
	JobTypeOllamaPull        = "OLLAMA_PULL"
	JobTypeLlamaCppInference = "LLAMACPP_INFERENCE"
	JobTypeVLLMInference     = "VLLM_INFERENCE"
	JobTypeOllamaInference   = "OLLAMA_INFERENCE"
	JobTypeLLMInference      = "llm_inference" // Redis worker format
	JobTypeEmbedding         = "embedding"     // Redis worker format
)
