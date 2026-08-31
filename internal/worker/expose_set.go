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

// ExposeRequest is the parsed EXPOSE_SET payload: expose either a local
// loopback service (Port) or a workspace-confined static directory (Path)
// under the gateway name Name with the given Visibility. Port and Path are
// mutually exclusive — exactly one must be set (issue #943).
type ExposeRequest struct {
	// Name is the exposed-service slug (the <name> in /expose/<name>/). Lowercase
	// alphanumeric + dashes; validated by the gateway.
	Name string `json:"name"`
	// Port is the service's loopback host port (e.g. 5000 for Frigate). Mutually
	// exclusive with Path.
	Port int `json:"port"`
	// Path is a workspace-relative or absolute directory to serve as a
	// read-only, auto-indexed static file share instead of proxying to a port.
	// Mutually exclusive with Port. Confinement to the node workspace (and, per
	// request, to the exposed directory itself) is enforced by the ExposeOps
	// implementation and the gateway — never trusted from the payload alone.
	Path string `json:"path"`
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
	//
	// Node-owned epoch custody (issue #944, design doc §5.3): the node — not the
	// caller — is the authority on the effective epoch. This field is kept for
	// wire back-compat and honored as a fast-forward hint (the node will never
	// adopt a value LOWER than the name's own high-water mark), never a rewind.
	// A blind/stateless caller that always sends the default (1) against a name
	// already living at a higher epoch is safe: the node preserves the current
	// epoch rather than silently reverting it. Use Rotate to explicitly revoke.
	Epoch int `json:"epoch"`
	// Rotate, when true, is the explicit revoke-all verb: the node advances the
	// name's epoch strictly past its own high-water mark, invalidating every
	// previously issued link token. A plain re-expose (Rotate=false) never does
	// this — it is safe to call blind, repeatedly, without revoking anything.
	Rotate bool `json:"rotate"`
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
	// Epoch is the AUTHORITATIVE effective epoch the node settled on (issue
	// #944) — the caller's Epoch/Rotate are a request, this is the truth. A
	// caller that wants to track epoch state at all should store this value,
	// not its own input.
	Epoch int `json:"epoch"`
}

// UnexposeResult is the worker-side mirror of the cmd-layer unexpose result
// (issue #944 design doc §6.1): kept here, not imported from cmd, since cmd
// imports worker and never the reverse. The live adapter (cmd.liveExposeOps)
// converts to/returns this shape directly.
type UnexposeResult struct {
	// Name is the exposed-service slug that was revoked.
	Name string `json:"name"`
	// WasExposed reports whether a live exposure actually existed. False still
	// means success (revoke is idempotent).
	WasExposed bool `json:"was_exposed"`
}

// ExposeOps is the live side-effect surface behind all three expose-custody
// verbs (issue #944): program the gateway (Expose), tear one down (Unexpose),
// and read back the durable inventory (List). One interface, one live adapter,
// one wiring site, one test fake — see the design doc §6.1. The live adapter is
// wired in cmd; a nil Ops makes each handler's Execute fail with a clear error
// rather than panic.
type ExposeOps interface {
	Expose(ctx context.Context, req ExposeRequest) (*ExposeResult, error)
	Unexpose(ctx context.Context, name string) (*UnexposeResult, error)
	List(ctx context.Context) (*ExposeListResult, error)
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

	h.cfg.Log("EXPOSE_SET: name=%q port=%d path=%q visibility=%q", req.Name, req.Port, req.Path, req.Visibility)

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
		// The node's authoritative effective epoch (issue #944 design doc §5.3),
		// not req.Epoch echoed back — a caller that wants to track epoch state at
		// all should store THIS value.
		"epoch": res.Epoch,
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
	req.Path = strings.TrimSpace(req.Path)
	req.Visibility = strings.ToLower(strings.TrimSpace(req.Visibility))
	if req.Name == "" {
		return req, fmt.Errorf("expose request is missing a name")
	}
	// Port and Path are mutually exclusive source types (issue #943): a proxy
	// target and a directory share cannot both back the same exposure.
	hasPort := req.Port > 0
	hasPath := req.Path != ""
	switch {
	case hasPort && hasPath:
		return req, fmt.Errorf("expose request must set exactly one of port or path, not both")
	case !hasPort && !hasPath:
		return req, fmt.Errorf("expose request requires either a port or a path")
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
