package jobs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/cacheindex"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/status"
	"github.com/aceteam-ai/citadel-cli/services"
)

// --- test fixtures -----------------------------------------------------

// seedHFHubEntry creates a real on-disk hf-hub model dir under cacheRoot and
// upserts the matching index entry, so Index.Verify (which the executor
// calls immediately before every deletion) succeeds. Sets HF_HUB_CACHE so
// deleteHFHubModelDir's hfCacheDir() resolver -- which independently
// computes the SAME "models--org--repo" sanitization -- finds the file
// under this test's cacheRoot instead of the real machine's home directory
// (mirrors TestEvictHuggingFace_RemovesCacheIndexEntry's existing pattern
// in cache_index_test.go).
func seedHFHubEntry(t *testing.T, store *cacheindex.Store, cacheRoot, model string, lastUsed time.Time, source string) {
	t.Helper()
	hubDir := filepath.Join(cacheRoot, services.HFHubCacheDirName, "hub")
	t.Setenv("HF_HUB_CACHE", hubDir)
	dirName := "models--" + strings.ReplaceAll(model, "/", "--") // matches hfCacheDir's own sanitization exactly
	full := filepath.Join(hubDir, dirName)
	if err := os.MkdirAll(full, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(full, "weights.bin"), []byte("weights"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.Upsert(cacheindex.Entry{
		CacheDir:  services.HFHubCacheDirName,
		Family:    services.CacheFamilyHFHub,
		Model:     model,
		Engine:    "vllm",
		Files:     []string{dirName},
		SizeBytes: 7,
		PulledAt:  lastUsed,
		LastUsed:  lastUsed,
		Source:    source,
	}); err != nil {
		t.Fatal(err)
	}
}

func seedGGUFDirEntry(t *testing.T, store *cacheindex.Store, cacheRoot, cacheDirName, engine, model, file string, lastUsed time.Time) {
	t.Helper()
	dir := filepath.Join(cacheRoot, cacheDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, file), []byte("gguf"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.Upsert(cacheindex.Entry{
		CacheDir:  cacheDirName,
		Family:    services.CacheFamilyGGUFDir,
		Model:     model,
		Engine:    engine,
		Files:     []string{file},
		SizeBytes: 4,
		PulledAt:  lastUsed,
		LastUsed:  lastUsed,
	}); err != nil {
		t.Fatal(err)
	}
}

// baseTestDeps returns cacheGCDeps wired to real, deterministic fakes: the
// disk is always "over high water" and never crosses low water (so a test
// can freely opt individual entries out via HighPercent/LowPercent
// overrides), the container runtime is reachable, and nothing is resident
// unless the test says so.
func baseTestDeps(cacheRoot string) cacheGCDeps {
	return cacheGCDeps{
		CacheRoot:              cacheRoot,
		DiskPath:               cacheRoot,
		HighPercent:            90,
		LowPercent:             80,
		MinAge:                 0,
		PinnedModels:           map[string]bool{},
		Now:                    func() time.Time { return time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC) },
		DiskUsedPercent:        func(string) (float64, bool) { return 95, true },
		RuntimeReachable:       func() bool { return true },
		DiscoverEngines:        func(ctx context.Context) []status.LocalEngine { return nil },
		EngineContainerRunning: func(string) bool { return false },
		DeleteEntry:            defaultCacheGCDeleteEntry,
		Logf:                   func(string, string, ...any) {},
	}
}

func openTestStore(t *testing.T) *cacheindex.Store {
	t.Helper()
	return cacheindex.Open(filepath.Join(t.TempDir(), cacheindex.FileName), nil)
}

// --- CacheGCEnabled: default-OFF ----------------------------------------

func TestCacheGCEnabled_DefaultOff(t *testing.T) {
	t.Setenv("CITADEL_CACHE_GC", "")
	if CacheGCEnabled() {
		t.Fatalf("CacheGCEnabled() must default to false when CITADEL_CACHE_GC is unset")
	}
}

func TestCacheGCEnabled_TruthyValues(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", "yes", "on"} {
		t.Setenv("CITADEL_CACHE_GC", v)
		if !CacheGCEnabled() {
			t.Errorf("CacheGCEnabled() = false for %q, want true", v)
		}
	}
	for _, v := range []string{"0", "false", "no", "off", "garbage"} {
		t.Setenv("CITADEL_CACHE_GC", v)
		if CacheGCEnabled() {
			t.Errorf("CacheGCEnabled() = true for %q, want false", v)
		}
	}
}

// --- Resident model is never evicted, even as the LRU candidate over high-water ---

func TestRunCacheGCPass_ResidentModelNeverEvicted(t *testing.T) {
	cacheRoot := t.TempDir()
	store := openTestStore(t)

	// The resident model is the OLDEST entry (best LRU candidate) so this
	// test fails if residency exemption is not applied.
	seedHFHubEntry(t, store, cacheRoot, "org/serving-model", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), cacheindex.SourcePull)
	seedHFHubEntry(t, store, cacheRoot, "org/idle-model", time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), cacheindex.SourcePull)

	deps := baseTestDeps(cacheRoot)
	deps.DiscoverEngines = func(ctx context.Context) []status.LocalEngine {
		return []status.LocalEngine{{Name: "vllm", Port: 8000, Models: []string{"org/serving-model"}}}
	}
	deps.EngineContainerRunning = func(engine string) bool { return engine == "vllm" }
	// Keep disk pressure above high-water for exactly one candidate so the
	// loop has a reason to attempt an eviction at all.
	deps.DiskUsedPercent = func(string) (float64, bool) { return 95, true }

	result := runCacheGCPass(store, deps)

	if _, ok := store.Snapshot().Lookup(services.HFHubCacheDirName, "org/serving-model"); !ok {
		t.Fatalf("resident model must NOT be evicted, but its index entry is gone")
	}
	if _, err := os.Stat(filepath.Join(cacheRoot, services.HFHubCacheDirName, "hub", "models--org--serving-model")); os.IsNotExist(err) {
		t.Fatalf("resident model's on-disk weights must NOT be deleted")
	}
	if _, ok := store.Snapshot().Lookup(services.HFHubCacheDirName, "org/idle-model"); ok {
		t.Fatalf("expected the non-resident idle model to be evicted; result=%+v", result)
	}
	if result.EvictedCount != 1 {
		t.Fatalf("EvictedCount = %d, want 1 (only the idle model)", result.EvictedCount)
	}
}

