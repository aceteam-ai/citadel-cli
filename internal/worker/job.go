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

	// SourceQueue is the queue this job was read from (for multi-queue ACK)
	SourceQueue string

	// RayID is the tracing/correlation ID for distributed tracing (JQS-Core)
	RayID string
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
//
// This const block is the source of truth for job types. When adding a new type
// here, also add it to allKnownJobTypes below so it is reported in a node's
// supported job-type set (issue #382).
const (
	JobTypeShellCommand      = "SHELL_COMMAND"
	JobTypeTmuxSession       = "TMUX_SESSION" // Create/list/attach a named tmux session (issue #302)
	JobTypeDownloadModel     = "DOWNLOAD_MODEL"
	JobTypeOllamaPull        = "OLLAMA_PULL"
	JobTypeLlamaCppInference = "LLAMACPP_INFERENCE"
	JobTypeVLLMInference     = "VLLM_INFERENCE"
	JobTypeOllamaInference   = "OLLAMA_INFERENCE"
	JobTypeLLMInference      = "llm_inference"       // Redis worker format
	JobTypeEmbedding         = "embedding"           // Redis worker format
	JobTypeApplyDeviceConfig = "APPLY_DEVICE_CONFIG" // Device config from onboarding
	JobTypeExtraction        = "GLINER_EXTRACTION"   // Entity/relation extraction via GLiNER2
	JobTypeHTTPProxy         = "HTTP_PROXY"          // Proxy HTTP requests through the local node
	JobTypeFileRead          = "FILE_READ"           // Read a file from the workspace
	JobTypeFileReadBytes     = "FILE_READ_BYTES"     // Read a file as raw base64-encoded bytes (binary-safe)
	JobTypeFileWrite         = "FILE_WRITE"          // Write a file to the workspace
	JobTypeFileWriteBytes    = "FILE_WRITE_BYTES"    // Write a file from raw base64-encoded bytes (binary-safe)
	JobTypeFileEdit          = "FILE_EDIT"           // Edit (string replace) a file in the workspace
	JobTypeFileList          = "FILE_LIST"           // List directory contents in the workspace
	JobTypeFileSearch        = "FILE_SEARCH"         // Search for text across files in the workspace
	JobTypeServiceStart      = "SERVICE_START"       // Start a service on the node
	JobTypeServiceStop       = "SERVICE_STOP"        // Stop a service on the node
	JobTypeServiceStatus     = "SERVICE_STATUS"      // Check if a service is running
	JobTypeSandboxSuspend    = "SANDBOX_SUSPEND"     // Pause a Docker container (sandbox suspend)
	JobTypeSandboxResume     = "SANDBOX_RESUME"      // Unpause a Docker container (sandbox resume)
	JobTypeModelCachePull    = "MODEL_CACHE_PULL"    // Pull model weights into local cache
	JobTypeModelCacheEvict   = "MODEL_CACHE_EVICT"   // Evict model weights from local cache
	JobTypeIOSBuild          = "IOS_BUILD"           // Build an iOS app via xcodebuild (macOS only)
	JobTypeAndroidBuild      = "ANDROID_BUILD"       // Build an Android app via the Gradle wrapper
	JobTypeGomobileBuild     = "GOMOBILE_BUILD"      // Cross-compile a Go package via gomobile bind
	JobTypeFileScreenshot    = "FILE_SCREENSHOT"     // Capture the node's display, return base64 PNG (issue #4179)
	JobTypeVNCScreenshot     = "VNC_SCREENSHOT"      // Capture the node's display via the VNC tool path (issue #4179)
	JobTypeVNCType           = "VNC_TYPE"            // Type text on the node's display (issue #4179)
	JobTypeVNCKeys           = "VNC_KEYS"            // Send a key combo to the node's display (issue #4179)
	JobTypeVNCActions        = "VNC_ACTIONS"         // Execute pointer/keyboard actions (click, move, drag) on the node's display (issue #4180)
	JobTypeCobrowse          = "COBROWSE"            // Human-in-the-loop co-browse over CDP (#4079)
	JobTypeTranscribeAudio   = "TRANSCRIBE_AUDIO"    // Transcribe workspace audio node-locally via the faster-whisper sidecar
)

// allKnownJobTypes enumerates every job type this citadel build knows about.
// It is probed against the runner's registered handlers to report the node's
// supported job-type set in the unsupported-type failure (issue #382). Handlers
// only answer CanHandle(type) for a single type each, so there is no way to
// enumerate what a node supports without a canonical list to probe.
var allKnownJobTypes = []string{
	JobTypeShellCommand,
	JobTypeTmuxSession,
	JobTypeDownloadModel,
	JobTypeOllamaPull,
	JobTypeLlamaCppInference,
	JobTypeVLLMInference,
	JobTypeOllamaInference,
	JobTypeLLMInference,
	JobTypeEmbedding,
	JobTypeApplyDeviceConfig,
	JobTypeExtraction,
	JobTypeHTTPProxy,
	JobTypeFileRead,
	JobTypeFileReadBytes,
	JobTypeFileWrite,
	JobTypeFileWriteBytes,
	JobTypeFileEdit,
	JobTypeFileList,
	JobTypeFileSearch,
	JobTypeServiceStart,
	JobTypeServiceStop,
	JobTypeServiceStatus,
	JobTypeSandboxSuspend,
	JobTypeSandboxResume,
	JobTypeModelCachePull,
	JobTypeModelCacheEvict,
	JobTypeIOSBuild,
	JobTypeAndroidBuild,
	JobTypeGomobileBuild,
	JobTypeFileScreenshot,
	JobTypeVNCScreenshot,
	JobTypeVNCType,
	JobTypeVNCKeys,
	JobTypeVNCActions,
	JobTypeCobrowse,
	JobTypeTranscribeAudio,
}
