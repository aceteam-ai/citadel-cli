// internal/jobs/model_exclusivity.go
//
// Model exclusivity (aceteam#8248/#8249, docs/design-model-exclusivity.md):
// `citadel run --exclusive` and the local_model_deploy/local_run_exclusive/
// local_model_stop MCP tools (cmd/run_exclusive.go, cmd/mcp_local.go) are
// thin callers of the three primitives in this file, on top of #832's
// Reserve/Release/ReconcileOrphanedReservations (reservation.go).
//
// Ownership shape is deliberately §2.3(a) from the design doc: a standalone
// CLI process or the `citadel mcp` process calls Reserve/ReserveExclusive/
// Release DIRECTLY (mirroring how citadel#846's `citadel module
// stop|start|restart` calls liveModuleOps directly, with no worklock) rather
// than dispatching a job into a running `citadel work` worker -- the design
// doc found no existing local job-submission path into internal/worker.Runner
// and flagged building one as real, non-trivial plumbing, not a thin wrapper.
//
// Crash-safety consequence, stated plainly (do not read the paragraph below
// as "this gap does not apply here" -- it does): the evicted_by_job tag is
// durable (survives a SIGKILL of the reserving process), and
// cmd/work.go's runWork already calls ReconcileOrphanedReservations right
// after acquiring internal/worklock, before its consume loop starts -- so a
// CLI/MCP process that dies mid-exclusive-run does not strand evicted
// services forever; the NEXT `citadel work` boot on this node restores them.
// But reservation.go's own doc comment (ReconcileOrphanedReservations, see
// "IMPORTANT" paragraph) names EXACTLY this shape as the hazard it warns
// about: "any tag found here is orphaned" is only true when nothing else is
// still using the reservation, and neither this CLI/MCP process NOR the
// control-center TUI's consume loop holds worklock. A `citadel work` that
// boots WHILE an exclusive run/deploy from this file is still legitimately
// in progress will conclude the tag is orphaned and restart the evicted
// peers out from under it. This is a real, live race on any node that might
// also run `citadel work` (or the control-center) concurrently with a
// standalone exclusive run -- not merely a latent one closed off by this
// design. Closing it needs owner identity on the marker (pid + start time,
// mirroring worklock's own stale-lock classification) or making every
// job-consuming path acquire worklock; both are deferred, tracked follow-up
// work, not solved here. `citadel module reservations release <jobID>`
// (cmd/module_reservations.go) is the operator escape hatch if this race (or
// any other stuck reservation) needs a manual out.
package jobs

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	embeddedservices "github.com/aceteam-ai/citadel-cli/services"
)

// ExclusiveReservationJobID returns the deterministic reservation job id for
// an exclusive run/deploy of the given service
// (docs/design-model-exclusivity.md §2.4): a stable, recomputable string so a
// LATER, independently invoked release call (a different CLI invocation, or
// a separate MCP tool call -- §2.3(a)'s shape means no in-memory handle
// survives between them) can compute the SAME id a Reserve/ReserveExclusive
// call used, without needing to persist or pass around an opaque reservation
// handle.
func ExclusiveReservationJobID(serviceName string) string {
	return "exclusive:" + strings.TrimSpace(serviceName)
}

