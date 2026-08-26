// internal/jobs/reservation.go
package jobs

import (
	"fmt"
	"sort"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/status"
)

// Package-level doc for the job-scoped GPU reservation primitive
// (citadel-cli#832). This EXTENDS #577's preemption (PlanPreemption / the
// pinned_services allowlist / the durable desired_status marker) rather than
// reimplementing it: Reserve reuses buildPreemptCandidates + PlanPreemption
// verbatim, and evicts through the exact same durable-stop path preemptForVRAM
// uses. The new piece is the auto-RESTORE leg: every service Reserve stops is
// additionally tagged evicted_by_job=<jobID> in citadel.yaml (see
// setEvictedMarkersInManifestFile), and Release(jobID) restarts exactly the
// services carrying that tag and clears it.
//
// Crash-safety is manifest-marker-driven, not in-process state, by design: a
// crashed or killed worker leaves the tag in place (the OS never rolls back a
// completed file write), and ReconcileOrphanedReservations restores everything
// still tagged. There is deliberately NO separate reservation record/ledger
// file — the per-service yaml tag IS the single source of truth, so there is
// nothing else that could drift out of sync with it.
//
// One-process-per-node precondition: "any evicted_by_job tag found at startup
// is orphaned" is only true because a citadel node runs (at most) one active
// job-consuming worker at a time. ReconcileOrphanedReservations therefore
// takes an explicit holdsWorkerLock bool instead of assuming its caller
// checked — see that function's doc for the exact contract, INCLUDING a
// currently-latent gap: internal/worklock only guards `citadel work` against
// a second `citadel work`, not against the control-center TUI's own worker
// path (cmd/controlcenter.go), which consumes jobs off the same handler set
// WITHOUT ever acquiring that lock. Read that doc fully before wiring any
// caller (e.g. #8248) into a handler reachable from the control-center path.

// Reservation is the result of a job-scoped GPU VRAM hold (citadel-cli#832).
type Reservation struct {
	// JobID is the caller-supplied identifier this reservation is scoped to.
	JobID string
	// RequiredVRAMBytes is the budget Reserve was asked to fit.
	RequiredVRAMBytes uint64
	// Evicted lists the non-pinned services this reservation durably stopped,
	// in eviction order (idle-first, then largest-VRAM-first — #577's
	// ordering, unchanged). Empty when the budget already fit without
	// eviction, or when RequiredVRAMBytes==0.
	Evicted []string
	// Reason is a human-readable explanation, mirroring status.PreemptPlan.Reason.
	Reason string
}

// ReservationSummary is the read-only, heartbeat-facing view of an active
// reservation (citadel-cli#832), returned by ActiveReservations. Kept
// jobs-local (not internal/status) and mapped to status.GPUReservation by the
// caller (cmd/work.go's reservationsFrom), mirroring how citadel-cli#717
// keeps worker.SwapStats local and projects it via swapStatsFrom — see that
// pattern's doc in CLAUDE.md for why: internal/status cannot import
// internal/jobs (jobs already imports status).
type ReservationSummary struct {
	JobID           string
	EvictedServices []string
}

