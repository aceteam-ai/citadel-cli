package jobs

import (
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/status"
)

func TestExclusiveReservationJobIDDeterministic(t *testing.T) {
	if got := ExclusiveReservationJobID("bonsai"); got != "exclusive:bonsai" {
		t.Fatalf("ExclusiveReservationJobID(%q) = %q, want %q", "bonsai", got, "exclusive:bonsai")
	}
	// Same service name always yields the same id -- this is the contract
	// callers rely on to recompute Release's jobID independently, without an
	// in-memory handle (docs/design-model-exclusivity.md §2.4).
	if a, b := ExclusiveReservationJobID("bonsai"), ExclusiveReservationJobID("bonsai"); a != b {
		t.Fatalf("ExclusiveReservationJobID is not deterministic: %q vs %q", a, b)
	}
	if got := ExclusiveReservationJobID("  vllm  "); got != "exclusive:vllm" {
		t.Fatalf("ExclusiveReservationJobID did not trim whitespace: %q", got)
	}
}

// TestReserveExclusiveEvictsEveryNonPinnedCandidateRegardlessOfVRAM pins the
// core VRAM-fix behavior (docs/design-model-exclusivity.md §2.1 option (ii)):
// ReserveExclusive evicts EVERY non-pinned running service, including one
// reporting VRAMBytes==0 (a Reserve-with-a-precomputed-budget call would skip
// it -- "stopping it frees nothing" -- but exclusivity is unconditional), and
// never touches a pinned service even though it holds VRAM.
func TestReserveExclusiveEvictsEveryNonPinnedCandidateRegardlessOfVRAM(t *testing.T) {
	st := fullGPUStatus(
		svcInfo("svc-pinned", false, 20),   // pinned, busy, huge -- must never be touched
		svcInfo("svc-idle-small", true, 2), // non-pinned, has VRAM
		svcInfo("svc-busy", false, 10),     // non-pinned, busy, has VRAM
		svcInfo("svc-no-vram", true, 0),    // non-pinned, but reports ZERO VRAM
	)
	h, exec := newReservationTestHandlerWithManifest(t, `node:
  name: test-node
services:
  - name: svc-pinned
    type: docker
    compose_file: ./services/svc-pinned.yml
  - name: svc-idle-small
    type: docker
    compose_file: ./services/svc-idle-small.yml
  - name: svc-busy
    type: docker
    compose_file: ./services/svc-busy.yml
  - name: svc-no-vram
    type: docker
    compose_file: ./services/svc-no-vram.yml
pinned_services:
  - svc-pinned
`, st)

	jobID := ExclusiveReservationJobID("target")
	res, err := h.ReserveExclusive(testCtx(), jobID, "target")
	if err != nil {
		t.Fatalf("ReserveExclusive: %v", err)
	}
	if !res.Exclusive {
		t.Error("Reservation.Exclusive = false, want true")
	}
	want := sortedStrings([]string{"svc-idle-small", "svc-busy", "svc-no-vram"})
	if !equalStrings(sortedStrings(res.Evicted), want) {
		t.Errorf("Evicted = %v, want %v (every non-pinned running candidate, incl. VRAMBytes==0)", res.Evicted, want)
	}
	if containsString(res.Evicted, "svc-pinned") {
		t.Error("pinned service was evicted by ReserveExclusive")
	}
	if !equalStrings(sortedStrings(exec.stopped), want) {
		t.Errorf("stop() called with %v, want %v", exec.stopped, want)
	}
	if !strings.Contains(res.Reason, "svc-no-vram") {
		t.Errorf("Reason does not call out the no-measured-VRAM service: %q", res.Reason)
	}
	if !strings.Contains(res.Reason, "3 non-pinned") {
		t.Errorf("Reason does not report the evicted count: %q", res.Reason)
	}

	// Marker durability: every evicted service is tagged, matching Reserve's
	// contract exactly (so Release/ReconcileOrphanedReservations work
	// identically regardless of which primitive created the reservation).
	m := readManifestMap(t, h.ConfigDir)
	for _, name := range want {
		entry := manifestServiceEntry(t, m, name)
		if entry == nil || entry["evicted_by_job"] != jobID {
			t.Errorf("%s not durably tagged: %v", name, entry)
		}
		if entry["desired_status"] != "stopped" {
			t.Errorf("%s desired_status = %v, want stopped", name, entry["desired_status"])
		}
	}
	pinnedEntry := manifestServiceEntry(t, m, "svc-pinned")
	if _, present := pinnedEntry["evicted_by_job"]; present {
		t.Errorf("svc-pinned unexpectedly tagged: %v", pinnedEntry)
	}
}

