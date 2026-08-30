// internal/worker/pairing_display.go
//
// SHOW_PAIRING_CODE / CLEAR_PAIRING_CODE job handlers (issue #659 P0). Lets
// the aceteam backend's node:exec grant flow (`_node_screen_delivery`,
// python-backend/routes/aceteam_mcp_node_exec_grant.py) render a short-lived
// pairing code on THIS node's active text console instead of always falling
// through to the operator's linked device. See
// docs/design-pairing-display.md Part II (§8-14) for the full design.
//
// # The security invariant this handler is one leg of
//
// The pairing code must NEVER reach the requesting agent. Concretely, that
// means the code must appear in NO log line this handler (or the packages it
// calls) emits, in NO field of the JobResult this handler returns, and in NO
// error string. Every log call and every constructed error in this file is
// deliberately built from grant_request_id / reason / structural facts only
// — never from the parsed code. If you add a new log line or error path
// here, keep that discipline; internal/worker/pairing_display_test.go's
// sentinel-leak test exists specifically to catch a regression of this.
//
// # Privilege gating
//
// Both job types mutate/query per-node display state, so — exactly like
// AGENT_UPDATE, MODULE_SET, EXPOSE_SET, and WHATSAPP_PROVISION — they are
// honored ONLY when the job arrives on the per-node stream
// (jobs:v1:shell:org_<id>:node:<nodeid>), never the shared org pool.
package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/pairingdisplay"
)

// pairingCodeMaxLen bounds the code's length — defense against a hostile or
// buggy dispatcher using the console as a billboard. Today's backend sends 8
// digits (_CODE_DIGITS = 8).
const pairingCodeMaxLen = 16

// pairingRequestedByMaxLen bounds the free-text requester line rendered
// under the code.
const pairingRequestedByMaxLen = 80

const (
	pairingTTLDefaultSeconds = 600 // matches the backend's _CHALLENGE_TTL_SECONDS
	pairingTTLMinSeconds     = 30
	pairingTTLMaxSeconds     = 900
)

// PairingDisplayOps is the live rendering/state surface. *pairingdisplay.Manager
// satisfies this directly (see cmd/nodejobs.go's wiring to pairingdisplay.Get());
// tests inject a fake so Execute is testable without a real console or the
// process-wide singleton.
type PairingDisplayOps interface {
	Show(code string, ttl time.Duration, grantRequestID, requestedBy string) pairingdisplay.ShowOutcome
	Clear(grantRequestID string) pairingdisplay.ClearOutcome
}

// PairingDisplayConfig configures a PairingDisplayHandler.
type PairingDisplayConfig struct {
	Ops PairingDisplayOps
	// Log receives operational lines only: grant_request_id, reason,
	// surface. NEVER pass the parsed code to this function.
	Log func(format string, args ...any)
}

// PairingDisplayHandler processes SHOW_PAIRING_CODE and CLEAR_PAIRING_CODE
// jobs.
type PairingDisplayHandler struct {
	cfg PairingDisplayConfig
}

// NewPairingDisplayHandler constructs the handler.
func NewPairingDisplayHandler(cfg PairingDisplayConfig) *PairingDisplayHandler {
	if cfg.Log == nil {
		cfg.Log = func(string, ...any) {}
	}
	return &PairingDisplayHandler{cfg: cfg}
}

// CanHandle reports whether this handler processes the given job type.
func (h *PairingDisplayHandler) CanHandle(jobType string) bool {
	return jobType == JobTypeShowPairingCode || jobType == JobTypeClearPairingCode
}

// showPairingCodePayload is the parsed SHOW_PAIRING_CODE payload (design doc
// §8.2). Code is the one sensitive field — see the package doc's leak
// discipline before touching how this struct is logged or echoed.
type showPairingCodePayload struct {
	Code           string `json:"code"`
	TTLSeconds     int    `json:"ttl_seconds"`
	GrantRequestID string `json:"grant_request_id"`
	RequestedBy    string `json:"requested_by"`
}

type clearPairingCodePayload struct {
	GrantRequestID string `json:"grant_request_id"`
}

// Execute dispatches SHOW_PAIRING_CODE / CLEAR_PAIRING_CODE. See the package
// doc for the privilege gate and the no-leak invariant.
func (h *PairingDisplayHandler) Execute(ctx context.Context, job *Job, stream StreamWriter) (*JobResult, error) {
	switch job.Type {
	case JobTypeShowPairingCode:
		return h.executeShow(job)
	case JobTypeClearPairingCode:
		return h.executeClear(job)
	default:
		return h.showFailure("forbidden", fmt.Errorf("PAIRING_DISPLAY: unsupported job type %q", job.Type)), nil
	}
}

