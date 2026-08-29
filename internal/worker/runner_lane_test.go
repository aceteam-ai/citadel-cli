package worker

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestSerializedLaneJobTypes pins the exact membership of the general
// unbounded-execution-lane routing set (citadel-cli#908). Like
// TestGPUBoundJobTypes for the GPU-slot gate, this is the authority for which
// job types are serialized on the exec-concurrency-1 lane -- read this, not a
// doc copy. It must be a SUPERSET of unboundedJobTypes (every unbounded job is a
// manifest writer or ran one-at-a-time on the sequential loop) plus the two
// manifest/lockfile writers that are deliberately NOT unbounded (they keep their
// watchdog tier): MODULE_SET and SERVICE_STOP.
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
		// Manifest/lockfile writers that are NOT unbounded but still must
		// serialize (they read-modify-write citadel.yaml / modules.lock).
		JobTypeModuleSet:         {},
		JobTypeServiceStop:       {},
		JobTypeApplyDeviceConfig: {},
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
	// A representative non-member must NOT route there.
	for _, jt := range []string{JobTypeShellCommand, JobTypeFileRead, JobTypeLLMInference, JobTypeMeetingJoin} {
		if needsSerializedLane(jt) {
			t.Errorf("%q must NOT route to the serialized lane", jt)
		}
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
