// internal/jobs/cache_gc.go
//
// P5 disk-pressure GC (citadel #682 P5, docs/design-cache-ownership.md §10):
// the executor half of PlanGC (internal/cacheindex/gc.go, the pure planner).
// This file resolves every live signal PlanGC needs into plain data
// (residency via status.DiscoverLocalEngines + an independent container
// check, pinning via citadel.yaml's pinned_models, disk pressure via
// gopsutil), drives the evict-one-then-remeasure hysteresis loop, and
// deletes cached weights via the SAME per-family resolvers
// MODEL_CACHE_EVICT already trusts (hfCacheDir, llamaCppCacheDir/
// bonsaiCacheDir via CacheDir, BuildOllamaRmCommand) -- so GC never grows a
// second opinion of where a family's files live.
//
// DEFAULT OFF (CacheGCEnabled, gated on CITADEL_CACHE_GC): a node running
// with this unset behaves byte-identically to a pre-P5 node. When enabled,
// GC only ever evicts an entry that is simultaneously: not pinned
// (pinned_models), not resident/serving (status.DiscoverLocalEngines plus
// an independent per-engine container check), and not younger than
// CITADEL_CACHE_GC_MIN_AGE_HOURS -- and only after a fresh Index.Verify
// confirms its recorded files still exist.
//
// cacheMutationMu is the process-wide mutex that keeps GC from racing a
// concurrent pull/evict into the same cache directory (design doc §10.4).
// MODEL_CACHE_PULL/MODEL_CACHE_EVICT/ensureOllamaModel each hold it
// (blocking Lock) for their entire body -- they are already serialized
// against EACH OTHER by the #908 exec-concurrency-1 lane, so this mutex's
// only real job is synchronizing them against GC, which runs entirely
// OUTSIDE that lane on its own goroutine (there is no local job-submission
// path into worker.Runner to dispatch GC as a lane job -- the same gap
// docs/design-model-exclusivity.md §2.3 already found and declined to
// build). GC only ever TryLocks: a pull/evict in flight makes GC skip this
// pass entirely and re-evaluate on the next ~30s heartbeat tick (fail-open
// in the safe direction -- disk at 90% survives another 30s; the #489
// meeting-profile-lock precedent for "TryLock, never block a reconciler").
package jobs

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/cacheindex"
	"github.com/aceteam-ai/citadel-cli/internal/catalog"
	"github.com/aceteam-ai/citadel-cli/internal/resmon"
	"github.com/aceteam-ai/citadel-cli/internal/status"
	"github.com/aceteam-ai/citadel-cli/internal/update"
	"github.com/aceteam-ai/citadel-cli/services"
	"github.com/shirou/gopsutil/v3/disk"
)

// cacheMutationMu is documented in the package doc comment above.
var cacheMutationMu sync.Mutex

// --- Env gating (design doc §10.1, §10.3.3) ---------------------------------

const (
	// cacheGCEnvVar is P5's master opt-in. Default OFF, matching every other
	// destructive/advisory toggle in this codebase (SERVICE_AUTO_STOP_WHEN_IDLE,
	// CITADEL_RESOURCE_ISOLATION, CITADEL_GROUNDING_GUARDRAIL, ...).
	cacheGCEnvVar            = "CITADEL_CACHE_GC"
	cacheGCHighPercentEnvVar = "CITADEL_CACHE_GC_HIGH_PERCENT"
	cacheGCLowPercentEnvVar  = "CITADEL_CACHE_GC_LOW_PERCENT"
	cacheGCMinAgeHoursEnvVar = "CITADEL_CACHE_GC_MIN_AGE_HOURS"

	defaultCacheGCHighPercent = 90.0
	defaultCacheGCLowPercent  = 80.0
	defaultCacheGCMinAgeHours = 24
)

// CacheGCEnabled reports whether the operator has opted into P5 disk-pressure
// GC. Truthy: "1"/"true"/"yes"/"on" (update.IsTruthy, case/whitespace
// insensitive). Default OFF.
func CacheGCEnabled() bool {
	return update.IsTruthy(os.Getenv(cacheGCEnvVar))
}

