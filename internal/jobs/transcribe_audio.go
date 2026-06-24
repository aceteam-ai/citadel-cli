// internal/jobs/transcribe_audio.go
package jobs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

// defaultTranscribeServiceURL is the local faster-whisper sidecar address.
// Mirrors the extraction service pattern (localhost HTTP, Docker compose
// managed via SERVICE_START). Port 8101 avoids the extraction service (8100).
const defaultTranscribeServiceURL = "http://localhost:8101"

// transcribeReadyTimeout bounds how long we wait for the whisper sidecar to
// load its model and report healthy. Model load (faster-whisper "base", int8)
// is a one-time cost on first job; subsequent jobs hit a warm service.
const transcribeReadyTimeout = 120 * time.Second

// transcribeRequestTimeout bounds a single transcription. Whisper on CPU is
// roughly real-time-ish for the "base" model, so a long meeting needs headroom.
const transcribeRequestTimeout = 30 * time.Minute

// TranscribeAudioHandler handles TRANSCRIBE_AUDIO jobs node-locally.
//
// It mirrors ExtractionHandler: the heavy Python/ML dependency (faster-whisper)
// lives in a Docker sidecar (services/compose/transcribe.yml) reachable over
// localhost, and this Go handler just proxies a request to it and relays the
// JSON result. Audio bytes were placed in the node workspace beforehand via
// FILE_WRITE_BYTES, so nothing here touches the cloud: the audio and the
// resulting transcript stay on the user's machine.
type TranscribeAudioHandler struct {
	// WorkspaceDir roots audio path validation, matching the file handlers.
	WorkspaceDir string
	// ServiceURL is the whisper sidecar base URL; defaults to localhost:8101.
	ServiceURL string
	// HTTPClient lets tests inject a stub; nil uses a default client.
	HTTPClient *http.Client
}

// NewTranscribeAudioHandler creates a handler rooted at workspace.
func NewTranscribeAudioHandler(workspace string) *TranscribeAudioHandler {
	return &TranscribeAudioHandler{
		WorkspaceDir: workspace,
		ServiceURL:   defaultTranscribeServiceURL,
	}
}

func (h *TranscribeAudioHandler) serviceURL() string {
	if h.ServiceURL != "" {
		return h.ServiceURL
	}
	return defaultTranscribeServiceURL
}

func (h *TranscribeAudioHandler) client() *http.Client {
	if h.HTTPClient != nil {
		return h.HTTPClient
	}
	return &http.Client{Timeout: transcribeRequestTimeout}
}

// Execute transcribes a workspace-local audio file via the whisper sidecar.
//
// Payload fields (all strings via nexus.Job):
//   - audio_path: workspace-relative or absolute path to the recorded audio.
//   - language:   optional ISO language hint (e.g. "en"); empty = auto-detect.
//   - diarize:    optional "true"/"false"; basic per-segment speaker labels.
//
// Response JSON (relayed verbatim from the sidecar):
//
//	{
//	  "text": "full transcript",
//	  "language": "en",
//	  "language_probability": 0.98,
//	  "segments": [
//	    {"start": 0.0, "end": 3.2, "text": "...", "speaker": "Speaker 1"}
//	  ]
//	}
func (h *TranscribeAudioHandler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
	audioPath, ok := job.Payload["audio_path"]
	if !ok || audioPath == "" {
		return nil, fmt.Errorf("job payload missing 'audio_path' field")
	}

	// Validate the path against the workspace so a malicious payload cannot
	// point the sidecar at an arbitrary file outside the sandbox.
	validated, err := ValidatePath(h.WorkspaceDir, audioPath)
	if err != nil {
		return nil, fmt.Errorf("path validation failed: %w", err)
	}

	// The whisper sidecar mounts the workspace at /workspace, so send the path
	// RELATIVE to the workspace root. The service joins it under its own mount;
	// it never sees (or can escape to) the host filesystem.
	//
	// ValidatePath resolves relative inputs against the SYMLINK-RESOLVED
	// workspace root, so compute the relative path against the resolved root
	// too. Using the raw WorkspaceDir here would emit spurious "../" prefixes
	// when the workspace contains a symlinked component (e.g. a symlinked
	// tmpdir, or /var -> /private/var on macOS), which the sidecar would reject.
	resolvedWorkspace, err := filepath.EvalSymlinks(h.WorkspaceDir)
	if err != nil {
		return nil, fmt.Errorf("cannot resolve workspace %q: %w", h.WorkspaceDir, err)
	}
	rel, err := filepath.Rel(resolvedWorkspace, validated)
	if err != nil {
		return nil, fmt.Errorf("cannot compute workspace-relative path: %w", err)
	}

	ctx.Log("info", "     - [Job %s] Waiting for transcription service to become ready...", job.ID)
	if err := h.waitForReady(); err != nil {
		return nil, err
	}
	ctx.Log("info", "     - [Job %s] TRANSCRIBE_AUDIO %s", job.ID, rel)

	requestPayload := map[string]any{
		"audio_path": rel,
	}
	if lang, ok := job.Payload["language"]; ok && lang != "" {
		requestPayload["language"] = lang
	}
	if diarize, ok := job.Payload["diarize"]; ok && diarize == "true" {
		requestPayload["diarize"] = true
	}

	reqBody, err := json.Marshal(requestPayload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	resp, err := h.client().Post(
		h.serviceURL()+"/transcribe",
		"application/json",
		bytes.NewBuffer(reqBody),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to transcription service: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return bodyBytes, fmt.Errorf("transcription API returned non-200 status: %s", resp.Status)
	}

	return bodyBytes, nil
}

func (h *TranscribeAudioHandler) waitForReady() error {
	healthURL := h.serviceURL() + "/health"
	pollInterval := 1 * time.Second
	startTime := time.Now()

	for time.Since(startTime) < transcribeReadyTimeout {
		resp, err := http.Get(healthURL)
		if err == nil && resp.StatusCode == http.StatusOK {
			resp.Body.Close()
			return nil
		}
		if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(pollInterval)
	}
	return fmt.Errorf("transcription service did not become ready within %v", transcribeReadyTimeout)
}

// Ensure TranscribeAudioHandler implements JobHandler.
var _ JobHandler = (*TranscribeAudioHandler)(nil)
