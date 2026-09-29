// internal/worker/run_job_template.go
//
// RUN_JOB_TEMPLATE handler (citadel-cli#1149, aceteam#10288): the node-side
// execution of a platform job template's runner descriptor. The platform
// dispatches a template it has already approved for this node; the node runs the
// template's compiled-in builtin handler and returns the output artifacts as
// node-backed file references (path + sha256 + bytes), never the bytes.
//
// # Privilege gating
//
// A template run programs the node (it runs a compiled-in handler over caller
// params and writes into the node workspace), so - exactly like APP_* /
// EXPOSE_SET / MODULE_SET - it is honored ONLY on the per-node stream
// (jobs:v1:shell:org_<id>:node:<nodeid>), never the shared org pool. Fail closed.
//
// # Two node-side security invariants, both enforced HERE (not in the ops)
//
//  1. Anti-tamper: the node recomputes the executable-manifest hash over
//     {template_key, version, input_schema, output_schema, runner} and REFUSES
//     if it does not match the approved content_hash in the payload, so a
//     compromised or buggy coordinator cannot silently swap a different manifest
//     (a different runner, say) in under a hash the node owner already approved.
//     The recomputation is jobs.ComputeTemplateManifestHash, byte-identical to
//     the platform authority compute_template_hash.
//  2. No arbitrary shell: only a runner whose kind is exactly "builtin"
//     dispatches to a compiled-in handler. runner.kind == "shell" (or anything
//     else) is refused; there is deliberately no shell handler kind at all.
//
// Both run in the handler so they are fake-ops unit-testable and cannot be
// bypassed by a buggy ops. The actual builtin dispatch, workspace I/O, output
// hashing, and AEP receipt signing live in the cmd layer behind TemplateRunOps
// (it needs the workspace root and the node signing key), the same seam shape as
// AppOps / ExposeOps.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/jobs"
)

// ErrTemplateUnknownHandler marks a runner.handler that is not registered as a
// compiled-in builtin on this node. It is terminal: retrying the same payload on
// the same binary cannot make an unknown handler appear.
var ErrTemplateUnknownHandler = errors.New("run_job_template: unknown builtin handler")

// ErrTemplateTransient marks a TemplateRunOps error the handler should Nack
// (retry) rather than fail terminally, mirroring ErrAppTransient. A builtin's own
// deterministic failure over the same inputs is terminal instead.
var ErrTemplateTransient = errors.New("run_job_template: transiently unavailable")

// TemplateRunRequest is the parsed RUN_JOB_TEMPLATE payload. On the wire every
// value is a string (the nexus.Job.Payload map[string]string convention), so the
// JSON-object fields (runner/params/input_files/input_schema/output_schema)
// arrive as JSON-encoded strings; see parseTemplateRunRequest.
type TemplateRunRequest struct {
	// JobID is set by the handler from the Job (never parsed from the payload) so
	// TemplateRunOps can bind the AEP receipt to this job.
	JobID string `json:"-"`

	// TemplateKey and TemplateVersion identify the template; ContentHash pins the
	// exact approved manifest version. All three feed the anti-tamper recompute.
	TemplateKey     string `json:"template_key"`
	TemplateVersion int    `json:"template_version"`
	ContentHash     string `json:"content_hash"`

	// Runner is the JSON runner descriptor {"kind":"builtin","handler":"..."}.
	Runner json.RawMessage `json:"runner"`
	// Params is the template's own input params object.
	Params json.RawMessage `json:"params,omitempty"`
	// InputFiles is a JSON array of {path,node_id,node_path} node-backed file
	// references the run has access to. A file not already on this node is out of
	// scope for this slice (the ops resolves each against the node workspace).
	InputFiles json.RawMessage `json:"input_files,omitempty"`

	// InputSchema and OutputSchema are the template's JSON Schemas. They are part
	// of the hashed manifest, so they MUST be present to recompute the content
	// hash. The platform dispatcher sends both fields (aceteam#10445).
	InputSchema  json.RawMessage `json:"input_schema"`
	OutputSchema json.RawMessage `json:"output_schema"`
}