// cacheGCHighLowPercent resolves the hysteresis pair (design doc §10.1):
// trigger at HIGH, evict down to LOW. An invalid/unparseable value for
// either falls back to that ONE default; an inverted or degenerate pair
// (low >= high, which could never satisfy the "stop at low-water" exit
// condition and would evict without bound) falls back to BOTH defaults
// rather than run with a configuration that can't safely hysteresis.
func cacheGCHighLowPercent() (high, low float64) {
	high = envFloatOrDefault(cacheGCHighPercentEnvVar, defaultCacheGCHighPercent, 0, 100)
	low = envFloatOrDefault(cacheGCLowPercentEnvVar, defaultCacheGCLowPercent, 0, 100)
	if low >= high {
		return defaultCacheGCHighPercent, defaultCacheGCLowPercent
	}
	return high, low
}

func envFloatOrDefault(envVar string, def, min, max float64) float64 {
	v := strings.TrimSpace(os.Getenv(envVar))
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < min || f > max {
		return def
	}
	return f
}

// cacheGCMinAge resolves CITADEL_CACHE_GC_MIN_AGE_HOURS (default 24h): an
// invalid/negative value falls back to the default rather than disabling
// the protection entirely (0 IS accepted explicitly -- an operator who
// really wants no min-age floor can set it to "0").
func cacheGCMinAge() time.Duration {
	v := strings.TrimSpace(os.Getenv(cacheGCMinAgeHoursEnvVar))
	if v == "" {
		return defaultCacheGCMinAgeHours * time.Hour
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return defaultCacheGCMinAgeHours * time.Hour
	}
	return time.Duration(n) * time.Hour
}

// --- Per-family deletion cores -----------------------------------------------

// deleteHFHubModelDir removes the resolved HF hub-cache directory for
// modelName via hfCacheDir -- the SAME resolver evictHuggingFace
// (MODEL_CACHE_EVICT) uses, so GC never grows a second os.RemoveAll with its
// own opinion of where HF-hub weights live (design doc §10.4).
func deleteHFHubModelDir(modelName string) (string, error) {
	cacheDir := hfCacheDir(modelName)
	if cacheDir == "" {
		return "", fmt.Errorf("model %q not found in HuggingFace cache", modelName)
	}
	if err := os.RemoveAll(cacheDir); err != nil {
		return "", fmt.Errorf("failed to remove cache directory %s: %w", cacheDir, err)
	}
	return cacheDir, nil
}

// deleteGGUFDirFile removes one file recorded in a gguf-dir entry's Files
// list (llamacpp or bonsai), given its path relative to that cache_dir.
// Unlike evictLlamaCppGGUF's job-payload path -- an operator-supplied
// modelName, sanitized down to a bare filename because it is UNTRUSTED
// external input -- this accepts a repo-relative path that may include
// subdirectories, exactly what a real cache-index entry can record
// (backfill.go's scanGGUFDir doc comment): the input here is the index's
// OWN previously-recorded provenance, a different trust level. Still guards
// against path traversal escaping cacheRoot/cacheDirName.
func deleteGGUFDirFile(cacheRoot, cacheDirName, relFile string) error {
	dir := filepath.Clean(filepath.Join(cacheRoot, cacheDirName))
	candidate := filepath.Clean(filepath.Join(dir, filepath.FromSlash(relFile)))
	if candidate != dir && !strings.HasPrefix(candidate, dir+string(filepath.Separator)) {
		return fmt.Errorf("invalid gguf file path %q for cache dir %s", relFile, cacheDirName)
	}
	if err := os.Remove(candidate); err != nil {
		return fmt.Errorf("failed to remove GGUF file %s: %w", candidate, err)
	}
	return nil
}

