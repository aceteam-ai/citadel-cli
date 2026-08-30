package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Fallback per-job execution budgets (issue #548). PR #552 added an OPT-IN
// per-job timeout carried in the payload (timeout_ms). But the wedge that
// motivated #548 -- a meeting/transcribe handler stuck in a permission-denied
// retry loop that silently blocked the whole sequential consume loop for 4+
// hours -- happened precisely BECAUSE no backend budget was present: with no
// timeout_ms the handler ran synchronously and unbounded. So the worker now
// applies a GENEROUS default deadline even when the backend sends none, chosen
// per job-type so a legitimately long job (a huge model pull, a long recording)
// is never killed while a genuinely wedged handler is bounded and abandoned.
//
// Precedence: an explicit payload timeout_ms always wins; otherwise the default
// for the job's class applies; a class configured to 0 seconds is unbounded.
const (
	// defaultJobTimeoutSeconds bounds ordinary jobs (inference, shell, file ops,
	// VNC, transcribe, ...). 60min comfortably exceeds every legitimate case --
	// e.g. a single CPU whisper transcription self-bounds at ~32min -- so a job
	// that blows past it is wedged, not merely slow.
	defaultJobTimeoutSeconds = 3600
	// defaultLongJobTimeoutSeconds bounds long-SESSION jobs whose real-world
	// duration is naturally large but finite (a recorded meeting, an
	// interactive co-browse). 4h catches a wedge while not killing a real
	// long meeting.
	defaultLongJobTimeoutSeconds = 14400
)

// jobTimeoutDefaultEnvVar / jobTimeoutLongEnvVar tune the two fallback tiers.
// Following the SERVICE_* env convention already used in the repo. Set either to
// 0 to make that tier unbounded (restore the pre-#548 no-cap behavior).
const (
	jobTimeoutDefaultEnvVar = "WORKER_JOB_TIMEOUT_SECONDS"
	jobTimeoutLongEnvVar    = "WORKER_JOB_TIMEOUT_LONG_SECONDS"
)

// longSessionJobTypes get the generous long-tier fallback. These legitimately
// run for the length of a human session but are still bounded in the real world.
var longSessionJobTypes = map[string]struct{}{
	JobTypeMeetingJoin: {},
	JobTypeCobrowse:    {},
}

// unboundedJobTypes get NO fallback deadline: their duration is dominated by
// external factors (download size / build time / VM clone) with opaque progress,
// so any blanket cap risks killing a legitimate job. They are still bounded when
// the backend sends an explicit timeout_ms, and the self-heal monitor (#548) is
// the backstop if one of these ever truly wedges.
var unboundedJobTypes = map[string]struct{}{
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
}

// serializedLaneJobTypes decides which job types are routed onto the general
// UNBOUNDED EXECUTION LANE (runner.go, exec-concurrency 1) -- a SEPARATE concern
// from unboundedJobTypes above, which decides the per-job WATCHDOG TIER. It is
// the set of job types that read-modify-write the whole citadel.yaml
// (addServiceToManifestFile / setDesiredStatusInManifestFile /
// setEvictedMarkersInManifestFile, internal/jobs/service_handler.go) or
// modules.lock (UpsertLockEntry / DeleteLockEntry, internal/catalog/lockfile.go)
// with NO file lock -- so running two of them concurrently is a whole-file
// read-modify-write race (citadel-cli#908 §1b/§2c). Routing them to a lane with
// exec-concurrency 1 preserves EXACTLY today's implicit single-writer guarantee
// (the sequential fetch loop was that lock) while decoupling claim from execute.
//
// It is a SUPERSET of unboundedJobTypes: every unbounded job is a manifest
// writer or a long opaque job that today ran one-at-a-time on the sequential
// loop, PLUS the manifest/lockfile writers that are NOT unbounded --
// MODULE_SET (drives cmd/module_ops.go's Install/Uninstall, writing both files),
// SERVICE_STOP (setDesiredStatusInManifestFile), and APPLY_DEVICE_CONFIG
// (ConfigHandler.updateManifest, internal/jobs/config_handler.go: a full
// ReadFile -> yaml.Unmarshal -> mutate -> yaml.Marshal -> non-atomic
// os.WriteFile of citadel.yaml). Keeping these OUT of unboundedJobTypes is
// deliberate: they keep their generous default watchdog deadline (they are not
// opaque unbounded work), they only join the serialized EXECUTION lane.
//
// APPLY_DEVICE_CONFIG is the sharp one: it is neither gpu-bound nor
// long-session, so without this membership it fell to the INLINE default branch
// and could truncate-write citadel.yaml CONCURRENTLY with a serialized-lane
// manifest writer (SERVICE_START/SERVICE_STOP/MODULE_SET) executing on the
// exec-cap-1 lane goroutine -- a torn read / lost update on a live node. The
// exec-cap-1 lane reproduces single-writer safety only for jobs ON the lane, so
// every manifest writer must be on it. Membership is pinned by
// TestSerializedLaneJobTypes; extend THIS set (not the routing check in
// runner.go) when a new manifest/lockfile writer is added, mirroring the
// needsGPUSlot/gpuBoundJobTypes precedent.
//
// MODEL_CACHE_EVICT joins the same way, for the identical reason applied to a
// DIFFERENT shared file (citadel-cli#682 P2a, docs/design-cache-ownership.md
// §8.2): it is not itself a citadel.yaml/modules.lock writer, but it now
// mutates internal/cacheindex's cache-index.json (jobs.CacheIndexStore) via a
// read-modify-write with no file lock of its own -- the SAME shape as the
// manifest writers above. Before this it ran on the INLINE default branch and
// could execute CONCURRENTLY with MODEL_CACHE_PULL (already unbounded, and
// therefore already on this lane): a pull's own before/after-size no-op
// detection (llamaCppPullSucceeded's doc comment, internal/jobs/
// model_cache_pull.go) already flagged a concurrent eviction mid-pull as a
// pre-existing accounting hazard, and an os.RemoveAll racing an in-progress
// `hf download` into the SAME hub directory is worse than an accounting bug.
// Routing it here makes the cache index a clean single-writer, closing both.
var serializedLaneJobTypes = func() map[string]struct{} {
	m := map[string]struct{}{
		JobTypeModuleSet:         {},
		JobTypeServiceStop:       {},
		JobTypeApplyDeviceConfig: {},
		JobTypeModelCacheEvict:   {},
	}
	for jt := range unboundedJobTypes {
		m[jt] = struct{}{}
	}
	return m
}()

// needsSerializedLane reports whether jobType executes on the general unbounded
// (exec-concurrency-1) lane. See serializedLaneJobTypes.
func needsSerializedLane(jobType string) bool {
	_, ok := serializedLaneJobTypes[jobType]
	return ok
}

// resolveJobTimeout returns the execution budget the runner should apply to a
// job. An explicit payload timeout_ms wins; otherwise the per-class fallback
// applies. ok=false means "run unbounded" (no watchdog), preserving the exact
// prior behavior for that path.
func (r *Runner) resolveJobTimeout(job *Job) (time.Duration, bool) {
	if d, ok := jobExecTimeout(job); ok {
		return d, true // explicit backend budget always wins
	}
	if job == nil {
		return 0, false
	}
	if _, unbounded := unboundedJobTypes[job.Type]; unbounded {
		return 0, false
	}
	if _, long := longSessionJobTypes[job.Type]; long {
		return envTimeoutSeconds(jobTimeoutLongEnvVar, defaultLongJobTimeoutSeconds)
	}
	return envTimeoutSeconds(jobTimeoutDefaultEnvVar, defaultJobTimeoutSeconds)
}

// envTimeoutSeconds reads a seconds-valued env var, falling back to def. A value
// of 0 (or a negative/garbage value that we clamp) means "unbounded" and returns
// ok=false. A positive value returns that many seconds as a duration.
func envTimeoutSeconds(envVar string, def int) (time.Duration, bool) {
	secs := def
	if v := strings.TrimSpace(os.Getenv(envVar)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			secs = n
		}
	}
	if secs <= 0 {
		return 0, false
	}
	return time.Duration(secs) * time.Second, true
}

