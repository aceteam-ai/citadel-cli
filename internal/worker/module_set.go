// internal/worker/module_set.go
//
// MODULE_SET job handler (interim, aceteam-ai/aceteam#5280). Lets the
// `node_module_set` MCP tool remote-control which service modules run on a
// user's OWN Citadel node by assigning a SINGLE module a desired state:
//
//   - running -- the module must be installed and running (install if missing)
//   - stopped -- the module must be installed but NOT running (durably)
//   - absent  -- the module must be uninstalled (removed from the node)
//
// # Why this handler exists (interim vs. the durable design)
//
// The full design (aceteam#4273) is a DECLARATIVE pull loop: the node fetches
// its whole desired module set from a control-plane serve endpoint and converges
// via the internal/reconcile engine. That serve endpoint + durable server-side
// desired-state storage do not exist yet. Meanwhile `node_module_set` already
// QUEUES a MODULE_SET job carrying ONE ModuleAssignment on the node's per-node
// stream, and today NO citadel handler consumes it (it nacks + dead-letters).
// This handler closes that gap by applying that single assignment IMPERATIVELY,
// reusing the SAME tested reconcile engine but scoped to one module. It converges
// into #4273 when the pull loop lands.
//
// # The single-module-scoped-actual trick
//
// The reconcile engine treats `desired` as AUTHORITATIVE for the whole node:
// anything in `actual` but not in `desired` is uninstalled. If we passed the
// node's FULL installed set as `actual` with a one-entry `desired`, the engine
// would uninstall EVERY OTHER module. So we scope BOTH sides to just the target
// module (matched by canonical Source), and align the desired assignment's key
// to the installed module's real service name to avoid the name-gap churn (see
// scopeToSingleModule). The engine then emits exactly the install/update/start/
// stop/uninstall step for this one module and touches nothing else.
//
// # Privilege gating
//
// MODULE_SET installs/uninstalls a compose stack + mutates the node manifest, so
// it is honored ONLY when the job arrives on the per-node stream
// (jobs:v1:shell:org_<id>:node:<nodeid>), never the shared org pool -- exactly
// like AGENT_UPDATE and WHATSAPP_PROVISION (isPerNodeStream). It fails closed.
package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/reconcile"
)

// statusAbsent is the desired_status value that means "uninstall". It is NOT a
// reconcile.DesiredStatus (which only models running/stopped); absent is realized
// as an EMPTY desired set for the scoped module, which the engine converges by
// uninstalling the module if it is installed (and is a no-op if it is not).
const statusAbsent = "absent"

// ModuleSetConfig configures a ModuleSetHandler. The Ops dependency is injectable
// so the handler's routing (single-module scope, absent-uninstall, idempotency)
// is unit-testable with a fake ModuleOps -- without touching docker, git, the
// catalog, or the node manifest.
type ModuleSetConfig struct {
	// Ops is the live side-effect surface (install/uninstall/start/stop/list).
	// The live adapter is wired in cmd (it needs cmd-level catalog/manifest edges
	// the worker package cannot import); a nil Ops makes Execute fail with a clear
	// error rather than panic.
	Ops reconcile.ModuleOps

	// Log reports progress. Nil is a no-op.
	Log func(format string, args ...any)
}

// ModuleSetHandler processes MODULE_SET jobs.
type ModuleSetHandler struct {
	cfg ModuleSetConfig
}

// NewModuleSetHandler constructs a MODULE_SET handler.
func NewModuleSetHandler(cfg ModuleSetConfig) *ModuleSetHandler {
	if cfg.Log == nil {
		cfg.Log = func(string, ...any) {}
	}
	return &ModuleSetHandler{cfg: cfg}
}

// CanHandle reports whether this handler processes the given job type.
func (h *ModuleSetHandler) CanHandle(jobType string) bool {
	return jobType == JobTypeModuleSet
}

// Execute applies a single module assignment to this node via the scoped
// reconcile engine. See the package doc for the privilege gate and the
// single-module-scoped-actual trick.
func (h *ModuleSetHandler) Execute(ctx context.Context, job *Job, stream StreamWriter) (*JobResult, error) {
	// Privilege gate: MODULE_SET must arrive on the per-node stream, not the
	// shared org pool -- installing/uninstalling a compose stack on the user's
	// node is privileged and node-targeted. Fail closed.
	if !isPerNodeStream(job.SourceQueue) {
		return h.failure(fmt.Errorf(
			"MODULE_SET refused: must be dispatched to the per-node stream, got source queue %q", job.SourceQueue)), nil
	}

	if h.cfg.Ops == nil {
		return h.failure(fmt.Errorf("MODULE_SET handler is misconfigured: no module ops")), nil
	}

	m, err := parseModuleAssignment(job.Payload)
	if err != nil {
		// A malformed assignment is a permanent (terminal) failure: retrying the
		// same bad payload cannot help.
		return h.failure(fmt.Errorf("MODULE_SET: %w", err)), nil
	}

	statusRaw := strings.ToLower(strings.TrimSpace(string(m.DesiredStatus)))
	absent := statusRaw == statusAbsent
	// Reject an unknown status up front (running/stopped/absent are the contract;
	// empty defaults to running). An unknown value is a terminal bad request.
	switch statusRaw {
	case "", string(reconcile.StatusRunning), string(reconcile.StatusStopped), statusAbsent:
	default:
		return h.failure(fmt.Errorf("MODULE_SET: unknown desired_status %q (want running|stopped|absent)", m.DesiredStatus)), nil
	}

	h.cfg.Log("MODULE_SET: source=%q desired_status=%q", m.Source, statusRaw)

	// Scope BOTH desired and actual to just this module. This is the crux: it
	// prevents the whole-node-authoritative engine from uninstalling every other
	// installed module.
	desired, actual, key, err := scopeToSingleModule(ctx, h.cfg.Ops, m, absent)
	if err != nil {
		// Listing installed state failed -- a transient node/docker problem worth a
		// retry rather than a terminal failure.
		return h.retry(fmt.Errorf("MODULE_SET: inspect installed modules: %w", err)), nil
	}

	plan, err := reconcile.Reconcile(ctx, desired, actual)
	if err != nil {
		return h.retry(fmt.Errorf("MODULE_SET: plan: %w", err)), nil
	}

	applyRes, err := reconcile.Apply(ctx, h.cfg.Ops, plan)
	if err != nil {
		// Context cancellation / caller-level problem -- retry.
		return h.retry(fmt.Errorf("MODULE_SET: apply: %w", err)), nil
	}

	steps := describeSteps(plan)
	if applyRes.Failed() {
		// Per-module failure isolation means, for our single scoped module, the
		// error is the one that matters. Treat it as transient (Nack -> retry,
		// DLQ-bounded by the runner's MaxAttempts) so a flaky clone/pull recovers.
		failErr := firstApplyError(applyRes)
		return h.retryOutput(
			fmt.Errorf("MODULE_SET: converge %q failed: %w", key, failErr),
			map[string]any{
				"module":         key,
				"source":         m.Source,
				"desired_status": statusRaw,
				"steps":          steps,
			}), nil
	}

	h.cfg.Log("MODULE_SET: converged %q (%d step(s))", key, len(plan.Steps))
	return &JobResult{
		Status: JobStatusSuccess,
		Output: map[string]any{
			"module":         key,
			"source":         m.Source,
			"desired_status": statusRaw,
			"steps":          steps,
			"converged":      true,
		},
	}, nil
}