// ReserveExclusive evicts EVERY non-pinned RUNNING service (except exclude,
// typically the caller's own deploy target) unconditionally -- no VRAM
// fit-check arithmetic -- durably tagging each with evicted_by_job=jobID
// exactly like Reserve, so Release(jobID) (or a crash-recovery reconcile)
// restores them later.
//
// Why this exists instead of just calling Reserve with a big precomputed
// number (docs/design-model-exclusivity.md §2.1): a caller-computed "whole
// card minus a margin" budget fed into Reserve is unsatisfiable BY
// CONSTRUCTION whenever VRAM is held by something status.PlanPreemption
// never sees as a candidate at all -- an unmanaged process, driver/CUDA
// context overhead, anything RESOURCE_SNAPSHOT tracks as "unmanaged" rather
// than a citadel-managed service. Reserve would then refuse with an
// insufficient-VRAM error even though evicting everything non-pinned is
// exactly what was asked for and would free real VRAM. Skipping the
// fit-check arithmetic entirely sidesteps that: this can never fail to "fit"
// because it isn't fitting anything against a target, it is unconditionally
// evicting every non-pinned candidate and reporting what that actually freed.
//
// Every non-pinned running candidate is evicted regardless of its measured
// VRAMBytes -- unlike Reserve/status.PlanPreemption's minimal ordered prefix,
// which stops only as many services as needed to hit a budget and skips a
// candidate reporting VRAMBytes==0 because stopping it "frees nothing" for
// that budget. A genuinely exclusive ask means nothing else non-pinned is
// left running, whether or not its footprint measurement happened to read
// zero (which can also mean a nil Footprint from a failed sub-collection,
// not "definitely holds no VRAM") -- silently skipping it here would be the
// wrong direction of error for an explicit "give me the whole card" request.
//
// Unlike Reserve, an unknown pre-eviction free-VRAM signal is NOT a hard
// error: Reserve's hard-error contract exists because it is verifying a
// caller's fit CLAIM, and an unverifiable claim must not be silently
// granted. ReserveExclusive makes no fit claim at all -- there is nothing to
// verify -- so eviction proceeds regardless of whether free VRAM can be
// read; the resulting free VRAM is reported best-effort on the returned
// Reservation (FreeVRAMKnown is false when even the POST-eviction reading is
// unavailable, e.g. no GPU).
func (h *ServiceHandler) ReserveExclusive(ctx JobContext, jobID, exclude string) (*Reservation, error) {
	jobID = strings.TrimSpace(jobID)
	if jobID == "" {
		return nil, fmt.Errorf("reserve exclusive: job id is required")
	}

	res := &Reservation{JobID: jobID, Exclusive: true}

	st, err := h.collectNodeStatus()
	if err != nil {
		return nil, fmt.Errorf("reserve exclusive %s: could not collect node status: %w", jobID, err)
	}

	manifest, err := h.loadManifest()
	if err != nil {
		return nil, fmt.Errorf("reserve exclusive %s: failed to load manifest: %w", jobID, err)
	}
	pinned := manifest.pinnedSet()

	candidates := buildPreemptCandidates(st, exclude, pinned)
	var stop []string
	var pinnedHeld []string
	var noVRAMSignal []string
	for _, c := range candidates {
		if c.Pinned {
			if c.VRAMBytes > 0 {
				pinnedHeld = append(pinnedHeld, c.Name)
			}
			continue
		}
		stop = append(stop, c.Name)
		if c.VRAMBytes == 0 {
			noVRAMSignal = append(noVRAMSignal, c.Name)
		}
	}
	sort.Strings(stop) // deterministic order -- exclusivity stops all of them, so ordering carries no significance beyond reproducibility

	var reason strings.Builder
	fmt.Fprintf(&reason, "exclusive reservation: evicting %d non-pinned service(s)", len(stop))
	if len(noVRAMSignal) > 0 {
		sort.Strings(noVRAMSignal)
		fmt.Fprintf(&reason, " (%s reported no measured VRAM -- stopped anyway, exclusivity is unconditional)", strings.Join(noVRAMSignal, ", "))
	}
	if len(pinnedHeld) > 0 {
		sort.Strings(pinnedHeld)
		fmt.Fprintf(&reason, "; pinned service(s) left running and still holding VRAM: %s", strings.Join(pinnedHeld, ", "))
	}
	res.Reason = reason.String()

	if len(stop) == 0 {
		res.FreeVRAMBytes, res.FreeVRAMKnown = freeVRAMBytes(st.GPU)
		return res, nil
	}

	// Look up each candidate's CURRENT desired_status so Release can restore
	// that exact prior durable intent rather than unconditionally clearing it
	// -- mirrors Reserve's identical lookup (see EvictedPriorStatus's doc).
	priorStatus := make(map[string]string, len(manifest.Services))
	for _, s := range manifest.Services {
		priorStatus[s.Name] = s.DesiredStatus
	}

	ctx.Log("info", "     - [reserve %s] %s", jobID, res.Reason)
	for _, name := range stop {
		// Durable FIRST (mirrors Reserve's "tag before stop" discipline): tag
		// with the job id and the prior status, THEN stop. If the process
		// dies between the tag write and the stop call, the tag alone is
		// enough for ReconcileOrphanedReservations to notice and self-heal.
		if err := h.setEvictedMarkersInManifestFile(name, jobID, priorStatus[name]); err != nil {
			return res, fmt.Errorf("reserve exclusive %s: could not tag %s as evicted: %w", jobID, name, err)
		}
		if err := h.setDesiredStatusInManifestFile(name, "stopped"); err != nil {
			ctx.Log("warning", "     - [reserve %s] could not mark %s stopped: %v", jobID, name, err)
		}
		if err := h.stopByName(name); err != nil {
			return res, fmt.Errorf("reserve exclusive %s: failed to evict %s: %w", jobID, name, err)
		}
		res.Evicted = append(res.Evicted, name)
		ctx.Log("info", "     - [reserve %s] evicted %s for exclusive access", jobID, name)
	}

	// Report ground truth resulting free VRAM (best-effort -- a re-collection
	// failure here does not undo the eviction, which already durably
	// succeeded; the caller still gets Evicted + a nil error either way).
	if st2, err2 := h.collectNodeStatus(); err2 == nil {
		res.FreeVRAMBytes, res.FreeVRAMKnown = freeVRAMBytes(st2.GPU)
	}
	return res, nil
}

