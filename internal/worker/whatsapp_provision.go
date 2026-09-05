// internal/worker/whatsapp_provision.go
//
// WHATSAPP_PROVISION job handler (aceteam#4454). Lets the WhatsApp MCP
// remote-control the user's OWN Citadel node to self-host the Baileys WhatsApp
// bridge -- the sovereign / BYO-infra alternative to a hosted multi-tenant
// bridge (#3990). The node does exactly what `citadel whatsapp up` does locally:
// deploy the bridge community module (Docker), mint (or reuse) a tenant, wait
// for readiness, and return the connection details + pairing QR so the shared
// backend can register the credential and surface the QR to the human.
//
// The handler reuses the exact orchestration behind `citadel whatsapp up`
// (whatsapp.Provision) rather than reimplementing the bridge. The CLI and this
// handler share whatsapp.Provision as the single source of truth.
//
// # Return contract (SHARED with the aceteam whatsapp_provision MCP tool)
//
// The handler emits a JSON document as its output bytes (under the JobResult
// "output" key, matching the legacy adapter's wire shape) so the backend parses
// through the {"output": "<json>"} wrapper -- identical to the COBROWSE handler:
//
//	{
//	  "api_url": "http://<mesh-ip>:<port>",   // node's Headscale mesh IP
//	  "api_key": "<per-tenant wab_ key>",
//	  "qr":      "data:image/png;base64,...",  // "" when already linked
//	  "tenant":  "<name>",
//	  "status":  "provisioned" | "already_linked",
//	  // Upgrade legibility (#718), always present:
//	  "upgraded": true,                        // the deploy moved the bridge onto a new image
//	  "image_id_before": "sha256:...",         // omitted when unknown (first deploy / unreadable)
//	  "image_id_after":  "sha256:...",         // omitted when unknown
//	  "image_pull_error": "...",               // omitted when the pull succeeded
//	  // Optional cert-publish contract (#448), omitted when off-mesh / no-TLS:
//	  "gateway_cert_pem": "-----BEGIN CERTIFICATE-----...", // trust to reach api_url
//	  "cert_refresh_url": "http://<mesh-ip>:<status-port>/gateway-cert.pem"
//	}
//
// # Upgrade vs. no-op (aceteam-ai/citadel-cli#718)
//
// `status` deliberately keeps its two original values. The aceteam backend
// branches on `status == "already_linked"` by equality, so a third value would
// silently fall through to the generic branch. The upgrade signal is carried
// additively instead: read `upgraded` (and the two image IDs) alongside `status`.
// A two-second `already_linked` with `upgraded: false` and identical image IDs is
// a no-op re-provision, not an upgrade -- and `image_pull_error` says when the
// node could not even reach the registry to try.
//
// # Privilege gating
//
// WHATSAPP_PROVISION deploys a container + mints credentials on the user's node,
// so it is honored ONLY when the job arrives on the per-node stream
// (jobs:v1:shell:org_<id>:node:<nodeid>), never the shared org pool -- exactly
// like AGENT_UPDATE (isPerNodeStream). It also requires Docker + the module's
// private-repo git credentials on the node; when those are missing the deploy
// edge returns a clear structured error rather than hanging (the CLI sets
// GIT_TERMINAL_PROMPT=0 so a credential-less clone fails fast).
package worker

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aceteam-ai/citadel-cli/internal/whatsapp"
)

// WhatsAppProvisionConfig configures a WhatsAppProvisionHandler. The Provision
// dependency is injectable so the handler can be unit-tested without touching
// Docker, git, the bridge, or the mesh network.
type WhatsAppProvisionConfig struct {
	// Provision runs the deploy -> mint -> wait -> fetch-QR flow. Defaults to a
	// closure over whatsapp.Provision wired with the node's real docker / git /
	// network edges (see cmd/work.go). Overridable in tests.
	Provision func(ctx context.Context, req whatsapp.ProvisionRequest) (*whatsapp.ProvisionResult, error)

	// Rotate performs an admin-key rotation, honored ONLY when the payload sets
	// `rotate_admin_key: true`. It is a closure over whatsapp.RotateAdminKey
	// wired with the node's real edges (cmd/nodejobs.go) -- the SAME rotation
	// primitive the `citadel whatsapp rotate-key` CLI uses. Nil means rotation is
	// not wired, and a rotate request then fails with a clear error rather than a
	// panic.
	Rotate func(ctx context.Context) (*whatsapp.RotateResult, error)

	// Log reports progress. Nil is a no-op.
	Log func(format string, args ...any)
}

