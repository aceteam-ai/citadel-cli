// internal/jobs/transcribe_audio.go
package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/status"
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

// The transcribe request timeout is sized PER REQUEST from the audio's real
// duration where possible. A fixed 30-minute cap was too
// short in the field: a real 43-minute meeting recorded an ~83 MB WAV whose
// end-of-call batch transcription ran past 30 minutes on CPU whisper and the
// client aborted mid-request ("Client.Timeout exceeded while awaiting
// headers"). The sidecar responds only once the WHOLE file is transcribed
// (there is no streaming/offset API), so the client must tolerate a budget
// proportional to the audio's real duration.
//
// Duration is essential for compressed recordings: a 38-minute Opus file can
// be only a few MB, for which the old PCM byte estimate provided only minutes
// of budget. ffprobe is deliberately best-effort and tightly bounded; when it
// cannot provide a sane duration we retain the established PCM byte estimate.
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

	// transcribeDurationProbeTimeout stops malformed containers or a stuck
	// ffprobe binary from delaying a job before the sidecar request begins.
	transcribeDurationProbeTimeout = 2 * time.Second
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
	// ConfigDir roots a live status.NodeStatus collection used ONLY by
	// citadel#891's readiness-failure diagnosis (waitForReady) and VRAM
	// preflight (checkVRAMPreflight). Empty (the zero value, and every
	// existing hermetic test) skips both: no annotation, no preflight check
	// -- fail open, not fail closed, on missing observability. See
	// meeting_vram_diagnosis.go.
	ConfigDir string
	// collectStatusFn overrides collectStatusForDiagnosis for tests; nil uses
	// a real status.NewCollector collection rooted at ConfigDir.
	collectStatusFn collectStatusFn
	// probeAudioDurationFn is a hermetic-test seam. Nil invokes ffprobe.
	probeAudioDurationFn func(string) (time.Duration, error)
}

// collectStatusForDiagnosis returns live node status for citadel#891's
// readiness-failure diagnosis and VRAM preflight, via the injected
// collectStatusFn test seam when set, else a real status.NewCollector
// collection rooted at ConfigDir. Errors (including "no ConfigDir
// configured") are the caller's cue to skip enrichment/preflight entirely --
// this never fails a job on its own.
func (h *TranscribeAudioHandler) collectStatusForDiagnosis() (*status.NodeStatus, error) {
	if h.collectStatusFn != nil {
		return h.collectStatusFn()
	}
	if h.ConfigDir == "" {
		return nil, fmt.Errorf("no config dir configured for status collection")
	}
	return status.NewCollector(status.CollectorConfig{ConfigDir: h.ConfigDir}).Collect()
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

// transcribeTimeoutForAudioDuration turns a trusted container duration into a
// bounded sidecar budget. The cap applies only to media-derived input; an
// explicit worker job deadline remains authoritative (see requestTimeout).
func transcribeTimeoutForAudioDuration(duration time.Duration) time.Duration {
	if duration <= 0 {
		return transcribeMaxRequestTimeout
	}
	if duration > time.Duration(math.MaxInt64/transcribeSecondsPerAudioSecond) {
		return transcribeMaxRequestTimeout
	}
	budget := duration * transcribeSecondsPerAudioSecond
	if budget < transcribeMinRequestTimeout {
		return transcribeMinRequestTimeout
	}
	if budget > transcribeMaxRequestTimeout {
		return transcribeMaxRequestTimeout
	}
	return budget
}

// probeAudioDuration invokes ffprobe with a fixed argv (not a shell) and a
// short deadline. It reads a single numeric format duration, rejecting NaN,
// infinity, zero, and values that cannot fit time.Duration. Callers treat any
// failure as a signal to use the conservative existing byte fallback.
func probeAudioDuration(path string) (time.Duration, error) {
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		return 0, fmt.Errorf("ffprobe unavailable: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), transcribeDurationProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, ffprobe,
		"-v", "error", "-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1", "--", path).Output()
	if err != nil {
		return 0, fmt.Errorf("ffprobe duration: %w", err)
	}
	seconds, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds <= 0 || seconds > float64(math.MaxInt64)/float64(time.Second) {
		return 0, fmt.Errorf("invalid ffprobe duration %q", strings.TrimSpace(string(out)))
	}
	return time.Duration(seconds * float64(time.Second)), nil
}

func (h *TranscribeAudioHandler) audioDuration(path string) (time.Duration, error) {
	if h.probeAudioDurationFn != nil {
		return h.probeAudioDurationFn(path)
	}
	return probeAudioDuration(path)
}

// requestTimeout sizes the transcribe request budget from the file at
// validatedPath. On stat failure it returns the generous ceiling rather than a
// small default: under-timing is precisely the failure mode being fixed, and a
// missing file surfaces as a transcribe error anyway.
//
// This is the MODEL-AGNOSTIC budget (today's behavior, kept for the existing
// tests). The Execute path uses requestTimeoutForModel, which layers a
// per-model factor + one-time load allowance on top for the larger, slower
// on-demand models (citadel#1045).
func (h *TranscribeAudioHandler) requestTimeout(validatedPath string) time.Duration {
	if duration, err := h.audioDuration(validatedPath); err == nil {
		return transcribeTimeoutForAudioDuration(duration)
	}
	info, err := os.Stat(validatedPath)
	if err != nil {
		return transcribeMaxRequestTimeout
	}
	return transcribeTimeoutForAudioBytes(info.Size())
}

// requestTimeoutWithJobBudget prevents this handler's own deadline from being
// shorter than the worker's already-authorized execution window. The worker
// context is still the parent, so shutdown/cancellation always wins; this only
// removes the accidental earlier client-side cutoff. No job deadline means the
// bounded media-derived budget remains in force.
func requestTimeoutWithJobBudget(ctx context.Context, mediaBudget time.Duration) time.Duration {
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining > mediaBudget {
			return remaining
		}
	}
	return mediaBudget
}