// HasActiveReservation reports whether jobID currently holds an active
// reservation (any service still tagged evicted_by_job==jobID). Pure read
// (one manifest parse via ActiveReservations), used by the local MCP release
// tool (local_model_stop, cmd/mcp_local.go) to decide whether stopping the
// model it served should ALSO Release an exclusive reservation, or is just
// an ordinary stop with nothing to restore.
func (h *ServiceHandler) HasActiveReservation(jobID string) (bool, error) {
	summaries, err := h.ActiveReservations()
	if err != nil {
		return false, err
	}
	for _, s := range summaries {
		if s.JobID == jobID {
			return true, nil
		}
	}
	return false, nil
}

// StartServiceWithModel starts a manifest-declared or embedded managed
// service by name, serving the given model with an optional VRAM budget
// (#577's ordinary, non-restoring preemption -- see preemptForVRAM). It is
// StartServiceByName generalized with the two SERVICE_START-job parameters
// #8249's local_model_deploy (and the start-half of local_run_exclusive, run
// via #8248's CLI/MCP callers in this file's package doc) need but
// StartServiceByName hardcodes away (model="", requiredVRAMBytes=0).
//
// requiredVRAMBytes==0 disables preemption entirely -- the fail-safe on an
// absent budget both #8248/#8249 callers rely on AFTER they have already
// evicted peers themselves via Reserve/ReserveExclusive (passing 0 here in
// that case is deliberate, not an oversight: preemptForVRAM would otherwise
// redundantly re-run the same decision against an already-cleared node).
func (h *ServiceHandler) StartServiceWithModel(ctx JobContext, name, model string, requiredVRAMBytes uint64) ([]byte, error) {
	manifest, err := h.loadManifest()
	if err != nil {
		return nil, fmt.Errorf("failed to load manifest: %w", err)
	}
	svc, ok := h.findService(manifest, name)
	if !ok {
		if _, embedded := embeddedservices.ServiceMap[name]; !embedded {
			return nil, fmt.Errorf("service %q not found in manifest", name)
		}
		svc, err = h.materializeEmbeddedService(name)
		if err != nil {
			return nil, fmt.Errorf("failed to reconcile embedded service %q: %w", name, err)
		}
	}
	res, err := h.serviceStart(ctx, svc, model, requiredVRAMBytes, 0, trustRemoteCodeUnspecified)
	if err != nil {
		return res, err
	}
	// serviceStart reports a failed start by embedding serviceResult.Error in
	// the returned JSON with a nil Go error (mirrors StartServiceByName's
	// identical unwrap, service_handler.go) -- surface it as a real error so
	// callers (run --exclusive, local_model_deploy/local_run_exclusive) can
	// tell success from failure without parsing JSON themselves.
	var parsed serviceResult
	if json.Unmarshal(res, &parsed) == nil && parsed.Error != "" {
		return res, fmt.Errorf("%s", parsed.Error)
	}
	return res, nil
}