// TestReserveExclusiveExcludesTargetService pins the exclude parameter: a
// service matching exclude (the caller's own deploy target, already running
// from a prior run) is never a candidate, mirroring preemptForVRAM's
// svc.Name exclusion.
func TestReserveExclusiveExcludesTargetService(t *testing.T) {
	st := fullGPUStatus(
		svcInfo("target", false, 8),
		svcInfo("svc-idle-small", true, 2),
	)
	h, exec := newReservationTestHandlerWithManifest(t, `node:
  name: test-node
services:
  - name: target
    type: docker
    compose_file: ./services/target.yml
  - name: svc-idle-small
    type: docker
    compose_file: ./services/svc-idle-small.yml
`, st)

	res, err := h.ReserveExclusive(testCtx(), ExclusiveReservationJobID("target"), "target")
	if err != nil {
		t.Fatalf("ReserveExclusive: %v", err)
	}
	if containsString(res.Evicted, "target") {
		t.Error("ReserveExclusive evicted the excluded (target) service")
	}
	if containsString(exec.stopped, "target") {
		t.Error("stop() was called for the excluded (target) service")
	}
	if !equalStrings(res.Evicted, []string{"svc-idle-small"}) {
		t.Errorf("Evicted = %v, want [svc-idle-small]", res.Evicted)
	}
}

// TestReserveExclusiveNothingToEvictIsANoOp pins the empty-candidate path: no
// non-pinned running services means an empty Evicted list, no manifest
// writes, and the resulting free VRAM is still reported.
func TestReserveExclusiveNothingToEvictIsANoOp(t *testing.T) {
	st := fullGPUStatus(svcInfo("svc-pinned", false, 20))
	h, exec := newReservationTestHandlerWithManifest(t, `node:
  name: test-node
services:
  - name: svc-pinned
    type: docker
    compose_file: ./services/svc-pinned.yml
pinned_services:
  - svc-pinned
`, st)

	res, err := h.ReserveExclusive(testCtx(), ExclusiveReservationJobID("target"), "target")
	if err != nil {
		t.Fatalf("ReserveExclusive: %v", err)
	}
	if len(res.Evicted) != 0 {
		t.Errorf("Evicted = %v, want none", res.Evicted)
	}
	if len(exec.stopped) != 0 {
		t.Errorf("stop() was called: %v", exec.stopped)
	}
	if !res.FreeVRAMKnown {
		t.Error("FreeVRAMKnown = false, want true (fullGPUStatus reports a GPU)")
	}
}

// TestReserveExclusiveNeverFailsOnUnsatisfiableBudget is the direct
// regression test for the bug ReserveExclusive exists to fix
// (docs/design-model-exclusivity.md §2.1): a scenario where a
// caller-precomputed "whole card minus margin" budget fed into Reserve would
// be REJECTED (VRAM is held by something PlanPreemption cannot see -- e.g.
// unmanaged/driver overhead -- so the sum of free + reclaimable candidate
// VRAM never reaches "the whole card"), but ReserveExclusive -- which makes
// no fit claim at all -- succeeds and evicts everything non-pinned anyway.
func TestReserveExclusiveNeverFailsOnUnsatisfiableBudget(t *testing.T) {
	st := &status.NodeStatus{
		GPU: []status.GPUMetrics{
			// 24GB card; only 1GB is reported free (23GB is "elsewhere" --
			// unmanaged process / driver overhead PlanPreemption cannot see).
			{Index: 0, Name: "Test GPU", MemoryTotalMB: 24576, MemoryUsedMB: 23552, MemoryFreeMB: 1024},
		},
		Services: []status.ServiceInfo{
			svcInfo("svc-idle-small", true, 2), // only 2GB is even attributable to a managed service
		},
	}
	h, _ := newReservationTestHandlerWithManifest(t, `node:
  name: test-node
services:
  - name: svc-idle-small
    type: docker
    compose_file: ./services/svc-idle-small.yml
`, st)

	// Sanity: the naive fix (a precomputed "whole card minus margin" budget)
	// really would be rejected here -- free (1GB) + reclaimable (2GB) = 3GB,
	// nowhere near "23GB reserved" (whole card minus a 1GB margin).
	naiveBudget := uint64(23) * 1024 * 1024 * 1024
	if _, err := h.Reserve(testCtx(), "naive-budget-job", naiveBudget); err == nil {
		t.Fatal("test setup invalid: Reserve with the naive precomputed budget unexpectedly succeeded")
	}

	// ReserveExclusive succeeds regardless -- it evicts everything non-pinned
	// and reports the ACTUAL resulting free VRAM rather than asking the
	// caller to have predicted it.
	res, err := h.ReserveExclusive(testCtx(), ExclusiveReservationJobID("target"), "target")
	if err != nil {
		t.Fatalf("ReserveExclusive failed on an unsatisfiable-for-Reserve scenario: %v", err)
	}
	if !equalStrings(res.Evicted, []string{"svc-idle-small"}) {
		t.Errorf("Evicted = %v, want [svc-idle-small]", res.Evicted)
	}
}

