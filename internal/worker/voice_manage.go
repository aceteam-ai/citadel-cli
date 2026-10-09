// internal/worker/voice_manage.go
//
// VOICE_MANAGE job handler (issue #1248). The node-side enrollment transport
// for the voice-clone module (aceteam-ai/citadel-services#28 / PR #29, loopback
// port 8215): the platform (aceteam-ai/aceteam#10823) dispatches enroll / list /
// delete actions, and this handler proxies them to the module's /v1/voices
// HTTP API over loopback. It is the scoped replacement for HTTP_PROXY, which is
// deliberately blocked for MCP callers, so platform-side enrollment has a
// transport at all.
//
// # Privilege gating
//
// Same fail-closed posture as EXPOSE_SET / APP_*: honored ONLY when the job
// arrives on the per-node stream (isPerNodeStream), never the shared org pool.
// Enrolling a cloned voice is a node-identity operation gated exactly like a
// node's other privileged mutations.
//
// # Verbatim passthrough is the whole point
//
// The module is the authority on consent and licensing. A missing/invalid
// consent record is a 422 and a license gate is a 403 — the platform MUST see
// those real refusals, so this handler NEVER validates, defaults, forges, or
// rewrites the consent record, and relays the module's HTTP status code and
// response body UNCHANGED. Because the runner publishes only the error string
// (WriteError) on JobStatusFailure and drops Output, any HTTP response the
// module actually returns — 2xx, 403, 404, 413, 422, 5xx alike — is relayed as
// a SUCCESSFUL proxy (JobStatusSuccess, body in Output via WriteEnd). A
// JobStatusFailure is reserved for the gate, a bad payload, an over-cap clip,
// and a transport error (the module not installed / not running).
package worker

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	embeddedservices "github.com/aceteam-ai/citadel-cli/services"
)

const (
	// defaultVoiceCloneMaxUploadBytes mirrors the voice-clone module's own
	// VOICE_CLONE_MAX_UPLOAD_MB default (20 MB). The node enforces it before
	// building the request so an over-cap clip fails fast and never streams
	// 20+ MB to the module; the module re-enforces it (413) as the authority.
	defaultVoiceCloneMaxUploadBytes int64 = 20 << 20

	// maxVoiceManageResponseBytes defensively bounds how much of the module's
	// response body is relayed. Voice-metadata responses are small JSON; this
	// is a memory guard against a misbehaving module, not an expected limit.
	maxVoiceManageResponseBytes int64 = 16 << 20

	// voiceEnrollTimeout is generous: enroll saves the reference clip and may
	// auto-transcribe it when a prompt needs a transcript.
	voiceEnrollTimeout = 5 * time.Minute
	// voiceQueryTimeout bounds the quick list/delete round trips.
	voiceQueryTimeout = 30 * time.Second
)

// VoiceManageConfig configures a VoiceManageHandler.
type VoiceManageConfig struct {
	// BaseURL is the voice-clone module's base URL (no trailing slash), e.g.
	// http://127.0.0.1:8215. Empty resolves to the services-registry default
	// (an IPv4 literal, matching the loopback bind — never "localhost", per the
	// citadel-cli#1186 IPv6-first-connect note).
	BaseURL string
	// HTTPClient lets tests inject a stub; nil uses a default client with no
	// fixed Timeout (per-request budgets come from the Execute context deadline).
	HTTPClient *http.Client
	// WorkspaceDir is the sandbox root for the audio_path delivery mechanism
	// (a clip pre-written via FILE_WRITE_BYTES). Empty means audio_path is
	// unsupported on this node; inline audio_base64 still works.
	WorkspaceDir string
	// MaxUploadBytes caps the reference audio. 0 uses defaultVoiceCloneMaxUploadBytes.
	// A field so a test can use a tiny cap instead of allocating 20 MB.
	MaxUploadBytes int64
	Log            func(format string, args ...any)
}

