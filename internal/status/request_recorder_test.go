package status

import (
	"testing"
	"time"
)

// newTestRequestLog returns a requestLog with a controllable clock so
// idle_seconds is deterministic without sleeping, mirroring newTestTracker in
// idle_test.go.
func newTestRequestLog(clock *time.Time) *requestLog {
	l := newRequestLog()
	l.now = func() time.Time { return *clock }
	return l
}

func TestRequestLog_NoRecord_ReportsNever(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	l := newTestRequestLog(&now)

	if _, ok := l.lastRequestAt("ollama"); ok {
		t.Fatal("expected no last-request time for an engine that never received a node-routed request")
	}
	st, ok := l.idleState("ollama", 300*time.Second)
	if ok {
		t.Fatalf("expected idleState ok=false (no local signal), got %+v", st)
	}
}

func TestRequestLog_RecordThenLastRequestAt(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	l := newTestRequestLog(&now)

	l.record("ollama")

	got, ok := l.lastRequestAt("ollama")
	if !ok {
		t.Fatal("expected a recorded request time")
	}
	if !got.Equal(now) {
		t.Fatalf("expected recorded time %v, got %v", now, got)
	}
}

func TestRequestLog_RecordIsCaseAndSpaceInsensitive(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	l := newTestRequestLog(&now)

	l.record(" Bonsai ")

	if _, ok := l.lastRequestAt("bonsai"); !ok {
		t.Fatal("expected the normalized key to match a differently-cased/spaced lookup")
	}
}

func TestRequestLog_Record_BlankIsNoOp(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	l := newTestRequestLog(&now)

	l.record("   ")

	if _, ok := l.lastRequestAt(""); ok {
		t.Fatal("a blank engine name must never be recorded")
	}
}

func TestRequestLog_IdleState_RecentRequestNotIdle(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	l := newTestRequestLog(&now)
	l.record("ollama")

	now = now.Add(10 * time.Second)
	st, ok := l.idleState("ollama", 300*time.Second)
	if !ok {
		t.Fatal("expected a local signal after a recorded request")
	}
	if st.Idle {
		t.Fatal("10s after a request with a 300s threshold must not be idle")
	}
	if st.IdleSeconds != 10 {
		t.Fatalf("expected idle_seconds 10, got %d", st.IdleSeconds)
	}
	if st.LastRequestAt == nil || !st.LastRequestAt.Equal(now.Add(-10*time.Second)) {
		t.Fatalf("expected last_request_at to be the recorded time, got %v", st.LastRequestAt)
	}
}

func TestRequestLog_IdleState_PastThresholdIsIdle(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	l := newTestRequestLog(&now)
	l.record("ollama")

	now = now.Add(301 * time.Second)
	st, ok := l.idleState("ollama", 300*time.Second)
	if !ok {
		t.Fatal("expected a local signal after a recorded request")
	}
	if !st.Idle {
		t.Fatal("301s after a request with a 300s threshold must be idle")
	}
	if st.LastRequestAt == nil {
		t.Fatal("an idle engine still had a real request; last_request_at must not be nil")
	}
}

func TestRequestLog_SeparateEnginesDoNotShareState(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	l := newTestRequestLog(&now)
	l.record("ollama")

	if _, ok := l.lastRequestAt("bonsai"); ok {
		t.Fatal("recording a request for ollama must not leak into bonsai's state")
	}
}

// TestRecordEngineRequest_GlobalSingleton covers the exported entry point
// gateway/worker call in production. It uses the shared process-wide log
// directly, so it only asserts presence/absence (not exact timing) to avoid
// interfering with any other test's use of the singleton within this run.
func TestRecordEngineRequest_GlobalSingleton(t *testing.T) {
	const engine = "test-engine-691-singleton"
	if _, ok := nodeRequestLog.lastRequestAt(engine); ok {
		t.Fatal("expected no prior record for a test-unique engine name")
	}
	RecordEngineRequest(engine)
	if _, ok := nodeRequestLog.lastRequestAt(engine); !ok {
		t.Fatal("expected RecordEngineRequest to stamp the shared process-wide log")
	}
}