// TemplateOutput is one artifact a template run produced, as a node-backed
// reference: a workspace-relative path plus its digest and size. The bytes never
// ride back through the synchronous job result (aceteam#7553 gaps 3/4); the flow
// node reads the file lazily as a node:<node_id>/<path> reference.
type TemplateOutput struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

// TemplateRunResult is the RUN_JOB_TEMPLATE result shape.
type TemplateRunResult struct {
	Outputs     []TemplateOutput `json:"outputs"`
	DurationMs  int64            `json:"duration_ms"`
	InputHashes []string         `json:"input_hashes"`
	Receipt     map[string]any   `json:"receipt,omitempty"`
}

// TemplateRunOps is the live side-effect surface behind RUN_JOB_TEMPLATE,
// injected from cmd (mirrors AppOps). One interface, one live adapter
// (cmd.liveTemplateRunOps), one wiring site, one test fake. It dispatches the
// (already hash-verified, builtin-kind) request to a compiled-in builtin, writes
// outputs under the node workspace, and signs the receipt. A nil Ops makes
// Execute fail with a clear error rather than panic.
type TemplateRunOps interface {
	Run(ctx context.Context, req TemplateRunRequest) (*TemplateRunResult, error)
}

// RunJobTemplateHandlerConfig configures a RunJobTemplateHandler.
type RunJobTemplateHandlerConfig struct {
	Ops TemplateRunOps
	// NodeID comes from local runner configuration, not the payload.
	NodeID string
	Log    func(format string, args ...any)
}

// RunJobTemplateHandler processes RUN_JOB_TEMPLATE jobs.
type RunJobTemplateHandler struct {
	cfg RunJobTemplateHandlerConfig
}

// NewRunJobTemplateHandler constructs a RUN_JOB_TEMPLATE handler.
func NewRunJobTemplateHandler(cfg RunJobTemplateHandlerConfig) *RunJobTemplateHandler {
	if cfg.Log == nil {
		cfg.Log = func(string, ...any) {}
	}
	return &RunJobTemplateHandler{cfg: cfg}
}

// CanHandle reports whether this handler processes the given job type.
func (h *RunJobTemplateHandler) CanHandle(jobType string) bool {
	return jobType == JobTypeRunJobTemplate
}

// runnerDescriptor is the closed runner vocabulary. Only Kind == "builtin"
// dispatches; there is deliberately no "shell" kind.
type runnerDescriptor = jobs.TemplateRunner

// Execute runs one RUN_JOB_TEMPLATE job. See the package doc for the privilege
// gate and the two security invariants enforced here before any ops runs.
func (h *RunJobTemplateHandler) Execute(ctx context.Context, job *Job, stream StreamWriter) (*JobResult, error) {
	if !isPerNodeStream(job.SourceQueue) {
		return h.failure(fmt.Errorf(
			"%s refused: must be dispatched to the per-node stream, got source queue %q",
			job.Type, job.SourceQueue)), nil
	}
	if h.cfg.Ops == nil {
		return h.failure(fmt.Errorf("%s handler is misconfigured: no template run ops", job.Type)), nil
	}

	req, err := parseTemplateRunRequest(job.Payload)
	if err != nil {
		// A malformed or incomplete request is terminal: retrying the same bad
		// payload cannot help.
		return h.failure(fmt.Errorf("RUN_JOB_TEMPLATE: %w", err)), nil
	}
	req.JobID = job.ID
	if h.cfg.NodeID == "" || !strings.HasSuffix(job.SourceQueue, ":node:"+h.cfg.NodeID) {
		return h.failure(fmt.Errorf("RUN_JOB_TEMPLATE: source queue does not match executing node")), nil
	}
	runner, err := ValidateTemplateRunRequest(req, h.cfg.NodeID)
	if err != nil {
		return h.failure(fmt.Errorf("RUN_JOB_TEMPLATE: %w", err)), nil
	}
	req.Params, err = jobs.NormalizeTemplateParams(req.Params)
	if err != nil {
		return h.failure(fmt.Errorf("RUN_JOB_TEMPLATE: %w", err)), nil
	}

	h.cfg.Log("RUN_JOB_TEMPLATE: template=%q v%d handler=%q", req.TemplateKey, req.TemplateVersion, runner.Handler)
	res, err := h.cfg.Ops.Run(ctx, req)
	if err != nil {
		if errors.Is(err, ErrTemplateTransient) {
			return h.retry(fmt.Errorf("RUN_JOB_TEMPLATE: run %q: %w", req.TemplateKey, err)), nil
		}
		return h.failure(fmt.Errorf("RUN_JOB_TEMPLATE: run %q: %w", req.TemplateKey, err)), nil
	}
	if res == nil {
		return h.failure(fmt.Errorf("RUN_JOB_TEMPLATE: ops returned no result")), nil
	}
	h.cfg.Log("RUN_JOB_TEMPLATE: %q v%d produced %d output(s) in %dms", req.TemplateKey, req.TemplateVersion, len(res.Outputs), res.DurationMs)
	return &JobResult{Status: JobStatusSuccess, Output: structToMap(res)}, nil
}

