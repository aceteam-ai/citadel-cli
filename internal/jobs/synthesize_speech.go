// internal/jobs/synthesize_speech.go
package jobs

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	embeddedservices "github.com/aceteam-ai/citadel-cli/services"
)

// synthesizeServiceURL is the local kokoro TTS sidecar base URL. The host port
// is owned by citadel (services/ports.go, CITADEL_TTS_HOST_PORT) and reached
// over loopback: the compose publishes 127.0.0.1 ONLY, since the service has
// no auth of its own and its sole consumer is this co-located worker. Built from
// the registry constant rather than a literal so it tracks the port citadel
// actually injects (the transcribe handler's hardcoded 8101 is the anti-pattern
// this avoids).
func synthesizeServiceURL() string {
	return fmt.Sprintf("http://localhost:%d", embeddedservices.TTSHostPort)
}

const (
	// defaultSynthesizeVoice is the Kokoro voice used when a job omits `voice`.
	// Matches the service's own KOKORO_DEFAULT_VOICE default (am_michael, the
	// book-narration voice).
	defaultSynthesizeVoice = "am_michael"

	// defaultSynthesizeFormat is the output container used when a job omits
	// `response_format`. Opus is compact and speech-tuned; matches the service's
	// KOKORO_DEFAULT_FORMAT default.
	defaultSynthesizeFormat = "opus"

	// synthesizeReadyTimeout bounds how long we wait for the kokoro sidecar to
	// load its model (Kokoro-82M, ~350 MB) and report healthy. Model load is a
	// one-time cost on first job; subsequent jobs hit a warm service. This budget
	// only applies once the sidecar is actually answering connections; see
	// synthesizeUnreachableTimeout for the case where nothing is listening.
	synthesizeReadyTimeout = 120 * time.Second

	// synthesizeUnreachableTimeout bounds how long waitForReady tolerates a
	// connection-refused health check, i.e. nothing listening on the sidecar's
	// port at all (the container was never started or crashed before binding).
	// Kept short so the handler fails well under the backend's request-gateway
	// budget and the backend can fall back to cloud TTS instead of hanging.
	synthesizeUnreachableTimeout = 8 * time.Second

	// synthesizeHealthTimeout bounds a single readiness GET so one poll cannot
	// hang if the sidecar accepts the connection but never answers.
	synthesizeHealthTimeout = 10 * time.Second

	// synthesizeRequestTimeout bounds a single synthesis POST. Unlike transcribe
	// (whose budget scales with a potentially multi-hour audio file), TTS input
	// is bounded text (KOKORO_MAX_INPUT_CHARS, default 5000) and Kokoro-82M runs
	// near real time even on CPU, so one generous fixed cap is enough.
	synthesizeRequestTimeout = 5 * time.Minute
)

// SynthesizeSpeechHandler handles SYNTHESIZE_SPEECH jobs node-locally.
//
// It is the synthesis counterpart to TranscribeAudioHandler: the heavy ML
// dependency (Kokoro-82M) lives in a Docker sidecar (services/compose/kokoro.yml)
// reachable over loopback, and this Go handler proxies an OpenAI-compatible
// speech request to it. The text and the resulting audio never leave the node.
//
// Unlike transcribe, it needs no workspace: the text arrives inline in the
// payload and the audio is returned inline (base64), so the handler is
// registered unconditionally alongside the other sandbox-less inference handlers.
type SynthesizeSpeechHandler struct {
	// ServiceURL is the kokoro sidecar base URL; defaults to the registry port.
	ServiceURL string
	// HTTPClient lets tests inject a stub; nil uses a default client.
	HTTPClient *http.Client
}

// NewSynthesizeSpeechHandler creates a handler pointed at the local kokoro
// sidecar.
func NewSynthesizeSpeechHandler() *SynthesizeSpeechHandler {
	return &SynthesizeSpeechHandler{ServiceURL: synthesizeServiceURL()}
}

func (h *SynthesizeSpeechHandler) serviceURL() string {
	if h.ServiceURL != "" {
		return h.ServiceURL
	}
	return synthesizeServiceURL()
}

// client returns the HTTP client used for both the health poll and the
// synthesis POST. It carries no fixed Timeout: per-request budgets are governed
// by context deadlines (synthesizeRequestTimeout / synthesizeHealthTimeout).
func (h *SynthesizeSpeechHandler) client() *http.Client {
	if h.HTTPClient != nil {
		return h.HTTPClient
	}
	return &http.Client{}
}