func TestRunCacheGCPass_ResidentGGUFDirWholeDirExempt(t *testing.T) {
	cacheRoot := t.TempDir()
	store := openTestStore(t)
	seedGGUFDirEntry(t, store, cacheRoot, services.LlamaCppCacheDirName, "llamacpp", "TheBloke/Old-GGUF", "old.gguf", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))

	deps := baseTestDeps(cacheRoot)
	deps.EngineContainerRunning = func(engine string) bool { return engine == "llamacpp" }

	runCacheGCPass(store, deps)

	if _, ok := store.Snapshot().Lookup(services.LlamaCppCacheDirName, "TheBloke/Old-GGUF"); !ok {
		t.Fatalf("gguf-dir entry must be exempt while its owning engine (llamacpp) is running")
	}
}

// --- Pinned model is exempt ----------------------------------------------

func TestRunCacheGCPass_PinnedModelExempt(t *testing.T) {
	cacheRoot := t.TempDir()
	store := openTestStore(t)
	seedHFHubEntry(t, store, cacheRoot, "org/pinned-model", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), cacheindex.SourcePull)
	seedHFHubEntry(t, store, cacheRoot, "org/other-model", time.Date(2020, 2, 1, 0, 0, 0, 0, time.UTC), cacheindex.SourcePull)

	deps := baseTestDeps(cacheRoot)
	deps.PinnedModels = map[string]bool{"org/pinned-model": true}

	runCacheGCPass(store, deps)

	if _, ok := store.Snapshot().Lookup(services.HFHubCacheDirName, "org/pinned-model"); !ok {
		t.Fatalf("pinned model must never be evicted")
	}
	if _, ok := store.Snapshot().Lookup(services.HFHubCacheDirName, "org/other-model"); ok {
		t.Fatalf("expected the non-pinned model to be evicted")
	}
}

// --- Hysteresis: stop at low-water, don't over-evict ---------------------