// ValidateTemplateRunRequest is shared by the worker and live adapter. All
// manifest, runner, params and reference checks precede filesystem or ops effects.
func ValidateTemplateRunRequest(req TemplateRunRequest, nodeID string) (jobs.TemplateRunner, error) {
	var empty jobs.TemplateRunner
	if nodeID == "" {
		return empty, fmt.Errorf("executing node identity is not configured")
	}
	if req.TemplateKey == "" || req.TemplateVersion <= 0 || req.ContentHash == "" {
		return empty, fmt.Errorf("missing template identity or content_hash")
	}

	// Invariant 1 - anti-tamper: recompute the manifest hash and refuse on
	// mismatch, BEFORE running anything.
	recomputed, err := jobs.ComputeTemplateManifestHash(req.TemplateKey, req.TemplateVersion, req.InputSchema, req.OutputSchema, req.Runner)
	if err != nil {
		return empty, fmt.Errorf("canonicalize manifest for %q v%d: %w", req.TemplateKey, req.TemplateVersion, err)
	}
	if !hashesEqual(recomputed, req.ContentHash) {
		return empty, fmt.Errorf(
			"RUN_JOB_TEMPLATE refused: recomputed manifest hash %s does not match approved content_hash %s for template %q v%d",
			recomputed, normalizeHash(req.ContentHash), req.TemplateKey, req.TemplateVersion)
	}

	// Invariant 2 - only a builtin runner kind dispatches; shell never does.
	runner, err := parseRunnerDescriptor(req.Runner)
	if err != nil {
		return empty, err
	}
	if runner.Kind != "builtin" {
		return empty, fmt.Errorf(
			"RUN_JOB_TEMPLATE refused: runner kind %q is not permitted; only \"builtin\" runs, shell execution is never allowed",
			runner.Kind)
	}
	if runner.Handler == "" {
		return empty, fmt.Errorf("RUN_JOB_TEMPLATE refused: builtin runner has no handler name")
	}
	if err := jobs.ValidateTemplateParams(req.Params, req.InputSchema, req.OutputSchema); err != nil {
		return empty, err
	}
	if _, err := jobs.ParseTemplateInputFiles(req.InputFiles, nodeID); err != nil {
		return empty, err
	}
	return runner, nil
}

