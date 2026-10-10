package worker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	jobhandlers "github.com/aceteam-ai/citadel-cli/internal/jobs"
)

// TestSerializedLaneJobTypes pins the exact membership of the general
// unbounded-execution-lane routing set (citadel-cli#908). Like
// TestGPUBoundJobTypes for the GPU-slot gate, this is the authority for which
// job types are serialized on the exec-concurrency-1 lane -- read this, not a
// doc copy. It must be a SUPERSET of unboundedJobTypes (every unbounded job is a
// manifest writer or ran one-at-a-time on the sequential loop) plus the
// explicitly serialized state/workspace writers that deliberately retain a
// watchdog tier.
func TestSerializedLaneJobTypes(t *testing.T) {
	want := map[string]struct{}{
		// The unbounded tier (also the watchdog "no fallback deadline" set).
		JobTypeDownloadModel:     {},
		JobTypeOllamaPull:        {},
		JobTypeModelCachePull:    {},
		JobTypeServiceStart:      {},
		JobTypeIOSBuild:          {},
		JobTypeAndroidBuild:      {},
		JobTypeGomobileBuild:     {},
		JobTypeInstanceProvision: {},
		JobTypeAgentUpdate:       {},
		JobTypeWhatsAppProvision: {},
		JobTypeAppDeploy:         {},
		// Manifest/lockfile writers that are NOT unbounded but still must
		// serialize (they read-modify-write citadel.yaml / modules.lock).
		JobTypeModuleSet:         {},
		JobTypeFineTuneStart:     {},
		JobTypeServiceStop:       {},
		JobTypeApplyDeviceConfig: {},
		// Per-run workspace writer with a long-tier watchdog fallback.
		JobTypeRunJobTemplate: {},
		// Not a manifest writer, but a read-modify-write of the SAME-shaped
		// shared-state file (cache-index.json, citadel-cli#682 P2a) -- see
		// serializedLaneJobTypes' doc comment.
		JobTypeModelCacheEvict: {},
	}
	if len(serializedLaneJobTypes) != len(want) {
		t.Fatalf("serializedLaneJobTypes has %d entries, want %d: %v",
			len(serializedLaneJobTypes), len(want), serializedLaneJobTypes)
	}
	for jt := range want {
		if !needsSerializedLane(jt) {
			t.Errorf("expected %q to route to the serialized lane", jt)
		}
	}
	// Every unbounded job type must be a member (superset invariant).
	for jt := range unboundedJobTypes {
		if !needsSerializedLane(jt) {
			t.Errorf("unbounded job type %q must also route to the serialized lane", jt)
		}
	}
	// A representative non-member must NOT route there. FILE_INDEX is explicitly
	// listed: it routes to the dedicated HEAVY lane, not the serialized lane
	// (aceteam#10876 C1) — a multi-hour index must never queue manifest writers.
	for _, jt := range []string{JobTypeShellCommand, JobTypeFileRead, JobTypeLLMInference, JobTypeMeetingJoin, JobTypeFileIndex} {
		if needsSerializedLane(jt) {
			t.Errorf("%q must NOT route to the serialized lane", jt)
		}
	}
}

// TestHeavyLaneJobTypes pins the exact membership of the dedicated heavy-lane
// routing set (aceteam#10876 C1). It is the authority for which job types run on
// the exec-1/admit-2 heavy lane; read this, not a doc copy.
func TestHeavyLaneJobTypes(t *testing.T) {
	want := map[string]struct{}{JobTypeFileIndex: {}}
	if len(heavyLaneJobTypes) != len(want) {
		t.Fatalf("heavyLaneJobTypes has %d entries, want %d: %v", len(heavyLaneJobTypes), len(want), heavyLaneJobTypes)
	}
	for jt := range want {
		if !needsHeavyLane(jt) {
			t.Errorf("expected %q to route to the heavy lane", jt)
		}
	}
	// The heavy lane is disjoint from the serialized/long/GPU routing sets.
	if needsSerializedLane(JobTypeFileIndex) {
		t.Error("FILE_INDEX must NOT be on the serialized lane (dedicated heavy lane instead)")
	}
	if _, long := longSessionJobTypes[JobTypeFileIndex]; long {
		t.Error("FILE_INDEX must NOT be in longSessionJobTypes (that would also route it to the #489 always-async lane and race the heavy lane)")
	}
	for _, jt := range []string{JobTypeServiceStart, JobTypeShellCommand, JobTypeLLMInference, JobTypeMeetingJoin} {
		if needsHeavyLane(jt) {
			t.Errorf("%q must NOT route to the heavy lane", jt)
		}
	}
}