// --- Collector.nodeRoutedIdle -----------------------------------------------

func TestCollector_NodeRoutedIdle_NilReqLog(t *testing.T) {
	c := &Collector{} // zero-value: reqLog is nil, matching a bare &Collector{} in other tests
	if got := c.nodeRoutedIdle("ollama"); got != nil {
		t.Fatalf("expected nil with no reqLog wired, got %+v", got)
	}
}

func TestCollector_NodeRoutedIdle_NoRecord(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	c := &Collector{reqLog: newTestRequestLog(&now)}
	if got := c.nodeRoutedIdle("ollama"); got != nil {
		t.Fatalf("expected nil (never recorded), got %+v", got)
	}
}

func TestCollector_NodeRoutedIdle_UsesRecordedRequest(t *testing.T) {
	t.Setenv("SERVICE_IDLE_THRESHOLD_SECONDS", "300")
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	log := newTestRequestLog(&now)
	log.record("bonsai")
	c := &Collector{reqLog: log}

	got := c.nodeRoutedIdle("bonsai")
	if got == nil {
		t.Fatal("expected a non-nil IdleState for a recorded engine")
	}
	if got.Idle {
		t.Fatal("a just-recorded request must not be idle")
	}
	if got.LastRequestAt == nil || !got.LastRequestAt.Equal(now) {
		t.Fatalf("expected last_request_at %v, got %v", now, got.LastRequestAt)
	}
}

// --- mergeNodeRoutedSignal (the central, safe-merge precedence) -----------

// TestMergeNodeRoutedSignal_NoExistingSignal_AdoptsLocal is the "reports
// never -> reports a real timestamp" half of the citadel #691 acceptance
// criteria for a non-vLLM engine with no other idle signal at all (e.g. a
// backstop-reported diffusers/sglang entry with no scrapeable metric and no
// usable NetIO/footprint signal).
func TestMergeNodeRoutedSignal_NoExistingSignal_AdoptsLocal(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	log := newTestRequestLog(&now)
	log.record("ollama")
	c := &Collector{reqLog: log}

	var dst *IdleState
	c.mergeNodeRoutedSignal("ollama", &dst)

	if dst == nil {
		t.Fatal("expected the node-routed record to be adopted when no other signal existed")
	}
	if dst.LastRequestAt == nil || !dst.LastRequestAt.Equal(now) {
		t.Fatalf("expected last_request_at %v, got %v", now, dst.LastRequestAt)
	}
}

// TestMergeNodeRoutedSignal_NeverRecorded_StaysNil is the "received none ->
// still reports never" half of the acceptance criteria.
func TestMergeNodeRoutedSignal_NeverRecorded_StaysNil(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	c := &Collector{reqLog: newTestRequestLog(&now)}

	var dst *IdleState
	c.mergeNodeRoutedSignal("ollama", &dst)

	if dst != nil {
		t.Fatalf("expected no signal to be manufactured, got %+v", dst)
	}
}

// TestMergeNodeRoutedSignal_VLLMScrapePathUnchanged is the "vLLM scrape path
// unchanged" half of the acceptance criteria: when a scraped IdleState is
// already present and agrees with (or is busier than) the local record, it
// must be left untouched -- vLLM's own Prometheus signal always wins on
// content, and here the two agree so nothing should change.
func TestMergeNodeRoutedSignal_VLLMScrapePathUnchanged(t *testing.T) {
	scrapedTime := time.Date(2026, 8, 25, 11, 59, 0, 0, time.UTC) // more recent than the local record
	scraped := &IdleState{Idle: false, IdleSeconds: 42, LastRequestAt: &scrapedTime}

	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	olderLocal := now.Add(-5 * time.Minute)
	log := newTestRequestLog(&now) // clock is "now"; the record itself is backdated below
	log.last["vllm"] = olderLocal  // older and less informative than the scraped signal
	c := &Collector{reqLog: log}

	dst := scraped
	c.mergeNodeRoutedSignal("vllm", &dst)

	if dst != scraped {
		t.Fatalf("expected the same IdleState pointer, got a different one: %+v", dst)
	}
	if dst.LastRequestAt == nil || !dst.LastRequestAt.Equal(scrapedTime) {
		t.Fatalf("expected the scraped (more recent) last_request_at %v to survive, got %v", scrapedTime, dst.LastRequestAt)
	}
	if dst.Idle {
		t.Fatal("expected Idle to remain false")
	}
	if dst.IdleSeconds != 42 {
		t.Fatalf("expected idle_seconds to remain 42, got %d", dst.IdleSeconds)
	}
}