// Reserve evicts non-pinned services to free requiredVRAMBytes of VRAM on
// behalf of jobID, durably tagging every service it stops with
// evicted_by_job=jobID so Release(jobID) (or a crash-recovery reconcile) can
// restore exactly this reservation's evictions later. It reuses #577's
// decision unchanged: buildPreemptCandidates (idle-first, largest-VRAM-first
// ordering) feeding status.PlanPreemption.
//
// jobID must be non-empty (it is both the tag value and the Release/reconcile
// lookup key). requiredVRAMBytes==0 is a valid no-op reservation (nothing to
// enforce, mirrors PlanPreemption's own contract) — it still returns a
// Reservation, just with an empty Evicted list, so a caller can always treat
// Reserve's return the same way regardless of budget.
//
// Deliberate divergence from preemptForVRAM (#577): preemptForVRAM SKIPS the
// VRAM fit check (logs and returns nil) when free VRAM cannot be determined —
// safe there because a SERVICE_START with no confirmed fit signal simply
// proceeds un-preempted. Reserve is a caller's explicit ask for a GUARANTEED
// hold, so an unknown free-VRAM signal is instead a hard error: silently
// granting a reservation with no fit evidence would defeat the point of
// reserving at all.
//
// On failure to fit (the budget cannot be met without evicting a pinned
// service), Reserve rejects the reservation: it returns a nil error only on
// success. On a MID-PLAN eviction failure (a stop call itself errors), Reserve
// still returns a valid, non-nil *Reservation reflecting exactly what it
// managed to evict (already durably tagged+stopped) alongside a non-nil error
// — the reservation is real and partially in effect, so the caller MUST still
// call Release(jobID) to clean it up (or leave it for a crash-recovery
// reconcile, which will restore it exactly as if the process had died).
func (h *ServiceHandler) Reserve(ctx JobContext, jobID string, requiredVRAMBytes uint64) (*Reservation, error) {
	jobID = strings.TrimSpace(jobID)
	if jobID == "" {
		return nil, fmt.Errorf("reserve: job id is required")
	}

	res := &Reservation{JobID: jobID, RequiredVRAMBytes: requiredVRAMBytes}
	if requiredVRAMBytes == 0 {
		res.Reason = "no VRAM requirement declared; nothing reserved"
		return res, nil
	}

	st, err := h.collectNodeStatus()
	if err != nil {
		return nil, fmt.Errorf("reserve %s: could not collect node status: %w", jobID, err)
	}
	freeVRAM, ok := freeVRAMBytes(st.GPU)
	if !ok {
		return nil, fmt.Errorf("reserve %s: GPU free VRAM unknown on this node; refusing to grant an unverifiable reservation", jobID)
	}

	manifest, err := h.loadManifest()
	if err != nil {
		return nil, fmt.Errorf("reserve %s: failed to load manifest: %w", jobID, err)
	}
	pinned := manifest.pinnedSet()

	// No exclusion: unlike preemptForVRAM (which excludes the deploy target
	// itself), a reservation is not necessarily a manifest service — it is a
	// job-scoped VRAM hold. "" never matches a real service name.
	candidates := buildPreemptCandidates(st, "", pinned)
	plan := status.PlanPreemption(candidates, requiredVRAMBytes, freeVRAM)
	res.Reason = plan.Reason
	if !plan.Fits {
		return nil, fmt.Errorf("cannot reserve for job %s: %s", jobID, plan.Reason)
	}
	if len(plan.Stop) == 0 {
		return res, nil // already fits; no eviction needed
	}

	// Look up each candidate's CURRENT desired_status so Release can restore
	// that exact prior durable intent rather than unconditionally clearing it
	// (see EvictedPriorStatus's doc).
	priorStatus := make(map[string]string, len(manifest.Services))
	for _, s := range manifest.Services {
		priorStatus[s.Name] = s.DesiredStatus
	}

	ctx.Log("info", "     - [reserve %s] %s", jobID, plan.Reason)
	for _, name := range plan.Stop {
		// Durable FIRST (mirrors preemptForVRAM's "durable FIRST" pattern): tag
		// with the job id and the prior status, THEN stop. If the process dies
		// between the tag write and the stop call, the tag alone is enough for
		// ReconcileOrphanedReservations to notice and self-heal (a start on an
		// already-running service is a harmless no-op).
		if err := h.setEvictedMarkersInManifestFile(name, jobID, priorStatus[name]); err != nil {
			return res, fmt.Errorf("reserve %s: could not tag %s as evicted: %w", jobID, name, err)
		}
		if err := h.setDesiredStatusInManifestFile(name, "stopped"); err != nil {
			ctx.Log("warning", "     - [reserve %s] could not mark %s stopped: %v", jobID, name, err)
		}
		if err := h.stopByName(name); err != nil {
			return res, fmt.Errorf("reserve %s: failed to evict %s: %w", jobID, name, err)
		}
		res.Evicted = append(res.Evicted, name)
		ctx.Log("info", "     - [reserve %s] evicted %s to free VRAM", jobID, name)
	}
	return res, nil
}