// jobTimeoutPayloadKey is the wire field the backend dispatcher injects to give
// a job a per-execution budget (aceteam#6000). It is a RELATIVE duration in
// milliseconds measured from the moment the worker begins executing the job --
// NOT an absolute epoch deadline. A relative duration is deliberate: nodes are
// user-owned hardware, so an absolute deadline would be hostage to clock skew
// between the backend and the node. Keep this string in sync with the backend
// (`python-backend/routes/aceteam_mcp_code.py`).
const jobTimeoutPayloadKey = "timeout_ms"

// jobExecTimeout extracts the optional per-job execution budget from a job
// payload. It returns (duration, true) only when jobTimeoutPayloadKey is present
// and strictly positive; every other case returns ok=false so the caller
// preserves the pre-existing no-timeout behavior exactly.
//
// This keeps the timeout strictly opt-in. Older backends that never set the
// field, and job types that are legitimately unbounded (model download, build,
// provision), are never capped -- there is deliberately no blanket ceiling.
func jobExecTimeout(job *Job) (time.Duration, bool) {
	if job == nil || job.Payload == nil {
		return 0, false
	}
	raw, ok := job.Payload[jobTimeoutPayloadKey]
	if !ok {
		return 0, false
	}
	ms, ok := coerceToInt64(raw)
	if !ok || ms <= 0 {
		return 0, false
	}
	return time.Duration(ms) * time.Millisecond, true
}

