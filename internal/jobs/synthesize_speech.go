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
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/status"
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

// omnivoiceServiceURL is the local omnivoice TTS sidecar base URL (citadel-cli
// #1007, the first citadel-inference-server-backed TTS engine). Same loopback
// reasoning as synthesizeServiceURL: no auth of its own, sole consumer is this
// co-located worker.
func omnivoiceServiceURL() string {
	return fmt.Sprintf("http://localhost:%d", embeddedservices.OmniVoiceHostPort)
}

const (
	// defaultSynthesizeBackend is the TTS backend used when a job payload omits
	// `backend`. Kept as kokoro so every existing dispatch (which has never sent
	// this field) is unaffected (citadel-cli#1007 §3.4 — zero change for every
	// existing dispatch).
	defaultSynthesizeBackend = "kokoro"

	// defaultSynthesizeVoice is the Kokoro voice used when a job omits `voice`.
	// Matches the service's own KOKORO_DEFAULT_VOICE default (am_michael, the
	// book-narration voice).
	defaultSynthesizeVoice = "am_michael"

	// defaultSynthesizeFormat is the output container used when a job omits
	// `response_format`. Opus is compact and speech-tuned; matches the service's
	// KOKORO_DEFAULT_FORMAT default.
	defaultSynthesizeFormat = "opus"

	// synthesizeReadyTimeout bounds how long we wait for the TTS sidecar to load
	// its model and report healthy. Model load is a one-time cost on first job;
	// subsequent jobs hit a warm service. This budget only applies once the
	// sidecar is actually answering connections; see synthesizeUnreachableTimeout
	// for the case where nothing is listening. OmniVoice's checkpoint (~1.2GB
	// fp16, 0.6B params) is the same order as kokoro's warm-up budget assumes
	// (citadel-cli#1007 §3.4), so one shared budget covers both backends for v1.
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
	// is bounded text (KOKORO_MAX_INPUT_CHARS / CIS_MAX_INPUT_CHARS, default
	// 5000) and both backends run near real time even on CPU/GPU, so one
	// generous fixed cap is enough.
	synthesizeRequestTimeout = 5 * time.Minute

	// synthesizeInfoTimeout bounds the best-effort GET /info probe used to learn
	// an engine's model_license (citadel-cli#1007). Short and non-fatal: the
	// audio artifact is the primary result, this is advisory heartbeat metadata.
	synthesizeInfoTimeout = 3 * time.Second
)

// ttsBackendDefaults holds the voice and response_format a backend falls back
// to when the job payload omits them.
type ttsBackendDefaults struct {
	voice  string
	format string
}

// synthesizeBackendDefaults resolves the omitted-field defaults PER BACKEND,
// not globally — a load-bearing distinction (citadel-cli#1007): the
// citadel-inference-server OmniVoice adapter is wav-only (an `opus` request
// 400s) and rejects any `voice` outside its own preset vocabulary (`am_michael`
// 400s), so kokoro's historical am_michael/opus defaults cannot be applied to
// it. omnivoice therefore defaults to its own auto-voice + wav. An unmapped
// backend falls back to kokoro's defaults, preserving pre-#1007 behavior for
// any caller that predates this map (including the ServiceURL test escape
// hatch, which resolves an arbitrary backend name).
var synthesizeBackendDefaults = map[string]ttsBackendDefaults{
	"kokoro":    {voice: defaultSynthesizeVoice, format: defaultSynthesizeFormat},
	"omnivoice": {voice: "auto", format: "wav"},
}

// backendDefaultsFor returns the omitted-field defaults for backend, falling
// back to kokoro's for any backend not in the map.
func backendDefaultsFor(backend string) ttsBackendDefaults {
	if d, ok := synthesizeBackendDefaults[backend]; ok {
		return d
	}
	return synthesizeBackendDefaults[defaultSynthesizeBackend]
}

// SynthesizeSpeechHandler handles SYNTHESIZE_SPEECH jobs node-locally.
//
// It is the synthesis counterpart to TranscribeAudioHandler: the heavy ML
// dependency lives in a Docker sidecar reachable over loopback (kokoro,
// services/compose/kokoro.yml, or the citadel-inference-server-backed
// omnivoice, services/compose/omnivoice.yml — citadel-cli#1007), and this Go
// handler proxies an OpenAI-compatible speech request to it. The text and the
// resulting audio never leave the node.
//
// Unlike transcribe, it needs no workspace: the text arrives inline in the
// payload and the audio is returned inline (base64), so the handler is
// registered unconditionally alongside the other sandbox-less inference handlers.
type SynthesizeSpeechHandler struct {
	// BaseURLs maps a TTS backend name to its loopback base URL. Built from the
	// services registry so a per-node CITADEL_*_HOST_PORT override is honored
	// (mirrors internal/worker.LLMInferenceHandler's baseURLs).
	BaseURLs map[string]string
	// ServiceURL, when non-empty, overrides the resolved backend URL
	// UNCONDITIONALLY, regardless of which backend a job requests. This is a
	// test-only escape hatch retained for pre-#1007 tests that point a single
	// stub server at the handler without touching BaseURLs (citadel-cli#1007
	// §3.4). Production code should leave this unset and let BaseURLs resolve
	// the backend.
	ServiceURL string
	// HTTPClient lets tests inject a stub; nil uses a default client.
	HTTPClient *http.Client
}