// defaultCacheGCDeleteEntry dispatches deletion by cache family -- the ONE
// per-family routing GC uses, mirroring MODEL_CACHE_EVICT's own dispatch
// (model_cache_evict.go's Execute switch) but operating on an already-
// planned, already-Verified cacheindex.Entry rather than a job payload.
// Only ever called with entries cacheindex.PlanGC actually returned as
// candidates, so a native-family entry here is always a real ollama model
// (PlanGC excludes the lmstudio/tei "_store" aggregate row structurally).
func defaultCacheGCDeleteEntry(cacheRoot string, e cacheindex.Entry) error {
	switch e.Family {
	case services.CacheFamilyHFHub:
		_, err := deleteHFHubModelDir(e.Model)
		return err
	case services.CacheFamilyGGUFDir:
		if len(e.Files) == 0 {
			return fmt.Errorf("gguf-dir entry %s/%s has no recorded files to delete", e.CacheDir, e.Model)
		}
		var firstErr error
		for _, f := range e.Files {
			if err := deleteGGUFDirFile(cacheRoot, e.CacheDir, f); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		return firstErr
	case services.CacheFamilyNative:
		out, err := BuildOllamaRmCommand(e.Model).CombinedOutput()
		if err != nil {
			return fmt.Errorf("ollama rm %s failed: %w: %s", e.Model, err, strings.TrimSpace(string(out)))
		}
		return nil
	default:
		return fmt.Errorf("unknown cache family %q for %s/%s", e.Family, e.CacheDir, e.Model)
	}
}

// --- Live residency signal (design doc §10.3.1) -----------------------------

// gcEngineContainerRunningFn checks whether a services.EngineCacheDirs
// engine's own container is currently running -- computed INDEPENDENTLY of
// status.DiscoverLocalEngines' model-serving probe. This independence is
// deliberate and load-bearing: DiscoverLocalEngines DROPS an engine from its
// result entirely on a model-probe error/timeout (documented, accepted
// behavior for its OWN different consumer, citadel #649), so relying on it
// ALONE for "is this engine running at all" would let a genuinely-serving-
// but-slow-to-answer engine's weights look falsely evictable -- exactly the
// false negative this whole feature must never produce. Overridable for
// tests.
var gcEngineContainerRunningFn = defaultGCEngineContainerRunning

func defaultGCEngineContainerRunning(engineName string) bool {
	containerName := embeddedContainerNameFor(engineName)
	out, err := exec.Command(gcContainerRuntimeBin(), "inspect", "--format", "{{.State.Status}}", containerName).Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "running"
}

// containerRuntimeReachableFn is the global fail-closed check: if the
// container runtime itself can't be reached at all, GC cannot trust ANY
// residency signal derived from it (both gcEngineContainerRunningFn and
// status.DiscoverLocalEngines ultimately shell out to it), so a whole pass
// is skipped rather than risk treating "the runtime is down" as "nothing is
// running" -- the requirement that GC does NOTHING when the live-serving
// signal can't be read. Overridable for tests.
var containerRuntimeReachableFn = defaultContainerRuntimeReachable

func defaultContainerRuntimeReachable() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, gcContainerRuntimeBin(), "ps").Run() == nil
}

func gcContainerRuntimeBin() string {
	bin := catalog.SelectContainerRuntime().EngineBin
	if bin == "" {
		bin = "docker"
	}
	return bin
}

// buildGCResidencyExemptions converts live signals into cacheindex.GCInputs'
// pure residency-exemption data (design doc §10.3.1). `running` is
// status.DiscoverLocalEngines' result (per-engine served models, when a
// probe succeeded); `containerRunning` is gcEngineContainerRunningFn (or a
// test double) -- see that function's doc comment for why the two are
// deliberately independent: containerRunning decides whether an engine is
// resident AT ALL; running's served-model lists only NARROW an
// already-running hf-hub/native engine's exemption from "whole dir" down to
// "just these specific models".
func buildGCResidencyExemptions(running []status.LocalEngine, containerRunning func(engine string) bool) (map[string]bool, map[cacheindex.DirModel]bool) {
	exemptDirs := map[string]bool{}
	exemptModels := map[cacheindex.DirModel]bool{}
	servedByEngine := map[string][]string{}
	for _, e := range running {
		servedByEngine[e.Name] = e.Models
	}
	for engine, ec := range services.EngineCacheDirs {
		if !containerRunning(engine) {
			continue
		}
		switch ec.Family {
		case services.CacheFamilyGGUFDir:
			// Single-engine dir (llamacpp, bonsai) -- the whole-dir rule
			// (design doc §10.3.1) applies unconditionally once the engine
			// is running; per-entry matching is untrustworthy here (a
			// repo-keyed pull entry vs. the engine's served MODEL name).
			exemptDirs[ec.Dir] = true
		case services.CacheFamilyHFHub:
			models, probed := servedByEngine[engine]
			if !probed || len(models) == 0 {
				// Running but we couldn't ask (probe failure) or nothing is
				// currently loaded -- "we couldn't ask" must never read as
				// "not serving" (design doc §10.3.1).
				exemptDirs[ec.Dir] = true
				continue
			}
			for _, m := range models {
				exemptModels[cacheindex.DirModel{CacheDir: ec.Dir, Model: strings.ToLower(m)}] = true
			}
		case services.CacheFamilyNative:
			if engine != "ollama" {
				// lmstudio/tei have no per-model tracking, and their "_store"
				// aggregate row is never a GC candidate in the first place
				// (cacheindex.PlanGC) -- nothing meaningful to exempt here.
				continue
			}
			models, probed := servedByEngine[engine]
			if !probed || len(models) == 0 {
				exemptDirs[ec.Dir] = true
				continue
			}
			for _, m := range models {
				exemptModels[cacheindex.DirModel{CacheDir: ec.Dir, Model: m}] = true
			}
		}
	}
	return exemptDirs, exemptModels
}

