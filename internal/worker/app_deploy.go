// internal/worker/app_deploy.go
//
// APP_* job handlers (CRAM slice A1, aceteam#9672): the node-side pod-per-app
// runner. The platform dispatches APP_DEPLOY / APP_STOP / APP_START /
// APP_STATUS / APP_LOGS / APP_DESTROY to run a real server-side web app as one
// pod on this node and front it with a gateway route.
//
// # Privilege gating
//
// Deploying/mutating a hosted app programs the node's container runtime and
// gateway route table, so - exactly like EXPOSE_SET / MODULE_SET / AGENT_UPDATE
// - every APP_* job is honored ONLY when it arrives on the per-node stream
// (jobs:v1:shell:org_<id>:node:<nodeid>), never the shared org pool. It fails
// closed. (The aceteam-side node:apps API-key scope, aceteam_mcp_expose.py's
// node:expose sibling, is minted in A4; citadel's own gate is this stream
// check.)
//
// # Standalone by design
//
// The pod lifecycle, image pull, host-port allocation, gateway wiring, and AEP
// receipt signing live in the cmd layer (they need the container runtime seam,
// the in-process gateway ref, and the node signing key), injected here as
// AppOps so this handler's routing/validation is unit-testable with a fake -
// the same shape as ExposeOps / InstanceProvider.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ErrAppTransient marks an AppOps error the handler should Nack (retry) rather
// than fail terminally: today only "no in-process gateway yet" on deploy, which
// clears once `citadel work --gateway` / the provisioned gateway is serving.
// Everything else (a rejected image, a bad manifest, an engine failure) is
// terminal - retrying the same payload cannot help. Mirrors EXPOSE_SET's
// transient-vs-terminal split.
var ErrAppTransient = errors.New("app op is transiently unavailable")

// AppDeployRequest is the parsed APP_DEPLOY payload. It is the contract the
// aceteam coordinator (A4, #9675) dispatches against; see the PR body for the
// authoritative field docs.
type AppDeployRequest struct {
	// JobID is set by the handler from the Job (never parsed from the payload)
	// so AppOps can bind the AEP receipt to this job.
	JobID string `json:"-"`

	// ShortCode is the Artifact short code. It derives the pod name
	// (aceapp-<short_code>) and the gateway route name (app-<short_code>).
	ShortCode string `json:"short_code"`
	// Image is the OCI image reference; validated against the registry allowlist
	// by the live ops (jobs.allowedImageRegistryPrefixes).
	Image string `json:"image"`
	// ContainerPort is the port the app listens on INSIDE the container. It is
	// published loopback-only to a node-allocated host port, and injected as the
	// PORT env var. cap-drop=ALL removes NET_BIND_SERVICE, so it must be >1024.
	ContainerPort int `json:"container_port"`
	// Env is passed to the app container verbatim (string->string).
	Env map[string]string `json:"env,omitempty"`
	// HealthPath is the HTTP path polled for readiness (default "/").
	HealthPath string `json:"health_path,omitempty"`
	// Size selects the resource limits (nano|small|medium|large; default small).
	Size string `json:"size,omitempty"`
	// Visibility is the gateway rung: private|org|link (default org). "platform"
	// is refused here - the DoR §3.4 platform rung is A5, not A1.
	Visibility string `json:"visibility,omitempty"`
	// Creator is the tailnet login authorized for a private exposure.
	Creator string `json:"creator,omitempty"`
	// Runtime optionally selects an allowlisted OCI runtime (runsc|kata) for
	// cross-org tenancy on a managed node (validated by the live ops).
	Runtime string `json:"runtime,omitempty"`
	// Org is the app's owning org, recorded as the aceteam.org container label.
	Org string `json:"org,omitempty"`
	// DeploymentID is recorded as the aceteam.deployment label and bound into the
	// receipt.
	DeploymentID string `json:"deployment_id,omitempty"`
	// StateVolumePath optionally mounts a ~-expanded, citadel-data-bounded host
	// path for durable state; when absent a per-app named volume is used.
	StateVolumePath string `json:"state_volume_path,omitempty"`
	// StateMountPath is the in-container mount point for the state volume
	// (default /data).
	StateMountPath string `json:"state_mount_path,omitempty"`
	// TTLSeconds bounds a link visibility token's lifetime (ignored otherwise).
	TTLSeconds int `json:"ttl_seconds,omitempty"`
	// SourceKind is git|workspace|image (default image). A1 implements image
	// only; git/workspace are refused with a clear "not yet implemented" so A4
	// can target the full shape now.
	SourceKind string `json:"source_kind,omitempty"`
	// SourceRef is the git URL / workspace path for the non-image source kinds
	// (reserved; A2 delivers node-side source cloning).
	SourceRef string `json:"source_ref,omitempty"`
	// ManifestSHA256 is the coordinator's digest of the canonical app manifest,
	// echoed as the receipt's input_sha256 so both sides agree byte-for-byte.
	ManifestSHA256 string `json:"manifest_sha256,omitempty"`
}