// NewSynthesizeSpeechHandler creates a handler pointed at the local TTS
// sidecars, keyed by backend name (citadel-cli#1007).
func NewSynthesizeSpeechHandler() *SynthesizeSpeechHandler {
	return &SynthesizeSpeechHandler{BaseURLs: map[string]string{
		"kokoro":    synthesizeServiceURL(),
		"omnivoice": omnivoiceServiceURL(),
	}}
}

// resolveServiceURL returns the base URL to dial for the given backend, and
// whether one is known. h.ServiceURL, when set, wins unconditionally (see its
// doc comment); otherwise backend must be a key in h.BaseURLs. An unknown
// backend (a typo, or a payload naming a backend this handler was never
// configured for) resolves to ok=false so the caller can fail WITHOUT making
// an HTTP call — the same "an explicit allowlist, a typo must still fail
// loudly" posture as selfProvisioningEngines (model_cache_pull.go).
func (h *SynthesizeSpeechHandler) resolveServiceURL(backend string) (string, bool) {
	if h.ServiceURL != "" {
		return h.ServiceURL, true
	}
	url, ok := h.BaseURLs[backend]
	return url, ok
}

// sortedBackends returns the configured backend names in a stable order, so
// an "unsupported backend" error message reads the same on every node.
func sortedBackends(baseURLs map[string]string) []string {
	names := make([]string, 0, len(baseURLs))
	for name := range baseURLs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
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

// Execute synthesizes speech from text via the resolved TTS sidecar.
//
// Payload fields (all strings via nexus.Job):
//   - text / input:       the text to synthesize (required; `text` preferred,
//     `input` accepted as an alias for the OpenAI request-body spelling).
//   - backend:            optional TTS backend selector ("kokoro" | "omnivoice");
//     empty defaults to kokoro, so every pre-#1007 dispatch is unaffected
//     (citadel-cli#1007). An unrecognized value fails the job without
//     attempting any HTTP call.
//   - voice:              optional voice; empty defaults PER BACKEND —
//     am_michael for kokoro, "auto" for omnivoice (am_michael is not an
//     omnivoice preset, so it would 400). See synthesizeBackendDefaults.
//   - response_format:    optional output container; empty defaults PER
//     BACKEND — opus for kokoro, wav for omnivoice (the OmniVoice adapter is
//     wav-only, so opus would 400). `format` accepted as an alias.
//   - speed:              optional playback speed (0.5-2.0); omitted from the
//     forwarded request entirely when absent, so a server relying on its own
//     default sees no change (citadel-cli#603's omit-if-empty rule).
//   - instructions:       optional free-text voice design, forwarded verbatim;
//     omitted entirely when absent. kokoro ignores it; omnivoice honors it.
//
// Response JSON (this handler DEFINES the envelope; nothing on the aceteam side
// parses it yet; the fabric may also call the endpoint directly):
//
//	{
//	  "encoding": "base64",          // the marker the coordinator checks
//	  "content":  "<base64 audio>",  // the synthesized audio bytes
//	  "format":   "opus",
//	  "voice":    "am_michael",
//	  "backend":  "kokoro",           // additive (citadel-cli#1007); existing
//	                                  // consumers read only encoding/content/
//	                                  // format/receipt and are unaffected
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

	backend := job.Payload["backend"]
	if backend == "" {
		backend = defaultSynthesizeBackend
	}
	serviceURL, ok := h.resolveServiceURL(backend)
	if !ok {
		return nil, fmt.Errorf("unsupported TTS backend %q (allowed: %s)", backend, strings.Join(sortedBackends(h.BaseURLs), ", "))
	}

	// Omitted voice/response_format default PER BACKEND (citadel-cli#1007):
	// kokoro -> am_michael/opus (unchanged); omnivoice -> auto/wav (opus 400s
	// on the wav-only OmniVoice adapter, and am_michael is not one of its
	// presets). See synthesizeBackendDefaults.
	backendDef := backendDefaultsFor(backend)

	voice := job.Payload["voice"]
	if voice == "" {
		voice = backendDef.voice
	}

	format := job.Payload["response_format"]
	if format == "" {
		format = job.Payload["format"]
	}
	if format == "" {
		format = backendDef.format
	}

	ctx.Log("info", "     - [Job %s] Waiting for TTS service (%s) to become ready...", job.ID, backend)
	if err := h.waitForReady(serviceURL); err != nil {
		return nil, err
	}
	ctx.Log("info", "     - [Job %s] SYNTHESIZE_SPEECH backend=%s voice=%s format=%s chars=%d", job.ID, backend, voice, format, len(text))

	requestPayload := map[string]any{
		"input":           text,
		"voice":           voice,
		"response_format": format,
	}
	// speed/instructions are omitted entirely when absent (not even an empty
	// string), so a server relying on its own pydantic/adapter default sees no
	// change from before this field existed (citadel-cli#603's omit-if-empty
	// rule, mirrored from buildMediaGenerateRequest).
	if speedStr := job.Payload["speed"]; speedStr != "" {
		if speed, err := strconv.ParseFloat(speedStr, 64); err == nil {
			requestPayload["speed"] = speed
		}
	}
	if instructions := job.Payload["instructions"]; instructions != "" {
		requestPayload["instructions"] = instructions
	}

	reqBody, err := json.Marshal(requestPayload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	reqCtx, cancel := context.WithTimeout(context.Background(), synthesizeRequestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, serviceURL+"/v1/audio/speech", bytes.NewBuffer(reqBody))
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

	// Best-effort: learn this backend's model_license from its own /info so the
	// heartbeat can surface it (citadel-cli#1007 — OmniVoice's checkpoint is
	// CC-BY-NC, unlike kokoro's Apache-2.0 Kokoro-82M). Never fails the job;
	// skipped once already recorded so a warm engine costs no extra round trip.
	h.recordModelLicense(serviceURL, backend)

	result := map[string]any{
		"encoding": "base64",
		"content":  base64.StdEncoding.EncodeToString(audio),
		"format":   format,
		"voice":    voice,
		"backend":  backend,
		"receipt":  synthesizeReceiptFromHeaders(resp.Header),
	}
	return json.Marshal(result)
}

// recordModelLicense performs a best-effort GET <serviceURL>/info and, if it
// names a model_license, records it against backend via
// status.RecordModelLicense so the heartbeat can surface it additively on
// ServiceInfo. Any failure (unreachable, non-200, unparsable body, missing
// field) is silently ignored — this is advisory metadata, never allowed to
// fail or delay the job whose audio artifact already succeeded.
func (h *SynthesizeSpeechHandler) recordModelLicense(serviceURL, backend string) {
	if _, known := status.ModelLicenseFor(backend); known {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), synthesizeInfoTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, serviceURL+"/info", nil)
	if err != nil {
		return
	}
	resp, err := h.client().Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return
	}
	var info struct {
		ModelLicense string `json:"model_license"`
	}
	if err := json.Unmarshal(body, &info); err != nil {
		return
	}
	if info.ModelLicense != "" {
		status.RecordModelLicense(backend, info.ModelLicense)
	}
}

