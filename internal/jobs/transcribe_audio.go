// internal/jobs/transcribe_audio.go
package jobs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

// defaultTranscribeServiceURL is the local faster-whisper sidecar address.
// Mirrors the extraction service pattern (localhost HTTP, Docker compose
// managed via SERVICE_START). Port 8101 avoids the extraction service (8100).
const defaultTranscribeServiceURL = "http://localhost:8101"

// transcribeReadyTimeout bounds how long we wait for the whisper sidecar to
// load its model and report healthy. Model load (faster-whisper "base", int8)
// is a one-time cost on first job; subsequent jobs hit a warm service. This
// budget only applies once the sidecar is actually answering connections —
// see transcribeUnreachableTimeout for the case where nothing is listening.
const transcribeReadyTimeout = 120 * time.Second

// transcribeUnreachableTimeout bounds how long waitForReady tolerates a
// connection-refused health check, i.e. nothing listening on the sidecar's
// port at all. A refused connection means the sidecar container was never
// started (or crashed before it could bind its port) — not that it is warming
// up — so there is nothing worth waiting the full transcribeReadyTimeout for.
// Keeping this short ensures the handler fails well under the backend's
// ~100s request-gateway budget, so the backend can fall back to cloud
// transcription instead of the client eventually seeing a bare "Failed to
// fetch" once the gateway times out first.
const transcribeUnreachableTimeout = 8 * time.Second

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
// Response JSON (relayed verbatim from the sidecar). Fields are additive: old
// callers reading text/language/segments still work. When diarize is set, each
// segment carries a raw speaker label and a speakers[] roster is included; with
// a HuggingFace token the labels are real pyannote identities ("SPEAKER_NN"),
// otherwise a silence-gap fallback ("Speaker N"). speakers[].id equals the
// segment's speaker label verbatim (the join key between a segment and the
// roster); speakers[].label is a human-friendly name. start/end are seconds.
//
//	{
//	  "text": "full transcript",
//	  "language": "en",
//	  "segments": [
//	    {"start": 0.0, "end": 3.2, "text": "...", "speaker": "SPEAKER_00"}
//	  ],
//	  "speakers": [
//	    {"id": "SPEAKER_00", "label": "Speaker 1", "talkTimePct": 62.5}
//	  ],
//	  "diarization": "pyannote"
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

	for {
		resp, err := h.client().Get(healthURL)
		if err == nil {
			ready := resp.StatusCode == http.StatusOK
			resp.Body.Close()
			if ready {
				return nil
			}
			// Reachable, just not ready yet (e.g. model still loading) —
			// fall through to the patient transcribeReadyTimeout budget below.
		} else if isConnectionRefused(err) && time.Since(startTime) >= transcribeUnreachableTimeout {
			// Nothing has ever answered on this port within the fast-fail
			// budget: treat the sidecar as absent rather than warming up, and
			// give up now instead of burning the full model-load timeout.
			return fmt.Errorf("transcription service unreachable at %s: %w", h.serviceURL(), err)
		}

		if time.Since(startTime) >= transcribeReadyTimeout {
			break
		}
		time.Sleep(pollInterval)
	}
	return fmt.Errorf("transcription service did not become ready within %v", transcribeReadyTimeout)
}

// isConnectionRefused reports whether err indicates nothing is listening on
// the target port at all, as distinct from a service that answered but isn't
// ready yet (e.g. still loading its model).
func isConnectionRefused(err error) bool {
	return err != nil && strings.Contains(err.Error(), "connection refused")
}

// Ensure TranscribeAudioHandler implements JobHandler.
var _ JobHandler = (*TranscribeAudioHandler)(nil)