func TestRunCacheGCPass_HysteresisStopsAtLowWater(t *testing.T) {
	cacheRoot := t.TempDir()
	store := openTestStore(t)
	// Three candidates, oldest first (LRU order): only the two oldest should
	// be evicted before the simulated disk usage crosses low-water.
	seedHFHubEntry(t, store, cacheRoot, "org/oldest", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), cacheindex.SourcePull)
	seedHFHubEntry(t, store, cacheRoot, "org/middle", time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC), cacheindex.SourcePull)
	seedHFHubEntry(t, store, cacheRoot, "org/newest", time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC), cacheindex.SourcePull)

	deps := baseTestDeps(cacheRoot)
	// Percent sequence: 95 (trigger check) -> 92 (loop iter 1, evict oldest)
	// -> 88 (loop iter 2, evict middle) -> 79 (loop iter 3, stop: <= 80 low
	// water, "newest" survives).
	percents := []float64{95, 92, 88, 79}
	call := 0
	deps.DiskUsedPercent = func(string) (float64, bool) {
		p := percents[call]
		if call < len(percents)-1 {
			call++
		}
		return p, true
	}

	result := runCacheGCPass(store, deps)

	if _, ok := store.Snapshot().Lookup(services.HFHubCacheDirName, "org/oldest"); ok {
		t.Errorf("expected org/oldest to be evicted")
	}
	if _, ok := store.Snapshot().Lookup(services.HFHubCacheDirName, "org/middle"); ok {
		t.Errorf("expected org/middle to be evicted")
	}
	if _, ok := store.Snapshot().Lookup(services.HFHubCacheDirName, "org/newest"); !ok {
		t.Errorf("expected org/newest to SURVIVE once usage dropped to/below low-water (hysteresis must not over-evict)")
	}
	if result.EvictedCount != 2 {
		t.Fatalf("EvictedCount = %d, want 2", result.EvictedCount)
	}
	if result.SkipReason != "below_low_water" {
		t.Errorf("SkipReason = %q, want below_low_water", result.SkipReason)
	}
}

func TestRunCacheGCPass_BelowHighWaterNeverEvicts(t *testing.T) {
	cacheRoot := t.TempDir()
	store := openTestStore(t)
	seedHFHubEntry(t, store, cacheRoot, "org/idle-model", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), cacheindex.SourcePull)

	deps := baseTestDeps(cacheRoot)
	deps.DiskUsedPercent = func(string) (float64, bool) { return 50, true } // well under high-water

	result := runCacheGCPass(store, deps)

	if result.SkipReason != "below_high_water" {
		t.Fatalf("SkipReason = %q, want below_high_water", result.SkipReason)
	}
	if result.EvictedCount != 0 {
		t.Fatalf("expected zero evictions below high-water")
	}
	if _, ok := store.Snapshot().Lookup(services.HFHubCacheDirName, "org/idle-model"); !ok {
		t.Fatalf("entry must survive when disk pressure never crossed high-water")
	}
}

// --- Backfill entries are eligible ---------------------------------------

func TestRunCacheGCPass_BackfillEntryIsEligible(t *testing.T) {
	cacheRoot := t.TempDir()
	store := openTestStore(t)
	seedHFHubEntry(t, store, cacheRoot, "org/backfilled", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), cacheindex.SourceBackfill)

	deps := baseTestDeps(cacheRoot)
	result := runCacheGCPass(store, deps)

	if result.EvictedCount != 1 {
		t.Fatalf("expected a SourceBackfill entry to be GC-eligible (Jason's 2026-08-25 decision); result=%+v", result)
	}
	if _, ok := store.Snapshot().Lookup(services.HFHubCacheDirName, "org/backfilled"); ok {
		t.Fatalf("expected the backfill entry to have been evicted")
	}
}

// --- cacheMutationMu prevents racing a concurrent pull --------------------

func TestRunCacheGCPass_SkipsWhenMutationMuHeldByAPull(t *testing.T) {
	cacheRoot := t.TempDir()
	store := openTestStore(t)
	seedHFHubEntry(t, store, cacheRoot, "org/idle-model", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), cacheindex.SourcePull)

	cacheMutationMu.Lock()
	defer cacheMutationMu.Unlock()

	deps := baseTestDeps(cacheRoot)
	result := runCacheGCPass(store, deps)

	if result.SkipReason != "pull_in_flight" {
		t.Fatalf("SkipReason = %q, want pull_in_flight", result.SkipReason)
	}
	if result.EvictedCount != 0 {
		t.Fatalf("expected zero evictions while cacheMutationMu is held by a simulated pull")
	}
	if _, ok := store.Snapshot().Lookup(services.HFHubCacheDirName, "org/idle-model"); !ok {
		t.Fatalf("entry must survive untouched while GC could not acquire cacheMutationMu")
	}
}