// parseTemplateRunRequest decodes the payload. It requires every field needed to
// recompute the anti-tamper hash (template_key, content_hash, runner,
// input_schema, output_schema); a run cannot be verified without them, so their
// absence is a terminal parse error rather than a silently skipped check.
func parseTemplateRunRequest(payload map[string]any) (TemplateRunRequest, error) {
	var req TemplateRunRequest
	if payload == nil {
		return req, fmt.Errorf("empty payload")
	}

	req.TemplateKey = tmplFieldString(payload, "template_key")
	if req.TemplateKey == "" {
		return req, fmt.Errorf("missing template_key")
	}
	req.ContentHash = tmplFieldString(payload, "content_hash")
	if req.ContentHash == "" {
		return req, fmt.Errorf("missing content_hash")
	}

	version, err := tmplFieldVersion(payload, "template_version")
	if err != nil {
		return req, err
	}
	req.TemplateVersion = version

	runner, ok := tmplFieldRawJSON(payload, "runner")
	if !ok {
		return req, fmt.Errorf("missing runner")
	}
	req.Runner = runner

	inputSchema, ok := tmplFieldRawJSON(payload, "input_schema")
	if !ok {
		return req, fmt.Errorf("missing input_schema (required to verify the content hash)")
	}
	req.InputSchema = inputSchema

	outputSchema, ok := tmplFieldRawJSON(payload, "output_schema")
	if !ok {
		return req, fmt.Errorf("missing output_schema (required to verify the content hash)")
	}
	req.OutputSchema = outputSchema

	// params and input_files are optional; absence is fine.
	if params, ok := tmplFieldRawJSON(payload, "params"); ok {
		req.Params = params
	} else if _, present := payload["params"]; present {
		return req, fmt.Errorf("params must be JSON-encoded object")
	} else {
		req.Params = json.RawMessage(`{}`)
	}
	if inputFiles, ok := tmplFieldRawJSON(payload, "input_files"); ok {
		req.InputFiles = inputFiles
	} else if _, present := payload["input_files"]; present {
		return req, fmt.Errorf("input_files must be JSON-encoded array")
	}

	return req, nil
}

func parseRunnerDescriptor(raw json.RawMessage) (runnerDescriptor, error) {
	return jobs.ParseTemplateRunner(raw)
}

// tmplFieldString reads a trimmed string value from the payload map.
func tmplFieldString(payload map[string]any, key string) string {
	if v, ok := payload[key]; ok {
		if s, ok := v.(string); ok {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// tmplFieldVersion reads template_version, accepting the wire string form ("3") as
// well as a JSON number, and requires a positive integer.
func tmplFieldVersion(payload map[string]any, key string) (int, error) {
	v, ok := payload[key]
	if !ok || v == nil {
		return 0, fmt.Errorf("missing %s", key)
	}
	var n int
	switch t := v.(type) {
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return 0, fmt.Errorf("empty %s", key)
		}
		parsed, err := strconv.Atoi(s)
		if err != nil {
			return 0, fmt.Errorf("%s %q is not an integer: %w", key, s, err)
		}
		n = parsed
	case json.Number:
		parsed, err := t.Int64()
		if err != nil {
			return 0, fmt.Errorf("%s %q is not an integer: %w", key, t.String(), err)
		}
		n = int(parsed)
	case float64:
		n = int(t)
		if float64(n) != t {
			return 0, fmt.Errorf("%s %v is not an integer", key, t)
		}
	default:
		return 0, fmt.Errorf("%s has unexpected type %T", key, v)
	}
	if n <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer, got %d", key, n)
	}
	return n, nil
}

// tmplFieldRawJSON returns a JSON-encoded string from the payload. The live job
// wire contract is map[string]string; accepting already-decoded maps or arrays
// here would create a second, test-only payload shape that production never
// receives. A missing, non-string, nil, or empty-string value returns false.
func tmplFieldRawJSON(payload map[string]any, key string) (json.RawMessage, bool) {
	v, ok := payload[key]
	if !ok || v == nil {
		return nil, false
	}
	s, ok := v.(string)
	if !ok {
		return nil, false
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, false
	}
	return json.RawMessage(s), true
}

// normalizeHash lowercases, trims, and strips an optional "sha256:" prefix so a
// bare-hex recomputation compares equal to a prefixed content_hash.
func normalizeHash(s string) string {
	return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(s)), "sha256:")
}

func hashesEqual(a, b string) bool {
	return normalizeHash(a) == normalizeHash(b)
}

func (h *RunJobTemplateHandler) failure(err error) *JobResult {
	return &JobResult{Status: JobStatusFailure, Error: err, Output: map[string]any{"error": err.Error()}}
}

func (h *RunJobTemplateHandler) retry(err error) *JobResult {
	return &JobResult{Status: JobStatusRetry, Error: err, Output: map[string]any{"error": err.Error()}}
}

// Ensure RunJobTemplateHandler implements JobHandler.
var _ JobHandler = (*RunJobTemplateHandler)(nil)
