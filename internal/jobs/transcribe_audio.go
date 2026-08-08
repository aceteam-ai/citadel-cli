// internal/jobs/transcribe_audio.go
package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
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

// The transcribe request timeout is sized PER REQUEST from the audio file's
// byte length rather than a single fixed cap. A fixed 30-minute cap was too
// short in the field: a real 43-minute meeting recorded an ~83 MB WAV whose
// end-of-call batch transcription ran past 30 minutes on CPU whisper and the
// client aborted mid-request ("Client.Timeout exceeded while awaiting
// headers"). The sidecar responds only once the WHOLE file is transcribed
// (there is no streaming/offset API), so the client must tolerate a budget
// proportional to the audio's real duration.
//
// Sizing by bytes also makes the rolling/windowed passes cheap for free: those
// transcribe only a short trailing clip (small file -> short budget), while the
// end-of-call batch pass over the full recording (large file -> long budget)
// gets the headroom it needs. Both flow through the same handler.
const (
	// transcribeBytesPerSecond estimates recorded audio duration from a WAV
	// file's size. The meeting recorder captures mono 16 kHz signed-16-bit PCM
	// (platform.buildAudioFFmpegArgs: "-ac 1 -ar 16000"), i.e.
	// 16000 * 2 bytes = 32000 bytes per second of audio. This is used ONLY to
	// size the request timeout, so an approximate rate is fine — the generous
	// per-second multiplier below absorbs the WAV header and any format slack.
	// NOTE: this is coupled to the recorder's uncompressed WAV format. The batch
	// pass transcribes the WAV (not the Opus backup added in #555); if the
	// transcribed input ever becomes compressed, revisit this constant so the
	// budget does not silently under-time.
	transcribeBytesPerSecond = 32000

	// transcribeSecondsPerAudioSecond is the wall-clock budget allowed per
	// second of recorded audio. faster-whisper "base" (int8) on CPU runs near
	// real time but can be slower on a loaded or modest node, so we budget a
	// generous multiple of the audio's own duration.
	transcribeSecondsPerAudioSecond = 3

	// transcribeMinRequestTimeout floors the per-request budget so tiny inputs
	// (short rolling clips, model warm-up, scheduling jitter) always get a
	// workable window even when the size-derived estimate is small.
	transcribeMinRequestTimeout = 2 * time.Minute

	// transcribeMaxRequestTimeout caps the per-request budget. At the
	// transcribeSecondsPerAudioSecond multiple, a full-length recording at the
	// defaultMeetingMaxDuration hard cap (4h) needs ~12h of budget, so this
	// ceiling is sized to cover it while still bounding a corrupt or absurdly
	// large file from wedging a worker slot indefinitely.
	transcribeMaxRequestTimeout = 12 * time.Hour

	// transcribeHealthTimeout bounds a single health-check GET. The readiness
	// loop's own budgets (transcribeReadyTimeout / transcribeUnreachableTimeout)
	// govern total wait; this just stops one poll from hanging if the sidecar
	// accepts the connection but never answers.
	transcribeHealthTimeout = 10 * time.Second
)

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

// client returns the HTTP client used for both the health poll and the
// transcribe POST. It deliberately carries NO fixed Timeout: the whole-request
// budget is governed by a per-request context deadline sized to the input (see
// requestTimeout / transcribeTimeoutForAudioBytes), so a long meeting's batch
// pass is not capped by a one-size-fits-all client timeout while a short rolling
// clip is not made to wait needlessly.
func (h *TranscribeAudioHandler) client() *http.Client {
	if h.HTTPClient != nil {
		return h.HTTPClient
	}
	return &http.Client{}
}