// coerceToInt64 best-effort converts a JSON-decoded payload value to an int64.
// Redis payloads reach the worker via json.Unmarshal, so a numeric field is a
// float64; a payload assembled directly may carry int/int64/json.Number, and a
// stringly-typed transport may carry a decimal string. Anything else fails.
func coerceToInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case float32:
		return int64(n), true
	case int:
		return int64(n), true
	case int64:
		return n, true
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return i, true
		}
		if f, err := n.Float64(); err == nil {
			return int64(f), true
		}
		return 0, false
	case string:
		if i, err := strconv.ParseInt(n, 10, 64); err == nil {
			return i, true
		}
		if f, err := strconv.ParseFloat(n, 64); err == nil {
			return int64(f), true
		}
		return 0, false
	default:
		return 0, false
	}
}

// deadlineExceededError marks a handler that was abandoned because it exceeded
// its per-job execution budget. It flows through the SAME terminal-error path
// as any other handler failure, so the backend dispatcher (subscribed to
// stream:v1:{jobId}) receives a fast, honest error instead of hanging until its
// own wait deadline.
type deadlineExceededError struct {
	timeout time.Duration
}

func (e *deadlineExceededError) Error() string {
	return fmt.Sprintf(
		"job exceeded its execution deadline of %s and was abandoned by the worker",
		e.timeout,
	)
}

// executeWithDeadline runs handler.Execute under a child context bounded by
// timeout, but never blocks the job loop past that deadline (aceteam#6000).
//
// The handler runs in its own goroutine. If it honors context cancellation
// (e.g. SHELL_COMMAND via exec.CommandContext) the underlying child process is
// terminated; if it ignores cancellation the goroutine keeps running in the
// background while this function returns and the loop advances. Either way one
// wedged handler can no longer stall every subsequent job on the node.
//
// On timeout it returns a *deadlineExceededError; the caller's existing failure
// path publishes the terminal error event and Nacks on the LIVE parent context
// (never the expired child) so the dispatcher receives a real error event.
func (r *Runner) executeWithDeadline(
	ctx context.Context,
	handler JobHandler,
	job *Job,
	stream StreamWriter,
	timeout time.Duration,
) (*JobResult, error) {
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	type handlerResult struct {
		result *JobResult
		err    error
	}
	// Buffered (size 1) so a handler that ignores cancellation and finishes
	// AFTER the deadline can still send its result and exit, rather than leaking
	// blocked on the channel forever.
	done := make(chan handlerResult, 1)
	go func() {
		result, err := handler.Execute(execCtx, job, stream)
		done <- handlerResult{result: result, err: err}
	}()

	select {
	case hr := <-done:
		// If the handler returned an error exactly as the deadline elapsed (e.g.
		// exec.CommandContext killed the child, yielding "signal: killed"),
		// prefer the clear deadline message over the incidental one.
		if hr.err != nil && errors.Is(execCtx.Err(), context.DeadlineExceeded) {
			return nil, &deadlineExceededError{timeout: timeout}
		}
		return hr.result, hr.err
	case <-execCtx.Done():
		if errors.Is(execCtx.Err(), context.DeadlineExceeded) {
			r.log("error", "Job %s abandoned: exceeded execution deadline of %s", job.ID, timeout)
			return nil, &deadlineExceededError{timeout: timeout}
		}
		// Parent context cancelled (worker shutdown): surface the raw error so
		// the loop unwinds without misreporting a deadline breach.
		return nil, execCtx.Err()
	}
}