// TestCooperativeCancelJobTypes pins which job types get the graceful mid-job
// cancel watch (aceteam#10876 C1). HUDDLE_JOIN is deliberately excluded — it has
// its own hard ctx-cancel watch.
func TestCooperativeCancelJobTypes(t *testing.T) {
	want := map[string]struct{}{JobTypeFileIndex: {}}
	if len(cooperativeCancelJobTypes) != len(want) {
		t.Fatalf("cooperativeCancelJobTypes has %d entries, want %d: %v", len(cooperativeCancelJobTypes), len(want), cooperativeCancelJobTypes)
	}
	if !needsCooperativeCancelWatch(JobTypeFileIndex) {
		t.Error("FILE_INDEX must get the cooperative cancel watch")
	}
	for _, jt := range []string{JobTypeHuddleJoin, JobTypeServiceStart, JobTypeShellCommand} {
		if needsCooperativeCancelWatch(jt) {
			t.Errorf("%q must NOT get the cooperative cancel watch", jt)
		}
	}
}

// TestRunnerHeavyLaneDoesNotBlockFetchLoop pins acceptance #4 (aceteam#10876
// C1): a long-running FILE_INDEX on the heavy lane must NOT block the fetch loop
// from claiming and executing a FILE_READ_BYTES queued behind it. The index runs
// off the fetch loop (heavy lane), so the file read is claimed and completed
// while the index is still executing.
func TestRunnerHeavyLaneDoesNotBlockFetchLoop(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	indexHandler := &blockingJobHandler{
		jobType: JobTypeFileIndex,
		onStart: func() { once.Do(func() { close(started) }) },
		release: release,
	}
	fileHandler := NewMockJobHandler(JobTypeFileReadBytes, false)

	jobs := []*Job{
		{ID: "index-1", Type: JobTypeFileIndex, Payload: map[string]any{"path": "/ws"}},
		{ID: "file-1", Type: JobTypeFileReadBytes, Payload: map[string]any{}},
	}
	source := NewMockJobSource("test", jobs)
	state := NewWorkerState()
	runner := NewRunner(source, []JobHandler{indexHandler, fileHandler}, RunnerConfig{
		WorkerID:       "test-worker",
		MaxConcurrency: 1,
		State:          state,
		ActivityFn:     func(string, string) {},
	})
	streams := newKeyedStreamWriterFactory()
	runner.WithStreamWriterFactory(streams.factory)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	runDone := make(chan struct{})
	go func() { runner.Run(ctx); close(runDone) }()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("FILE_INDEX handler never started")
	}
	if s := streams.get("index-1"); s == nil || !s.claimed {
		t.Error("expected FILE_INDEX to publish its claim-ack synchronously at fetch time")
	}
	// While the index is still blocked in its handler, the file read must complete.
	if !laneWaitFor(5*time.Second, func() bool { return len(fileHandler.ExecutedJobs()) >= 1 }) {
		t.Fatal("FILE_READ_BYTES was not dispatched while FILE_INDEX was still in flight (heavy lane head-of-line blocking regression)")
	}
	if s := streams.get("index-1"); s != nil && s.ended {
		t.Error("FILE_INDEX should not have a terminal event yet — its handler is still blocked")
	}
	// The heavy lane reports the executing index.
	if !laneWaitFor(5*time.Second, func() bool {
		for _, s := range runner.LaneSnapshots() {
			if s.Lane == "heavy" && s.Executing >= 1 {
				return true
			}
		}
		return false
	}) {
		t.Fatalf("heavy lane never showed the executing index; got %+v", runner.LaneSnapshots())
	}

	close(release)
	cancel()
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// coopCancelObserverHandler is a FILE_INDEX worker.JobHandler that waits for a
// cooperative cancel to be surfaced on its context (via jobs.CancelRequested)
// and then returns a partial `cancelled` result — WITHOUT its context being hard
// cancelled. It proves the runner's cooperative-cancel watch sets the graceful
// flag on the handler ctx without cancelling it (aceteam#10876 C1).
type coopCancelObserverHandler struct {
	started     chan struct{}
	observed    chan struct{}
	hardCancel  chan struct{}
	startedOnce sync.Once
}

