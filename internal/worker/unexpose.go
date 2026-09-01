// internal/worker/unexpose.go
//
// UNEXPOSE job handler (issue #944, design doc §4). Remote teardown of a
// gateway exposure — the imperative inverse of EXPOSE_SET, and a dedicated
// job type rather than an EXPOSE_SET `remove:true` flag (design doc §4.1: a
// removal payload is `{name}` only, so folding it into EXPOSE_SET's parser
// would make every existing validation requirement conditional; imperative
// verbs get their own type in this codebase, mirroring SERVICE_START/
// SERVICE_STOP; and a distinct job type keeps revocation visible in
// job-history/dispatch logs).
//
// # Privilege gating
//
// Same fail-closed posture as EXPOSE_SET: honored ONLY on the per-node
// stream. Tearing down a node's ingress is exactly as privileged as creating
// it.
//
// # Epoch interaction
//
// UNEXPOSE does not itself bump anything — it just drops the live route and
// the durable record. What keeps a torn-down name's old tokens dead even
// after a LATER re-expose is the epoch high-water store (design doc §5.3),
// enforced inside the ops layer's Expose, not here.
package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// UnexposeConfig configures an UnexposeHandler.
type UnexposeConfig struct {
	Ops ExposeOps
	Log func(format string, args ...any)
}

// UnexposeHandler processes UNEXPOSE jobs.
type UnexposeHandler struct {
	cfg UnexposeConfig
}

// NewUnexposeHandler constructs an UNEXPOSE handler.
func NewUnexposeHandler(cfg UnexposeConfig) *UnexposeHandler {
	if cfg.Log == nil {
		cfg.Log = func(string, ...any) {}
	}
	return &UnexposeHandler{cfg: cfg}
}

// CanHandle reports whether this handler processes the given job type.
func (h *UnexposeHandler) CanHandle(jobType string) bool {
	return jobType == JobTypeUnexpose
}

// Execute revokes one exposure by name. See the package doc for the privilege
// gate and epoch interaction.
func (h *UnexposeHandler) Execute(ctx context.Context, job *Job, stream StreamWriter) (*JobResult, error) {
	if !isPerNodeStream(job.SourceQueue) {
		return h.failure(fmt.Errorf(
			"UNEXPOSE refused: must be dispatched to the per-node stream, got source queue %q", job.SourceQueue)), nil
	}
	if h.cfg.Ops == nil {
		return h.failure(fmt.Errorf("UNEXPOSE handler is misconfigured: no expose ops")), nil
	}

	name, err := parseUnexposeRequest(job.Payload)
	if err != nil {
		// A malformed request is terminal: retrying the same bad payload cannot help.
		return h.failure(fmt.Errorf("UNEXPOSE: %w", err)), nil
	}

	h.cfg.Log("UNEXPOSE: name=%q", name)

	res, err := h.cfg.Ops.Unexpose(ctx, name)
	if err != nil {
		// Mirrors EXPOSE_SET's retry-vs-failure split (design doc §4.2): "no
		// in-process gateway" is transient and retried; a durable-delete failure
		// after the live route is already down is a FAILURE, since retrying it
		// would just re-run the (already-succeeded) gateway teardown forever
		// while never fixing the disk error.
		if isNoGatewayErr(err) {
			return h.retry(fmt.Errorf("UNEXPOSE: unexpose %q: %w", name, err)), nil
		}
		return h.failure(fmt.Errorf("UNEXPOSE: unexpose %q: %w", name, err)), nil
	}

	out := map[string]any{
		"name": res.Name,
		// "removed" is the platform wire contract (aceteam PR #8631's parser:
		// `removed = bool(result.get("removed")) if ... is not None else True`).
		// Without this key the parser silently defaults removed=True even for
		// the idempotent not-currently-exposed case (WasExposed=false), which
		// misreports the outcome to the caller. "was_exposed" is kept alongside
		// for back-compat with any existing reader of the raw job output.
		"removed":     res.WasExposed,
		"was_exposed": res.WasExposed,
	}
	h.cfg.Log("UNEXPOSE: %q was_exposed=%v", name, res.WasExposed)
	return &JobResult{Status: JobStatusSuccess, Output: out}, nil
}

// parseUnexposeRequest extracts the required "name" field from the job
// payload. Deliberately tiny (unlike parseExposeRequest): a removal payload
// has exactly one required field, which is the point of a dedicated job type.
func parseUnexposeRequest(payload map[string]any) (string, error) {
	if payload == nil {
		return "", fmt.Errorf("empty payload")
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal payload: %w", err)
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return "", fmt.Errorf("decode unexpose request: %w", err)
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return "", fmt.Errorf("unexpose request is missing a name")
	}
	return name, nil
}

// isNoGatewayErr reports whether err is the "no in-process gateway" transient
// condition liveExposeOps.Unexpose returns before it has torn anything down —
// the only Unexpose failure mode that is safe to retry (design doc §4.2).
// String-matched (mirrors the same transient-vs-terminal judgment call
// EXPOSE_SET's caller makes at the ops boundary) since ExposeOps is an
// interface with no typed error contract.
func isNoGatewayErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no in-process gateway")
}

func (h *UnexposeHandler) failure(err error) *JobResult {
	return &JobResult{Status: JobStatusFailure, Error: err, Output: map[string]any{"error": err.Error()}}
}

func (h *UnexposeHandler) retry(err error) *JobResult {
	return &JobResult{Status: JobStatusRetry, Error: err, Output: map[string]any{"error": err.Error()}}
}

// Ensure UnexposeHandler implements JobHandler.
var _ JobHandler = (*UnexposeHandler)(nil)