// TestMergeNodeRoutedSignal_NeverIncreasesIdleness is the blocking regression
// case: an existing cascade signal proves the engine busy RIGHT NOW
// (Idle=false, e.g. from the #433 NetIO tier or a GPU-hot override), while the
// node-routed log's only record is old enough to read idle on its own (a
// single dispatch stamped at the start of a long-running generation, still in
// flight past the idle threshold). The merge must NOT flip Idle to true or
// grow IdleSeconds -- a stale local record must never argue FOR more
// idleness than a fresher/more direct signal already ruled out.
func TestMergeNodeRoutedSignal_NeverIncreasesIdleness(t *testing.T) {
	existing := &IdleState{Idle: false, IdleSeconds: 3, LastRequestAt: nil} // e.g. NetIO-derived, no timestamp
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	staleLocal := now.Add(-10 * time.Minute) // long past a 300s threshold
	log := newTestRequestLog(&now)
	log.record("bonsai")
	// Backdate the record directly so idleState() reads it as long-idle.
	log.last["bonsai"] = staleLocal
	c := &Collector{reqLog: log}

	dst := existing
	c.mergeNodeRoutedSignal("bonsai", &dst)

	if dst.Idle {
		t.Fatal("a stale local record must never flip an existing Idle=false to true")
	}
	if dst.IdleSeconds != 3 {
		t.Fatalf("expected idle_seconds to remain 3 (unaffected), got %d", dst.IdleSeconds)
	}
	// LastRequestAt MAY be backfilled (existing had none) -- that is pure
	// addition and does not affect Idle/IdleSeconds.
	if dst.LastRequestAt == nil || !dst.LastRequestAt.Equal(staleLocal) {
		t.Fatalf("expected the stale local timestamp to be backfilled (informational only), got %v", dst.LastRequestAt)
	}
}

// TestMergeNodeRoutedSignal_ReducesIdlenessWhenLocalIsFresher covers the
// allowed direction: an existing cascade signal (e.g. the coarse 60s
// footprint-CPU/GPU heuristic) says idle, but a node-routed request landed
// more recently than the idle threshold. The merge MAY -- and here should --
// flip Idle to false, since a proven recent request is strictly more
// informative than "CPU looked quiet."
func TestMergeNodeRoutedSignal_ReducesIdlenessWhenLocalIsFresher(t *testing.T) {
	existing := &IdleState{Idle: true, IdleSeconds: 90, LastRequestAt: nil} // footprint tier: no timestamp
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	log := newTestRequestLog(&now)
	log.record("bonsai") // just now: not idle under any reasonable threshold
	c := &Collector{reqLog: log}

	dst := existing
	c.mergeNodeRoutedSignal("bonsai", &dst)

	if dst.Idle {
		t.Fatal("expected a fresher, proven request to flip Idle to false")
	}
	if dst.LastRequestAt == nil || !dst.LastRequestAt.Equal(now) {
		t.Fatalf("expected last_request_at %v, got %v", now, dst.LastRequestAt)
	}
}

// --- applyNodeRoutedRequestSignal (the Collect()-level wiring) ------------

