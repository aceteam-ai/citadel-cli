package jobs

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/status"
)

// reservationTestManifestYAML declares four docker services with distinct
// idle/VRAM/pin profiles so Reserve's eviction ordering (idle-first, then
// largest-VRAM-first — #577's PlanPreemption, unchanged) is exercised through
// the Reserve API, not just the pure PlanPreemption unit tests.
const reservationTestManifestYAML = `node:
  name: test-node
  tags:
    - gpu
services:
  - name: svc-pinned
    type: docker
    compose_file: ./services/svc-pinned.yml
  - name: svc-idle-small
    type: docker
    compose_file: ./services/svc-idle-small.yml
  - name: svc-idle-large
    type: docker
    compose_file: ./services/svc-idle-large.yml
  - name: svc-busy
    type: docker
    compose_file: ./services/svc-busy.yml
pinned_services:
  - svc-pinned
`

// fakeReservationExec is a stand-in for the docker-shelling start/stop calls,
// so Reserve/Release are exercised without a live docker daemon. It also lets
// tests assert exactly which services were told to stop/start, and in what
// order.
type fakeReservationExec struct {
	mu      sync.Mutex
	stopped []string
	started []string
	// failStop/failStart, when set, make the named call fail once (used to
	// pin the partial-failure contract).
	failStop  map[string]bool
	failStart map[string]bool
}

func (f *fakeReservationExec) stop(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, name)
	if f.failStop[name] {
		return fmt.Errorf("simulated stop failure for %s", name)
	}
	return nil
}

func (f *fakeReservationExec) start(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, name)
	if f.failStart[name] {
		return fmt.Errorf("simulated start failure for %s", name)
	}
	return nil
}

// newReservationTestHandler builds a ServiceHandler wired with a synthetic
// NodeStatus (via collectStatus) and a fake stop/start executor, rooted at a
// temp dir carrying reservationTestManifestYAML.
func newReservationTestHandler(t *testing.T, st *status.NodeStatus) (*ServiceHandler, *fakeReservationExec) {
	t.Helper()
	return newReservationTestHandlerWithManifest(t, reservationTestManifestYAML, st)
}

// newReservationTestHandlerWithManifest is newReservationTestHandler
// generalized over the manifest content, so a test that needs a different
// fixture -- e.g. avoiding services.ServiceMap/type:docker entirely, see
// nativeReservationTestManifestYAML below -- can still share the
// collectStatus/stopServiceFn/startServiceFn seam wiring.
func newReservationTestHandlerWithManifest(t *testing.T, manifestYAML string, st *status.NodeStatus) (*ServiceHandler, *fakeReservationExec) {
	t.Helper()
	dir := t.TempDir()
	writeManifestFile(t, dir, manifestYAML)
	exec := &fakeReservationExec{}
	h := NewServiceHandler(dir)
	h.collectStatus = func() (*status.NodeStatus, error) { return st, nil }
	h.stopServiceFn = exec.stop
	h.startServiceFn = exec.start
	return h, exec
}

// nativeReservationTestManifestYAML mirrors reservationTestManifestYAML but
// declares every service `type: native` instead of `type: docker`. Reserve
// and Release never consult this field (they evict/restore exclusively
// through the injected stopServiceFn/startServiceFn seams above), but
// Execute()'s SERVICE_START/SERVICE_STOP dispatch does, via resolveKind --
// and TestExplicitServiceStopClearsReservationTag /
// TestExplicitServiceStartClearsReservationTag deliberately drive the REAL
// Execute() path (not the seams) to prove its tag-clearing side effect
// end-to-end. `type: docker` there would route serviceStart/serviceStop into
// branches that shell out to a real `docker` binary (`docker inspect`, the
// preflight's `docker info`, `docker compose up|down`) -- harmless against a
// nonexistent container/compose file today, but a real subprocess spawned
// against whatever Docker daemon happens to be reachable from the test
// machine, which on a citadel dev/CI host can be a LIVE production node.
// `type: native` routes those branches into services.IsNativeServiceServing /
// StartNativeService / IsNativeServiceRunning / StopNativeService instead,
// every one of which does a services.NativeServices[name] map lookup FIRST
// and returns immediately (no exec.Command anywhere) when the name is
// unknown -- true for every synthetic service name in this file, since none
// of them are "ollama"/"llamacpp"/"vllm". This keeps the assertion (an
// explicit SERVICE_START/SERVICE_STOP clears the evicted_by_job tag)
// hermetic without weakening what it proves.
const nativeReservationTestManifestYAML = `node:
  name: test-node
services:
  - name: svc-pinned
    type: native
    compose_file: ./services/svc-pinned.yml
  - name: svc-idle-small
    type: native
    compose_file: ./services/svc-idle-small.yml
pinned_services:
  - svc-pinned
`

