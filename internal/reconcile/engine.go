package reconcile

import (
	"context"
	"fmt"
	"sort"
)

// ActionType is the kind of convergence action a Step performs.
type ActionType string

const (
	// ActionInstall installs a module that is in desired but not in actual.
	ActionInstall ActionType = "install"
	// ActionUpdate re-installs a module whose source/ref or config drifted.
	ActionUpdate ActionType = "update"
	// ActionStart starts an installed module that should be running but isn't.
	ActionStart ActionType = "start"
	// ActionStop stops an installed module that should be stopped but is running.
	ActionStop ActionType = "stop"
	// ActionUninstall removes a module that is installed but not in desired.
	ActionUninstall ActionType = "uninstall"
)

// Step is one idempotent convergence action in a Plan, keyed by module name.
type Step struct {
	Action ActionType
	Name   string
	// Assignment is the desired assignment driving this step (empty Source for
	// ActionUninstall, where there is no desired entry).
	Assignment ModuleAssignment
	// Reason is a human-readable explanation for drift display / logs.
	Reason string
}

// Plan is the ordered set of steps needed to converge actual -> desired. An
// empty Plan means the node is already converged (the steady state — and the
// proof of idempotency: reconciling a converged node yields no steps).
type Plan struct {
	Steps []Step
}

// IsEmpty reports whether the plan has no steps (node already converged).
func (p Plan) IsEmpty() bool { return len(p.Steps) == 0 }

// Reconcile computes the idempotent diff between the control-plane desired state
// and the node's actual installed state. It does NOT perform any side effects;
// it returns a Plan that Apply executes.
//
// Drift rules (per module, matched by Key()/Name):
//   - in desired, not in actual            -> install (then stop if desired stopped)
//   - in both, source/ref differs          -> update  (then start/stop to match)
//   - in both, config differs              -> update  (then start/stop to match)
//   - in both, run-state differs           -> start or stop
//   - in actual, not in desired            -> uninstall
//
// Steps are ordered deterministically: uninstalls first (free resources),
// then installs/updates, then start/stop transitions — and within each group
// sorted by name so a Plan is stable and testable.
func Reconcile(ctx context.Context, desired DesiredState, actual []InstalledModule) (Plan, error) {
	if err := ctx.Err(); err != nil {
		return Plan{}, err
	}

	actualByName := make(map[string]InstalledModule, len(actual))
	for _, im := range actual {
		actualByName[im.Name] = im
	}
	desiredByName := make(map[string]ModuleAssignment, len(desired.Modules))
	for _, m := range desired.Modules {
		desiredByName[m.Key()] = m
	}

	var uninstalls, installsUpdates, transitions []Step

	// Uninstall anything installed but no longer desired.
	for name := range actualByName {
		if _, ok := desiredByName[name]; !ok {
			uninstalls = append(uninstalls, Step{
				Action: ActionUninstall,
				Name:   name,
				Reason: "installed but not in desired state",
			})
		}
	}

	// Install / update / transition for everything desired.
	for name, want := range desiredByName {
		cur, installed := actualByName[name]
		if !installed {
			installsUpdates = append(installsUpdates, Step{
				Action:     ActionInstall,
				Name:       name,
				Assignment: want,
				Reason:     "in desired state but not installed",
			})
			// A freshly installed module is running; if it should be stopped,
			// queue the stop transition.
			if want.EffectiveStatus() == StatusStopped {
				transitions = append(transitions, Step{
					Action:     ActionStop,
					Name:       name,
					Assignment: want,
					Reason:     "desired status is stopped",
				})
			}
			continue
		}

		// Installed: detect source/ref or config drift -> update.
		if reason, drifted := updateReason(want, cur); drifted {
			installsUpdates = append(installsUpdates, Step{
				Action:     ActionUpdate,
				Name:       name,
				Assignment: want,
				Reason:     reason,
			})
			// After an update the module is (re)installed and running; reconcile
			// its run-state against desired.
			if want.EffectiveStatus() == StatusStopped {
				transitions = append(transitions, Step{
					Action:     ActionStop,
					Name:       name,
					Assignment: want,
					Reason:     "desired status is stopped (post-update)",
				})
			}
			continue
		}

		// No content drift: only reconcile run-state.
		if step, ok := transitionStep(want, cur); ok {
			transitions = append(transitions, step)
		}
	}

	sortSteps(uninstalls)
	sortSteps(installsUpdates)
	sortSteps(transitions)

	steps := make([]Step, 0, len(uninstalls)+len(installsUpdates)+len(transitions))
	steps = append(steps, uninstalls...)
	steps = append(steps, installsUpdates...)
	steps = append(steps, transitions...)
	return Plan{Steps: steps}, nil
}

// updateReason reports whether the desired assignment differs from the current
// installed module in a way that requires a re-install (source/ref or config),
// and a human-readable reason.
func updateReason(want ModuleAssignment, cur InstalledModule) (string, bool) {
	if !sameSource(want.Source, cur) {
		return fmt.Sprintf("source changed: %q -> %q", cur.Source, want.Source), true
	}
	if !sameConfig(want.Config, cur.Config) {
		return "config changed", true
	}
	return "", false
}

// transitionStep returns a start/stop step if the current run-state does not
// match desired, for an already-installed, content-matching module.
func transitionStep(want ModuleAssignment, cur InstalledModule) (Step, bool) {
	switch want.EffectiveStatus() {
	case StatusRunning:
		if cur.Health != HealthRunning {
			return Step{
				Action:     ActionStart,
				Name:       cur.Name,
				Assignment: want,
				Reason:     "desired running, currently not running",
			}, true
		}
	case StatusStopped:
		if cur.Health == HealthRunning {
			return Step{
				Action:     ActionStop,
				Name:       cur.Name,
				Assignment: want,
				Reason:     "desired stopped, currently running",
			}, true
		}
	}
	return Step{}, false
}

// sameSource compares a desired source string against an installed module.
//
// It is a raw string equality on the REQUESTED source form (not a resolved
// ref). The InstalledModule.Source canonical-form contract requires the control
// plane and the node adapter to express Source identically (requested form) —
// otherwise a constraint/channel like "owner/repo@^1.2" reported by the node
// would never equal a resolved "owner/repo@v1.4.0" from the plane and the
// engine would re-update every pass. Resolved refs live in Commit/Ref, which
// are NOT part of this diff key.
func sameSource(wantSource string, cur InstalledModule) bool {
	return wantSource == cur.Source
}

// sameConfig compares two config-override maps for equality, treating nil and
// empty as equal.
func sameConfig(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

// sortSteps sorts steps by name for deterministic, testable plans.
func sortSteps(steps []Step) {
	sort.Slice(steps, func(i, j int) bool { return steps[i].Name < steps[j].Name })
}