// TestApplyNodeRoutedRequestSignal_SkipsStartingServices matches the existing
// !responded contract in collectManagedEngineStatus: a service reported
// Health=starting (did not answer any probe this cycle) must not have a local
// timestamp applied, even with a fresh record on file.
func TestApplyNodeRoutedRequestSignal_SkipsStartingServices(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	log := newTestRequestLog(&now)
	log.record("ollama")
	c := &Collector{reqLog: log}

	status := &NodeStatus{Services: []ServiceInfo{
		{Name: "ollama", Status: ServiceStatusRunning, Health: HealthStatusStarting},
	}}
	c.applyNodeRoutedRequestSignal(status)

	if status.Services[0].IdleState != nil {
		t.Fatalf("a starting service must not surface a node-routed idle signal, got %+v", status.Services[0].IdleState)
	}
}

// TestApplyNodeRoutedRequestSignal_CoversBackstopEngines is the fix for the
// gap the scattered per-producer fallback missed: an engine reported ONLY by
// the collectRunningEmbeddedServices backstop (diffusers, sglang, kokoro,
// transcribe, extraction, lmstudio -- none of which had a fallback wired
// in-line) still gets a last_request_at once it is in status.Services and
// running, because this pass runs once over the fully-assembled list.
func TestApplyNodeRoutedRequestSignal_CoversBackstopEngines(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	log := newTestRequestLog(&now)
	log.record("sglang")
	c := &Collector{reqLog: log}

	status := &NodeStatus{Services: []ServiceInfo{
		{Name: "sglang", Status: ServiceStatusRunning, Health: HealthStatusOK},
	}}
	c.applyNodeRoutedRequestSignal(status)

	if status.Services[0].IdleState == nil {
		t.Fatal("expected a backstop-reported engine to receive the node-routed fallback")
	}
	if !status.Services[0].IdleState.LastRequestAt.Equal(now) {
		t.Fatalf("expected last_request_at %v, got %v", now, status.Services[0].IdleState.LastRequestAt)
	}
}

// TestApplyNodeRoutedRequestSignal_StoppedServiceUntouched guards that a
// stopped service is never given a fabricated idle signal.
func TestApplyNodeRoutedRequestSignal_StoppedServiceUntouched(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	log := newTestRequestLog(&now)
	log.record("ollama")
	c := &Collector{reqLog: log}

	status := &NodeStatus{Services: []ServiceInfo{
		{Name: "ollama", Status: ServiceStatusStopped, Health: HealthStatusOK},
	}}
	c.applyNodeRoutedRequestSignal(status)

	if status.Services[0].IdleState != nil {
		t.Fatalf("a stopped service must never surface an idle signal, got %+v", status.Services[0].IdleState)
	}
}

// TestApplyNodeRoutedRequestSignal_AppsCovered guards the AppInfo path
// (catalog apps), keyed on the app's bare name.
func TestApplyNodeRoutedRequestSignal_AppsCovered(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	log := newTestRequestLog(&now)
	log.record("my-vllm-app")
	c := &Collector{reqLog: log}

	status := &NodeStatus{Apps: []AppInfo{
		{Name: "my-vllm-app", Status: "running"},
	}}
	c.applyNodeRoutedRequestSignal(status)

	if status.Apps[0].IdleState == nil {
		t.Fatal("expected a running app to receive the node-routed fallback")
	}
}

// TestApplyNodeRoutedRequestSignal_NilReqLogNoOp guards a zero-value
// Collector (no reqLog wired, e.g. a test-constructed &Collector{}) against
// panicking or otherwise touching status.
func TestApplyNodeRoutedRequestSignal_NilReqLogNoOp(t *testing.T) {
	c := &Collector{}
	status := &NodeStatus{Services: []ServiceInfo{
		{Name: "ollama", Status: ServiceStatusRunning, Health: HealthStatusOK},
	}}
	c.applyNodeRoutedRequestSignal(status)

	if status.Services[0].IdleState != nil {
		t.Fatalf("expected no signal with reqLog unset, got %+v", status.Services[0].IdleState)
	}
}