// WhatsAppProvisionHandler processes WHATSAPP_PROVISION jobs.
type WhatsAppProvisionHandler struct {
	cfg WhatsAppProvisionConfig
}

// NewWhatsAppProvisionHandler constructs a WHATSAPP_PROVISION handler. The
// caller must supply cfg.Provision (it depends on cmd-level edges the worker
// package cannot import); a nil Provision makes Execute fail with a clear error
// rather than panic.
func NewWhatsAppProvisionHandler(cfg WhatsAppProvisionConfig) *WhatsAppProvisionHandler {
	if cfg.Log == nil {
		cfg.Log = func(string, ...any) {}
	}
	return &WhatsAppProvisionHandler{cfg: cfg}
}

// CanHandle reports whether this handler processes the given job type.
func (h *WhatsAppProvisionHandler) CanHandle(jobType string) bool {
	return jobType == JobTypeWhatsAppProvision
}

// Execute provisions the WhatsApp bridge on this node and returns the connection
// details + pairing QR as a JSON document. See the package doc for the return
// contract and the privilege gate.
func (h *WhatsAppProvisionHandler) Execute(ctx context.Context, job *Job, stream StreamWriter) (*JobResult, error) {
	// Privilege gate: WHATSAPP_PROVISION must arrive on the per-node stream, not
	// the shared org pool. Deploying a container + minting creds on the user's
	// node is privileged and node-targeted -- mirror AGENT_UPDATE.
	if !isPerNodeStream(job.SourceQueue) {
		return h.failure(fmt.Errorf(
			"WHATSAPP_PROVISION refused: must be dispatched to the per-node stream, got source queue %q", job.SourceQueue)), nil
	}

	// Admin-key rotation short-circuits BEFORE provisioning: it is a distinct,
	// additive operation on the SAME per-node-gated job type (citadel#624 part 3),
	// not a variant of provision. Rotate-only keeps "one implementation, two entry
	// points" honest (the CLI `citadel whatsapp rotate-key` is rotate-only too).
	if payloadBool(job.Payload, "rotate_admin_key") {
		return h.rotate(ctx), nil
	}

	if h.cfg.Provision == nil {
		return h.failure(fmt.Errorf("WHATSAPP_PROVISION handler is misconfigured: no provision function")), nil
	}

	req := whatsapp.ProvisionRequest{
		Tenant:    payloadString(job.Payload, "tenant"),
		Proxy:     payloadString(job.Payload, "proxy"),
		PublicURL: payloadString(job.Payload, "public_url"),
		// Optional explicit host-port override. When absent/0, Provision
		// auto-selects a free host port so the bridge does not collide with
		// citadel's own 8080 listener (aceteam-ai/citadel-cli#438).
		Port: payloadInt(job.Payload, "port"),
	}
	if req.Tenant == "" {
		req.Tenant = "default"
	}
	h.cfg.Log("WHATSAPP_PROVISION: provisioning bridge (tenant=%q)", req.Tenant)

	res, err := h.cfg.Provision(ctx, req)
	if err != nil {
		// Docker/creds-missing, off-mesh, or bridge-not-ready all surface here as
		// a structured failure (never a hang).
		return h.failure(fmt.Errorf("WHATSAPP_PROVISION failed: %w", err)), nil
	}

	// Prefer a base64 data-URL PNG so the server can render the QR without the
	// phone (or the backend) reaching the bridge directly. An already-linked
	// tenant has no QR.
	qrDataURL, err := whatsapp.QRDataURL(res.QR)
	if err != nil {
		// Fall back to the raw payload string so the caller still gets something
		// usable rather than failing the whole provision over a render error.
		h.cfg.Log("WHATSAPP_PROVISION: QR PNG render failed (%v); returning raw payload", err)
		qrDataURL = res.QR
	}

	status := "provisioned"
	if res.AlreadyLinked {
		status = "already_linked"
		qrDataURL = ""
	}

	doc := map[string]any{
		"api_url": res.APIURL,
		"api_key": res.APIKey,
		"qr":      qrDataURL,
		"tenant":  res.Tenant,
		"status":  status,
		// Always present so a caller can tell an upgrade from a no-op without
		// having to know whether the node is new enough to report image IDs.
		"upgraded": res.Upgraded,
	}
	// Image identity is omitted when unknown (bridge not previously running, or
	// Docker unreadable) rather than reported as an empty string that could be
	// mistaken for "no image".
	if res.ImageIDBefore != "" {
		doc["image_id_before"] = res.ImageIDBefore
	}
	if res.ImageIDAfter != "" {
		doc["image_id_after"] = res.ImageIDAfter
	}
	// A failed pull is not fatal (the cached image still runs), but it means this
	// provision CANNOT have upgraded anything -- say so.
	if res.ImagePullError != "" {
		doc["image_pull_error"] = res.ImagePullError
	}
	// Optional cert-publish contract (aceteam-ai/citadel-cli#448): hand the backend
	// the gateway leaf cert to trust (the api_url is an https gateway route) plus
	// the plaintext URL to re-fetch it from on rotation. Omitted (omitempty) when
	// the gateway runs --gateway-no-tls or the node is off-mesh, so older backends
	// that ignore these keys are unaffected.
	if res.GatewayCertPEM != "" {
		doc["gateway_cert_pem"] = res.GatewayCertPEM
	}
	if res.CertRefreshURL != "" {
		doc["cert_refresh_url"] = res.CertRefreshURL
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return h.failure(fmt.Errorf("marshal provision result: %w", err)), nil
	}

	// Emit the JSON as the "output" string, matching the legacy adapter's wire
	// shape so the backend unwraps {"output": "<json>"} exactly like COBROWSE.
	return &JobResult{
		Status: JobStatusSuccess,
		Output: map[string]any{"output": string(out)},
	}, nil
}

