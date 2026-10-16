// internal/jobs/media_generate.go
package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	embeddedservices "github.com/aceteam-ai/citadel-cli/services"
)

// mediaGenerateServiceURL is the local diffusers-service sidecar base URL. The
// host port is owned by citadel (services/ports.go, CITADEL_DIFFUSERS_HOST_PORT)
// and reached over loopback. Built from the registry constant rather than a
// literal so it tracks the port citadel actually injects -- see
// synthesizeServiceURL for the sibling pattern this mirrors.
func mediaGenerateServiceURL() string {
	return fmt.Sprintf("http://localhost:%d", embeddedservices.DiffusersHostPort)
}

const (
	// mediaGenerateTaskTextToImage / mediaGenerateTaskTextToVideo are the exact
	// `task` payload values the aceteam dispatcher sends (TEXT_TO_IMAGE_TASK /
	// TEXT_TO_VIDEO_TASK on the platform side).
	mediaGenerateTaskTextToImage = "text-to-image"
	mediaGenerateTaskTextToVideo = "text-to-video"

	// mediaGenerateReadyTimeout bounds how long we wait for the diffusers
	// sidecar to become reachable and answer /health. Unlike kokoro, the
	// sidecar's /health deliberately never forces (or waits on) a model load --
	// it returns immediately so container healthchecks pass while a large
	// model is still downloading -- so "ready" here means reachable, not
	// "model already loaded". The actual model load (which can be long for a
	// diffusion/video pipeline) happens lazily inside the generation POST
	// itself and is bounded by mediaGenerateRequestTimeout instead. This
	// budget is generous anyway since the platform already probed
	// SERVICE_STATUS before dispatch.
	mediaGenerateReadyTimeout = 180 * time.Second

	// mediaGenerateUnreachableTimeout bounds how long waitForReady tolerates a
	// connection-refused health check, i.e. nothing listening on the sidecar's
	// port at all. Kept short so the handler fails well under the backend's
	// request-gateway budget instead of hanging.
	mediaGenerateUnreachableTimeout = 8 * time.Second

	// mediaGenerateHealthTimeout bounds a single readiness GET so one poll
	// cannot hang if the sidecar accepts the connection but never answers.
	mediaGenerateHealthTimeout = 10 * time.Second

	// mediaGenerateRequestTimeout bounds a single generation POST. Image
	// generation is typically seconds; a 50-step video render can take
	// several minutes (including a cold model load on first use), so this is
	// generous. The worker watchdog and the platform's own timeout_ms bound
	// it further if either sends a tighter budget.
	mediaGenerateRequestTimeout = 30 * time.Minute
)

// MediaGenerateHandler handles MEDIA_GENERATE jobs node-locally.
//
// It bridges a fabric image/video generation request to the node-local
// diffusers-service sidecar (services/diffusers-service/app.py), which exposes
// two endpoints: POST /generate (image, SDXL-Turbo-class) and POST
// /generate/video (video, Wan2.1-class). Both return their artifact inline as
// base64, so this handler needs no workspace: prompt in, media out.
//
// It is the image/video counterpart to SynthesizeSpeechHandler and follows the
// identical shape (injectable ServiceURL/HTTPClient, fast-fail-if-unreachable +
// patient-if-loading readiness probe, per-request context deadlines).
type MediaGenerateHandler struct {
	// ServiceURL is the diffusers sidecar base URL; defaults to the registry
	// port.
	ServiceURL string
	// HTTPClient lets tests inject a stub; nil uses a default client.
	HTTPClient *http.Client
	// RequestTimeout bounds the whole generation call including model_warming
	// retries; zero uses mediaGenerateRequestTimeout. Tests shorten it.
	RequestTimeout time.Duration
	// Sleep waits between model_warming retries and returns early (false) if
	// the job context is cancelled; nil uses a timer. Tests stub it.
	Sleep func(ctx context.Context, d time.Duration) bool
}

func (h *MediaGenerateHandler) requestTimeout() time.Duration {
	if h.RequestTimeout > 0 {
		return h.RequestTimeout
	}
	return mediaGenerateRequestTimeout
}

