package platform

import (
	"context"
	"errors"
)

// Restore scroll only on an already navigated and selected, complete pinned
// page set. This does not create/navigate/select tabs, launch a browser, grant
// authority or publish a live registry record. No manager/job calls it yet.
func restoreSuspendPageScrolls(ctx context.Context, plan suspendRestoredPagePlan, active func() (string, error), fence func() error) error {
	return restoreSuspendPageScrollsWith(ctx, plan, active, fence,
		browserSuspendRestoredPageSource(true), browserSuspendRestoredPageSource(false))
}

// One immutable bounded plan and one unchanged caller budget cover both passes.
// Restoring a later page can invalidate an earlier observation, so success
// requires a separate read-only whole-manifest pass after ALL restore effects.
// This is sequential observation, not atomic layout/future-stability proof.
// The caller holds lifecycle exclusion and pins the provisional incarnation;
// on partial failure it must not publish live and must reconcile its runtime.
// Providers cooperate with ctx; active/fence remain bounded local reads.
func restoreSuspendPageScrollsWith(ctx context.Context, plan suspendRestoredPagePlan, active func() (string, error), fence func() error,
	restore, observe suspendRestoredPageSource) error {
	fail := func() error { return errors.New("could not restore complete browser page scroll positions") }
	if ctx == nil || ctx.Err() != nil || active == nil || fence == nil ||
		restore.verify == nil || restore.list == nil || restore.position == nil ||
		observe.verify == nil || observe.list == nil || observe.position == nil ||
		validateSuspendManifest(plan.manifest, plan.manifest.SessionID, plan.manifest.Generation) != nil ||
		len(plan.targetIDs) != len(plan.manifest.Tabs) {
		return fail()
	}
	if _, finite := ctx.Deadline(); !finite {
		return fail()
	}
	encoded, err := encodeSuspendManifest(plan.manifest)
	clear(encoded)
	if err != nil {
		return fail()
	}
	// The inner verifier copies its own pass. Keep a separate outer snapshot
	// so a callback cannot replace the plan between restoration and observation.
	plan.manifest.Tabs = append([]suspendTab(nil), plan.manifest.Tabs...)
	plan.targetIDs = append([]string(nil), plan.targetIDs...)
	if verifySuspendRestoredPagesWith(ctx, plan, active, fence, restore) != nil ||
		verifySuspendRestoredPagesWith(ctx, plan, active, fence, observe) != nil {
		return fail()
	}
	return nil
}