func writeManifestFile(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "citadel.yaml"), []byte(content), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

// svcInfo builds a status.ServiceInfo running-service candidate with the given
// idle/VRAM profile for the synthetic collectStatus fixtures.
func svcInfo(name string, idle bool, vramGB float64) status.ServiceInfo {
	cpu := 0.0
	if !idle {
		cpu = 50.0
	}
	return status.ServiceInfo{
		Name:   name,
		Status: status.ServiceStatusRunning,
		Footprint: &status.ServiceFootprint{
			CPUPercent: cpu,
			VRAMBytes:  uint64(vramGB * 1024 * 1024 * 1024),
		},
	}
}

// fullGPUStatus is a NodeStatus with a single GPU reporting zero free VRAM and
// the four reservationTestManifestYAML services running with the profile
// under test.
func fullGPUStatus(services ...status.ServiceInfo) *status.NodeStatus {
	return &status.NodeStatus{
		GPU: []status.GPUMetrics{
			// MemoryFreeMB deliberately 0 (unset, so freeVRAMBytes falls back to
			// the derived total-used) with MemoryUsedMB==MemoryTotalMB: a
			// genuinely full GPU with zero free VRAM, forcing Reserve to actually
			// evict to fit any non-zero budget.
			{Index: 0, Name: "Test GPU", MemoryTotalMB: 24576, MemoryUsedMB: 24576, MemoryFreeMB: 0},
		},
		Services: services,
	}
}

const testJobID = "job-1"

func testCtx() JobContext {
	return JobContext{LogFn: func(string, string) {}}
}

// TestReserveEvictsIdleFirstLargestVRAM pins Reserve's eviction ORDER (not
// just the set — PlanPreemption's own tests already cover the set) through
// the Reserve API: idle before busy, largest-VRAM-first within idle, pinned
// never touched, and every evicted service tagged with the job id.
func TestReserveEvictsIdleFirstLargestVRAM(t *testing.T) {
	st := fullGPUStatus(
		svcInfo("svc-pinned", false, 20),   // pinned, busy, huge -- must never be touched
		svcInfo("svc-idle-small", true, 2), // idle, 2GB
		svcInfo("svc-idle-large", true, 5), // idle, 5GB -- should be evicted BEFORE svc-idle-small
		svcInfo("svc-busy", false, 10),     // busy, 10GB -- should be spared (not needed to fit)
	)
	h, exec := newReservationTestHandler(t, st)

	// Free=0, required=6GB: idle-first + largest-first order is
	// [svc-idle-large(5GB), svc-idle-small(2GB), svc-busy(10GB)]; the minimal
	// prefix freeing >=6GB is [svc-idle-large, svc-idle-small] (5+2=7 >= 6).
	res, err := h.Reserve(testCtx(), testJobID, 6*1024*1024*1024)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	wantOrder := []string{"svc-idle-large", "svc-idle-small"}
	if !equalStrings(res.Evicted, wantOrder) {
		t.Errorf("Reservation.Evicted = %v, want %v (idle-first, largest-VRAM-first)", res.Evicted, wantOrder)
	}
	if !equalStrings(exec.stopped, wantOrder) {
		t.Errorf("stop() called with %v, want %v in that exact order", exec.stopped, wantOrder)
	}
	if containsString(exec.stopped, "svc-pinned") {
		t.Error("pinned service svc-pinned was stopped")
	}
	if containsString(exec.stopped, "svc-busy") {
		t.Error("svc-busy was stopped even though evicting the idle services already freed enough VRAM")
	}

	// Every evicted service must be durably tagged with the reserving job id.
	m := readManifestMap(t, h.ConfigDir)
	for _, name := range wantOrder {
		entry := manifestServiceEntry(t, m, name)
		if entry == nil {
			t.Fatalf("%s entry missing after Reserve", name)
		}
		if entry["evicted_by_job"] != testJobID {
			t.Errorf("%s evicted_by_job = %v, want %s", name, entry["evicted_by_job"], testJobID)
		}
		if entry["desired_status"] != "stopped" {
			t.Errorf("%s desired_status = %v, want stopped", name, entry["desired_status"])
		}
	}
	// The untouched services must carry neither marker.
	for _, name := range []string{"svc-pinned", "svc-busy"} {
		entry := manifestServiceEntry(t, m, name)
		if entry == nil {
			t.Fatalf("%s entry missing", name)
		}
		if _, present := entry["evicted_by_job"]; present {
			t.Errorf("%s unexpectedly tagged evicted_by_job: %v", name, entry["evicted_by_job"])
		}
	}
}