// Release restores every service tagged evicted_by_job==jobID: restarts each
// one, then restores EvictedPriorStatus (rather than unconditionally clearing
// desired_status — see that field's doc), THEN clears the reservation tag —
// in that order, deliberately (see the inline comment at the call site for
// why the order is load-bearing, not cosmetic). A service is appended to the
// returned slice, and its tag cleared, only when EVERY step succeeds; a
// failure at any step (start, desired_status restore, or tag clear) KEEPS its
// tag and is folded into a non-nil returned error — Release never reports
// success while leaving inconsistent on-disk state. A still-tagged service is
// picked up again by a retried Release (or a later crash-recovery reconcile)
// — this is what makes Release safe to call more than once.
//
// Idempotent: when no service carries jobID's tag (nothing ever evicted, or a
// prior Release/reconcile already restored everything), Release is a no-op:
// it returns an empty slice and a nil error.
//
// Tag-scoped by construction: Release only ever acts on services whose
// CURRENT evicted_by_job tag equals jobID, so it can never restart a service
// an operator independently stopped for another reason — an explicit
// SERVICE_STOP/SERVICE_START clears the tag (see Execute()), which is exactly
// what makes that service invisible to every future Release call.
func (h *ServiceHandler) Release(ctx JobContext, jobID string) ([]string, error) {
	jobID = strings.TrimSpace(jobID)
	if jobID == "" {
		return nil, fmt.Errorf("release: job id is required")
	}

	manifest, err := h.loadManifest()
	if err != nil {
		return nil, fmt.Errorf("release %s: failed to load manifest: %w", jobID, err)
	}

	var restored []string
	var errs []string
	for _, s := range manifest.Services {
		if s.EvictedByJob != jobID {
			continue
		}
		name := s.Name
		if err := h.startByName(name); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", name, err))
			ctx.Log("warning", "     - [release %s] failed to restore %s: %v", jobID, name, err)
			continue // leave the tag in place so a retry/reconcile can pick it up
		}
		// Durable-FIRST, mirroring Reserve's "tag before stop" discipline in
		// reverse: restore the prior desired_status BEFORE clearing the
		// evicted_by_job/evicted_prior_status tag, and fold EITHER write's
		// failure into errs (never just logged) so Release cannot report
		// success while leaving inconsistent on-disk state.
		//
		// This order matters because the two writes are not symmetric: if
		// desired_status is restored first and the tag-clear then fails, the
		// tag is still present describing a fully-consistent, RECOVERABLE
		// state (desired_status already correct, service running) — a retried
		// Release or a crash-recovery reconcile revisits it and finishes the
		// job (both remaining writes are idempotent). The inverse order
		// (clear the tag first, as an earlier version of this function did)
		// risks the OPPOSITE, UNRECOVERABLE state on the same kind of
		// mid-write failure (disk full, I/O error, process killed here): tag
		// gone — so nothing (not a retry, not reconcile, which keys on the
		// tag) ever revisits this service again — while desired_status still
		// reads "stopped", silently stranding a running-but-marked-stopped
		// service off forever on the next boot (serviceStartDisabled skips
		// it). A service is only appended to restored once BOTH writes
		// succeed; otherwise it stays tagged and is deliberately excluded so
		// callers can rely on "not in restored ⇒ still tagged ⇒ will be
		// retried" as the invariant.
		if err := h.setDesiredStatusInManifestFile(name, s.EvictedPriorStatus); err != nil {
			errs = append(errs, fmt.Sprintf("%s: could not restore desired_status: %v", name, err))
			ctx.Log("warning", "     - [release %s] restored %s but could not restore prior desired_status: %v", jobID, name, err)
			continue
		}
		if err := h.setEvictedMarkersInManifestFile(name, "", ""); err != nil {
			errs = append(errs, fmt.Sprintf("%s: could not clear reservation tag: %v", name, err))
			ctx.Log("warning", "     - [release %s] restored %s but could not clear reservation tag: %v", jobID, name, err)
			continue
		}
		restored = append(restored, name)
		ctx.Log("info", "     - [release %s] restored %s", jobID, name)
	}
	if len(errs) > 0 {
		return restored, fmt.Errorf("release %s: failed to restore: %s", jobID, strings.Join(errs, "; "))
	}
	return restored, nil
}