// AppDeployResult is what the live ops returns after creating the pod and
// wiring its gateway route.
type AppDeployResult struct {
	Name        string         `json:"name"`
	ShortCode   string         `json:"short_code"`
	URL         string         `json:"url"`
	HostPort    int            `json:"host_port"`
	PodID       string         `json:"pod_id"`
	Image       string         `json:"image"`
	ImageDigest string         `json:"image_digest,omitempty"`
	State       string         `json:"state"`
	Visibility  string         `json:"visibility"`
	Epoch       int            `json:"epoch"`
	Token       string         `json:"token,omitempty"`
	ExpiresAt   string         `json:"expires_at,omitempty"`
	Runtime     string         `json:"runtime,omitempty"`
	Receipt     map[string]any `json:"receipt,omitempty"`
}

// AppLifecycleResult is the shape STOP/START/DESTROY return.
type AppLifecycleResult struct {
	Name      string `json:"name"`
	ShortCode string `json:"short_code"`
	State     string `json:"state"`
}

// AppStatusResult is the shape STATUS returns.
type AppStatusResult struct {
	Name      string `json:"name"`
	ShortCode string `json:"short_code"`
	State     string `json:"state"` // running | stopped | not_found
	HostPort  int    `json:"host_port,omitempty"`
	PodID     string `json:"pod_id,omitempty"`
	Image     string `json:"image,omitempty"`
}

// AppLogsResult is the shape LOGS returns.
type AppLogsResult struct {
	ShortCode string `json:"short_code"`
	Logs      string `json:"logs"`
}

// AppOps is the live side-effect surface behind the APP_* verbs, injected from
// cmd (mirrors ExposeOps). One interface, one live adapter (cmd.liveAppOps),
// one wiring site, one test fake. A nil Ops makes each Execute fail with a
// clear error rather than panic.
type AppOps interface {
	Deploy(ctx context.Context, req AppDeployRequest) (*AppDeployResult, error)
	Stop(ctx context.Context, shortCode string) (*AppLifecycleResult, error)
	Start(ctx context.Context, shortCode string) (*AppLifecycleResult, error)
	Status(ctx context.Context, shortCode string) (*AppStatusResult, error)
	Logs(ctx context.Context, shortCode string, tail int) (*AppLogsResult, error)
	Destroy(ctx context.Context, shortCode string) (*AppLifecycleResult, error)
}

// AppHandlerConfig configures an AppHandler.
type AppHandlerConfig struct {
	Ops AppOps
	Log func(format string, args ...any)
}

// AppHandler processes the APP_* job family.
type AppHandler struct {
	cfg AppHandlerConfig
}

// NewAppHandler constructs an APP_* handler.
func NewAppHandler(cfg AppHandlerConfig) *AppHandler {
	if cfg.Log == nil {
		cfg.Log = func(string, ...any) {}
	}
	return &AppHandler{cfg: cfg}
}

// CanHandle reports whether this handler processes the given job type.
func (h *AppHandler) CanHandle(jobType string) bool {
	switch jobType {
	case JobTypeAppDeploy, JobTypeAppStop, JobTypeAppStart,
		JobTypeAppStatus, JobTypeAppLogs, JobTypeAppDestroy:
		return true
	}
	return false
}

// defaultAppLogTail is the log tail returned when APP_LOGS omits "tail".
const defaultAppLogTail = 200

// Execute dispatches one APP_* job. See the package doc for the privilege gate.
func (h *AppHandler) Execute(ctx context.Context, job *Job, stream StreamWriter) (*JobResult, error) {
	if !isPerNodeStream(job.SourceQueue) {
		return h.failure(fmt.Errorf(
			"%s refused: must be dispatched to the per-node stream, got source queue %q",
			job.Type, job.SourceQueue)), nil
	}
	if h.cfg.Ops == nil {
		return h.failure(fmt.Errorf("%s handler is misconfigured: no app ops", job.Type)), nil
	}

	switch job.Type {
	case JobTypeAppDeploy:
		return h.deploy(ctx, job)
	case JobTypeAppStop, JobTypeAppStart, JobTypeAppStatus, JobTypeAppLogs, JobTypeAppDestroy:
		return h.lifecycle(ctx, job)
	default:
		return h.failure(fmt.Errorf("unhandled app job type %q", job.Type)), nil
	}
}