// Model-aware request budget (citadel#1045). The base sizing above assumes
// faster-whisper "base" (int8) on CPU runs near real time. A caller-selected
// larger model (small/medium/large-v3) runs several-x slower AND downloads +
// loads its weights INSIDE the first /transcribe call (health reports ok
// regardless, since the model is lazy-loaded), so a short clip transcribed with
// "medium" would otherwise hit the 2-minute floor mid-download. These factors
// scale the base budget and add a one-time load allowance so the client does
// not abort a legitimately-slow larger-model pass. Package vars, not consts, so
// a future operator override is a one-line change and tests can pin them.
var (
	// transcribeModelTimeoutFactors multiplies the base (size-derived) budget
	// per model family. An empty modelSize (the default, no caller override)
	// and the base tier both resolve to factor 1 with no load allowance, so
	// requestTimeoutForModel(size, "") == requestTimeoutForAudioBytes(size)
	// exactly — the byte-identical no-op the "no new params" contract requires.
	transcribeModelTimeoutFactors = map[string]int{
		"tiny": 1, "tiny.en": 1,
		"base": 1, "base.en": 1,
		"small": 2, "small.en": 2, "distil-small.en": 2,
		"medium": 4, "medium.en": 4, "distil-medium.en": 4,
		"large": 8, "large-v1": 8, "large-v2": 8, "large-v3": 8,
		"distil-large-v2": 6, "distil-large-v3": 6,
	}
	// transcribeModelLoadAllowance is the extra one-time budget added for a
	// larger model's first-request download+load. Keyed by the same names; the
	// base tier and unset default add nothing.
	transcribeModelLoadAllowance = map[string]time.Duration{
		"small": 5 * time.Minute, "small.en": 5 * time.Minute, "distil-small.en": 5 * time.Minute,
		"medium": 15 * time.Minute, "medium.en": 15 * time.Minute, "distil-medium.en": 15 * time.Minute,
		"large": 30 * time.Minute, "large-v1": 30 * time.Minute, "large-v2": 30 * time.Minute, "large-v3": 30 * time.Minute,
		"distil-large-v2": 20 * time.Minute, "distil-large-v3": 20 * time.Minute,
	}
)

// transcribeTimeoutFor sizes the per-request budget from the audio byte length
// AND the selected model. modelSize == "" (or an unrecognized value) reduces to
// exactly transcribeTimeoutForAudioBytes(sizeBytes), preserving today's default
// behavior; a larger model scales the budget and adds a load allowance, clamped
// to [min, max]. Pure and table-testable.
func transcribeTimeoutFor(sizeBytes int64, modelSize string) time.Duration {
	return transcribeTimeoutFromBase(transcribeTimeoutForAudioBytes(sizeBytes), modelSize)
}