// TestReservePinnedNeverEvictedRejectsWhenCannotFit pins the #577 pinning
// contract through Reserve: a pinned service is never a candidate, and when
// the budget cannot be met without one, Reserve rejects the reservation
// (error) and leaves the manifest untouched -- no partial eviction of the
// non-pinned services either, since PlanPreemption itself decides !Fits before
// Reserve's eviction loop ever runs.
func TestReservePinnedNeverEvictedRejectsWhenCannotFit(t *testing.T) {
	st := fullGPUStatus(
		svcInfo("svc-pinned", false, 20), // pinned, holds nearly all the VRAM
		svcInfo("svc-idle-small", true, 2),
	)
	h, exec := newReservationTestHandler(t, st)

	// Only 2GB is reclaimable from non-pinned candidates; asking for 10GB can
	// only be met by evicting the pinned service.
	res, err := h.Reserve(testCtx(), testJobID, 10*1024*1024*1024)
	if err == nil {
		t.Fatalf("Reserve succeeded (res=%+v), want a rejection naming the pinned holder", res)
	}
	if !strings.Contains(err.Error(), "svc-pinned") {
		t.Errorf("error %q does not name the blocking pinned service", err.Error())
	}
	if len(exec.stopped) != 0 {
		t.Errorf("stop() was called (%v) for a rejected reservation", exec.stopped)
	}
	m := readManifestMap(t, h.ConfigDir)
	for _, name := range []string{"svc-pinned", "svc-idle-small"} {
		entry := manifestServiceEntry(t, m, name)
		if entry == nil {
			t.Fatalf("%s entry missing", name)
		}
		if _, present := entry["evicted_by_job"]; present {
			t.Errorf("%s unexpectedly tagged after a rejected reservation", name)
		}
	}
}

// TestReserveMidPlanStopFailureLeavesReleasableReservation pins the chosen
// Reserve partial-failure contract (see Reserve's doc): when a stop call
// fails partway through the plan, Reserve still returns a valid, non-nil
// *Reservation reflecting exactly what it managed to evict (already durably
// tagged+stopped) alongside a non-nil error -- and the caller's obligation to
// then call Release(jobID) actually cleans it up, not just "should" on paper.
func TestReserveMidPlanStopFailureLeavesReleasableReservation(t *testing.T) {
	st := fullGPUStatus(
		svcInfo("svc-pinned", false, 20),
		svcInfo("svc-idle-small", true, 2),
		svcInfo("svc-idle-large", true, 5),
	)
	h, exec := newReservationTestHandler(t, st)
	// Order for a 6GB requirement is [svc-idle-large(5GB), svc-idle-small(2GB)].
	// Fail the SECOND stop in that plan.
	exec.failStop = map[string]bool{"svc-idle-small": true}

	res, err := h.Reserve(testCtx(), testJobID, 6*1024*1024*1024)
	if err == nil {
		t.Fatal("Reserve succeeded despite a simulated mid-plan stop failure")
	}
	if res == nil {
		t.Fatal("Reserve returned a nil *Reservation on a mid-plan failure; the doc promises a valid, releasable one")
	}
	if !equalStrings(res.Evicted, []string{"svc-idle-large"}) {
		t.Errorf("res.Evicted = %v, want [svc-idle-large] (only what actually succeeded)", res.Evicted)
	}

	// The service whose stop FAILED must still be tagged (stopByName was
	// called -- and therefore the durable tag+desired_status were written --
	// even though the actual stop errored and it never made it into
	// res.Evicted). This is what makes it recoverable at all.
	m := readManifestMap(t, h.ConfigDir)
	failedEntry := manifestServiceEntry(t, m, "svc-idle-small")
	if failedEntry == nil || failedEntry["evicted_by_job"] != testJobID {
		t.Fatalf("svc-idle-small (failed stop) not tagged: %v", failedEntry)
	}

	// The caller's documented obligation: Release(jobID) on the error path
	// must still find and clean up everything Reserve touched, including the
	// failed one.
	exec.failStop = nil
	exec.failStart = nil
	restored, err := h.Release(testCtx(), testJobID)
	if err != nil {
		t.Fatalf("Release after a partial Reserve failure: %v", err)
	}
	wantRestored := sortedStrings([]string{"svc-idle-large", "svc-idle-small"})
	if !equalStrings(sortedStrings(restored), wantRestored) {
		t.Errorf("Release restored %v, want %v", restored, wantRestored)
	}
	m = readManifestMap(t, h.ConfigDir)
	for _, name := range wantRestored {
		entry := manifestServiceEntry(t, m, name)
		if entry != nil {
			if _, present := entry["evicted_by_job"]; present {
				t.Errorf("%s still tagged after cleanup Release: %v", name, entry["evicted_by_job"])
			}
		}
	}
}