// VoiceManageHandler processes VOICE_MANAGE jobs.
type VoiceManageHandler struct {
	cfg VoiceManageConfig
}

// NewVoiceManageHandler constructs a VOICE_MANAGE handler.
func NewVoiceManageHandler(cfg VoiceManageConfig) *VoiceManageHandler {
	if cfg.Log == nil {
		cfg.Log = func(string, ...any) {}
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = fmt.Sprintf("http://127.0.0.1:%d", embeddedservices.VoiceCloneHostPort)
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.MaxUploadBytes <= 0 {
		cfg.MaxUploadBytes = defaultVoiceCloneMaxUploadBytes
	}
	return &VoiceManageHandler{cfg: cfg}
}

// CanHandle reports whether this handler processes the given job type.
func (h *VoiceManageHandler) CanHandle(jobType string) bool {
	return jobType == JobTypeVoiceManage
}

// Execute dispatches enroll / list / delete to the voice-clone module. See the
// package doc for the privilege gate and the verbatim-passthrough contract.
//
// Payload fields:
//   - action:        "enroll" | "list" | "delete" (required).
//   - enroll:        consent (object {speaker_name, attested_by, statement,
//     attested_at}, forwarded verbatim; REQUIRED by the module), plus reference
//     audio as EITHER audio_base64 (inline) XOR audio_path (a workspace clip
//     delivered via FILE_WRITE_BYTES). Optional: name, transcript, engines
//     (array or comma-separated string).
//   - delete:        voice_id (required).
func (h *VoiceManageHandler) Execute(ctx context.Context, job *Job, stream StreamWriter) (*JobResult, error) {
	if !isPerNodeStream(job.SourceQueue) {
		return h.failure(fmt.Errorf(
			"VOICE_MANAGE refused: must be dispatched to the per-node stream, got source queue %q", job.SourceQueue)), nil
	}

	action := strings.TrimSpace(payloadString(job.Payload, "action"))
	switch action {
	case "enroll":
		return h.enroll(ctx, job)
	case "list":
		return h.proxy(ctx, job, http.MethodGet, h.cfg.BaseURL+"/v1/voices", "", nil, voiceQueryTimeout)
	case "delete":
		id := strings.TrimSpace(payloadString(job.Payload, "voice_id"))
		if id == "" {
			return h.failure(fmt.Errorf("VOICE_MANAGE delete requires a voice_id")), nil
		}
		// PathEscape so a malformed id (e.g. "../x") becomes a path the module
		// resolves and rejects (404) verbatim, never traversal on our side.
		endpoint := h.cfg.BaseURL + "/v1/voices/" + url.PathEscape(id)
		return h.proxy(ctx, job, http.MethodDelete, endpoint, "", nil, voiceQueryTimeout)
	case "":
		return h.failure(fmt.Errorf("VOICE_MANAGE requires an action (enroll|list|delete)")), nil
	default:
		return h.failure(fmt.Errorf("VOICE_MANAGE: unknown action %q (want enroll|list|delete)", action)), nil
	}
}

// enroll builds the multipart/form-data request the module's enrollment parser
// expects and relays its response verbatim.
func (h *VoiceManageHandler) enroll(ctx context.Context, job *Job) (*JobResult, error) {
	audio, err := h.resolveAudio(job.Payload)
	if err != nil {
		return h.failure(fmt.Errorf("VOICE_MANAGE enroll: %w", err)), nil
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	// consent: forward EXACTLY what arrived (object -> JSON string, string ->
	// as-is), never defaulted or forged. Absent/empty -> omit the field so the
	// module returns its own canonical "consent is required" 422 (issue #1248:
	// the module is the consent authority, and the platform must see that 422).
	consentValue, haveConsent, cerr := consentFormValue(job.Payload["consent"])
	if cerr != nil {
		return h.failure(fmt.Errorf("VOICE_MANAGE enroll: %w", cerr)), nil
	}
	if haveConsent {
		if werr := mw.WriteField("consent", consentValue); werr != nil {
			return h.failure(fmt.Errorf("VOICE_MANAGE enroll: build consent part: %w", werr)), nil
		}
	}

	for _, k := range []string{"name", "transcript"} {
		if v := payloadString(job.Payload, k); v != "" {
			if werr := mw.WriteField(k, v); werr != nil {
				return h.failure(fmt.Errorf("VOICE_MANAGE enroll: build %s part: %w", k, werr)), nil
			}
		}
	}
	if engines := voiceEnginesField(job.Payload["engines"]); engines != "" {
		if werr := mw.WriteField("engines", engines); werr != nil {
			return h.failure(fmt.Errorf("VOICE_MANAGE enroll: build engines part: %w", werr)), nil
		}
	}

	fw, err := mw.CreateFormFile("audio", "reference.wav")
	if err != nil {
		return h.failure(fmt.Errorf("VOICE_MANAGE enroll: build audio part: %w", err)), nil
	}
	if _, werr := fw.Write(audio); werr != nil {
		return h.failure(fmt.Errorf("VOICE_MANAGE enroll: write audio part: %w", werr)), nil
	}
	if cerr := mw.Close(); cerr != nil {
		return h.failure(fmt.Errorf("VOICE_MANAGE enroll: finalize multipart: %w", cerr)), nil
	}

	return h.proxy(ctx, job, http.MethodPost, h.cfg.BaseURL+"/v1/voices", mw.FormDataContentType(), buf.Bytes(), voiceEnrollTimeout)
}

// resolveAudio returns the reference-audio bytes from exactly one of
// audio_base64 (inline) or audio_path (a workspace clip). It enforces the
// upload cap BEFORE returning so an over-cap clip fails fast and is never sent.
func (h *VoiceManageHandler) resolveAudio(payload map[string]any) ([]byte, error) {
	b64 := payloadString(payload, "audio_base64")
	path := payloadString(payload, "audio_path")

	switch {
	case b64 != "" && path != "":
		return nil, fmt.Errorf("provide exactly one of audio_base64 or audio_path, not both")

	case b64 != "":
		// Reject on the ENCODED length first so an over-cap clip never allocates
		// its decoded form; the cap is on the decoded audio bytes.
		if len(b64) > base64.StdEncoding.EncodedLen(int(h.cfg.MaxUploadBytes)) {
			return nil, fmt.Errorf("reference audio exceeds the %d-byte cap", h.cfg.MaxUploadBytes)
		}
		audio, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return nil, fmt.Errorf("audio_base64 is not valid base64: %w", err)
		}
		if int64(len(audio)) > h.cfg.MaxUploadBytes {
			return nil, fmt.Errorf("reference audio exceeds the %d-byte cap", h.cfg.MaxUploadBytes)
		}
		if len(audio) == 0 {
			return nil, fmt.Errorf("reference audio is empty")
		}
		return audio, nil

	case path != "":
		if h.cfg.WorkspaceDir == "" {
			return nil, fmt.Errorf("audio_path requires a configured workspace; deliver the clip inline via audio_base64 instead")
		}
		resolved, err := jobs.ValidateReadPath(h.cfg.WorkspaceDir, path, false)
		if err != nil {
			return nil, fmt.Errorf("audio_path: %w", err)
		}
		f, err := os.Open(resolved)
		if err != nil {
			return nil, fmt.Errorf("audio_path: %w", err)
		}
		defer f.Close()
		// LimitReader(cap+1) detects an over-cap file without reading all of it.
		audio, err := io.ReadAll(io.LimitReader(f, h.cfg.MaxUploadBytes+1))
		if err != nil {
			return nil, fmt.Errorf("audio_path: %w", err)
		}
		if int64(len(audio)) > h.cfg.MaxUploadBytes {
			return nil, fmt.Errorf("reference audio exceeds the %d-byte cap", h.cfg.MaxUploadBytes)
		}
		if len(audio) == 0 {
			return nil, fmt.Errorf("reference audio file is empty")
		}
		return audio, nil

	default:
		return nil, fmt.Errorf("enroll requires reference audio (audio_base64 or audio_path)")
	}
}

// proxy performs the single HTTP round trip to the module and relays the
// response VERBATIM. Any HTTP response (2xx or an error status) is a successful
// proxy; only a transport error (module unreachable) is a job failure.
func (h *VoiceManageHandler) proxy(ctx context.Context, job *Job, method, endpoint, contentType string, body []byte, timeout time.Duration) (*JobResult, error) {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(reqCtx, method, endpoint, reader)
	if err != nil {
		return h.failure(fmt.Errorf("VOICE_MANAGE: build request: %w", err)), nil
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := h.client().Do(req)
	if err != nil {
		// A transport error almost always means the voice-clone module is not
		// installed or not running on this node. Terminal failure with a clear,
		// actionable message — NOT a retry, which an uninstalled module would
		// just loop to the DLQ.
		return h.failure(fmt.Errorf(
			"VOICE_MANAGE: cannot reach the voice-clone module at %s: %w (check it is installed and running — SERVICE_STATUS voice-clone)",
			endpoint, err)), nil
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxVoiceManageResponseBytes))
	if err != nil {
		return h.failure(fmt.Errorf("VOICE_MANAGE: read voice-clone response: %w", err)), nil
	}

	ok := resp.StatusCode >= 200 && resp.StatusCode < 300
	out := map[string]any{
		"action":       strings.TrimSpace(payloadString(job.Payload, "action")),
		"status_code":  resp.StatusCode,
		"ok":           ok,
		"body":         string(respBody),
		"content_type": resp.Header.Get("Content-Type"),
	}
	h.cfg.Log("VOICE_MANAGE %s %s -> %d (%d bytes)", method, endpoint, resp.StatusCode, len(respBody))
	return &JobResult{Status: JobStatusSuccess, Output: out}, nil
}

// consentFormValue converts the payload's consent field into the exact string
// the multipart `consent` part must carry, WITHOUT validating, defaulting, or
// forging it (issue #1248). ok=false means consent is absent/blank, so the
// field is omitted and the module returns its canonical "consent is required"
// 422 — the refusal the platform must see.
func consentFormValue(v any) (string, bool, error) {
	switch c := v.(type) {
	case nil:
		return "", false, nil
	case string:
		if strings.TrimSpace(c) == "" {
			return "", false, nil
		}
		return c, true, nil
	default:
		// The normal case: an object {speaker_name, attested_by, statement,
		// attested_at} marshaled to a JSON string verbatim.
		raw, err := json.Marshal(c)
		if err != nil {
			return "", false, fmt.Errorf("consent is not serializable: %w", err)
		}
		return string(raw), true, nil
	}
}

// voiceEnginesField renders an optional engine selector as the comma-separated
// string the module's parser splits on. Accepts a JSON array or a raw string.
func voiceEnginesField(v any) string {
	switch e := v.(type) {
	case nil:
		return ""
	case string:
		return e
	case []any:
		parts := make([]string, 0, len(e))
		for _, item := range e {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				parts = append(parts, strings.TrimSpace(s))
			}
		}
		return strings.Join(parts, ",")
	default:
		return ""
	}
}

func (h *VoiceManageHandler) client() *http.Client {
	if h.cfg.HTTPClient != nil {
		return h.cfg.HTTPClient
	}
	return &http.Client{}
}

func (h *VoiceManageHandler) failure(err error) *JobResult {
	return &JobResult{Status: JobStatusFailure, Error: err, Output: map[string]any{"error": err.Error()}}
}

// Ensure VoiceManageHandler implements JobHandler.
var _ JobHandler = (*VoiceManageHandler)(nil)
