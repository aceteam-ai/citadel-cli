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
//	  "status":  "provisioned" | "already_linked"
//	}
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

	if h.cfg.Provision == nil {
		return h.failure(fmt.Errorf("WHATSAPP_PROVISION handler is misconfigured: no provision function")), nil
	}

	req := whatsapp.ProvisionRequest{
		Tenant:    payloadString(job.Payload, "tenant"),
		Proxy:     payloadString(job.Payload, "proxy"),
		PublicURL: payloadString(job.Payload, "public_url"),
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

func (h *WhatsAppProvisionHandler) failure(err error) *JobResult {
	return &JobResult{
		Status: JobStatusFailure,
		Error:  err,
		Output: map[string]any{"error": err.Error()},
	}
}

// Ensure WhatsAppProvisionHandler implements JobHandler.
var _ JobHandler = (*WhatsAppProvisionHandler)(nil)