// TestReleaseRestoresExactlyEvictedSetIdempotent pins Release's contract:
// restart exactly the tagged services, clear both markers on success, and be
// a no-op on a second call.
func TestReleaseRestoresExactlyEvictedSetIdempotent(t *testing.T) {
	st := fullGPUStatus(
		svcInfo("svc-pinned", false, 20),
		svcInfo("svc-idle-small", true, 2),
		svcInfo("svc-idle-large", true, 5),
		svcInfo("svc-busy", false, 10),
	)
	h, exec := newReservationTestHandler(t, st)

	if _, err := h.Reserve(testCtx(), testJobID, 6*1024*1024*1024); err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	restored, err := h.Release(testCtx(), testJobID)
	if err != nil {
		t.Fatalf("Release: %v", err)
	}
	wantRestored := []string{"svc-idle-large", "svc-idle-small"}
	sort.Strings(restored)
	sortedWant := append([]string(nil), wantRestored...)
	sort.Strings(sortedWant)
	if !equalStrings(restored, sortedWant) {
		t.Errorf("Release restored %v, want %v", restored, sortedWant)
	}
	if !equalStrings(sortedStrings(exec.started), sortedWant) {
		t.Errorf("start() called for %v, want %v", exec.started, sortedWant)
	}

	m := readManifestMap(t, h.ConfigDir)
	for _, name := range wantRestored {
		entry := manifestServiceEntry(t, m, name)
		if entry == nil {
			t.Fatalf("%s entry missing after Release", name)
		}
		if _, present := entry["evicted_by_job"]; present {
			t.Errorf("%s still tagged evicted_by_job after Release: %v", name, entry["evicted_by_job"])
		}
		if _, present := entry["desired_status"]; present {
			t.Errorf("%s still has desired_status after Release: %v", name, entry["desired_status"])
		}
	}

	// Idempotent: nothing left to restore.
	restored2, err := h.Release(testCtx(), testJobID)
	if err != nil {
		t.Fatalf("second Release: %v", err)
	}
	if len(restored2) != 0 {
		t.Errorf("second Release restored %v, want none (idempotent no-op)", restored2)
	}
}

// TestReleasePartialFailureLeavesFailedServiceTagged pins the chosen
// partial-failure contract: a service whose restart fails KEEPS its tag (so a
// retry or crash-recovery reconcile can pick it up again), while a
// successfully-restored sibling in the same reservation has its tag cleared.
func TestReleasePartialFailureLeavesFailedServiceTagged(t *testing.T) {
	st := fullGPUStatus(
		svcInfo("svc-pinned", false, 20),
		svcInfo("svc-idle-small", true, 2),
		svcInfo("svc-idle-large", true, 5),
	)
	h, exec := newReservationTestHandler(t, st)
	if _, err := h.Reserve(testCtx(), testJobID, 6*1024*1024*1024); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	exec.failStart = map[string]bool{"svc-idle-large": true}

	restored, err := h.Release(testCtx(), testJobID)
	if err == nil {
		t.Fatal("Release succeeded despite a simulated start failure")
	}
	if containsString(restored, "svc-idle-large") {
		t.Errorf("svc-idle-large reported restored despite a failed start: %v", restored)
	}
	if !containsString(restored, "svc-idle-small") {
		t.Errorf("svc-idle-small (which started fine) was not restored: %v", restored)
	}

	m := readManifestMap(t, h.ConfigDir)
	failedEntry := manifestServiceEntry(t, m, "svc-idle-large")
	if failedEntry == nil || failedEntry["evicted_by_job"] != testJobID {
		t.Errorf("svc-idle-large lost its reservation tag despite a failed restore: %v", failedEntry)
	}
	okEntry := manifestServiceEntry(t, m, "svc-idle-small")
	if okEntry != nil {
		if _, present := okEntry["evicted_by_job"]; present {
			t.Errorf("svc-idle-small still tagged after a successful restore: %v", okEntry)
		}
	}

	// A retried Release only touches what's still tagged.
	exec.failStart = nil
	exec.started = nil
	restored2, err := h.Release(testCtx(), testJobID)
	if err != nil {
		t.Fatalf("retried Release: %v", err)
	}
	if !equalStrings(restored2, []string{"svc-idle-large"}) {
		t.Errorf("retried Release restored %v, want [svc-idle-large]", restored2)
	}
}