// synthesizeReceiptFromHeaders extracts the per-item metering receipt the TTS
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

// waitForReady polls the TTS sidecar's /health until it reports ready, with the
// same fast-fail-if-absent / patient-if-loading policy as the transcribe
// handler: an unreachable port (nothing listening) gives up within
// synthesizeUnreachableTimeout, while a reachable-but-loading sidecar gets the
// full synthesizeReadyTimeout budget.
func (h *SynthesizeSpeechHandler) waitForReady(serviceURL string) error {
	healthURL := serviceURL + "/health"
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
			return fmt.Errorf("TTS service unreachable at %s: %w", serviceURL, err)
		}

		if time.Since(startTime) >= synthesizeReadyTimeout {
			break
		}
		time.Sleep(pollInterval)
	}
	return fmt.Errorf("TTS service did not become ready within %v", synthesizeReadyTimeout)
}

// synthesizeHealthReady reports whether a /health response means the TTS
// sidecar is actually ready to synthesize. Unlike the whisper sidecar (which
// returns non-200 until its model loads), kokoro's (and, by contract,
// omnivoice's — citadel-cli#1007 §4) /health ALWAYS returns 200 and carries
// readiness in the body: {"status":"up"|"loading","model_loaded":true|false}.
// Gating on the status code alone would let a cold node POST before the model
// has loaded, so parse model_loaded. A 200 whose body cannot be parsed is
// treated as not-ready (fail closed) rather than assuming readiness.
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