func (h *PairingDisplayHandler) executeShow(job *Job) (*JobResult, error) {
	if !isPerNodeStream(job.SourceQueue) {
		return h.showFailure("forbidden", fmt.Errorf(
			"SHOW_PAIRING_CODE refused: must be dispatched to the per-node stream, got source queue %q", job.SourceQueue)), nil
	}
	if h.cfg.Ops == nil {
		return h.showFailure("render_error", fmt.Errorf("SHOW_PAIRING_CODE handler is misconfigured: no display ops")), nil
	}

	req, err := parseShowPairingCodePayload(job.Payload)
	if err != nil {
		// Deliberately does NOT include err's underlying value in a log call
		// or echo the raw payload — parseShowPairingCodePayload's own error
		// text never embeds field VALUES (only field names), so this is
		// safe to both log and return.
		h.cfg.Log("SHOW_PAIRING_CODE: bad payload: %v", err)
		return h.showFailure("bad_payload", fmt.Errorf("SHOW_PAIRING_CODE: %w", err)), nil
	}

	h.cfg.Log("SHOW_PAIRING_CODE: grant_request_id=%s ttl_seconds=%d", req.GrantRequestID, req.TTLSeconds)

	outcome := h.cfg.Ops.Show(req.Code, time.Duration(req.TTLSeconds)*time.Second, req.GrantRequestID, req.RequestedBy)

	h.cfg.Log("SHOW_PAIRING_CODE: grant_request_id=%s delivered=%v surface=%q reason=%q",
		req.GrantRequestID, outcome.Delivered, outcome.Surface, outcome.Reason)

	out := map[string]any{"delivered": outcome.Delivered, "surface": outcome.Surface}
	if outcome.Reason != "" {
		out["reason"] = outcome.Reason
	}
	return &JobResult{Status: JobStatusSuccess, Output: out}, nil
}

func (h *PairingDisplayHandler) executeClear(job *Job) (*JobResult, error) {
	if !isPerNodeStream(job.SourceQueue) {
		return h.clearFailure("forbidden", fmt.Errorf(
			"CLEAR_PAIRING_CODE refused: must be dispatched to the per-node stream, got source queue %q", job.SourceQueue)), nil
	}
	if h.cfg.Ops == nil {
		return h.clearFailure("render_error", fmt.Errorf("CLEAR_PAIRING_CODE handler is misconfigured: no display ops")), nil
	}

	var payload clearPairingCodePayload
	if job.Payload != nil {
		raw, err := json.Marshal(job.Payload)
		if err == nil {
			_ = json.Unmarshal(raw, &payload)
		}
	}
	payload.GrantRequestID = strings.TrimSpace(payload.GrantRequestID)
	if payload.GrantRequestID == "" {
		return h.clearFailure("bad_payload", fmt.Errorf("CLEAR_PAIRING_CODE: missing grant_request_id")), nil
	}

	h.cfg.Log("CLEAR_PAIRING_CODE: grant_request_id=%s", payload.GrantRequestID)

	outcome := h.cfg.Ops.Clear(payload.GrantRequestID)

	h.cfg.Log("CLEAR_PAIRING_CODE: grant_request_id=%s cleared=%v reason=%q",
		payload.GrantRequestID, outcome.Cleared, outcome.Reason)

	out := map[string]any{"cleared": outcome.Cleared}
	if outcome.Reason != "" {
		out["reason"] = outcome.Reason
	}
	// A mismatch/already-expired code is an idempotent success (design doc
	// §8.2), never a job failure — the backend fires this on confirm/revoke
	// and must not see a spurious failure for an already-expired code.
	return &JobResult{Status: JobStatusSuccess, Output: out}, nil
}

// parseShowPairingCodePayload decodes and validates a SHOW_PAIRING_CODE
// payload. Every returned error is built from FIELD NAMES and structural
// facts only (max length, character class, presence) — never from the field
// VALUES — so it is always safe to log or return verbatim (see the package
// doc's no-leak discipline).
func parseShowPairingCodePayload(payload map[string]any) (showPairingCodePayload, error) {
	var req showPairingCodePayload
	if payload == nil {
		return req, fmt.Errorf("empty payload")
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return req, fmt.Errorf("marshal payload: %w", err)
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return req, fmt.Errorf("decode payload: %w", err)
	}

	req.GrantRequestID = strings.TrimSpace(req.GrantRequestID)
	if req.GrantRequestID == "" {
		return req, fmt.Errorf("missing grant_request_id")
	}

	if req.Code == "" {
		return req, fmt.Errorf("missing code")
	}
	if len(req.Code) > pairingCodeMaxLen {
		return req, fmt.Errorf("code exceeds max length (%d)", pairingCodeMaxLen)
	}
	if !isPrintableASCII(req.Code) {
		return req, fmt.Errorf("code must be printable ASCII")
	}

	if req.TTLSeconds <= 0 {
		req.TTLSeconds = pairingTTLDefaultSeconds
	}
	if req.TTLSeconds < pairingTTLMinSeconds {
		req.TTLSeconds = pairingTTLMinSeconds
	}
	if req.TTLSeconds > pairingTTLMaxSeconds {
		req.TTLSeconds = pairingTTLMaxSeconds
	}

	if len(req.RequestedBy) > pairingRequestedByMaxLen {
		req.RequestedBy = req.RequestedBy[:pairingRequestedByMaxLen]
	}

	return req, nil
}

// isPrintableASCII reports whether every byte of s is in the printable ASCII
// range (0x20-0x7E). A code outside this range is refused as bad_payload
// rather than rendered — the console frame builder assumes single-byte,
// single-cell characters.
func isPrintableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7E {
			return false
		}
	}
	return true
}

func (h *PairingDisplayHandler) showFailure(reason string, err error) *JobResult {
	return &JobResult{
		Status: JobStatusFailure,
		Output: map[string]any{"delivered": false, "surface": "", "reason": reason},
		Error:  err,
	}
}

func (h *PairingDisplayHandler) clearFailure(reason string, err error) *JobResult {
	return &JobResult{
		Status: JobStatusFailure,
		Output: map[string]any{"cleared": false, "reason": reason},
		Error:  err,
	}
}

// Ensure PairingDisplayHandler implements JobHandler.
var _ JobHandler = (*PairingDisplayHandler)(nil)
