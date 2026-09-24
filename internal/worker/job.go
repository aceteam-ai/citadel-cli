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
	JobTypeShellCommand       = "SHELL_COMMAND"
	JobTypeTmuxSession        = "TMUX_SESSION" // Create/list/attach a named tmux session (issue #302)
	JobTypeDownloadModel      = "DOWNLOAD_MODEL"
	JobTypeOllamaPull         = "OLLAMA_PULL"
	JobTypeLlamaCppInference  = "LLAMACPP_INFERENCE"
	JobTypeVLLMInference      = "VLLM_INFERENCE"
	JobTypeOllamaInference    = "OLLAMA_INFERENCE"
	JobTypeLLMInference       = "llm_inference"        // Redis worker format
	JobTypeEmbedding          = "embedding"            // Redis worker format
	JobTypeApplyDeviceConfig  = "APPLY_DEVICE_CONFIG"  // Device config from onboarding
	JobTypeExtraction         = "GLINER_EXTRACTION"    // Entity/relation extraction via GLiNER2
	JobTypeHTTPProxy          = "HTTP_PROXY"           // Proxy HTTP requests through the local node
	JobTypeWebFetch           = "WEB_FETCH"            // Native HTTP fetch from node egress with SSRF guards (aceteam#5995)
	JobTypeFileRead           = "FILE_READ"            // Read a file from the workspace
	JobTypeFileReadBytes      = "FILE_READ_BYTES"      // Read a file as raw base64-encoded bytes (binary-safe)
	JobTypeFileWrite          = "FILE_WRITE"           // Write a file to the workspace
	JobTypeFileWriteBytes     = "FILE_WRITE_BYTES"     // Write a file from raw base64-encoded bytes (binary-safe)
	JobTypeFileEdit           = "FILE_EDIT"            // Edit (string replace) a file in the workspace
	JobTypeFileList           = "FILE_LIST"            // List directory contents in the workspace
	JobTypeFileSearch         = "FILE_SEARCH"          // Search for text across files in the workspace
	JobTypeFileIndex          = "FILE_INDEX"           // (Re)build the node-local semantic index over workspace files (aceteam#6087)
	JobTypeFileSemanticSearch = "FILE_SEMANTIC_SEARCH" // KNN over the node-local semantic index (aceteam#6087)
	JobTypeServiceStart       = "SERVICE_START"        // Start a service on the node
	JobTypeServiceStop        = "SERVICE_STOP"         // Stop a service on the node
	JobTypeServiceStatus      = "SERVICE_STATUS"       // Check if a service is running
	JobTypeSandboxSuspend     = "SANDBOX_SUSPEND"      // Pause a Docker container (sandbox suspend)
	JobTypeSandboxResume      = "SANDBOX_RESUME"       // Unpause a Docker container (sandbox resume)
	JobTypeModelCachePull     = "MODEL_CACHE_PULL"     // Pull model weights into local cache
	JobTypeModelCacheEvict    = "MODEL_CACHE_EVICT"    // Evict model weights from local cache
	JobTypeIOSBuild           = "IOS_BUILD"            // Build an iOS app via xcodebuild (macOS only)
	JobTypeAndroidBuild       = "ANDROID_BUILD"        // Build an Android app via the Gradle wrapper
	JobTypeGomobileBuild      = "GOMOBILE_BUILD"       // Cross-compile a Go package via gomobile bind
	JobTypeFileScreenshot     = "FILE_SCREENSHOT"      // Capture the node's display, return base64 PNG (issue #4179)
	JobTypeVNCScreenshot      = "VNC_SCREENSHOT"       // Capture the node's display via the VNC tool path (issue #4179)
	JobTypeVNCType            = "VNC_TYPE"             // Type text on the node's display (issue #4179)
	JobTypeVNCKeys            = "VNC_KEYS"             // Send a key combo to the node's display (issue #4179)
	JobTypeVNCActions         = "VNC_ACTIONS"          // Execute pointer/keyboard actions (click, move, drag) on the node's display (issue #4180)
	JobTypeCobrowse           = "COBROWSE"             // Human-in-the-loop co-browse over CDP (#4079)
	JobTypeCobrowseSession    = "COBROWSE_SESSION"     // Isolated, concurrent interactive browser sessions (start/status/stop) (#793)
	JobTypeTranscribeAudio    = "TRANSCRIBE_AUDIO"     // Transcribe workspace audio node-locally via the faster-whisper sidecar
	JobTypeSynthesizeSpeech   = "SYNTHESIZE_SPEECH"    // Synthesize speech node-locally via the kokoro TTS sidecar (aceteam#6104)
	JobTypeMediaGenerate      = "MEDIA_GENERATE"       // Generate an image or video node-locally via the diffusers sidecar (issue #968/#970)
	JobTypeAgentUpdate        = "AGENT_UPDATE"         // Remotely update + restart this node's own citadel agent (aceteam#4427)
	JobTypeWhatsAppProvision  = "WHATSAPP_PROVISION"   // Remotely deploy + provision the WhatsApp bridge on this node (aceteam#4454)
	JobTypeResourceSnapshot   = "RESOURCE_SNAPSHOT"    // Return the node's full GPU/host resource-consumer snapshot, managed and unmanaged (issue #427)
	JobTypeInstanceMessage    = "INSTANCE_MESSAGE"     // Deliver a turn to a BYOC instance's loopback container (aceteam#5241)
	JobTypeModuleSet          = "MODULE_SET"           // Set the desired state of a single module on this node (interim, aceteam#5280)
	JobTypeExposeSet          = "EXPOSE_SET"           // Expose a local service on the gateway with private/org/link visibility (issue #598)
	JobTypeExposeList         = "EXPOSE_LIST"          // Read back this node's durable exposure inventory (issue #944)
	JobTypeUnexpose           = "UNEXPOSE"             // Remotely revoke a gateway exposure (issue #944)
	JobTypeMeetingJoin        = "MEETING_JOIN"         // Auto-join a video call, record + transcribe it node-locally (aceteam#5098)
	JobTypeDocumentRasterize  = "document_rasterize"   // Render selected PDF pages to images on this node so a scan can reach an OCR model (issue #675)
	JobTypeShowPairingCode    = "SHOW_PAIRING_CODE"    // Render a node:exec pairing code on this node's console (issue #659)
	JobTypeClearPairingCode   = "CLEAR_PAIRING_CODE"   // Clear a displayed node:exec pairing code (issue #659)

	// Fabric instance provisioning on a local hypervisor (aceteam#5963). These
	// act on hypervisor VMs; JobTypeInstanceMessage above predates this family
	// and addresses hosted agent instances, not VMs.
	JobTypeInstanceProvision = "INSTANCE_PROVISION" // Clone + size + mesh-enroll + start a new instance VM
	JobTypeInstanceStart     = "INSTANCE_START"     // Start an existing instance VM
	JobTypeInstanceStop      = "INSTANCE_STOP"      // Stop an existing instance VM
	JobTypeInstanceDestroy   = "INSTANCE_DESTROY"   // Destroy an instance VM and its resources
	JobTypeInstanceStatus    = "INSTANCE_STATUS"    // Report an instance VM's live status
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
	JobTypeWebFetch,
	JobTypeFileRead,
	JobTypeFileReadBytes,
	JobTypeFileWrite,
	JobTypeFileWriteBytes,
	JobTypeFileEdit,
	JobTypeFileList,
	JobTypeFileSearch,
	JobTypeFileIndex,
	JobTypeFileSemanticSearch,
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
	JobTypeCobrowseSession,
	JobTypeTranscribeAudio,
	JobTypeSynthesizeSpeech,
	JobTypeMediaGenerate,
	JobTypeAgentUpdate,
	JobTypeWhatsAppProvision,
	JobTypeResourceSnapshot,
	JobTypeInstanceMessage,
	JobTypeModuleSet,
	JobTypeExposeSet,
	JobTypeExposeList,
	JobTypeUnexpose,
	JobTypeMeetingJoin,
	JobTypeDocumentRasterize,
	JobTypeShowPairingCode,
	JobTypeClearPairingCode,
	JobTypeInstanceProvision,
	JobTypeInstanceStart,
	JobTypeInstanceStop,
	JobTypeInstanceDestroy,
	JobTypeInstanceStatus,
}