func (h *AppHandler) deploy(ctx context.Context, job *Job) (*JobResult, error) {
	req, err := parseAppDeployRequest(job.Payload)
	if err != nil {
		// A malformed request is terminal: retrying the same bad payload cannot help.
		return h.failure(fmt.Errorf("APP_DEPLOY: %w", err)), nil
	}
	req.JobID = job.ID
	h.cfg.Log("APP_DEPLOY: short_code=%q image=%q container_port=%d visibility=%q size=%q",
		req.ShortCode, req.Image, req.ContainerPort, req.Visibility, req.Size)

	res, err := h.cfg.Ops.Deploy(ctx, req)
	if err != nil {
		if errors.Is(err, ErrAppTransient) {
			return h.retry(fmt.Errorf("APP_DEPLOY: deploy %q: %w", req.ShortCode, err)), nil
		}
		return h.failure(fmt.Errorf("APP_DEPLOY: deploy %q: %w", req.ShortCode, err)), nil
	}
	h.cfg.Log("APP_DEPLOY: %q %s at %q (host_port=%d pod=%s)", req.ShortCode, res.State, res.URL, res.HostPort, res.PodID)
	return &JobResult{Status: JobStatusSuccess, Output: structToMap(res)}, nil
}

func (h *AppHandler) lifecycle(ctx context.Context, job *Job) (*JobResult, error) {
	shortCode, tail, err := parseAppTargetRequest(job.Payload)
	if err != nil {
		return h.failure(fmt.Errorf("%s: %w", job.Type, err)), nil
	}
	h.cfg.Log("%s: short_code=%q", job.Type, shortCode)

	var (
		out    any
		opErr  error
		retryB bool
	)
	switch job.Type {
	case JobTypeAppStop:
		out, opErr = h.cfg.Ops.Stop(ctx, shortCode)
		retryB = true
	case JobTypeAppStart:
		out, opErr = h.cfg.Ops.Start(ctx, shortCode)
		retryB = true
	case JobTypeAppStatus:
		out, opErr = h.cfg.Ops.Status(ctx, shortCode)
	case JobTypeAppLogs:
		out, opErr = h.cfg.Ops.Logs(ctx, shortCode, tail)
	case JobTypeAppDestroy:
		out, opErr = h.cfg.Ops.Destroy(ctx, shortCode)
		retryB = true
	}
	if opErr != nil {
		// Lifecycle ops are idempotent on the engine side, so a transient engine
		// error is worth a retry (DLQ-bounded). STATUS/LOGS are read-only reports
		// and fail terminally instead - a retry of an unreadable read is unlikely
		// to differ within the DLQ window.
		if retryB {
			return h.retry(fmt.Errorf("%s: %w", job.Type, opErr)), nil
		}
		return h.failure(fmt.Errorf("%s: %w", job.Type, opErr)), nil
	}
	return &JobResult{Status: JobStatusSuccess, Output: structToMap(out)}, nil
}