func TestModelCachePullHandler_HoldsCacheMutationMu(t *testing.T) {
	// Structural pin: MODEL_CACHE_PULL's dispatch must hold cacheMutationMu
	// for a bad-payload call too (the lock is taken before payload
	// validation), so a concurrent GC TryLock always observes a real pull
	// in flight for the handler's WHOLE body, not just its happy path.
	h := &ModelCachePullHandler{}
	done := make(chan struct{})
	cacheMutationMu.Lock()
	go func() {
		defer close(done)
		// This will block on cacheMutationMu.Lock() inside Execute until we
		// release it below -- if it did NOT block, Execute would return
		// (with a payload error, since Payload is empty) almost instantly.
		_, _ = h.Execute(JobContext{}, &nexus.Job{ID: "job-1", Type: "MODEL_CACHE_PULL", Payload: map[string]string{}})
	}()
	select {
	case <-done:
		t.Fatalf("Execute returned without blocking on cacheMutationMu")
	case <-time.After(50 * time.Millisecond):
		// expected: still blocked
	}
	cacheMutationMu.Unlock()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("Execute did not return after cacheMutationMu was released")
	}
}

// --- Fail-closed: unknown disk usage --------------------------------------

func TestRunCacheGCPass_FailsClosedOnUnknownDiskUsage(t *testing.T) {
	cacheRoot := t.TempDir()
	store := openTestStore(t)
	seedHFHubEntry(t, store, cacheRoot, "org/idle-model", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), cacheindex.SourcePull)

	deps := baseTestDeps(cacheRoot)
	deps.DiskUsedPercent = func(string) (float64, bool) { return 0, false }

	result := runCacheGCPass(store, deps)

	if result.SkipReason != "unknown_disk_usage" {
		t.Fatalf("SkipReason = %q, want unknown_disk_usage", result.SkipReason)
	}
	if result.EvictedCount != 0 {
		t.Fatalf("expected zero evictions when free-space cannot be read")
	}
	if _, ok := store.Snapshot().Lookup(services.HFHubCacheDirName, "org/idle-model"); !ok {
		t.Fatalf("entry must survive when disk usage is unreadable (fail closed)")
	}
}

func TestRunCacheGCPass_FailsClosedOnUnreachableRuntime(t *testing.T) {
	cacheRoot := t.TempDir()
	store := openTestStore(t)
	seedHFHubEntry(t, store, cacheRoot, "org/idle-model", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), cacheindex.SourcePull)

	deps := baseTestDeps(cacheRoot)
	deps.RuntimeReachable = func() bool { return false }

	result := runCacheGCPass(store, deps)

	if result.SkipReason != "runtime_unreachable" {
		t.Fatalf("SkipReason = %q, want runtime_unreachable", result.SkipReason)
	}
	if _, ok := store.Snapshot().Lookup(services.HFHubCacheDirName, "org/idle-model"); !ok {
		t.Fatalf("entry must survive when the residency signal can't be trusted (fail closed)")
	}
}

// --- No candidates -----------------------------------------------------

func TestRunCacheGCPass_NoCandidatesReportsSkipReason(t *testing.T) {
	cacheRoot := t.TempDir()
	store := openTestStore(t)
	// Only entry is pinned -- no eligible candidates.
	seedHFHubEntry(t, store, cacheRoot, "org/pinned", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), cacheindex.SourcePull)

	deps := baseTestDeps(cacheRoot)
	deps.PinnedModels = map[string]bool{"org/pinned": true}

	result := runCacheGCPass(store, deps)
	if result.SkipReason != "no_candidates" {
		t.Fatalf("SkipReason = %q, want no_candidates", result.SkipReason)
	}
}

// --- CacheGCReconciler wiring: single-flight + Stats -----------------------

func TestCacheGCReconciler_NilReceiverIsSafe(t *testing.T) {
	var r *CacheGCReconciler
	r.Reconcile(nil) // must not panic
	if stats := r.Stats(); stats.Enabled {
		t.Fatalf("nil reconciler's Stats() must report Enabled=false")
	}
}

func TestCacheGCReconciler_StatsReflectsRuns(t *testing.T) {
	r := NewCacheGCReconciler(nil, nil)
	r.recordResult(cacheGCRunResult{RanAt: time.Now(), EvictedCount: 2, BytesReclaimed: 500, SkipReason: ""})
	stats := r.Stats()
	if !stats.Enabled {
		t.Fatalf("expected Enabled=true once constructed")
	}
	if stats.TotalReclaimedBytes != 500 {
		t.Fatalf("TotalReclaimedBytes = %d, want 500", stats.TotalReclaimedBytes)
	}
	r.recordResult(cacheGCRunResult{RanAt: time.Now(), SkipReason: "no_candidates"})
	stats = r.Stats()
	if stats.TotalReclaimedBytes != 500 {
		t.Fatalf("TotalReclaimedBytes should accumulate across runs, got %d", stats.TotalReclaimedBytes)
	}
	if stats.LastSkipReason != "no_candidates" {
		t.Fatalf("LastSkipReason = %q, want no_candidates", stats.LastSkipReason)
	}
}