// gatedJobTypeReasons names, for a job type this build knows about (it is in
// allKnownJobTypes) but that CreateLegacyHandlersWithOpts (handler_adapter.go)
// registers only conditionally, the runtime condition that must be satisfied
// for this node to serve it. It exists so failUnsupportedJobType (runner.go)
// can tell an operator "this node's files permission is disabled" instead of
// "update the node" -- the two have completely different remedies, and
// conflating them sent operators chasing a binary update for what was really a
// permission toggle (aceteam#9962).
//
// Keep this in sync with the registration gates in handler_adapter.go:
//   - files: FILE_READ/READ_BYTES/WRITE/WRITE_BYTES/EDIT/LIST/SEARCH/INDEX/
//     SEMANTIC_SEARCH register only when WorkspaceDir != "" and !FilesDisabled.
//     FilesDisabled tracks the node's `files` permission, default-DENY on a
//     fresh node (aceteam#6524).
//   - desktop: FILE_SCREENSHOT/VNC_SCREENSHOT/VNC_TYPE/VNC_KEYS/VNC_ACTIONS
//     register only when !DesktopDisabled. DesktopDisabled tracks the node's
//     `desktop` permission, also default-DENY.
//   - config dir: SERVICE_START/SERVICE_STOP/SERVICE_STATUS register only when
//     ConfigDir != "" (a resolved citadel.yaml manifest directory).
//   - workspace: MEETING_JOIN registers only when WorkspaceDir != "" and the
//     node's meeting capability is enabled (config.LoadMeeting(...).MeetingEnabled,
//     default-on).
//
// A type absent from this map but also absent from a node's registered
// handlers is either genuinely unsupported by this build (not in
// allKnownJobTypes at all) or unconditionally registered and thus never
// reaches failUnsupportedJobType in a gated state.
var gatedJobTypeReasons = map[string]string{
	JobTypeFileRead:           "the node's \"files\" permission is disabled (default-deny on a fresh node; enable it with citadel_set_worker_permission)",
	JobTypeFileReadBytes:      "the node's \"files\" permission is disabled (default-deny on a fresh node; enable it with citadel_set_worker_permission)",
	JobTypeFileWrite:          "the node's \"files\" permission is disabled (default-deny on a fresh node; enable it with citadel_set_worker_permission)",
	JobTypeFileWriteBytes:     "the node's \"files\" permission is disabled (default-deny on a fresh node; enable it with citadel_set_worker_permission)",
	JobTypeFileEdit:           "the node's \"files\" permission is disabled (default-deny on a fresh node; enable it with citadel_set_worker_permission)",
	JobTypeFileList:           "the node's \"files\" permission is disabled (default-deny on a fresh node; enable it with citadel_set_worker_permission)",
	JobTypeFileSearch:         "the node's \"files\" permission is disabled (default-deny on a fresh node; enable it with citadel_set_worker_permission)",
	JobTypeFileIndex:          "the node's \"files\" permission is disabled (default-deny on a fresh node; enable it with citadel_set_worker_permission)",
	JobTypeFileSemanticSearch: "the node's \"files\" permission is disabled (default-deny on a fresh node; enable it with citadel_set_worker_permission)",
	JobTypeFileScreenshot:     "the node's \"desktop\" permission is disabled (default-deny on a fresh node; enable it with citadel_set_worker_permission)",
	JobTypeVNCScreenshot:      "the node's \"desktop\" permission is disabled (default-deny on a fresh node; enable it with citadel_set_worker_permission)",
	JobTypeVNCType:            "the node's \"desktop\" permission is disabled (default-deny on a fresh node; enable it with citadel_set_worker_permission)",
	JobTypeVNCKeys:            "the node's \"desktop\" permission is disabled (default-deny on a fresh node; enable it with citadel_set_worker_permission)",
	JobTypeVNCActions:         "the node's \"desktop\" permission is disabled (default-deny on a fresh node; enable it with citadel_set_worker_permission)",
	JobTypeServiceStart:       "this node has no citadel.yaml manifest / config directory configured",
	JobTypeServiceStop:        "this node has no citadel.yaml manifest / config directory configured",
	JobTypeServiceStatus:      "this node has no citadel.yaml manifest / config directory configured",
	JobTypeMeetingJoin:        "this node has no workspace directory configured, or its meeting capability is toggled off",
}