// TestReserveExclusiveUnknownVRAMIsNotAHardError pins the deliberate
// divergence from Reserve: ReserveExclusive makes no fit claim, so an
// unknown pre-eviction free-VRAM signal (no GPU reporting a memory total)
// does not refuse -- it still evicts every non-pinned candidate.
func TestReserveExclusiveUnknownVRAMIsNotAHardError(t *testing.T) {
	st := &status.NodeStatus{
		// No GPU entries at all -- freeVRAMBytes returns ok=false.
		Services: []status.ServiceInfo{svcInfo("svc-idle-small", true, 2)},
	}
	h, exec := newReservationTestHandlerWithManifest(t, `node:
  name: test-node
services:
  - name: svc-idle-small
    type: docker
    compose_file: ./services/svc-idle-small.yml
`, st)

	res, err := h.ReserveExclusive(testCtx(), ExclusiveReservationJobID("target"), "target")
	if err != nil {
		t.Fatalf("ReserveExclusive refused on unknown VRAM (should never, unlike Reserve): %v", err)
	}
	if !equalStrings(res.Evicted, []string{"svc-idle-small"}) {
		t.Errorf("Evicted = %v, want [svc-idle-small]", res.Evicted)
	}
	if !equalStrings(exec.stopped, []string{"svc-idle-small"}) {
		t.Errorf("stop() called with %v", exec.stopped)
	}
	if res.FreeVRAMKnown {
		t.Error("FreeVRAMKnown = true, want false (no GPU reporting a memory total)")
	}
}

// TestReserveExclusiveCollectsStatusTwice pins that ReserveExclusive
// re-collects node status AFTER eviction to report the ACTUAL resulting free
// VRAM (not the pre-eviction snapshot) -- a caller stubbing collectStatus in
// a test must expect two calls, and production code relying on
// FreeVRAMBytes must not assume it reflects the pre-eviction reading.
func TestReserveExclusiveCollectsStatusTwice(t *testing.T) {
	calls := 0
	st := fullGPUStatus(svcInfo("svc-idle-small", true, 2))
	h, _ := newReservationTestHandlerWithManifest(t, `node:
  name: test-node
services:
  - name: svc-idle-small
    type: docker
    compose_file: ./services/svc-idle-small.yml
`, st)
	h.collectStatus = func() (*status.NodeStatus, error) {
		calls++
		return st, nil
	}

	if _, err := h.ReserveExclusive(testCtx(), ExclusiveReservationJobID("target"), "target"); err != nil {
		t.Fatalf("ReserveExclusive: %v", err)
	}
	if calls != 2 {
		t.Errorf("collectStatus called %d times, want 2 (pre-eviction candidates + post-eviction free-VRAM report)", calls)
	}
}

// TestReserveExclusiveMidPlanStopFailureLeavesReleasableReservation mirrors
// Reserve's identical partial-failure contract: a stop failure partway
// through still returns a valid, non-nil *Reservation reflecting exactly
// what succeeded (already durably tagged), with a non-nil error -- and the
// failed service KEEPS its tag so a retried Release still finds it.
func TestReserveExclusiveMidPlanStopFailureLeavesReleasableReservation(t *testing.T) {
	st := fullGPUStatus(
		svcInfo("svc-a", true, 2),
		svcInfo("svc-b", true, 5),
	)
	h, exec := newReservationTestHandlerWithManifest(t, `node:
  name: test-node
services:
  - name: svc-a
    type: docker
    compose_file: ./services/svc-a.yml
  - name: svc-b
    type: docker
    compose_file: ./services/svc-b.yml
`, st)
	exec.failStop = map[string]bool{"svc-b": true}

	jobID := ExclusiveReservationJobID("target")
	res, err := h.ReserveExclusive(testCtx(), jobID, "target")
	if err == nil {
		t.Fatal("ReserveExclusive succeeded despite a simulated mid-plan stop failure")
	}
	if res == nil {
		t.Fatal("ReserveExclusive returned a nil *Reservation on a mid-plan failure")
	}
	if !equalStrings(res.Evicted, []string{"svc-a"}) {
		t.Errorf("res.Evicted = %v, want [svc-a] (only what actually succeeded)", res.Evicted)
	}

	m := readManifestMap(t, h.ConfigDir)
	failedEntry := manifestServiceEntry(t, m, "svc-b")
	if failedEntry == nil || failedEntry["evicted_by_job"] != jobID {
		t.Fatalf("svc-b (failed stop) not tagged: %v", failedEntry)
	}

	exec.failStop = nil
	restored, err := h.Release(testCtx(), jobID)
	if err != nil {
		t.Fatalf("Release after partial ReserveExclusive failure: %v", err)
	}
	if !equalStrings(sortedStrings(restored), sortedStrings([]string{"svc-a", "svc-b"})) {
		t.Errorf("Release restored %v, want [svc-a svc-b]", restored)
	}
}