// --- Disk-pressure signal (design doc §10.1) --------------------------------

// diskUsedPercentFn resolves the current disk-used percent at path.
// Overridable for tests. ok=false when the signal can't be read at all --
// the fail-closed case: GC must do NOTHING rather than guess.
var diskUsedPercentFn = defaultDiskUsedPercent

func defaultDiskUsedPercent(path string) (float64, bool) {
	u, err := disk.Usage(path)
	if err != nil || u == nil {
		return 0, false
	}
	return u.UsedPercent, true
}

// --- Executor: the evict-one-then-remeasure pass (design doc §10.1/§10.4) --

// cacheGCDeps carries every dependency runCacheGCPass needs as plain
// function values -- the seam that makes the ORCHESTRATION logic (the
// hysteresis loop, the fail-closed rules, the residency/pinning wiring)
// unit-testable against a REAL cacheindex.Store (backed by a t.TempDir())
// without a live docker daemon, a live mesh, or gopsutil.
type cacheGCDeps struct {
	CacheRoot              string
	DiskPath               string
	HighPercent            float64
	LowPercent             float64
	MinAge                 time.Duration
	PinnedModels           map[string]bool
	Now                    func() time.Time
	DiskUsedPercent        func(path string) (float64, bool)
	RuntimeReachable       func() bool
	DiscoverEngines        func(ctx context.Context) []status.LocalEngine
	EngineContainerRunning func(engine string) bool
	DeleteEntry            func(cacheRoot string, e cacheindex.Entry) error
	Logf                   func(level, format string, args ...any)
}

// cacheGCRunResult is runCacheGCPass's outcome, both for logging and for the
// heartbeat's gc sub-struct (design doc §10.4).
type cacheGCRunResult struct {
	RanAt          time.Time
	SkipReason     string
	EvictedCount   int
	BytesReclaimed int64
}