func (h *coopCancelObserverHandler) CanHandle(jt string) bool { return jt == JobTypeFileIndex }

func (h *coopCancelObserverHandler) Execute(ctx context.Context, job *Job, stream StreamWriter) (*JobResult, error) {
	h.startedOnce.Do(func() { close(h.started) })
	pred := jobhandlers.CancelRequestedFrom(ctx)
	if pred == nil {
		return nil, errors.New("no cooperative-cancel predicate wired on the handler context")
	}
	for {
		if pred() {
			close(h.observed)
			return &JobResult{Status: JobStatusSuccess, Output: map[string]any{"status": "cancelled", "checkpoint": "partial", "files_indexed": 1}}, nil
		}
		select {
		case <-ctx.Done():
			// A cooperative cancel must NOT hard-cancel the handler ctx.
			close(h.hardCancel)
			return nil, ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// TestRunnerFileIndexCooperativeCancelObservedByHandler pins acceptance #1
// plumbing (aceteam#10876 C1): a job:cancelled marker set AFTER execution begins
// is surfaced to the FILE_INDEX handler as a cooperative (graceful) cancel on its
// context — never a hard ctx cancel — and the resulting partial result is ACKed.
func TestRunnerFileIndexCooperativeCancelObservedByHandler(t *testing.T) {
	handler := &coopCancelObserverHandler{
		started:    make(chan struct{}),
		observed:   make(chan struct{}),
		hardCancel: make(chan struct{}),
	}
	source := NewMockJobSource("test", []*Job{{ID: "index-1", Type: JobTypeFileIndex, Payload: map[string]any{"path": "/ws"}}})
	runner := NewRunner(source, []JobHandler{handler}, RunnerConfig{
		WorkerID:       "test-worker",
		MaxConcurrency: 1,
		ActivityFn:     func(string, string) {},
	})
	runner.coopCancelPoll = 5 * time.Millisecond
	streams := newKeyedStreamWriterFactory()
	runner.WithStreamWriterFactory(streams.factory)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	runDone := make(chan struct{})
	go func() { runner.Run(ctx); close(runDone) }()

	select {
	case <-handler.started:
	case <-time.After(5 * time.Second):
		t.Fatal("FILE_INDEX handler never started")
	}
	// Trigger the cooperative cancel AFTER execution has begun.
	source.SetCancelled("index-1")

	select {
	case <-handler.observed:
		// Good: the handler saw CancelRequested()==true.
	case <-handler.hardCancel:
		t.Fatal("cooperative cancel hard-cancelled the handler context (must be a graceful flag only)")
	case <-time.After(3 * time.Second):
		t.Fatal("handler never observed the cooperative cancel within ~3s")
	}

	// The partial result must be ACKed (not Nacked/Failed), with one terminal end.
	if !laneWaitFor(5*time.Second, func() bool {
		for _, j := range source.AckedJobs() {
			if j.ID == "index-1" {
				return true
			}
		}
		return false
	}) {
		t.Fatalf("index-1 was not ACKed after a cooperative cancel; acked=%v nacked=%v", jobIDs(source.AckedJobs()), jobIDs(source.NackedJobs()))
	}
	if s := streams.get("index-1"); s == nil || !s.ended {
		t.Fatal("expected a terminal end event carrying the partial result")
	} else if s.endResult["status"] != "cancelled" {
		t.Errorf("terminal result status = %v, want cancelled", s.endResult["status"])
	}

	cancel()
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// waitFor polls cond until it is true or the deadline passes.
func laneWaitFor(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

// TestRunnerUnboundedLaneDoesNotBlockFetchLoop is the direct regression test for
// citadel-cli#908's reported incident shape: on a maxConcurrency=1 node, a
// long-running SERVICE_START (an unbounded/manifest-writer job type) must NOT
// block the fetch loop from claiming and executing a FILE_READ_BYTES queued
// behind it. Before #908 the SERVICE_START ran inline in the fetch loop, so the
// file read was never even claimed within the backend's short claim-ack window
// and fast-failed as "unreachable". Now the SERVICE_START runs on the unbounded
// lane (off the fetch loop), so the file read is claimed and completed while it
// is still executing.
func TestRunnerUnboundedLaneDoesNotBlockFetchLoop(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	deployHandler := &blockingJobHandler{
		jobType: JobTypeServiceStart,
		onStart: func() { once.Do(func() { close(started) }) },
		release: release,
	}
	fileHandler := NewMockJobHandler(JobTypeFileReadBytes, false)

	jobs := []*Job{
		{ID: "deploy-1", Type: JobTypeServiceStart, Payload: map[string]any{"service": "vllm"}},
		{ID: "file-1", Type: JobTypeFileReadBytes, Payload: map[string]any{}},
	}
	source := NewMockJobSource("test", jobs)
	state := NewWorkerState()
	runner := NewRunner(source, []JobHandler{deployHandler, fileHandler}, RunnerConfig{
		WorkerID:       "test-worker",
		MaxConcurrency: 1,
		State:          state,
		ActivityFn:     func(string, string) {},
	})
	streams := newKeyedStreamWriterFactory()
	runner.WithStreamWriterFactory(streams.factory)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	runDone := make(chan struct{})
	go func() { runner.Run(ctx); close(runDone) }()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("SERVICE_START handler never started")
	}

	// The deploy's claim-ack must have fired the instant it was read, before any
	// execution completes -- this is what keeps the backend from fast-failing it.
	if s := streams.get("deploy-1"); s == nil || !s.claimed {
		t.Error("expected the SERVICE_START to have published its claim-ack synchronously at fetch time")
	}

	// While the deploy is still blocked in its handler, the file read must be
	// claimed and completed.
	if !laneWaitFor(5*time.Second, func() bool { return len(fileHandler.ExecutedJobs()) >= 1 }) {
		t.Fatal("FILE_READ_BYTES was not dispatched while SERVICE_START was still in flight (head-of-line blocking regression)")
	}
	if !laneWaitFor(5*time.Second, func() bool {
		for _, j := range source.AckedJobs() {
			if j.ID == "file-1" {
				return true
			}
		}
		return false
	}) {
		t.Fatal("file-1 was not acked while SERVICE_START blocked")
	}

	// The deploy must still be queued/executing, not terminal.
	if s := streams.get("deploy-1"); s != nil && s.ended {
		t.Error("SERVICE_START should not have a terminal event yet -- its handler is still blocked")
	}
	if snap := state.Snapshot(); snap.InFlight < 1 {
		t.Errorf("InFlight = %d, want >= 1 while SERVICE_START is still running", snap.InFlight)
	}

	close(release)
	if !laneWaitFor(5*time.Second, func() bool {
		for _, j := range source.AckedJobs() {
			if j.ID == "deploy-1" {
				return true
			}
		}
		return false
	}) {
		t.Fatal("deploy-1 was not acked after releasing its handler")
	}

	cancel()
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// TestRunnerUnboundedLaneExecutesSequentially pins the single-writer safety the
// exec-concurrency-1 general lane provides (citadel-cli#908 §2c): two unbounded
// manifest-writing jobs (SERVICE_START) must execute STRICTLY one at a time,
// exactly as the pre-#908 sequential fetch loop guaranteed. The second must not
// even START until the first finishes -- this is the property that lets the
// unlocked citadel.yaml/modules.lock read-modify-write paths stay race-free
// without any new locking. It is the analogue of
// TestRunnerGPUBoundJobsSequentialWithoutTracker for the general lane.
func TestRunnerUnboundedLaneExecutesSequentially(t *testing.T) {
	starts := make(chan struct{}, 2)
	release := make(chan struct{})
	handler := &blockingJobHandler{
		jobType: JobTypeServiceStart,
		onStart: func() { starts <- struct{}{} },
		release: release,
	}
	jobs := []*Job{
		{ID: "deploy-1", Type: JobTypeServiceStart, Payload: map[string]any{}},
		{ID: "deploy-2", Type: JobTypeServiceStart, Payload: map[string]any{}},
	}
	source := NewMockJobSource("test", jobs)
	runner := NewRunner(source, []JobHandler{handler}, RunnerConfig{
		WorkerID:       "test-worker",
		MaxConcurrency: 1,
		ActivityFn:     func(string, string) {},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	runDone := make(chan struct{})
	go func() { runner.Run(ctx); close(runDone) }()

	// First job starts.
	select {
	case <-starts:
	case <-time.After(5 * time.Second):
		t.Fatal("first SERVICE_START never started")
	}

	// While job 1 blocks, job 2 must NOT start -- the exec-concurrency-1 lane
	// serializes them. This is the core single-writer assertion.
	select {
	case <-starts:
		t.Fatal("second SERVICE_START started while the first was still executing -- the unbounded lane must serialize manifest writers")
	case <-time.After(300 * time.Millisecond):
		// Expected: no second start.
	}
	if got := len(handler.ExecutedJobs()); got != 1 {
		t.Fatalf("executions while job 1 blocked = %d, want 1", got)
	}

	// Release job 1; job 2 then runs to completion sequentially.
	close(release)
	select {
	case <-starts:
	case <-time.After(5 * time.Second):
		t.Fatal("second SERVICE_START never started after the first was released")
	}
	if !laneWaitFor(5*time.Second, func() bool { return len(source.AckedJobs()) >= 2 }) {
		t.Fatalf("both jobs should complete sequentially; acked = %d", len(source.AckedJobs()))
	}

	cancel()
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// TestRunnerApplyDeviceConfigSerializesWithServiceStart is the regression test
// for the review BLOCK: APPLY_DEVICE_CONFIG (ConfigHandler.updateManifest) does
// a full non-atomic read-modify-write of citadel.yaml, so it MUST execute on the
// serialized (exec-cap-1) lane with the other manifest writers -- never inline
// and concurrently with a SERVICE_START/SERVICE_STOP/MODULE_SET that is also
// mid-write. This asserts the two DIFFERENT manifest-writer types cannot execute
// at the same time: while the first (whichever the lane admits first) is blocked
// in its handler, the second must not start. Before the fix APPLY_DEVICE_CONFIG
// fell to the inline default branch and could truncate-write citadel.yaml
// concurrently with a lane manifest writer -> torn read / lost update.
func TestRunnerApplyDeviceConfigSerializesWithServiceStart(t *testing.T) {
	starts := make(chan string, 2)
	release := make(chan struct{})
	applyHandler := &blockingJobHandler{
		jobType: JobTypeApplyDeviceConfig,
		onStart: func() { starts <- JobTypeApplyDeviceConfig },
		release: release,
	}
	deployHandler := &blockingJobHandler{
		jobType: JobTypeServiceStart,
		onStart: func() { starts <- JobTypeServiceStart },
		release: release,
	}
	jobs := []*Job{
		{ID: "apply-1", Type: JobTypeApplyDeviceConfig, Payload: map[string]any{}},
		{ID: "deploy-1", Type: JobTypeServiceStart, Payload: map[string]any{}},
	}
	source := NewMockJobSource("test", jobs)
	runner := NewRunner(source, []JobHandler{applyHandler, deployHandler}, RunnerConfig{
		WorkerID:       "test-worker",
		MaxConcurrency: 1,
		ActivityFn:     func(string, string) {},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	runDone := make(chan struct{})
	go func() { runner.Run(ctx); close(runDone) }()

	// Exactly one manifest writer starts and blocks; the other must NOT start
	// while it holds the single serialized exec slot.
	select {
	case <-starts:
	case <-time.After(5 * time.Second):
		t.Fatal("neither manifest-writer job started")
	}
	select {
	case second := <-starts:
		t.Fatalf("%s started while another manifest writer was still executing -- APPLY_DEVICE_CONFIG must serialize with SERVICE_START on the exec-cap-1 lane", second)
	case <-time.After(300 * time.Millisecond):
		// Expected: no concurrent second manifest writer.
	}
	total := len(applyHandler.ExecutedJobs()) + len(deployHandler.ExecutedJobs())
	if total != 1 {
		t.Fatalf("manifest-writer executions while one blocked = %d, want 1", total)
	}

	// Release; both must complete sequentially.
	close(release)
	if !laneWaitFor(5*time.Second, func() bool { return len(source.AckedJobs()) >= 2 }) {
		t.Fatalf("both manifest writers should complete sequentially; acked = %d", len(source.AckedJobs()))
	}

	cancel()
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// TestRunnerLaneActivityPopulatedWhenBusy pins the LaneActivity heartbeat field
// (citadel-cli#908 §4): while a job is executing on the unbounded lane and
// another is queued behind it, LaneSnapshots reports Executing==ExecCapacity
// with BusySince set, and Queued>=1.
func TestRunnerLaneActivityPopulatedWhenBusy(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	handler := &blockingJobHandler{
		jobType: JobTypeServiceStart,
		onStart: func() { once.Do(func() { close(started) }) },
		release: release,
	}
	jobs := []*Job{
		{ID: "deploy-1", Type: JobTypeServiceStart, Payload: map[string]any{}},
		{ID: "deploy-2", Type: JobTypeServiceStart, Payload: map[string]any{}},
	}
	source := NewMockJobSource("test", jobs)
	runner := NewRunner(source, []JobHandler{handler}, RunnerConfig{
		WorkerID:       "test-worker",
		MaxConcurrency: 1,
		ActivityFn:     func(string, string) {},
	})

	// Idle: no lanes report activity.
	for _, s := range runner.LaneSnapshots() {
		if s.Executing != 0 || s.Queued != 0 || s.BusySince != nil {
			t.Fatalf("idle lane %q reported activity: %+v", s.Lane, s)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	runDone := make(chan struct{})
	go func() { runner.Run(ctx); close(runDone) }()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("handler never started")
	}

	// Wait for the second job to be admitted-and-queued behind the executing one.
	var unbounded *LaneSnapshot
	if !laneWaitFor(5*time.Second, func() bool {
		for _, s := range runner.LaneSnapshots() {
			if s.Lane == "unbounded" && s.Executing >= 1 && s.Queued >= 1 {
				snap := s
				unbounded = &snap
				return true
			}
		}
		return false
	}) {
		t.Fatalf("unbounded lane never showed executing+queued; got %+v", runner.LaneSnapshots())
	}
	if unbounded.ExecCapacity != 1 {
		t.Errorf("unbounded lane ExecCapacity = %d, want 1", unbounded.ExecCapacity)
	}
	if unbounded.BusySince == nil {
		t.Error("expected BusySince to be set while the lane is fully saturated")
	}

	close(release)
	cancel()
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// TestRunnerLaneSaturatedNacks pins the admission-bound behavior (citadel-cli
// #908 §2b path iii): when a lane is at its admission bound, a further claimed
// job is Nacked (transparent retry) with NO terminal stream event and NO counter
// left dangling -- the same shape as the #825 GPU-slot-full Nack. The lane's
// admit depth is overridden to 1 so a single blocking job saturates it.
func TestRunnerLaneSaturatedNacks(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	handler := &blockingJobHandler{
		jobType: JobTypeServiceStart,
		onStart: func() { once.Do(func() { close(started) }) },
		release: release,
	}
	jobs := []*Job{
		{ID: "deploy-1", Type: JobTypeServiceStart, Payload: map[string]any{}},
		{ID: "deploy-2", Type: JobTypeServiceStart, Payload: map[string]any{}},
	}
	source := NewMockJobSource("test", jobs)
	state := NewWorkerState()
	runner := NewRunner(source, []JobHandler{handler}, RunnerConfig{
		WorkerID:       "test-worker",
		MaxConcurrency: 1,
		State:          state,
		ActivityFn:     func(string, string) {},
	})
	// Admission bound of 1: the blocking deploy-1 holds it for its whole life, so
	// deploy-2 cannot be admitted and must Nack.
	runner.unboundedLane = newLane("unbounded", 1, 1, false)
	streams := newKeyedStreamWriterFactory()
	runner.WithStreamWriterFactory(streams.factory)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	runDone := make(chan struct{})
	go func() { runner.Run(ctx); close(runDone) }()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("deploy-1 handler never started")
	}

	// deploy-2 must be Nacked (lane saturated), never executed, no terminal event.
	if !laneWaitFor(5*time.Second, func() bool {
		for _, j := range source.NackedJobs() {
			if j.ID == "deploy-2" {
				return true
			}
		}
		return false
	}) {
		t.Fatalf("deploy-2 was not Nacked when the lane was saturated; nacked=%v", jobIDs(source.NackedJobs()))
	}
	if len(handler.ExecutedJobs()) != 1 {
		t.Errorf("handler executions = %d, want 1 (deploy-2 must never run)", len(handler.ExecutedJobs()))
	}
	if s := streams.get("deploy-2"); s != nil && (s.ended || s.errored) {
		t.Error("a lane-saturated Nack must not publish a terminal event")
	}

	close(release)
	cancel()
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// TestRunnerInferenceQueueReturnsWarmingOnWaitExceeded is the citadel-slice of
// aceteam#8254: when an inference job cannot get an execution slot within the
// queue-wait budget, the node returns the EXISTING model_warming success signal
// (which the platform already retries) rather than a silent Nack. Also asserts
// the latency metrics ride the output.
func TestRunnerInferenceQueueReturnsWarmingOnWaitExceeded(t *testing.T) {
	release := make(chan struct{})
	// blockingJobHandler blocks EVERY execution until release -- job 1 holds the
	// single exec slot; job 2 never reaches the handler (it times out at the
	// lane's queue wait).
	handler := &blockingJobHandler{jobType: JobTypeLLMInference, release: release}

	jobs := []*Job{
		{ID: "inference-1", Type: JobTypeLLMInference, Payload: map[string]any{"model": "m"}},
		{ID: "inference-2", Type: JobTypeLLMInference, Payload: map[string]any{"model": "m"}},
	}
	source := NewMockJobSource("test", jobs)
	runner := NewRunner(source, []JobHandler{handler}, RunnerConfig{
		WorkerID:       "test-worker",
		MaxConcurrency: 1,
		GPUTracker:     NewGPUTracker(1), // one exec slot on the inference lane
		ActivityFn:     func(string, string) {},
	})
	// Short queue wait so job 2 gives up fast.
	runner.inferenceQueueWait = 60 * time.Millisecond
	streams := newKeyedStreamWriterFactory()
	runner.WithStreamWriterFactory(streams.factory)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	runDone := make(chan struct{})
	go func() { runner.Run(ctx); close(runDone) }()

	// Exactly one job wins the single exec slot and blocks in the handler; which
	// one is nondeterministic, so identify it rather than assuming. The OTHER
	// job is the one that must exceed the queue-wait and return warming.
	if !laneWaitFor(5*time.Second, func() bool { return len(handler.ExecutedJobs()) == 1 }) {
		t.Fatalf("expected exactly one inference job to acquire the exec slot; executed=%v", jobIDs(handler.ExecutedJobs()))
	}
	runningID := handler.ExecutedJobs()[0].ID
	queuedID := "inference-1"
	if runningID == "inference-1" {
		queuedID = "inference-2"
	}

	// The queued job must be Acked (warming is a SUCCESS terminal), never Nacked.
	if !laneWaitFor(5*time.Second, func() bool {
		for _, j := range source.AckedJobs() {
			if j.ID == queuedID {
				return true
			}
		}
		return false
	}) {
		t.Fatalf("%s did not receive a warming success Ack; acked=%v nacked=%v",
			queuedID, jobIDs(source.AckedJobs()), jobIDs(source.NackedJobs()))
	}
	for _, j := range source.NackedJobs() {
		if j.ID == queuedID {
			t.Fatalf("%s must return model_warming (Ack), never a Nack, on queue-wait exceeded", queuedID)
		}
	}
	if len(handler.ExecutedJobs()) != 1 {
		t.Errorf("handler executions = %d, want 1 (the queued job must not reach the handler)", len(handler.ExecutedJobs()))
	}

	// The terminal payload must be the model_warming signal with latency metrics.
	s := streams.get(queuedID)
	if s == nil || !s.ended {
		t.Fatalf("expected a terminal end event for %s", queuedID)
	}
	if got := s.endResult["status"]; got != "model_warming" {
		t.Errorf("status = %v, want model_warming", got)
	}
	if _, ok := s.endResult["queue_wait_ms"]; !ok {
		t.Error("expected queue_wait_ms in the warming output")
	}
	if _, ok := s.endResult["total_ms"]; !ok {
		t.Error("expected total_ms in the warming output")
	}

	close(release)
	cancel()
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// TestRunnerInferenceSuccessCarriesLatencyMetrics pins that a NORMAL (non-queued)
// inference success on the inference lane carries the per-request latency
// metrics in its output (aceteam#8254 §3c), and that a non-inference job does
// NOT (the metrics are scoped to the inference lane).
func TestRunnerInferenceSuccessCarriesLatencyMetrics(t *testing.T) {
	jobs := []*Job{
		{ID: "inference-1", Type: JobTypeLLMInference, Payload: map[string]any{"model": "m"}},
		{ID: "shell-1", Type: JobTypeShellCommand, Payload: map[string]any{}},
	}
	source := NewMockJobSource("test", jobs)
	runner := NewRunner(source, []JobHandler{
		NewMockJobHandler(JobTypeLLMInference, false),
		NewMockJobHandler(JobTypeShellCommand, false),
	}, RunnerConfig{
		WorkerID:       "test-worker",
		MaxConcurrency: 1,
		GPUTracker:     NewGPUTracker(1),
		ActivityFn:     func(string, string) {},
	})
	streams := newKeyedStreamWriterFactory()
	runner.WithStreamWriterFactory(streams.factory)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runner.Run(ctx)

	inf := streams.get("inference-1")
	if inf == nil || !inf.ended {
		t.Fatal("expected inference-1 to complete with a terminal end")
	}
	if _, ok := inf.endResult["total_ms"]; !ok {
		t.Error("expected total_ms on the inference-lane success output")
	}
	if _, ok := inf.endResult["queue_wait_ms"]; !ok {
		t.Error("expected queue_wait_ms on the inference-lane success output")
	}

	// A non-inference job must NOT carry the inference latency metrics.
	sh := streams.get("shell-1")
	if sh == nil || !sh.ended {
		t.Fatal("expected shell-1 to complete with a terminal end")
	}
	if _, ok := sh.endResult["total_ms"]; ok {
		t.Error("a non-inference job must not carry inference latency metrics")
	}
}
