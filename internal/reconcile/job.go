package reconcile

import "context"

// ReconcileJobPayload is the (currently empty) payload of a push-nudge
// `reconcile` job (jobs.JobTypeReconcile). The job carries no parameters today:
// it simply asks the node to run an immediate reconcile pass against its
// control-plane-assigned desired state. Fields may be added later (e.g. a
// correlation id or a "force full re-pull" flag) without breaking the empty
// case.
type ReconcileJobPayload struct{}

// HandleReconcileJob is the push-nudge handler skeleton for a `reconcile` job
// on the node's queue. It is INERT unless the reconcile feature is enabled:
//
//   - If cfg.Enabled is false (the DEFAULT), it does nothing and returns nil —
//     a node that has not opted into remote management ignores the job.
//   - If a loop is provided, it Nudges the running loop (debounced) so the pass
//     coalesces with the periodic pull.
//   - Otherwise, with no loop but an explicit reconciler, it runs a single
//     immediate pass synchronously.
//
// This handler is intentionally NOT wired into the live worker dispatch in this
// increment; wiring (adding a case to the worker switch behind cfg.Enabled) is
// a documented TODO to keep the live worker path untouched. See the package doc
// and PR notes.
func HandleReconcileJob(ctx context.Context, cfg Config, loop *Loop, rec *Reconciler) error {
	if !cfg.Enabled {
		return nil // inert: feature disabled
	}
	if loop != nil {
		loop.Nudge()
		return nil
	}
	if rec != nil {
		_, _, err := rec.ReconcileOnce(ctx)
		return err
	}
	return nil
}
