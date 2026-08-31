// internal/worker/expose_list.go
//
// EXPOSE_LIST job handler (issue #944, design doc §3). Reads back this node's
// durable gateway-exposure inventory: what the node has agreed to serve, not
// merely what is currently listening. Read-only — no gateway state changes.
//
// # Privilege gating
//
// Same fail-closed posture as EXPOSE_SET/UNEXPOSE: honored ONLY on the
// per-node stream. A read is gated exactly like a write here because the
// inventory (names, ports/paths, visibility, whether a revocable-token
// surface exists) has real recon value — see the design doc §2.
package worker

import (
	"context"
	"fmt"
)

// ExposureInfo is one row of the durable exposure inventory.
type ExposureInfo struct {
	// Name is the exposed-service slug (the <name> in /expose/<name>/).
	Name string `json:"name"`
	// Port is the loopback host port the route proxies to. Mutually exclusive
	// with Path (issue #943) — exactly one is non-zero/non-empty.
	Port int `json:"port,omitempty"`
	// Path is the workspace-confined directory served as a static file share,
	// when this exposure is a directory source instead of a port.
	Path string `json:"path,omitempty"`
	// Visibility is "private", "org", or "link".
	Visibility string `json:"visibility"`
	// Creator is the tailnet login authorized for a `private` exposure.
	Creator string `json:"creator,omitempty"`
	// Epoch is the exposure's current TokenEpoch verbatim — not a secret (it
	// rides inside every minted token's payload), and returning it is what lets
	// a caller rebuild its own epoch bookkeeping from truth (design doc §3.2).
	Epoch int `json:"epoch"`
	// CreatedAt is the RFC3339 timestamp the exposure was FIRST created,
	// preserved across later replace-by-name re-exposes. Empty for a record
	// written before this field existed.
	CreatedAt string `json:"created_at,omitempty"`
	// Live reports whether the gateway currently has this name's route/policy
	// programmed in-process (vs. a durable record with no live counterpart —
	// e.g. one that failed to restore, or was persisted but never wired).
	Live bool `json:"live"`
}

// ExposeListResult is the EXPOSE_LIST job output: the durable set as the
// authority, plus any live-only exposures the durable set is missing.
type ExposeListResult struct {
	Exposures []ExposureInfo `json:"exposures"`
	// LiveOnly names exposures the gateway currently serves but the durable set
	// has no record of (a divergence the design doc §3.2 says to surface, not
	// hide).
	LiveOnly []string `json:"live_only,omitempty"`
}

// ExposeListConfig configures an ExposeListHandler.
type ExposeListConfig struct {
	Ops ExposeOps
	Log func(format string, args ...any)
}

// ExposeListHandler processes EXPOSE_LIST jobs.
type ExposeListHandler struct {
	cfg ExposeListConfig
}

// NewExposeListHandler constructs an EXPOSE_LIST handler.
func NewExposeListHandler(cfg ExposeListConfig) *ExposeListHandler {
	if cfg.Log == nil {
		cfg.Log = func(string, ...any) {}
	}
	return &ExposeListHandler{cfg: cfg}
}

// CanHandle reports whether this handler processes the given job type.
func (h *ExposeListHandler) CanHandle(jobType string) bool {
	return jobType == JobTypeExposeList
}

// Execute reads back the durable exposure inventory. See the package doc for
// the privilege gate. No payload fields are required — an empty payload lists
// everything, and the sets involved are a handful of records.
func (h *ExposeListHandler) Execute(ctx context.Context, job *Job, stream StreamWriter) (*JobResult, error) {
	if !isPerNodeStream(job.SourceQueue) {
		return h.failure(fmt.Errorf(
			"EXPOSE_LIST refused: must be dispatched to the per-node stream, got source queue %q", job.SourceQueue)), nil
	}
	if h.cfg.Ops == nil {
		return h.failure(fmt.Errorf("EXPOSE_LIST handler is misconfigured: no expose ops")), nil
	}

	res, err := h.cfg.Ops.List(ctx)
	if err != nil {
		// A corrupt durable store is a FAILURE with the parse error surfaced, not
		// an empty success — a reader silently reporting "nothing exposed" is
		// exactly the blindness this job exists to end (design doc §3.2). Never
		// retried: retrying a corrupt on-disk file cannot help.
		return h.failure(fmt.Errorf("EXPOSE_LIST: %w", err)), nil
	}

	out := map[string]any{
		"exposures": res.Exposures,
		"count":     len(res.Exposures),
	}
	if len(res.LiveOnly) > 0 {
		out["live_only"] = res.LiveOnly
	}
	h.cfg.Log("EXPOSE_LIST: %d exposure(s), %d live-only", len(res.Exposures), len(res.LiveOnly))
	return &JobResult{Status: JobStatusSuccess, Output: out}, nil
}

func (h *ExposeListHandler) failure(err error) *JobResult {
	return &JobResult{Status: JobStatusFailure, Error: err, Output: map[string]any{"error": err.Error()}}
}

// Ensure ExposeListHandler implements JobHandler.
var _ JobHandler = (*ExposeListHandler)(nil)
