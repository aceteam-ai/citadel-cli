// internal/worker/expose_set.go
//
// EXPOSE_SET job handler (issue #598). Lets the `expose` MCP verb / console
// action program THIS node's gateway to expose a local service on the fabric
// with a page-style visibility ladder (private/org/link), and returns the
// managed URL (plus a link token when visibility=link).
//
// # Privilege gating
//
// Exposing a node service mutates the node's gateway route table + exposure
// policy, so — exactly like MODULE_SET, AGENT_UPDATE, and WHATSAPP_PROVISION —
// it is honored ONLY when the job arrives on the per-node stream
// (jobs:v1:shell:org_<id>:node:<nodeid>), never the shared org pool. It fails
// closed.
//
// # Standalone by design
//
// The gateway wiring, link-token minting, and mesh-URL construction live in the
// cmd layer (they need the in-process gateway ref, the node's signing key, and
// the mesh IP), injected here as ExposeOps so this handler's routing/validation
// is unit-testable with a fake — without a live gateway or mesh.
package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// ExposeRequest is the parsed EXPOSE_SET payload: expose the local loopback
// service at Port under the gateway name Name with the given Visibility.
type ExposeRequest struct {
	// Name is the exposed-service slug (the <name> in /expose/<name>/). Lowercase
	// alphanumeric + dashes; validated by the gateway.
	Name string `json:"name"`
	// Port is the service's loopback host port (e.g. 5000 for Frigate).
	Port int `json:"port"`
	// Visibility is "private", "org", or "link".
	Visibility string `json:"visibility"`
	// TTLSeconds bounds a `link` token's lifetime. Ignored for private/org. A
	// non-positive value lets the ops layer apply its default.
	TTLSeconds int `json:"ttl_seconds"`
	// Creator is the tailnet login authorized for a `private` exposure. Only the
	// backend/MCP caller knows the remote creator's login; empty makes a private
	// exposure inert (fails closed at the gateway).
	Creator string `json:"creator"`
	// Epoch, when >0, is bound into a `link` token so the backend can revoke all
	// outstanding tokens for this exposure by bumping it. Defaults to 1.
	Epoch int `json:"epoch"`
}

// ExposeResult is what the ops layer returns after programming the gateway.
type ExposeResult struct {
	// URL is the managed gateway URL the service is reachable at over the mesh,
	// or "" when the node is off-mesh.
	URL string `json:"url"`
	// Token is the signed link access token (visibility=link only), else "".
	Token string `json:"token,omitempty"`
	// ExpiresAt is the link token's RFC3339 expiry (visibility=link only).
	ExpiresAt string `json:"expires_at,omitempty"`
}

// ExposeOps is the live side-effect surface: program the gateway and return the
// managed URL/token. The live adapter is wired in cmd; a nil Ops makes Execute
// fail with a clear error rather than panic.
type ExposeOps interface {
	Expose(ctx context.Context, req ExposeRequest) (*ExposeResult, error)
}

// ExposeSetConfig configures an ExposeSetHandler.
type ExposeSetConfig struct {
	Ops ExposeOps
	Log func(format string, args ...any)
}

// ExposeSetHandler processes EXPOSE_SET jobs.
type ExposeSetHandler struct {
	cfg ExposeSetConfig
}

// NewExposeSetHandler constructs an EXPOSE_SET handler.
func NewExposeSetHandler(cfg ExposeSetConfig) *ExposeSetHandler {
	if cfg.Log == nil {
		cfg.Log = func(string, ...any) {}
	}
	return &ExposeSetHandler{cfg: cfg}
}

// CanHandle reports whether this handler processes the given job type.
func (h *ExposeSetHandler) CanHandle(jobType string) bool {
	return jobType == JobTypeExposeSet
}

// validVisibilities is the accepted visibility set (mirrors gateway.Visibility;
// kept local to avoid a worker->gateway dependency).
var validVisibilities = map[string]bool{"private": true, "org": true, "link": true}

// Execute programs the gateway to expose one local service. See the package doc
// for the privilege gate.
func (h *ExposeSetHandler) Execute(ctx context.Context, job *Job, stream StreamWriter) (*JobResult, error) {
	// Privilege gate: exposing a node service is privileged + node-targeted.
	if !isPerNodeStream(job.SourceQueue) {
		return h.failure(fmt.Errorf(
			"EXPOSE_SET refused: must be dispatched to the per-node stream, got source queue %q", job.SourceQueue)), nil
	}
	if h.cfg.Ops == nil {
		return h.failure(fmt.Errorf("EXPOSE_SET handler is misconfigured: no expose ops")), nil
	}

	req, err := parseExposeRequest(job.Payload)
	if err != nil {
		// A malformed request is terminal: retrying the same bad payload cannot help.
		return h.failure(fmt.Errorf("EXPOSE_SET: %w", err)), nil
	}

	h.cfg.Log("EXPOSE_SET: name=%q port=%d visibility=%q", req.Name, req.Port, req.Visibility)

	res, err := h.cfg.Ops.Expose(ctx, req)
	if err != nil {
		// Programming the gateway failed — transient (no in-process gateway yet,
		// mesh not ready). Retry (DLQ-bounded by the runner).
		return h.retry(fmt.Errorf("EXPOSE_SET: expose %q: %w", req.Name, err)), nil
	}

	out := map[string]any{
		"name":       req.Name,
		"visibility": req.Visibility,
		"url":        res.URL,
	}
	if res.Token != "" {
		out["token"] = res.Token
	}
	if res.ExpiresAt != "" {
		out["expires_at"] = res.ExpiresAt
	}
	h.cfg.Log("EXPOSE_SET: exposed %q at %q", req.Name, res.URL)
	return &JobResult{Status: JobStatusSuccess, Output: out}, nil
}

// parseExposeRequest reconstructs an ExposeRequest from the flattened job
// payload (top-level fields, same convention as parseModuleAssignment) and
// validates the required fields.
func parseExposeRequest(payload map[string]any) (ExposeRequest, error) {
	var req ExposeRequest
	if payload == nil {
		return req, fmt.Errorf("empty payload")
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return req, fmt.Errorf("marshal payload: %w", err)
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return req, fmt.Errorf("decode expose request: %w", err)
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Visibility = strings.ToLower(strings.TrimSpace(req.Visibility))
	if req.Name == "" {
		return req, fmt.Errorf("expose request is missing a name")
	}
	if req.Port <= 0 {
		return req, fmt.Errorf("expose request has an invalid port %d", req.Port)
	}
	if !validVisibilities[req.Visibility] {
		return req, fmt.Errorf("unknown visibility %q (want private|org|link)", req.Visibility)
	}
	if req.Epoch <= 0 {
		req.Epoch = 1
	}
	return req, nil
}

func (h *ExposeSetHandler) failure(err error) *JobResult {
	return &JobResult{Status: JobStatusFailure, Error: err, Output: map[string]any{"error": err.Error()}}
}

func (h *ExposeSetHandler) retry(err error) *JobResult {
	return &JobResult{Status: JobStatusRetry, Error: err, Output: map[string]any{"error": err.Error()}}
}

// Ensure ExposeSetHandler implements JobHandler.
var _ JobHandler = (*ExposeSetHandler)(nil)