// writeFailAfterN is an os.WriteFile stand-in that, once armed, fails the Nth
// write it observes and succeeds (delegating to a real write) on every other
// call. Used to simulate a manifest write failing partway through Release's
// two-write sequence (restore desired_status, then clear the reservation tag)
// without needing a real disk-full/IO-error condition.
type writeFailAfterN struct {
	mu     sync.Mutex
	armed  bool
	n      int
	count  int
	writes [][]byte
}

func (w *writeFailAfterN) write(path string, data []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes = append(w.writes, append([]byte(nil), data...))
	if w.armed {
		w.count++
		if w.count == w.n {
			return fmt.Errorf("simulated manifest write failure")
		}
	}
	return os.WriteFile(path, data, 0o600)
}

// TestReleaseSecondManifestWriteFailureIsRecoverable is the regression test
// for the stranding bug a review caught: Release used to clear the
// evicted_by_job tag BEFORE restoring desired_status, so a failure on that
// SECOND write (the desired_status restore) left the tag gone but
// desired_status still "stopped" on a service that was, by then, actually
// running -- serviceStartDisabled would then skip it forever on every future
// boot, and nothing (not a retry, not reconcile, both keyed on the tag) could
// ever find it again. Release now writes desired_status FIRST, then clears
// the tag, so the analogous failure (the tag-clear, now the SECOND write)
// leaves a state a retry/reconcile CAN still repair: tag present, prior
// desired_status already restored, service running.
func TestReleaseSecondManifestWriteFailureIsRecoverable(t *testing.T) {
	st := fullGPUStatus(
		svcInfo("svc-pinned", false, 20),
		svcInfo("svc-idle-small", true, 6),
	)
	h, exec := newReservationTestHandler(t, st)
	if _, err := h.Reserve(testCtx(), testJobID, 5*1024*1024*1024); err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	// Arm AFTER Reserve's own writes, so only Release's two writes for
	// svc-idle-small are counted: 1st = desired_status restore (must
	// succeed), 2nd = evicted_by_job/evicted_prior_status clear (fails here).
	writer := &writeFailAfterN{armed: true, n: 2}
	h.writeManifestFn = writer.write

	restored, err := h.Release(testCtx(), testJobID)
	if err == nil {
		t.Fatal("Release succeeded despite a simulated failure on its second manifest write -- false success is exactly the bug this test guards against")
	}
	if containsString(restored, "svc-idle-small") {
		t.Errorf("svc-idle-small reported restored despite the tag-clear write failing: %v", restored)
	}

	// The on-disk state must be RECOVERABLE: still tagged (so a retry or
	// reconcile finds it again), with desired_status already correctly
	// restored -- never the stranding combination (tag gone, desired_status
	// still "stopped", service actually running).
	m := readManifestMap(t, h.ConfigDir)
	entry := manifestServiceEntry(t, m, "svc-idle-small")
	if entry == nil {
		t.Fatal("svc-idle-small entry missing")
	}
	if entry["evicted_by_job"] != testJobID {
		t.Fatalf("STRANDING BUG: evicted_by_job was cleared despite the write that should have failed: %v", entry)
	}
	if _, stillMarkedStopped := entry["desired_status"]; stillMarkedStopped {
		t.Errorf("desired_status was NOT restored before the failing write: %v (this is the ordering the fix requires)", entry)
	}

	// A retry (mirroring what ReconcileOrphanedReservations would do on the
	// next worker start) must find the still-tagged service and finish the job.
	writer.armed = false
	exec.started = nil
	restored2, err := h.Release(testCtx(), testJobID)
	if err != nil {
		t.Fatalf("retried Release: %v", err)
	}
	if !equalStrings(restored2, []string{"svc-idle-small"}) {
		t.Errorf("retried Release restored %v, want [svc-idle-small]", restored2)
	}
	m = readManifestMap(t, h.ConfigDir)
	entry = manifestServiceEntry(t, m, "svc-idle-small")
	if entry != nil {
		if _, present := entry["evicted_by_job"]; present {
			t.Errorf("svc-idle-small still tagged after the successful retry: %v", entry)
		}
	}
}

