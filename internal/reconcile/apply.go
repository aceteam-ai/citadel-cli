package reconcile

import (
	"context"
	"fmt"
	"sort"
)

// StepResult records the outcome of applying a single Step.
type StepResult struct {
	Step Step
	Err  error // nil on success
}

// ApplyResult is the outcome of applying a whole Plan. Per-module failure
// isolation: a step that errors is recorded here and does NOT abort the rest of
// the plan.
type ApplyResult struct {
	// Results is one entry per executed step, in execution order.
	Results []StepResult
	// Errors maps module name -> the error that module hit (if any). It is the
	// per-module failure-isolation surface that feeds Health == HealthError in
	// the actual-state report.
	Errors map[string]error
}

// Failed reports whether any step errored.
func (r ApplyResult) Failed() bool { return len(r.Errors) > 0 }

// Apply executes a Plan via the injected ModuleOps. It applies every step,
// isolating per-module failures: if a step errors, the error is recorded
// against that module's name and execution CONTINUES with the remaining steps.
// This guarantees one bad module cannot block convergence of the others.
//
// Apply never returns a non-nil error for a step failure (those live in
// ApplyResult.Errors); it only returns an error for a caller-level problem such
// as a cancelled context.
func Apply(ctx context.Context, ops ModuleOps, plan Plan) (ApplyResult, error) {
	res := ApplyResult{Errors: map[string]error{}}

	for _, step := range plan.Steps {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		// Skip further steps for a module that already failed in this pass — a
		// later start/stop on a module whose install failed is meaningless and
		// would only produce a confusing second error.
		if _, alreadyFailed := res.Errors[step.Name]; alreadyFailed {
			res.Results = append(res.Results, StepResult{
				Step: step,
				Err:  fmt.Errorf("skipped: earlier step for %q failed", step.Name),
			})
			continue
		}

		err := applyStep(ctx, ops, step)
		res.Results = append(res.Results, StepResult{Step: step, Err: err})
		if err != nil {
			res.Errors[step.Name] = err
		}
	}

	return res, nil
}

// applyStep dispatches a single step to the matching ModuleOps operation.
func applyStep(ctx context.Context, ops ModuleOps, step Step) error {
	switch step.Action {
	case ActionInstall, ActionUpdate:
		return ops.Install(ctx, step.Assignment)
	case ActionUninstall:
		return ops.Uninstall(ctx, step.Name)
	case ActionStart:
		return ops.Start(ctx, step.Name)
	case ActionStop:
		return ops.Stop(ctx, step.Name)
	default:
		return fmt.Errorf("unknown action %q for module %q", step.Action, step.Name)
	}
}

// BuildActualState reads the node's installed modules via ListInstalled and
// overlays any per-module errors from an ApplyResult, producing the
// ActualState report to send back to the control plane. A module that failed to
// converge is reported with Health == HealthError and its error string, so the
// failure surfaces in the report without having blocked the others.
//
// node is the reporting node's identifier (device identity); it may be empty
// and filled in by the transport layer.
func BuildActualState(ctx context.Context, ops ModuleOps, applyErrs map[string]error, node string) (ActualState, error) {
	installed, err := ops.ListInstalled(ctx)
	if err != nil {
		return ActualState{}, fmt.Errorf("list installed modules: %w", err)
	}

	// Index reported modules so we can attach errors and surface modules that
	// failed before they ever became installed (e.g. a failed first install).
	byName := make(map[string]int, len(installed))
	out := make([]InstalledModule, 0, len(installed)+len(applyErrs))
	for _, im := range installed {
		byName[im.Name] = len(out)
		out = append(out, im)
	}

	for name, e := range applyErrs {
		if idx, ok := byName[name]; ok {
			out[idx].Health = HealthError
			out[idx].Error = e.Error()
			continue
		}
		// A module that errored and is not in ListInstalled (failed first
		// install) is still reported so the drift/error is visible.
		out = append(out, InstalledModule{
			Name:   name,
			Health: HealthError,
			Error:  e.Error(),
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return ActualState{Node: node, Modules: out}, nil
}