func (h *MediaGenerateHandler) sleep(ctx context.Context, d time.Duration) bool {
	if h.Sleep != nil {
		return h.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// warmingRetryDelay extracts how long to wait before retrying a model_warming
// response: the Retry-After header, else the body's retry_after, floored at 1s
// and capped at 60s so one odd value cannot stall the job.
func warmingRetryDelay(header string, body []byte) time.Duration {
	secs := 0
	if n, err := strconv.Atoi(strings.TrimSpace(header)); err == nil {
		secs = n
	} else {
		var probe struct {
			RetryAfter int `json:"retry_after"`
		}
		if json.Unmarshal(body, &probe) == nil {
			secs = probe.RetryAfter
		}
	}
	if secs < 1 {
		secs = 1
	}
	if secs > 60 {
		secs = 60
	}
	return time.Duration(secs) * time.Second
}

// NewMediaGenerateHandler creates a handler pointed at the local diffusers
// sidecar.
func NewMediaGenerateHandler() *MediaGenerateHandler {
	return &MediaGenerateHandler{ServiceURL: mediaGenerateServiceURL()}
}

func (h *MediaGenerateHandler) serviceURL() string {
	if h.ServiceURL != "" {
		return h.ServiceURL
	}
	return mediaGenerateServiceURL()
}

// client returns the HTTP client used for both the health poll and the
// generation POST. It carries no fixed Timeout: per-request budgets are
// governed by context deadlines.
func (h *MediaGenerateHandler) client() *http.Client {
	if h.HTTPClient != nil {
		return h.HTTPClient
	}
	return &http.Client{}
}

// diffusersImageResponse mirrors the diffusers-service /generate response
// shape (services/diffusers-service/app.py's generate()).
type diffusersImageResponse struct {
	Model       string `json:"model"`
	Device      string `json:"device"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	Seed        *int64 `json:"seed"`
	ImageBase64 string `json:"image_base64"`
	ContentType string `json:"content_type"`
}

// diffusersVideoResponse mirrors the diffusers-service /generate/video
// response shape (services/diffusers-service/app.py's generate_video()).
type diffusersVideoResponse struct {
	Model       string `json:"model"`
	Device      string `json:"device"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	NumFrames   int    `json:"num_frames"`
	FPS         int    `json:"fps"`
	Seed        *int64 `json:"seed"`
	VideoBase64 string `json:"video_base64"`
	ContentType string `json:"content_type"`
}

// Execute generates an image or video from a text prompt via the diffusers
// sidecar, routed by the job's `task` field.
//
// Payload fields (all strings via nexus.Job.Payload):
//   - task:                 "text-to-image" or "text-to-video" (required,
//     these EXACT strings -- the aceteam TEXT_TO_IMAGE_TASK/TEXT_TO_VIDEO_TASK
//     constants). Routes to /generate or /generate/video.
//   - prompt:                required.
//   - model:                 optional; empty omits the field so the sidecar's
//     own configured default (DIFFUSERS_MODEL/DIFFUSERS_VIDEO_MODEL) applies.
//     For text-to-image the sidecar validates it against its allowlist (a
//     non-allowlisted id is a 400, surfaced as a failure) and swaps the model
//     in on demand; a cold load returns the typed model_warming object below.
//   - negative_prompt:       optional.
//   - width / height:        optional numeric strings; empty/invalid omits the
//     field so the sidecar's own pydantic default applies -- never sent as 0.
//   - seed:                  optional numeric string; empty/invalid omits.
//   - num_inference_steps:   optional numeric string; empty/invalid omits.
//   - guidance_scale:        optional numeric string; empty/invalid omits.
//   - num_frames / fps:      video-only; same omit-if-empty/invalid rule.
//
// Response JSON (the STRICT envelope the aceteam platform parser reads --
// python-backend/routes/aceteam_mcp_media.py):
//
//	{
//	  "encoding": "base64",           // exact marker the platform checks
//	  "content":  "<base64 PNG/MP4>", // the sidecar's image_base64/video_base64, verbatim
//	  "format":   "png" | "mp4",      // derived from task
//	  "model":    "<model used>",     // from the sidecar response
//	  "receipt":  { ... }             // best-effort advisory metering fields
//	}
//
// While the requested image model is still loading, the output is instead the
// sidecar's retryable warming object (no "encoding"/"content"):
//
//	{"status": "model_warming", "model": "...", "eta_seconds": N, "retry_after": N, "reason": "..."}
func (h *MediaGenerateHandler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
	task := job.Payload["task"]
	prompt := job.Payload["prompt"]
	if prompt == "" {
		return nil, fmt.Errorf("job payload missing 'prompt' field")
	}

	var (
		endpoint string
		format   string
		isVideo  bool
	)
	switch task {
	case mediaGenerateTaskTextToImage:
		endpoint = "/generate"
		format = "png"
	case mediaGenerateTaskTextToVideo:
		endpoint = "/generate/video"
		format = "mp4"
		isVideo = true
	default:
		return nil, fmt.Errorf("job payload has unknown or missing 'task' value %q (want %q or %q)", task, mediaGenerateTaskTextToImage, mediaGenerateTaskTextToVideo)
	}

	ctx.Log("info", "     - [Job %s] Waiting for diffusers service to become ready...", job.ID)
	if err := h.waitForReady(); err != nil {
		return nil, err
	}
	ctx.Log("info", "     - [Job %s] MEDIA_GENERATE task=%s endpoint=%s", job.ID, task, endpoint)

	requestPayload, requestEcho := buildMediaGenerateRequest(job.Payload, isVideo)

	reqBody, err := json.Marshal(requestPayload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	reqCtx, cancel := context.WithTimeout(context.Background(), h.requestTimeout())
	defer cancel()
	deadline, _ := reqCtx.Deadline()

	// A cold or swapping image model answers 503 with a typed model_warming
	// body (citadel-cli#1335). Retry it here, honouring Retry-After, for as long
	// as the request budget allows, so a caller that does not understand
	// model_warming still gets an image. Only when the next wait would pass the
	// budget is the warming object returned as the (retryable) job output.
	var body []byte
	for {
		req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, h.serviceURL()+endpoint, bytes.NewBuffer(reqBody))
		if err != nil {
			return nil, fmt.Errorf("failed to build generation request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := h.client().Do(req)
		if err != nil {
			return nil, fmt.Errorf("failed to connect to diffusers service: %w", err)
		}
		body, _ = io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode == http.StatusServiceUnavailable && isModelWarmingBody(body) {
			delay := warmingRetryDelay(resp.Header.Get("Retry-After"), body)
			if time.Now().Add(delay).After(deadline) {
				ctx.Log("info", "     - [Job %s] diffusers model still warming past the request budget; returning retryable model_warming", job.ID)
				return body, nil
			}
			ctx.Log("info", "     - [Job %s] diffusers model warming; retrying in %s", job.ID, delay)
			if !h.sleep(ctx.Context(), delay) {
				return nil, fmt.Errorf("job cancelled while waiting for the diffusers model to warm")
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			// On error the body is a JSON error, not media; surface it verbatim.
			return body, fmt.Errorf("diffusers service returned non-200 status: %s: %s", resp.Status, string(body))
		}
		break
	}

	var content, model string
	receipt := map[string]any{"task": task}
	for k, v := range requestEcho {
		receipt[k] = v
	}

	if isVideo {
		var vr diffusersVideoResponse
		if err := json.Unmarshal(body, &vr); err != nil {
			return nil, fmt.Errorf("failed to parse diffusers video response: %w", err)
		}
		if vr.VideoBase64 == "" {
			return nil, fmt.Errorf("diffusers service returned an empty video_base64")
		}
		content = vr.VideoBase64
		model = vr.Model
		receipt["width"] = vr.Width
		receipt["height"] = vr.Height
		receipt["num_frames"] = vr.NumFrames
		receipt["fps"] = vr.FPS
		if vr.Seed != nil {
			receipt["seed"] = *vr.Seed
		}
	} else {
		var ir diffusersImageResponse
		if err := json.Unmarshal(body, &ir); err != nil {
			return nil, fmt.Errorf("failed to parse diffusers image response: %w", err)
		}
		if ir.ImageBase64 == "" {
			return nil, fmt.Errorf("diffusers service returned an empty image_base64")
		}
		content = ir.ImageBase64
		model = ir.Model
		receipt["width"] = ir.Width
		receipt["height"] = ir.Height
		if ir.Seed != nil {
			receipt["seed"] = *ir.Seed
		}
	}

	result := map[string]any{
		"encoding": "base64",
		"content":  content,
		"format":   format,
		"model":    model,
		"receipt":  receipt,
	}
	return json.Marshal(result)
}

// isModelWarmingBody reports whether a sidecar response body is the typed
// model_warming object (status == "model_warming"). Anything else, including
// unparseable bodies and the sidecar's other 503 (insufficient_vram), is left
// to the ordinary error path.
func isModelWarmingBody(body []byte) bool {
	var probe struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return false
	}
	return probe.Status == "model_warming"
}

// buildMediaGenerateRequest translates job payload strings into the diffusers
// sidecar's JSON request body, omitting any field whose payload string is
// empty or fails to parse -- so the sidecar's own pydantic defaults apply
// rather than a fabricated 0/"" value. It also returns a small echo map of
// the numeric fields it actually forwarded, used to populate the advisory
// receipt with what was ACTUALLY sent (fields the sidecar's response does not
// itself echo back, e.g. num_inference_steps/guidance_scale).
func buildMediaGenerateRequest(payload map[string]string, isVideo bool) (map[string]any, map[string]any) {
	req := map[string]any{"prompt": payload["prompt"]}
	echo := map[string]any{}

	if v := payload["model"]; v != "" {
		req["model"] = v
	}
	if v := payload["negative_prompt"]; v != "" {
		req["negative_prompt"] = v
	}
	if v, ok := parseOmitInt(payload["width"]); ok {
		req["width"] = v
	}
	if v, ok := parseOmitInt(payload["height"]); ok {
		req["height"] = v
	}
	if v, ok := parseOmitInt64(payload["seed"]); ok {
		req["seed"] = v
	}
	if v, ok := parseOmitInt(payload["num_inference_steps"]); ok {
		req["num_inference_steps"] = v
		echo["num_inference_steps"] = v
	}
	if v, ok := parseOmitFloat(payload["guidance_scale"]); ok {
		req["guidance_scale"] = v
		echo["guidance_scale"] = v
	}
	if isVideo {
		if v, ok := parseOmitInt(payload["num_frames"]); ok {
			req["num_frames"] = v
		}
		if v, ok := parseOmitInt(payload["fps"]); ok {
			req["fps"] = v
		}
	}
	return req, echo
}

// parseOmitInt parses s as an int, reporting ok=false for an empty or
// unparseable string so the caller can omit the field entirely.
func parseOmitInt(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return v, true
}

// parseOmitInt64 is parseOmitInt's int64 counterpart, used for seed.
func parseOmitInt64(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// parseOmitFloat is parseOmitInt's float64 counterpart, used for
// guidance_scale.
func parseOmitFloat(s string) (float64, bool) {
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// waitForReady polls the diffusers sidecar's /health until it is reachable
// and answering, with the same fast-fail-if-absent policy as the synthesize
// handler: an unreachable port (nothing listening) gives up within
// mediaGenerateUnreachableTimeout. Unlike synthesize, readiness here does NOT
// gate on a "model loaded" body flag -- see mediaGenerateReadyTimeout's doc
// comment for why: the sidecar's /health never forces or reflects a completed
// model load, so gating on it would either be meaningless or hang forever.
func (h *MediaGenerateHandler) waitForReady() error {
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
			// Reachable but not yet answering 200: fall through to the
			// patient mediaGenerateReadyTimeout budget below.
		} else if isConnectionRefused(err) && time.Since(startTime) >= mediaGenerateUnreachableTimeout {
			return fmt.Errorf("diffusers service unreachable at %s: %w", h.serviceURL(), err)
		}

		if time.Since(startTime) >= mediaGenerateReadyTimeout {
			break
		}
		time.Sleep(pollInterval)
	}
	return fmt.Errorf("diffusers service did not become ready within %v", mediaGenerateReadyTimeout)
}

// healthCheck performs a single readiness GET bounded by
// mediaGenerateHealthTimeout.
func (h *MediaGenerateHandler) healthCheck(healthURL string) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(context.Background(), mediaGenerateHealthTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
	if err != nil {
		return nil, err
	}
	return h.client().Do(req)
}

// Ensure MediaGenerateHandler implements JobHandler.
var _ JobHandler = (*MediaGenerateHandler)(nil)