// TestReconcileOrphanedReservationsRestoresCrashedWorkerState is the
// crash-safety test: a manifest carrying a durable evicted_by_job marker with
// no corresponding live Reservation (simulating a worker that evicted a
// service, then crashed before releasing it) is fully restored by
// ReconcileOrphanedReservations, exactly once.
func TestReconcileOrphanedReservationsRestoresCrashedWorkerState(t *testing.T) {
	const crashedManifest = `node:
  name: test-node
services:
  - name: svc-orphaned
    type: docker
    compose_file: ./services/svc-orphaned.yml
    desired_status: stopped
    evicted_by_job: dead-job-42
  - name: svc-unrelated-stopped
    type: docker
    compose_file: ./services/svc-unrelated-stopped.yml
    desired_status: stopped
`
	dir := t.TempDir()
	writeManifestFile(t, dir, crashedManifest)
	exec := &fakeReservationExec{}
	h := NewServiceHandler(dir)
	h.startServiceFn = exec.start
	h.stopServiceFn = exec.stop

	restored, err := h.ReconcileOrphanedReservations(testCtx(), true)
	if err != nil {
		t.Fatalf("ReconcileOrphanedReservations: %v", err)
	}
	if !equalStrings(restored, []string{"svc-orphaned"}) {
		t.Errorf("restored = %v, want [svc-orphaned]", restored)
	}
	if !equalStrings(exec.started, []string{"svc-orphaned"}) {
		t.Errorf("start() called for %v, want [svc-orphaned] only", exec.started)
	}

	m := readManifestMap(t, dir)
	orphaned := manifestServiceEntry(t, m, "svc-orphaned")
	if orphaned == nil {
		t.Fatal("svc-orphaned entry missing")
	}
	if _, present := orphaned["evicted_by_job"]; present {
		t.Errorf("svc-orphaned still tagged after reconcile: %v", orphaned["evicted_by_job"])
	}
	if _, present := orphaned["desired_status"]; present {
		t.Errorf("svc-orphaned still has desired_status after reconcile: %v", orphaned["desired_status"])
	}

	// The unrelated stopped service (an operator stop, no reservation tag)
	// must NEVER be touched by reconcile.
	unrelated := manifestServiceEntry(t, m, "svc-unrelated-stopped")
	if unrelated == nil || unrelated["desired_status"] != "stopped" {
		t.Errorf("svc-unrelated-stopped was disturbed by reconcile: %v", unrelated)
	}
	if containsString(exec.started, "svc-unrelated-stopped") {
		t.Error("reconcile started svc-unrelated-stopped, an operator-stopped service with no reservation tag")
	}

	// Idempotent: nothing left to restore on a second call.
	restored2, err := h.ReconcileOrphanedReservations(testCtx(), true)
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if len(restored2) != 0 {
		t.Errorf("second reconcile restored %v, want none", restored2)
	}
}

// TestReconcileOrphanedReservationsRefusesWithoutWorkerLock pins the required
// precondition (#832 STOP-condition guardrail): reconcile must refuse outright
// when the caller has not asserted it holds the node's single-instance
// worker lock, rather than silently trusting an unstated call-order
// assumption. See ReconcileOrphanedReservations' doc for why.
func TestReconcileOrphanedReservationsRefusesWithoutWorkerLock(t *testing.T) {
	const manifest = `node:
  name: test-node
services:
  - name: svc-orphaned
    type: docker
    compose_file: ./services/svc-orphaned.yml
    desired_status: stopped
    evicted_by_job: dead-job-42
`
	dir := t.TempDir()
	writeManifestFile(t, dir, manifest)
	exec := &fakeReservationExec{}
	h := NewServiceHandler(dir)
	h.startServiceFn = exec.start
	h.stopServiceFn = exec.stop

	if _, err := h.ReconcileOrphanedReservations(testCtx(), false); err == nil {
		t.Fatal("expected an error when holdsWorkerLock is false")
	}
	if len(exec.started) != 0 {
		t.Errorf("start() was called (%v) despite the missing-lock refusal", exec.started)
	}
	m := readManifestMap(t, dir)
	entry := manifestServiceEntry(t, m, "svc-orphaned")
	if entry == nil || entry["evicted_by_job"] != "dead-job-42" {
		t.Errorf("manifest was modified despite the missing-lock refusal: %v", entry)
	}
}