// transcribeTimeoutForAudioBytes maps an audio file's byte length to a
// generous request timeout. It estimates the audio's real duration from the
// byte count (uncompressed 16 kHz mono PCM WAV) and multiplies it by
// transcribeSecondsPerAudioSecond, clamped to [min, max]. Pure and
// table-testable; callers stat the file and pass its size.
func transcribeTimeoutForAudioBytes(sizeBytes int64) time.Duration {
	if sizeBytes <= 0 {
		// Unknown/empty size: prefer the generous ceiling over under-timing,
		// since a premature client timeout is the exact bug being fixed.
		return transcribeMaxRequestTimeout
	}
	estSeconds := sizeBytes / transcribeBytesPerSecond
	budget := time.Duration(estSeconds*transcribeSecondsPerAudioSecond) * time.Second
	if budget < transcribeMinRequestTimeout {
		return transcribeMinRequestTimeout
	}
	if budget > transcribeMaxRequestTimeout {
		return transcribeMaxRequestTimeout
	}
	return budget
}

// requestTimeout sizes the transcribe request budget from the file at
// validatedPath. On stat failure it returns the generous ceiling rather than a
// small default: under-timing is precisely the failure mode being fixed, and a
// missing file surfaces as a transcribe error anyway.
func (h *TranscribeAudioHandler) requestTimeout(validatedPath string) time.Duration {
	info, err := os.Stat(validatedPath)
	if err != nil {
		return transcribeMaxRequestTimeout
	}
	return transcribeTimeoutForAudioBytes(info.Size())
}

// Execute transcribes a workspace-local audio file via the whisper sidecar.
//
// Payload fields (all strings via nexus.Job):
//   - audio_path: workspace-relative or absolute path to the recorded audio.
//   - language:   optional ISO language hint (e.g. "en"); empty = auto-detect.
//   - diarize:    optional speaker-labelling mode. "speaker" = real pyannote
//     diarization (reprocess path); "true" = basic silence-gap
//     labelling (quick path); anything else = no labels.
//
// Response JSON (relayed verbatim from the sidecar). Fields are additive: old
// callers reading text/language/segments still work. When diarize is set, each
// segment carries a raw speaker label and a speakers[] roster is included. The
// `diarization` field reports the tier that actually ran: "speaker" (real
// pyannote identities "SPEAKER_NN"; needs a HuggingFace token), "basic"
// (silence-gap fallback "Speaker N"), or "none". speakers[].id equals the
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
//	  "diarization": "speaker"
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
	// diarize modes: "speaker" requests REAL pyannote diarization (reprocess
	// path); "true" requests BASIC silence-gap labelling (quick path). The
	// sidecar reports which tier actually ran in its `diarization` response
	// field ("speaker"/"basic"/"none") so the backend can decide whether to
	// replace the stored transcript or keep retrying.
	switch job.Payload["diarize"] {
	case "speaker":
		requestPayload["diarize"] = true
		requestPayload["speaker"] = true
	case "true":
		requestPayload["diarize"] = true
	}

	reqBody, err := json.Marshal(requestPayload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	// Size the whole-request budget from the audio's byte length so a long
	// meeting's full-file transcription is not cut off mid-flight. The context
	// governs the entire request including the body read below, so cancel only
	// after Execute is done with the response.
	reqTimeout := h.requestTimeout(validated)
	reqCtx, cancel := context.WithTimeout(context.Background(), reqTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, h.serviceURL()+"/transcribe", bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, fmt.Errorf("failed to build transcription request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.client().Do(req)
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
		resp, err := h.healthCheck(healthURL)
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

// healthCheck performs a single readiness GET bounded by transcribeHealthTimeout
// so one poll cannot hang now that the shared client carries no fixed timeout.
// A dead port still returns connection-refused immediately (before the
// deadline), preserving the fast-fail path in waitForReady.
func (h *TranscribeAudioHandler) healthCheck(healthURL string) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(context.Background(), transcribeHealthTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
	if err != nil {
		return nil, err
	}
	return h.client().Do(req)
}

// isConnectionRefused reports whether err indicates nothing is listening on
// the target port at all, as distinct from a service that answered but isn't
// ready yet (e.g. still loading its model).
func isConnectionRefused(err error) bool {
	return err != nil && strings.Contains(err.Error(), "connection refused")
}

// Ensure TranscribeAudioHandler implements JobHandler.
var _ JobHandler = (*TranscribeAudioHandler)(nil)