// Execute synthesizes speech from text via the kokoro sidecar.
//
// Payload fields (all strings via nexus.Job):
//   - text / input:       the text to synthesize (required; `text` preferred,
//     `input` accepted as an alias for the OpenAI request-body spelling).
//   - voice:              optional Kokoro voice; empty defaults to am_michael.
//   - response_format:    optional output container (opus/mp3); empty defaults
//     to opus (`format` accepted as an alias).
//
// Response JSON (this handler DEFINES the envelope; nothing on the aceteam side
// parses it yet; the fabric may also call the endpoint directly):
//
//	{
//	  "encoding": "base64",          // the marker the coordinator checks
//	  "content":  "<base64 audio>",  // the synthesized audio bytes
//	  "format":   "opus",
//	  "voice":    "am_michael",
//	  "receipt": {                   // metering receipt, from the X-TTS-* headers
//	    "chars":            24,
//	    "duration_seconds": 3.575,
//	    "model_version":    "kokoro-0.9.4+hexgrad/Kokoro-82M",
//	    "cache_key":        "<sha256>",
//	    "cache_hit":        false
//	  }
//	}
func (h *SynthesizeSpeechHandler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
	text := job.Payload["text"]
	if text == "" {
		text = job.Payload["input"]
	}
	if text == "" {
		return nil, fmt.Errorf("job payload missing 'text' field")
	}

	voice := job.Payload["voice"]
	if voice == "" {
		voice = defaultSynthesizeVoice
	}

	format := job.Payload["response_format"]
	if format == "" {
		format = job.Payload["format"]
	}
	if format == "" {
		format = defaultSynthesizeFormat
	}

	ctx.Log("info", "     - [Job %s] Waiting for TTS service to become ready...", job.ID)
	if err := h.waitForReady(); err != nil {
		return nil, err
	}
	ctx.Log("info", "     - [Job %s] SYNTHESIZE_SPEECH voice=%s format=%s chars=%d", job.ID, voice, format, len(text))

	requestPayload := map[string]any{
		"input":           text,
		"voice":           voice,
		"response_format": format,
	}
	reqBody, err := json.Marshal(requestPayload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	reqCtx, cancel := context.WithTimeout(context.Background(), synthesizeRequestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, h.serviceURL()+"/v1/audio/speech", bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, fmt.Errorf("failed to build synthesis request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to TTS service: %w", err)
	}
	defer resp.Body.Close()

	audio, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		// On error the body is a JSON error, not audio; surface it verbatim.
		return audio, fmt.Errorf("TTS API returned non-200 status: %s", resp.Status)
	}

	result := map[string]any{
		"encoding": "base64",
		"content":  base64.StdEncoding.EncodeToString(audio),
		"format":   format,
		"voice":    voice,
		"receipt":  synthesizeReceiptFromHeaders(resp.Header),
	}
	return json.Marshal(result)
}

// synthesizeReceiptFromHeaders extracts the per-item metering receipt the kokoro
// service returns in its X-TTS-* response headers. Parsing is best-effort: a
// missing or malformed header yields the field's zero value rather than failing
// the job, since the audio artifact is the primary result and the receipt is
// advisory metering.
func synthesizeReceiptFromHeaders(headers http.Header) map[string]any {
	receipt := map[string]any{
		"model_version": headers.Get("X-TTS-Model-Version"),
		"cache_key":     headers.Get("X-TTS-Cache-Key"),
	}
	if chars, err := strconv.Atoi(headers.Get("X-TTS-Chars")); err == nil {
		receipt["chars"] = chars
	}
	if secs, err := strconv.ParseFloat(headers.Get("X-TTS-Duration-Seconds"), 64); err == nil {
		receipt["duration_seconds"] = secs
	}
	// X-TTS-Cache-Hit is "0" or "1".
	receipt["cache_hit"] = headers.Get("X-TTS-Cache-Hit") == "1"
	return receipt
}

// waitForReady polls the kokoro sidecar's /health until it reports ready, with
// the same fast-fail-if-absent / patient-if-loading policy as the transcribe
// handler: an unreachable port (nothing listening) gives up within
// synthesizeUnreachableTimeout, while a reachable-but-loading sidecar gets the
// full synthesizeReadyTimeout budget.
func (h *SynthesizeSpeechHandler) waitForReady() error {
	healthURL := h.serviceURL() + "/health"
	pollInterval := 1 * time.Second
	startTime := time.Now()

	for {
		resp, err := h.healthCheck(healthURL)
		if err == nil {
			ready := synthesizeHealthReady(resp)
			resp.Body.Close()
			if ready {
				return nil
			}
			// Reachable, just not ready yet (model still loading): fall through
			// to the patient synthesizeReadyTimeout budget below.
		} else if isConnectionRefused(err) && time.Since(startTime) >= synthesizeUnreachableTimeout {
			return fmt.Errorf("TTS service unreachable at %s: %w", h.serviceURL(), err)
		}

		if time.Since(startTime) >= synthesizeReadyTimeout {
			break
		}
		time.Sleep(pollInterval)
	}
	return fmt.Errorf("TTS service did not become ready within %v", synthesizeReadyTimeout)
}

// synthesizeHealthReady reports whether a /health response means the kokoro
// sidecar is actually ready to synthesize. Unlike the whisper sidecar (which
// returns non-200 until its model loads), kokoro's /health ALWAYS returns 200
// and carries readiness in the body: {"status":"up"|"loading","model_loaded":
// true|false}. Gating on the status code alone would let a cold node POST before
// Kokoro-82M has loaded, so parse model_loaded. A 200 whose body cannot be
// parsed is treated as not-ready (fail closed) rather than assuming readiness.
func synthesizeHealthReady(resp *http.Response) bool {
	if resp.StatusCode != http.StatusOK {
		return false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return false
	}
	var health struct {
		ModelLoaded bool `json:"model_loaded"`
	}
	if err := json.Unmarshal(body, &health); err != nil {
		return false
	}
	return health.ModelLoaded
}

// healthCheck performs a single readiness GET bounded by synthesizeHealthTimeout.
func (h *SynthesizeSpeechHandler) healthCheck(healthURL string) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(context.Background(), synthesizeHealthTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
	if err != nil {
		return nil, err
	}
	return h.client().Do(req)
}

// Ensure SynthesizeSpeechHandler implements JobHandler.
var _ JobHandler = (*SynthesizeSpeechHandler)(nil)