// TestExplicitServiceStopClearsReservationTag pins the operator-override
// contract: an explicit SERVICE_STOP job clears any reservation tag a service
// carries, so a later Release for the (now irrelevant) reserving job never
// restarts a service the operator independently stopped for another reason.
func TestExplicitServiceStopClearsReservationTag(t *testing.T) {
	st := fullGPUStatus(
		svcInfo("svc-pinned", false, 20),
		svcInfo("svc-idle-small", true, 6),
	)
	// type: native (see nativeReservationTestManifestYAML's doc): this test
	// drives the real Execute() dispatch below, which must not shell out to
	// docker.
	h, exec := newReservationTestHandlerWithManifest(t, nativeReservationTestManifestYAML, st)
	if _, err := h.Reserve(testCtx(), testJobID, 5*1024*1024*1024); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	m := readManifestMap(t, h.ConfigDir)
	if entry := manifestServiceEntry(t, m, "svc-idle-small"); entry == nil || entry["evicted_by_job"] != testJobID {
		t.Fatalf("precondition failed: svc-idle-small not tagged after Reserve: %v", entry)
	}

	// Operator explicitly stops the already-stopped (reservation-evicted)
	// service for an unrelated reason.
	stop := &nexus.Job{ID: "j-stop", Type: "SERVICE_STOP", Payload: map[string]string{"service": "svc-idle-small"}}
	if _, err := h.Execute(testCtx(), stop); err != nil {
		t.Fatalf("SERVICE_STOP execute: %v", err)
	}
	m = readManifestMap(t, h.ConfigDir)
	entry := manifestServiceEntry(t, m, "svc-idle-small")
	if entry == nil {
		t.Fatal("svc-idle-small entry missing after SERVICE_STOP")
	}
	if _, present := entry["evicted_by_job"]; present {
		t.Errorf("SERVICE_STOP did not clear the reservation tag: %v", entry["evicted_by_job"])
	}

	// The reservation's Release must now be a no-op for this service.
	exec.started = nil
	restored, err := h.Release(testCtx(), testJobID)
	if err != nil {
		t.Fatalf("Release: %v", err)
	}
	if containsString(restored, "svc-idle-small") {
		t.Errorf("Release restarted an operator-stopped service: %v", restored)
	}
	if containsString(exec.started, "svc-idle-small") {
		t.Errorf("start() was called for an operator-stopped service: %v", exec.started)
	}
}

// TestExplicitServiceStartClearsReservationTag mirrors
// TestExplicitServiceStopClearsReservationTag for the other clear site
// (service_handler.go's SERVICE_START branch): an operator/platform-issued
// SERVICE_START on a reservation-evicted service must clear its
// evicted_by_job tag too, so a later Release for the (now irrelevant)
// reserving job cannot restart-on-top-of an already-running service the
// operator explicitly started for an unrelated reason.
func TestExplicitServiceStartClearsReservationTag(t *testing.T) {
	st := fullGPUStatus(
		svcInfo("svc-pinned", false, 20),
		svcInfo("svc-idle-small", true, 6),
	)
	// type: native (see nativeReservationTestManifestYAML's doc): this test
	// drives the real Execute() dispatch below, which must not shell out to
	// docker.
	h, exec := newReservationTestHandlerWithManifest(t, nativeReservationTestManifestYAML, st)
	if _, err := h.Reserve(testCtx(), testJobID, 5*1024*1024*1024); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	m := readManifestMap(t, h.ConfigDir)
	if entry := manifestServiceEntry(t, m, "svc-idle-small"); entry == nil || entry["evicted_by_job"] != testJobID {
		t.Fatalf("precondition failed: svc-idle-small not tagged after Reserve: %v", entry)
	}

	// Operator explicitly starts the reservation-evicted service for an
	// unrelated reason (e.g. manually resuming it before the reservation
	// itself was released).
	start := &nexus.Job{ID: "j-start", Type: "SERVICE_START", Payload: map[string]string{"service": "svc-idle-small"}}
	if _, err := h.Execute(testCtx(), start); err != nil {
		t.Fatalf("SERVICE_START execute: %v", err)
	}
	m = readManifestMap(t, h.ConfigDir)
	entry := manifestServiceEntry(t, m, "svc-idle-small")
	if entry == nil {
		t.Fatal("svc-idle-small entry missing after SERVICE_START")
	}
	if _, present := entry["evicted_by_job"]; present {
		t.Errorf("SERVICE_START did not clear the reservation tag: %v", entry["evicted_by_job"])
	}

	// The reservation's Release must now be a no-op for this service: it must
	// not issue a second, redundant start against a service the operator
	// already explicitly started.
	exec.started = nil
	restored, err := h.Release(testCtx(), testJobID)
	if err != nil {
		t.Fatalf("Release: %v", err)
	}
	if containsString(restored, "svc-idle-small") {
		t.Errorf("Release restarted an operator-started service: %v", restored)
	}
	if containsString(exec.started, "svc-idle-small") {
		t.Errorf("start() was called again for an operator-started service: %v", exec.started)
	}
}