// runCacheGCPass is P5 GC's executor core (design doc §10.4). Order of
// checks is deliberate: the disk-percent read (cheap, no lock needed) comes
// FIRST so a node nowhere near high-water never touches cacheMutationMu or
// the container runtime at all; only once pressure is confirmed do the more
// expensive/contending checks run.
func runCacheGCPass(store *cacheindex.Store, deps cacheGCDeps) cacheGCRunResult {
	now := deps.Now()
	result := cacheGCRunResult{RanAt: now}

	percent, ok := deps.DiskUsedPercent(deps.DiskPath)
	if !ok {
		// Fail closed: an unreadable free-space signal must never be
		// guessed at (design doc §10, the same rule #828's disk preflight
		// and #831's RAM preflight already apply).
		result.SkipReason = "unknown_disk_usage"
		return result
	}
	if percent < deps.HighPercent {
		result.SkipReason = "below_high_water"
		return result
	}
	if !deps.RuntimeReachable() {
		// Fail closed: the residency signal cannot be trusted at all if the
		// runtime itself is unreachable -- see containerRuntimeReachableFn's
		// doc comment.
		result.SkipReason = "runtime_unreachable"
		return result
	}
	if !cacheMutationMu.TryLock() {
		result.SkipReason = "pull_in_flight"
		return result
	}
	defer cacheMutationMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	running := deps.DiscoverEngines(ctx)
	cancel()
	exemptDirs, exemptModels := buildGCResidencyExemptions(running, deps.EngineContainerRunning)

	snapshot := store.Snapshot()
	plan := cacheindex.PlanGC(snapshot.All(), cacheindex.GCInputs{
		Now:          now,
		MinAge:       deps.MinAge,
		PinnedModels: deps.PinnedModels,
		ExemptDirs:   exemptDirs,
		ExemptModels: exemptModels,
	})
	if len(plan.Candidates) == 0 {
		result.SkipReason = "no_candidates"
		return result
	}

	for _, e := range plan.Candidates {
		p, ok := deps.DiskUsedPercent(deps.DiskPath)
		if !ok {
			result.SkipReason = "unknown_disk_usage"
			break
		}
		if p <= deps.LowPercent {
			result.SkipReason = "below_low_water"
			break
		}

		// Re-checked immediately before THIS individual deletion (design
		// doc §10.3: "the plan snapshot is advisory, the pre-delete check
		// is the guarantee") -- reuses the SAME exemptDirs/exemptModels
		// computed once above (one docker/mesh sweep per pass, not one per
		// candidate) rather than re-probing per entry.
		if cacheindex.IsResident(e, exemptDirs, exemptModels) {
			deps.Logf("info", "[cache-gc] skipping %s/%s: became resident since planning", e.CacheDir, e.Model)
			continue
		}

		verified, verifyOK := store.Snapshot().Verify(e, deps.CacheRoot)
		if !verifyOK {
			deps.Logf("warn", "[cache-gc] dropping stale entry %s/%s (recorded files no longer found on disk)", e.CacheDir, e.Model)
			if err := store.Remove(e.CacheDir, e.Model); err != nil {
				deps.Logf("warn", "[cache-gc] failed to drop stale index entry %s/%s: %v", e.CacheDir, e.Model, err)
			}
			continue
		}

		if err := deps.DeleteEntry(deps.CacheRoot, verified); err != nil {
			deps.Logf("warn", "[cache-gc] failed to evict %s/%s (engine=%s): %v", e.CacheDir, e.Model, e.Engine, err)
			continue
		}
		if err := store.Remove(e.CacheDir, e.Model); err != nil {
			deps.Logf("warn", "[cache-gc] index update failed removing %s/%s (files already deleted): %v", e.CacheDir, e.Model, err)
		}

		result.EvictedCount++
		result.BytesReclaimed += verified.SizeBytes
		deps.Logf("info", "[cache-gc] evicted %s/%s (engine=%s, %s, last_used=%s, source=%s)",
			e.CacheDir, e.Model, e.Engine, humanBytes(verified.SizeBytes), formatRecencyOrUnknown(verified), verified.Source)
	}

	if result.SkipReason == "" {
		if result.EvictedCount == 0 {
			result.SkipReason = "below_low_water"
		}
	}
	return result
}

// formatRecencyOrUnknown renders an entry's effective recency (LastUsed if
// known, else PulledAt) for a log line, or "unknown" for a defensive
// both-zero entry (should not occur for a well-formed record).
func formatRecencyOrUnknown(e cacheindex.Entry) string {
	rec := e.LastUsed
	if rec.IsZero() {
		rec = e.PulledAt
	}
	if rec.IsZero() {
		return "unknown"
	}
	return rec.UTC().Format(time.RFC3339)
}

// --- Reconciler: the OnStatus-driven trigger (design doc §10.4) ------------

// CacheGCReconciler drives P5 disk-pressure GC off the heartbeat's existing
// OnStatus fan-out -- the #612/#416/MarkUsed precedent, no new sweep: the
// disk-percent check is one statfs per ~30s tick. Construct via
// NewCacheGCReconciler; cmd/work.go's wrapper (newCacheGCReconciler) returns
// nil when CacheGCEnabled() is false, so a disabled node registers nothing
// at all (the #612 "missingQueues" lesson -- a constructor-time gate, not a
// per-tick no-op, so a disabled node pays zero cost).
type CacheGCReconciler struct {
	pinnedModels map[string]bool
	highPercent  float64
	lowPercent   float64
	minAge       time.Duration
	cacheRoot    string
	diskPath     string
	logf         func(level, format string, args ...any)

	// inFlight is the single-flight guard (the #858 captureStdout "refuse
	// to nest, don't queue" posture): Reconcile is called on every ~30s
	// heartbeat tick, but a GC pass (network-free but disk-I/O-bound,
	// evict-one-then-remeasure) could in principle outlive one tick. A
	// second concurrent pass is refused rather than queued.
	inFlight atomic.Bool

	mu             sync.Mutex
	lastRunAt      time.Time
	lastReclaimed  int64
	totalReclaimed int64
	lastSkipReason string
}