// parseAppDeployRequest decodes and light-validates the APP_DEPLOY payload. It
// does NOT do the node-edge validation (image allowlist, runtime allowlist,
// state-volume confinement) - that lives in the live ops (jobs.ParseAppSpec),
// reusing service_payload.go's validators - so this stays a pure, fake-testable
// boundary check.
func parseAppDeployRequest(payload map[string]any) (AppDeployRequest, error) {
	var req AppDeployRequest
	if payload == nil {
		return req, fmt.Errorf("empty payload")
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return req, fmt.Errorf("marshal payload: %w", err)
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return req, fmt.Errorf("decode app deploy request: %w", err)
	}
	req.ShortCode = strings.TrimSpace(req.ShortCode)
	req.Image = strings.TrimSpace(req.Image)
	req.Visibility = strings.ToLower(strings.TrimSpace(req.Visibility))
	req.SourceKind = strings.ToLower(strings.TrimSpace(req.SourceKind))
	req.HealthPath = strings.TrimSpace(req.HealthPath)

	if req.ShortCode == "" {
		return req, fmt.Errorf("app deploy request is missing a short_code")
	}
	// The gateway route name is "app-<short_code>", which must satisfy the
	// gateway's exposure name grammar (lowercase alphanumerics and single
	// dashes). Reject a short code that would produce an invalid route rather
	// than fail later inside the gateway.
	if !isValidAppShortCode(req.ShortCode) {
		return req, fmt.Errorf("invalid short_code %q: expected lowercase alphanumerics and single dashes", req.ShortCode)
	}
	if req.Image == "" {
		return req, fmt.Errorf("app deploy request is missing an image")
	}
	if req.ContainerPort < 1024 || req.ContainerPort > 65535 {
		// cap-drop=ALL strips NET_BIND_SERVICE, so an app cannot bind <=1024
		// inside the container.
		return req, fmt.Errorf("container_port %d out of range (must be 1025-65535; the pod drops NET_BIND_SERVICE)", req.ContainerPort)
	}
	if req.Visibility == "" {
		req.Visibility = "org"
	}
	switch req.Visibility {
	case "private", "org", "link":
	case "platform":
		return req, fmt.Errorf("visibility %q is not available for hosted apps in A1 (the platform ingress rung is A5)", req.Visibility)
	default:
		return req, fmt.Errorf("unknown visibility %q (want private|org|link)", req.Visibility)
	}
	if req.SourceKind == "" {
		req.SourceKind = "image"
	}
	if req.SourceKind != "image" {
		return req, fmt.Errorf("source_kind %q is not yet implemented on the node (A1 supports image only; git/workspace source delivery is A2)", req.SourceKind)
	}
	return req, nil
}

// parseAppTargetRequest decodes the short-code-only payload shared by
// STOP/START/STATUS/LOGS/DESTROY, plus the optional "tail" for LOGS.
func parseAppTargetRequest(payload map[string]any) (shortCode string, tail int, err error) {
	if payload == nil {
		return "", 0, fmt.Errorf("empty payload")
	}
	raw, mErr := json.Marshal(payload)
	if mErr != nil {
		return "", 0, fmt.Errorf("marshal payload: %w", mErr)
	}
	var req struct {
		ShortCode string `json:"short_code"`
		Tail      int    `json:"tail"`
	}
	if uErr := json.Unmarshal(raw, &req); uErr != nil {
		return "", 0, fmt.Errorf("decode app request: %w", uErr)
	}
	req.ShortCode = strings.TrimSpace(req.ShortCode)
	if req.ShortCode == "" {
		return "", 0, fmt.Errorf("app request is missing a short_code")
	}
	if !isValidAppShortCode(req.ShortCode) {
		return "", 0, fmt.Errorf("invalid short_code %q", req.ShortCode)
	}
	tail = req.Tail
	if tail <= 0 {
		tail = defaultAppLogTail
	}
	return req.ShortCode, tail, nil
}

// isValidAppShortCode reports whether "app-"+code satisfies the gateway's
// exposure name grammar (lowercase alphanumerics and single dashes, no leading
// or trailing dash, no double dash). Kept local for the same worker->gateway
// no-import reason as validVisibilities.
func isValidAppShortCode(code string) bool {
	if code == "" || strings.HasPrefix(code, "-") || strings.HasSuffix(code, "-") {
		return false
	}
	prevDash := false
	for _, r := range code {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			prevDash = false
		case r == '-':
			if prevDash {
				return false
			}
			prevDash = true
		default:
			return false
		}
	}
	return true
}

// structToMap renders a result struct to a map[string]any via its json tags, so
// the wire output matches the documented shape exactly (same idiom as
// AEPReceiptV2.ToMap). A marshal failure yields an error map rather than a
// panic; the structs here are plain data so it cannot fail in practice.
func structToMap(v any) map[string]any {
	raw, err := json.Marshal(v)
	if err != nil {
		return map[string]any{"error": fmt.Sprintf("marshal result: %v", err)}
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return map[string]any{"error": fmt.Sprintf("unmarshal result: %v", err)}
	}
	return m
}

func (h *AppHandler) failure(err error) *JobResult {
	return &JobResult{Status: JobStatusFailure, Error: err, Output: map[string]any{"error": err.Error()}}
}

func (h *AppHandler) retry(err error) *JobResult {
	return &JobResult{Status: JobStatusRetry, Error: err, Output: map[string]any{"error": err.Error()}}
}

// Ensure AppHandler implements JobHandler.
var _ JobHandler = (*AppHandler)(nil)