// TestHasActiveReservation pins the release-tool lookup primitive
// (local_model_stop, #8249) end to end: false before any reservation exists,
// true while one is active, false again after Release.
func TestHasActiveReservation(t *testing.T) {
	st := fullGPUStatus(svcInfo("svc-idle-small", true, 2))
	h, _ := newReservationTestHandlerWithManifest(t, `node:
  name: test-node
services:
  - name: svc-idle-small
    type: docker
    compose_file: ./services/svc-idle-small.yml
`, st)

	jobID := ExclusiveReservationJobID("target")
	if has, err := h.HasActiveReservation(jobID); err != nil || has {
		t.Fatalf("HasActiveReservation before Reserve = (%v, %v), want (false, nil)", has, err)
	}

	if _, err := h.ReserveExclusive(testCtx(), jobID, "target"); err != nil {
		t.Fatalf("ReserveExclusive: %v", err)
	}
	if has, err := h.HasActiveReservation(jobID); err != nil || !has {
		t.Fatalf("HasActiveReservation after ReserveExclusive = (%v, %v), want (true, nil)", has, err)
	}
	// A DIFFERENT job id must not report active just because some
	// reservation exists.
	if has, err := h.HasActiveReservation("exclusive:someone-else"); err != nil || has {
		t.Fatalf("HasActiveReservation for an unrelated job id = (%v, %v), want (false, nil)", has, err)
	}

	if _, err := h.Release(testCtx(), jobID); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if has, err := h.HasActiveReservation(jobID); err != nil || has {
		t.Fatalf("HasActiveReservation after Release = (%v, %v), want (false, nil)", has, err)
	}
}

// TestStartServiceWithModelUnknownServiceErrorsWithoutTouchingDocker exercises
// StartServiceWithModel's manifest-lookup/materialize-fallback branch
// hermetically: a `type: native` service name absent from
// internal/services.NativeServices makes the actual start fail fast (an
// "unknown service" error) WITHOUT any exec.Command ever running -- the same
// technique reservation_test.go's nativeReservationTestManifestYAML already
// establishes for exercising real Execute()/serviceStart dispatch without a
// live docker daemon. This pins that model/vram parameters reach serviceStart
// (a real synthetic-native error, not a manifest-lookup error) rather than
// verifying a successful start, which would require a live engine.
func TestStartServiceWithModelUnknownServiceErrorsWithoutTouchingDocker(t *testing.T) {
	st := fullGPUStatus()
	h, _ := newReservationTestHandlerWithManifest(t, `node:
  name: test-node
services:
  - name: synthetic-native-engine
    type: native
    compose_file: ./services/synthetic-native-engine.yml
`, st)

	_, err := h.StartServiceWithModel(testCtx(), "synthetic-native-engine", "some-model", 0)
	if err == nil {
		t.Fatal("StartServiceWithModel succeeded against a synthetic native service with no real binary")
	}
	if !strings.Contains(err.Error(), "unknown service") {
		t.Errorf("error = %q, want it to come from services.StartNativeService's \"unknown service\" path (proves no real docker/network was touched)", err.Error())
	}
}

// TestStartServiceWithModelUnknownName pins the not-in-manifest,
// not-embedded error path (mirrors StartServiceByName's identical guard).
func TestStartServiceWithModelUnknownName(t *testing.T) {
	st := fullGPUStatus()
	h, _ := newReservationTestHandlerWithManifest(t, `node:
  name: test-node
services: []
`, st)
	if _, err := h.StartServiceWithModel(testCtx(), "totally-unknown-service", "", 0); err == nil {
		t.Fatal("StartServiceWithModel succeeded for a service not in the manifest and not embedded")
	}
}