// NewCacheGCReconciler builds a GC reconciler. pinnedModels is the
// pinned_models manifest allowlist (design doc §10.3.2, read ONCE at
// construction -- like PinnedServices elsewhere in this codebase, a
// mid-run manifest edit needs a worker restart to take effect). logf may be
// nil (no-op). Thresholds/min-age are resolved from the environment here,
// once, at construction -- not re-read per tick.
func NewCacheGCReconciler(pinnedModels []string, logf func(level, format string, args ...any)) *CacheGCReconciler {
	high, low := cacheGCHighLowPercent()
	pinned := make(map[string]bool, len(pinnedModels))
	for _, m := range pinnedModels {
		if m = strings.TrimSpace(m); m != "" {
			pinned[m] = true
		}
	}
	if logf == nil {
		logf = func(string, string, ...any) {}
	}
	return &CacheGCReconciler{
		pinnedModels: pinned,
		highPercent:  high,
		lowPercent:   low,
		minAge:       cacheGCMinAge(),
		cacheRoot:    cacheindex.DefaultCacheRoot(),
		diskPath:     resmon.HostDiskPath(),
		logf:         logf,
	}
}

// Reconcile is the OnStatus callback. Nil-receiver-safe so cmd/work.go can
// wire it unconditionally behind the same "if r != nil" pattern
// autoStop/cacheIndexMarkUsedReconciler already use. Never blocks the
// heartbeat: the actual pass runs on its own goroutine, single-flight
// guarded.
func (r *CacheGCReconciler) Reconcile(_ *status.NodeStatus) {
	if r == nil {
		return
	}
	if !r.inFlight.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer r.inFlight.Store(false)
		store := CacheIndexStore()
		if store == nil {
			r.recordResult(cacheGCRunResult{RanAt: time.Now(), SkipReason: "no_index"})
			return
		}
		r.recordResult(runCacheGCPass(store, r.deps()))
	}()
}

func (r *CacheGCReconciler) deps() cacheGCDeps {
	return cacheGCDeps{
		CacheRoot:              r.cacheRoot,
		DiskPath:               r.diskPath,
		HighPercent:            r.highPercent,
		LowPercent:             r.lowPercent,
		MinAge:                 r.minAge,
		PinnedModels:           r.pinnedModels,
		Now:                    time.Now,
		DiskUsedPercent:        diskUsedPercentFn,
		RuntimeReachable:       containerRuntimeReachableFn,
		DiscoverEngines:        status.DiscoverLocalEngines,
		EngineContainerRunning: gcEngineContainerRunningFn,
		DeleteEntry:            defaultCacheGCDeleteEntry,
		Logf:                   r.logf,
	}
}

func (r *CacheGCReconciler) recordResult(res cacheGCRunResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastRunAt = res.RanAt
	r.lastReclaimed = res.BytesReclaimed
	r.totalReclaimed += res.BytesReclaimed
	r.lastSkipReason = res.SkipReason
	if res.EvictedCount > 0 {
		r.logf("info", "[cache-gc] pass complete: evicted %d entries, reclaimed %s", res.EvictedCount, humanBytes(res.BytesReclaimed))
	}
}

// CacheGCStats mirrors status.CacheGCReport (cmd/work.go's cacheGCReportFrom
// projects it) -- internal/status cannot import internal/jobs (jobs already
// imports status), the same reason SwapStats/GPUReservation/LaneSnapshot
// each have a status-package mirror (citadel-cli#717).
type CacheGCStats struct {
	Enabled               bool
	LastRunAt             time.Time
	LastRunReclaimedBytes int64
	TotalReclaimedBytes   int64
	LastSkipReason        string
}

// Stats returns a snapshot for the heartbeat. Nil-receiver-safe: a disabled
// node's cmd/work.go closure loads a possibly-nil reconciler and calls this
// unconditionally, getting the zero value back (Enabled==false tells the
// caller to omit the heartbeat's gc sub-struct entirely).
func (r *CacheGCReconciler) Stats() CacheGCStats {
	if r == nil {
		return CacheGCStats{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return CacheGCStats{
		Enabled:               true,
		LastRunAt:             r.lastRunAt,
		LastRunReclaimedBytes: r.lastReclaimed,
		TotalReclaimedBytes:   r.totalReclaimed,
		LastSkipReason:        r.lastSkipReason,
	}
}