// scopeToSingleModule builds the desired/actual pair for a ONE-module reconcile.
//
// It lists the node's installed modules, finds the one whose canonical Source
// equals the assignment's Source, and:
//
//   - aligns the desired assignment's Name (reconcile key) to the installed
//     module's REAL service name, so start/stop/uninstall steps carry the name
//     the ops adapter can map back to a manifest service -- this sidesteps the
//     "payload has no name, basename != service name" gap (see the PR note).
//   - scopes `actual` to just that module (or empty when not installed), so the
//     whole-node-authoritative engine cannot uninstall the node's other modules.
//
// For absent, `desired` is empty (the engine uninstalls the scoped module if it
// is installed; a no-op if not).
func scopeToSingleModule(ctx context.Context, ops reconcile.ModuleOps, m reconcile.ModuleAssignment, absent bool) (reconcile.DesiredState, []reconcile.InstalledModule, string, error) {
	installed, err := ops.ListInstalled(ctx)
	if err != nil {
		return reconcile.DesiredState{}, nil, "", err
	}

	var actual []reconcile.InstalledModule
	for i := range installed {
		if installed[i].Source == m.Source {
			// Align the desired key to the installed module's real name.
			m.Name = installed[i].Name
			actual = []reconcile.InstalledModule{installed[i]}
			break
		}
	}

	key := m.Key() // aligned name if installed, else NameFromSource(source)

	if absent {
		// Empty desired for the scoped module => uninstall it if installed.
		return reconcile.DesiredState{}, actual, key, nil
	}
	return reconcile.DesiredState{Modules: []reconcile.ModuleAssignment{m}}, actual, key, nil
}

// parseModuleAssignment reconstructs a ModuleAssignment from the flattened job
// payload. `node_module_set` xadds `payload = json(assignment)` and the redis
// source unmarshals that JSON directly into Job.Payload, so the assignment fields
// (source, desired_status, config) sit at the TOP LEVEL of Job.Payload. We
// re-marshal and decode into the typed assignment so the json tags do the field
// mapping (extra keys such as target_node are ignored).
func parseModuleAssignment(payload map[string]any) (reconcile.ModuleAssignment, error) {
	var m reconcile.ModuleAssignment
	if payload == nil {
		return m, fmt.Errorf("empty payload")
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return m, fmt.Errorf("marshal payload: %w", err)
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return m, fmt.Errorf("decode assignment: %w", err)
	}
	if strings.TrimSpace(m.Source) == "" {
		return m, fmt.Errorf("assignment is missing a source")
	}
	return m, nil
}

// describeSteps renders a plan's steps for the job output (observability).
func describeSteps(plan reconcile.Plan) []map[string]any {
	out := make([]map[string]any, 0, len(plan.Steps))
	for _, s := range plan.Steps {
		out = append(out, map[string]any{
			"action": string(s.Action),
			"name":   s.Name,
			"reason": s.Reason,
		})
	}
	return out
}

// firstApplyError returns any one per-module error from an ApplyResult (for a
// single-module plan there is at most one).
func firstApplyError(res reconcile.ApplyResult) error {
	for _, e := range res.Errors {
		return e
	}
	return fmt.Errorf("unknown convergence error")
}

func (h *ModuleSetHandler) failure(err error) *JobResult {
	return &JobResult{
		Status: JobStatusFailure,
		Error:  err,
		Output: map[string]any{"error": err.Error()},
	}
}

func (h *ModuleSetHandler) retry(err error) *JobResult {
	return h.retryOutput(err, map[string]any{"error": err.Error()})
}

func (h *ModuleSetHandler) retryOutput(err error, output map[string]any) *JobResult {
	if output == nil {
		output = map[string]any{}
	}
	output["error"] = err.Error()
	return &JobResult{Status: JobStatusRetry, Error: err, Output: output}
}

// Ensure ModuleSetHandler implements JobHandler.
var _ JobHandler = (*ModuleSetHandler)(nil)