func transcribeTimeoutFromBase(base time.Duration, modelSize string) time.Duration {
	factor := transcribeModelTimeoutFactors[modelSize]
	if factor < 1 {
		factor = 1
	}
	budget := time.Duration(factor)*base + transcribeModelLoadAllowance[modelSize]
	if budget < transcribeMinRequestTimeout {
		return transcribeMinRequestTimeout
	}
	if budget > transcribeMaxRequestTimeout {
		return transcribeMaxRequestTimeout
	}
	return budget
}

// requestTimeoutForModel sizes the request budget from the file at
// validatedPath and the selected model (citadel#1045). On stat failure it
// falls back to the model-aware ceiling, same fail-generous direction as
// requestTimeout.
func (h *TranscribeAudioHandler) requestTimeoutForModel(validatedPath, modelSize string) time.Duration {
	if duration, err := h.audioDuration(validatedPath); err == nil {
		return transcribeTimeoutForDuration(duration, modelSize)
	}
	info, err := os.Stat(validatedPath)
	if err != nil {
		return transcribeTimeoutFor(0, modelSize)
	}
	return transcribeTimeoutFor(info.Size(), modelSize)
}

// transcribeTimeoutForDuration applies the existing selected-model factor to a
// duration-derived base budget. Kept separate from the byte fallback so a
// compressed input can never silently be treated as PCM.
func transcribeTimeoutForDuration(duration time.Duration, modelSize string) time.Duration {
	return transcribeTimeoutFromBase(transcribeTimeoutForAudioDuration(duration), modelSize)
}

// allowedWhisperModelSizes whitelists the model_size values accepted from the
// payload, matching faster-whisper 1.0.3's _MODELS keys (verified against the
// pinned tag, faster_whisper/utils.py). The Python sidecar enforces the same
// set as defense in depth. A value outside this set is a hard error (fail fast
// with a clear message) rather than a silent fall-back to the default model —
// the caller asked for a specific model and should be told it is unavailable.
var allowedWhisperModelSizes = []string{
	"tiny", "tiny.en",
	"base", "base.en",
	"small", "small.en",
	"medium", "medium.en",
	"large-v1", "large-v2", "large-v3", "large",
	"distil-large-v2", "distil-medium.en", "distil-small.en", "distil-large-v3",
}

func isAllowedModelSize(v string) bool {
	for _, m := range allowedWhisperModelSizes {
		if v == m {
			return true
		}
	}
	return false
}

// applyTranscribeOptions validates the optional tuning params and copies them
// onto the request payload sent to the whisper sidecar (citadel#1045). It is
// pure (map in, map out, error) so param plumbing + validation are unit-testable
// without a running sidecar. It only ADDS a key when the payload actually
// carries it, so a request with none of the new params produces a byte-identical
// request to before this change:
//
//	{audio_path[, language][, diarize]}
//
// language/diarize keep their exact pre-existing semantics (diarize is a literal
// "true" string compare, NOT lenient bool parsing); only the new params use
// strconv parsing and are validated.
func applyTranscribeOptions(req map[string]any, payload map[string]string) error {
	if lang, ok := payload["language"]; ok && lang != "" {
		req["language"] = lang
	}
	if diarize, ok := payload["diarize"]; ok && diarize == "true" {
		req["diarize"] = true
	}

	if v, ok := payload["model_size"]; ok && v != "" {
		if !isAllowedModelSize(v) {
			return fmt.Errorf("invalid model_size %q (allowed: %s)", v, strings.Join(allowedWhisperModelSizes, ", "))
		}
		req["model_size"] = v
	}

	// Optional booleans: denoise (ffmpeg afftdn preprocessing), vad_filter
	// (faster-whisper VAD gate), condition_on_previous_text (the mechanism
	// behind the looped-filler hallucination — a caller can turn it off).
	for _, key := range []string{"denoise", "vad_filter", "condition_on_previous_text"} {
		v, ok := payload[key]
		if !ok || v == "" {
			continue
		}
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("invalid %s %q: must be true or false", key, v)
		}
		req[key] = b
	}

	// Optional float thresholds passed through to faster-whisper decoding.
	// logprob_threshold maps to the engine's log_prob_threshold kwarg
	// (renamed on the Python side); no_speech_threshold and
	// compression_ratio_threshold keep their names.
	for _, key := range []string{"no_speech_threshold", "compression_ratio_threshold", "logprob_threshold"} {
		v, ok := payload[key]
		if !ok || v == "" {
			continue
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return fmt.Errorf("invalid %s %q: must be a number", key, v)
		}
		req[key] = f
	}

	return nil
}