// TestActiveReservationsGroupsByJob exercises the heartbeat-facing read.
func TestActiveReservationsGroupsByJob(t *testing.T) {
	st := fullGPUStatus(
		svcInfo("svc-pinned", false, 20),
		svcInfo("svc-idle-small", true, 2),
		svcInfo("svc-idle-large", true, 5),
	)
	h, _ := newReservationTestHandler(t, st)
	if _, err := h.Reserve(testCtx(), testJobID, 6*1024*1024*1024); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	list, err := h.ActiveReservations()
	if err != nil {
		t.Fatalf("ActiveReservations: %v", err)
	}
	if len(list) != 1 || list[0].JobID != testJobID {
		t.Fatalf("ActiveReservations = %+v, want one entry for %s", list, testJobID)
	}
	want := []string{"svc-idle-large", "svc-idle-small"}
	sort.Strings(want)
	if !equalStrings(list[0].EvictedServices, want) {
		t.Errorf("EvictedServices = %v, want %v", list[0].EvictedServices, want)
	}
}

// TestReservePreservesPriorDesiredStatusOnRelease pins the "restore to PRIOR
// state" fidelity fix: a service that already carried desired_status:stopped
// before Reserve touched it (e.g. an earlier operator SERVICE_STOP whose
// compose-down failed, leaving the container still running and therefore a
// preemption candidate) must come back from Release with that same prior
// stopped intent intact -- Release must NOT silently flip it to start-on-boot
// just because a reservation happened to restart the container once.
func TestReservePreservesPriorDesiredStatusOnRelease(t *testing.T) {
	const manifest = `node:
  name: test-node
services:
  - name: svc-pinned
    type: docker
    compose_file: ./services/svc-pinned.yml
  - name: svc-prior-stopped
    type: docker
    compose_file: ./services/svc-prior-stopped.yml
    desired_status: stopped
pinned_services:
  - svc-pinned
`
	dir := t.TempDir()
	writeManifestFile(t, dir, manifest)
	exec := &fakeReservationExec{}
	h := NewServiceHandler(dir)
	h.stopServiceFn = exec.stop
	h.startServiceFn = exec.start
	st := fullGPUStatus(
		svcInfo("svc-pinned", false, 20),
		// Still actually RUNNING despite desired_status:stopped above -- the
		// divergence the test is about.
		svcInfo("svc-prior-stopped", true, 5),
	)
	h.collectStatus = func() (*status.NodeStatus, error) { return st, nil }

	if _, err := h.Reserve(testCtx(), testJobID, 4*1024*1024*1024); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	m := readManifestMap(t, dir)
	entry := manifestServiceEntry(t, m, "svc-prior-stopped")
	if entry == nil || entry["evicted_prior_status"] != "stopped" {
		t.Fatalf("evicted_prior_status not recorded: %v", entry)
	}

	if _, err := h.Release(testCtx(), testJobID); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if !containsString(exec.started, "svc-prior-stopped") {
		t.Errorf("Release did not restart svc-prior-stopped: started=%v", exec.started)
	}
	m = readManifestMap(t, dir)
	entry = manifestServiceEntry(t, m, "svc-prior-stopped")
	if entry == nil {
		t.Fatal("svc-prior-stopped entry missing after Release")
	}
	if _, present := entry["evicted_by_job"]; present {
		t.Errorf("evicted_by_job still set after Release: %v", entry["evicted_by_job"])
	}
	if _, present := entry["evicted_prior_status"]; present {
		t.Errorf("evicted_prior_status still set after Release: %v", entry["evicted_prior_status"])
	}
	// The key assertion: the PRIOR "stopped" intent must survive Release,
	// even though the container was just restarted.
	if entry["desired_status"] != "stopped" {
		t.Errorf("desired_status = %v, want stopped (prior operator intent must be restored, not cleared)", entry["desired_status"])
	}
}

// --- small local helpers (kept here, not shared, to avoid coupling to
// desired_status_test.go's exact shape) ---

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