// ReconcileOrphanedReservations restores every service still carrying a
// non-empty evicted_by_job tag at the moment it is called, grouped and
// restored per job id via Release.
//
// holdsWorkerLock is a REQUIRED, explicit assertion from the caller — not a
// convenience default — that this process currently holds
// internal/worklock's single-instance lock for this node. That is the ONLY
// thing that makes "any tag found here is orphaned" true: this ServiceHandler
// has created no reservations of its own yet (Reserve only ever runs from job
// dispatch, which starts after this call), so if exactly one worker can ever
// be live for a node, every tag found here was necessarily written by a
// PREVIOUS process invocation that exited (crashed, was killed, or was
// restarted) before calling Release for it — there is no live job anywhere
// else to wait for. The only correct call site today is cmd/work.go's
// runWork, immediately after a successful worklock.Acquire, before the job
// consume loop starts.
//
// IMPORTANT — this parameter guards only ONE of the two ways a second
// job-consuming process can exist for a node. worklock guards `citadel work`
// vs a SECOND `citadel work`: a genuinely live holder makes Acquire fail, so a
// second invocation either exits (attach/no-op) or refuses, and never reaches
// this function with holdsWorkerLock==true while another citadel-work process
// is also live. It does NOT cover the control-center TUI's OWN worker path:
// when no dedicated `citadel work` holds the lock (workerHeld==false in
// cmd/controlcenter.go), the control center runs its own consume loop off the
// SAME buildNodeJobHandlers handler set — WITHOUT ever calling
// worklock.Acquire. If a future caller (e.g. #8248) wires Reserve/Release into
// a handler reachable from that path, a control-center reservation and a
// LATER `citadel work` startup (which legitimately Acquires — nobody is
// holding it) collide exactly the way this parameter is meant to prevent: the
// new worker's reconcile would see the tag, conclude "orphaned", and
// destructively restart a service the still-live control-center job is
// actively using. holdsWorkerLock does not detect this case; it is a
// documented, currently-latent gap (nothing calls Reserve yet). A future
// caller reachable from the control-center path MUST NOT rely on this
// parameter alone — either make the control center's own worker path
// Acquire the lock too, or extend the marker with owner identity (pid +
// start time, classified the way worklock.decideStaleLock already classifies
// a stale lock's recorded PID) so reconcile can tell "orphaned" from "owned by
// a still-live sibling process" without assuming single-process exclusivity.
//
// Idempotent: a service already restored (tag cleared) is not visited again,
// so calling this twice restores nothing the second time — see Release.
func (h *ServiceHandler) ReconcileOrphanedReservations(ctx JobContext, holdsWorkerLock bool) ([]string, error) {
	if !holdsWorkerLock {
		return nil, fmt.Errorf("reconcile reservations: refusing to run without the node's single-instance worker lock (internal/worklock) -- see cmd/work.go's runWork for the only safe call site")
	}

	manifest, err := h.loadManifest()
	if err != nil {
		return nil, fmt.Errorf("reconcile reservations: failed to load manifest: %w", err)
	}
	jobIDs := map[string]bool{}
	for _, s := range manifest.Services {
		if s.EvictedByJob != "" {
			jobIDs[s.EvictedByJob] = true
		}
	}
	if len(jobIDs) == 0 {
		return nil, nil
	}
	ids := make([]string, 0, len(jobIDs))
	for id := range jobIDs {
		ids = append(ids, id)
	}
	sort.Strings(ids) // deterministic order for logs/tests

	var restored []string
	var errs []string
	for _, id := range ids {
		ctx.Log("info", "     - [reservation reconcile] found orphaned reservation %s from a previous run; restoring", id)
		r, err := h.Release(ctx, id)
		restored = append(restored, r...)
		if err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return restored, fmt.Errorf("reconcile reservations: %s", strings.Join(errs, "; "))
	}
	return restored, nil
}

// ActiveReservations returns the currently-active job-scoped GPU reservations
// (citadel-cli#832), derived from the evicted_by_job tags in citadel.yaml and
// grouped by job id. Pure read (one manifest parse), safe to call on every
// heartbeat tick — see cmd/work.go's reservationsFn / reservationsFrom for how
// this is surfaced on NodeStatus.GPUReservations.
func (h *ServiceHandler) ActiveReservations() ([]ReservationSummary, error) {
	manifest, err := h.loadManifest()
	if err != nil {
		return nil, err
	}
	byJob := map[string][]string{}
	var order []string
	for _, s := range manifest.Services {
		if s.EvictedByJob == "" {
			continue
		}
		if _, seen := byJob[s.EvictedByJob]; !seen {
			order = append(order, s.EvictedByJob)
		}
		byJob[s.EvictedByJob] = append(byJob[s.EvictedByJob], s.Name)
	}
	sort.Strings(order)
	out := make([]ReservationSummary, 0, len(order))
	for _, id := range order {
		names := byJob[id]
		sort.Strings(names)
		out = append(out, ReservationSummary{JobID: id, EvictedServices: names})
	}
	return out, nil
}