// rotate handles a `rotate_admin_key: true` WHATSAPP_PROVISION job: it rotates
// the bridge's admin secret via the shared whatsapp.RotateAdminKey primitive and
// returns an ADDITIVE result document. It deliberately does NOT emit the
// provision result's `status` field: the aceteam backend branches on
// `status == "already_linked"` by equality, and a rotate is neither provisioned
// nor already_linked -- omitting the field lets that equality be simply false
// rather than inventing a third value or lying. The result carries fingerprints
// only (sha256:...), never key bytes.
func (h *WhatsAppProvisionHandler) rotate(ctx context.Context) *JobResult {
	if h.cfg.Rotate == nil {
		return h.failure(fmt.Errorf("WHATSAPP_PROVISION rotate_admin_key requested but the handler is misconfigured: no rotate function"))
	}
	h.cfg.Log("WHATSAPP_PROVISION: rotating bridge admin key")

	res, err := h.cfg.Rotate(ctx)
	if err != nil {
		return h.failure(fmt.Errorf("WHATSAPP_PROVISION admin-key rotation failed: %w", err))
	}

	doc := map[string]any{
		"rotated":               res.Rotated,
		"admin_key_fingerprint": res.NewFingerprint,
	}
	// The prior fingerprint is omitted when there was no key before (first key),
	// mirroring the "" in / "" out fingerprint contract.
	if res.OldFingerprint != "" {
		doc["old_admin_key_fingerprint"] = res.OldFingerprint
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return h.failure(fmt.Errorf("marshal rotation result: %w", err))
	}
	// Emit the JSON as the "output" string, matching the provision path's wire
	// shape so the backend unwraps {"output": "<json>"} identically.
	return &JobResult{
		Status: JobStatusSuccess,
		Output: map[string]any{"output": string(out)},
	}
}

func (h *WhatsAppProvisionHandler) failure(err error) *JobResult {
	return &JobResult{
		Status: JobStatusFailure,
		Error:  err,
		Output: map[string]any{"error": err.Error()},
	}
}

// Ensure WhatsAppProvisionHandler implements JobHandler.
var _ JobHandler = (*WhatsAppProvisionHandler)(nil)