// Execute transcribes a workspace-local audio file via the whisper sidecar.
//
// Payload fields (all strings via nexus.Job):
//   - audio_path: workspace-relative or absolute path to the recorded audio.
//   - language:   optional ISO language hint (e.g. "en"); empty = auto-detect.
//   - diarize:    optional "true"/"false"; basic per-segment speaker labels.
//   - model_size: optional faster-whisper model (tiny|base|small|medium|
//     large-v3|...); empty = the sidecar's configured default. Loaded on demand.
//   - denoise:    optional "true"/"false"; ffmpeg afftdn denoise preprocessing.
//   - vad_filter, no_speech_threshold, compression_ratio_threshold,
//     logprob_threshold, condition_on_previous_text: optional faster-whisper
//     decoding tuning, passed through to the sidecar.
//
// Every param above beyond audio_path is OPTIONAL and backward-compatible: a
// request carrying none of them behaves exactly as before. The result always
// carries an additive `transcription_guard` advisory (citadel#1045); it never
// alters the transcript itself.
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
	// citadel#891: refuse fast, before burning the transcribeReadyTimeout
	// budget, if the payload declares a VRAM budget this node cannot fit.
	// Inert today (the backend does not send vram_mb/vram_gb on
	// TRANSCRIBE_AUDIO yet); see meeting_vram_diagnosis.go.
	if err := checkVRAMPreflight(ctx, job.Payload, h.collectStatusForDiagnosis); err != nil {
		return nil, err
	}

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

	// Build + validate the request payload BEFORE waiting on the sidecar, so an
	// invalid model_size/threshold fails fast with a clear error instead of
	// burning the readiness budget first.
	requestPayload := map[string]any{
		"audio_path": rel,
	}
	if err := applyTranscribeOptions(requestPayload, job.Payload); err != nil {
		return nil, err
	}
	languageHinted := job.Payload["language"] != ""
	modelSize := job.Payload["model_size"]

	reqBody, err := json.Marshal(requestPayload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	ctx.Log("info", "     - [Job %s] Waiting for transcription service to become ready...", job.ID)
	if err := h.waitForReady(); err != nil {
		return nil, err
	}
	ctx.Log("info", "     - [Job %s] TRANSCRIBE_AUDIO %s", job.ID, rel)

	// Size the whole-request budget from the audio's byte length AND the chosen
	// model so a long meeting's full-file transcription — or a slow, larger
	// on-demand model's first-request load+decode — is not cut off mid-flight.
	// The context governs the entire request including the body read below, so
	// cancel only after Execute is done with the response.
	reqTimeout := requestTimeoutWithJobBudget(ctx.Context(), h.requestTimeoutForModel(validated, modelSize))
	reqCtx, cancel := context.WithTimeout(ctx.Context(), reqTimeout)
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

	// Attach the additive no-speech/low-confidence guard (citadel#1045). On any
	// parse failure attachTranscriptionGuard returns bodyBytes verbatim.
	return attachTranscriptionGuard(bodyBytes, languageHinted), nil
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
			return h.annotateReadinessError(fmt.Errorf("transcription service unreachable at %s: %w", h.serviceURL(), err))
		}

		if time.Since(startTime) >= transcribeReadyTimeout {
			break
		}
		time.Sleep(pollInterval)
	}
	return h.annotateReadinessError(fmt.Errorf("transcription service did not become ready within %v", transcribeReadyTimeout))
}

// annotateReadinessError appends citadel#891's diagnosis (free VRAM/RAM +
// top resource holders) to a waitForReady failure, so "did not become
// ready"/"unreachable" stops being silent about WHY. Best-effort: a
// collection failure (or no ConfigDir configured) returns err unchanged
// rather than masking the original failure with a collection error.
func (h *TranscribeAudioHandler) annotateReadinessError(err error) error {
	st, cerr := h.collectStatusForDiagnosis()
	if cerr != nil {
		return err
	}
	if ann := diagnoseReadinessFailure(st); ann != "" {
		return fmt.Errorf("%w%s", err, ann)
	}
	return err
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
