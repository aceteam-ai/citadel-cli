package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/signal"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/usage"
)

// Runner orchestrates job processing from a source through handlers.
type Runner struct {
	source       JobSource
	handlers     []JobHandler
	config       RunnerConfig
	agentVersion string

	// Optional integrations (set via WithXxx methods)
	streamWriterFactory func(job *Job) StreamWriter
	activityFn          func(level, msg string)
	jobRecordFn         func(record usage.UsageRecord)

	// Concurrency support
	maxConcurrency int
	gpuTracker     *GPUTracker

	// Bounded execution lanes (citadel-cli#908, aceteam#8254). unboundedLane
	// carries every manifest/lockfile-writing job type (serializedLaneJobTypes)
	// at exec-concurrency 1 -- preserving today's implicit single-writer
	// guarantee off the fetch loop. inferenceLane carries GPU-bound inference at
	// exec-concurrency = GPUTracker.Total() with a bounded queue wait; it is nil
	// on a node with no discrete GPU (GPUTracker nil or Total()<1), where
	// GPU-bound jobs keep the pre-#908 inline/sequential fallback. See the lane
	// routing in Run.
	unboundedLane      *lane
	inferenceLane      *lane
	inferenceQueueWait time.Duration

	// state, when set, records live introspection metrics (poll time, job
	// counts) for the out-of-band status/control path (issue #236).
	state *WorkerState

	// Lifecycle observability for safe self-update.
	// activeJobs counts jobs currently executing in a handler.
	// draining, when set, stops the run loop from fetching new jobs so
	// in-flight work can finish before the process is replaced/restarted.
	activeJobs int64
	draining   int32
}

// RunnerConfig holds configuration for the runner.
type RunnerConfig struct {
	// WorkerID identifies this worker instance
	WorkerID string

	// NodeID is this node's Headscale numeric node ID (e.g. "758"). It is the
	// identity the target_node filter compares against: a job addressed to a
	// different node is acknowledged and skipped.
	//
	// An empty NodeID means this node could not resolve its own identity. The
	// filter stays ACTIVE in that case (citadel-cli#654) -- an unidentified node
	// matches no target_node at all, so it declines every addressed job rather
	// than claiming work meant for a peer. Untargeted jobs are unaffected.
	NodeID string

	// AgentVersion is this node's citadel build version (e.g. "v2.46.0").
	// It is surfaced in the structured failure for an unsupported job type so
	// the backend can render an actionable "node on vX.Y.Z doesn't support TYPE
	// -- update the node" message instead of an opaque dispatch timeout (#382).
	AgentVersion string

	// Verbose enables detailed logging
	Verbose bool

	// ActivityFn is called for log messages (if set, suppresses stdout)
	ActivityFn func(level, msg string)

	// JobRecordFn is called when a job completes (for usage tracking)
	JobRecordFn func(record usage.UsageRecord)

	// MaxConcurrency is the max number of concurrent jobs (0 or 1 = sequential)
	MaxConcurrency int

	// GPUTracker manages GPU slot allocation (optional, for GPU-aware jobs)
	GPUTracker *GPUTracker

	// State, when set, is updated with live introspection metrics so the
	// status/control path can report consume/job activity (issue #236).
	State *WorkerState
}

// NewRunner creates a new job runner.
func NewRunner(source JobSource, handlers []JobHandler, config RunnerConfig) *Runner {
	r := &Runner{
		source:             source,
		handlers:           handlers,
		config:             config,
		agentVersion:       config.AgentVersion,
		activityFn:         config.ActivityFn,
		jobRecordFn:        config.JobRecordFn,
		maxConcurrency:     config.MaxConcurrency,
		gpuTracker:         config.GPUTracker,
		state:              config.State,
		inferenceQueueWait: resolveInferenceQueueWait(),
	}

	// The general unbounded lane always exists (exec-concurrency 1): it is the
	// relocated single-writer lock over the unlocked manifest/lockfile paths.
	r.unboundedLane = newLane("unbounded", resolveUnboundedLaneQueue(), 1, false)

	// The inference admission queue exists ONLY on a node with a real discrete
	// GPU (a non-nil tracker with >=1 slot). On a GPU-less node that still
	// serves GPU-typed inference (CPU-only, native ollama, Apple Silicon), it
	// stays nil and GPU-bound jobs fall through to the pre-#908 inline/sequential
	// path -- the only backpressure such a node has against its single serving
	// engine (the #903 nil-tracker gate, preserved). Total()<1 would make an
	// unbuffered exec channel that could never run a job, so it is excluded too.
	if config.GPUTracker != nil && config.GPUTracker.Total() >= 1 {
		total := config.GPUTracker.Total()
		r.inferenceLane = newLane("inference", total*3, total, true)
	}

	return r
}

// log outputs a message - uses activity callback if set, otherwise prints to stdout/stderr
func (r *Runner) log(level, format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	if r.activityFn != nil {
		r.activityFn(level, msg)
	} else {
		// Fall back to stdout/stderr
		if level == "error" || level == "warning" {
			fmt.Fprintf(os.Stderr, "%s\n", msg)
		} else {
			fmt.Printf("%s\n", msg)
		}
	}
}

// consumeStatusReporter is implemented by sources that can report the HTTP
// status of their most recent consume call (currently APISource via the
// redisapi client). Used to surface the pre-fix #3924 400s (issue #236).
type consumeStatusReporter interface {
	LastConsumeStatus() int
}

// fetchErrLogLevel decides whether (and how loudly) a job-fetch failure on the
// Nth consecutive cycle should be surfaced to the activity log. It returns
// ("", false) to stay silent. The policy: announce the first blip quietly
// (info), escalate to a single warning once failures are sustained (== threshold),
// then re-warn sparingly (every `repeat` cycles) while it keeps failing.
func fetchErrLogLevel(consecutive, threshold, repeat int) (level string, shouldLog bool) {
	switch {
	case consecutive == 1:
		return "info", true
	case consecutive == threshold:
		return "warning", true
	case consecutive > threshold && repeat > 0 && (consecutive-threshold)%repeat == 0:
		return "warning", true
	default:
		return "", false
	}
}

// recordConsumeStatus copies the source's last consume HTTP status and error
// into the introspection state after each poll cycle.
func (r *Runner) recordConsumeStatus(pollErr error) {
	if r.state == nil {
		return
	}
	status := 0
	if rep, ok := r.source.(consumeStatusReporter); ok {
		status = rep.LastConsumeStatus()
	}
	errStr := ""
	if pollErr != nil {
		errStr = pollErr.Error()
	}
	r.state.RecordConsumeStatus(status, errStr)
}

// recordJob records a job completion for usage tracking
func (r *Runner) recordJob(record usage.UsageRecord) {
	if r.jobRecordFn != nil {
		r.jobRecordFn(record)
	}
}

// ActiveJobs returns the number of jobs currently executing in a handler.
// It is safe to call concurrently and is used by the auto-updater to find an
// idle moment before swapping the binary.
func (r *Runner) ActiveJobs() int {
	return int(atomic.LoadInt64(&r.activeJobs))
}

// Drain signals the run loop to stop fetching new jobs. In-flight jobs are
// allowed to finish. This is used by the auto-updater so that no new work is
// picked up once an update has been downloaded and is ready to apply.
func (r *Runner) Drain() {
	atomic.StoreInt32(&r.draining, 1)
}

// isDraining reports whether Drain has been called.
func (r *Runner) isDraining() bool {
	return atomic.LoadInt32(&r.draining) == 1
}

// IsDraining is the exported view of isDraining, used by the self-heal monitor
// to skip a wedge check while the loop is intentionally paused for an
// auto-update drain (issue #548).
func (r *Runner) IsDraining() bool {
	return r.isDraining()
}

// WithStreamWriterFactory sets a factory for creating stream writers.
// If not set, a NoOpStreamWriter is used.
func (r *Runner) WithStreamWriterFactory(factory func(job *Job) StreamWriter) *Runner {
	r.streamWriterFactory = factory
	return r
}

// Run starts the job processing loop.
// This method blocks until the context is cancelled or a signal is received.
// When MaxConcurrency > 1, jobs are processed concurrently via a goroutine pool.
func (r *Runner) Run(ctx context.Context) error {
	// Setup signal handling
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	// Connect to source
	r.log("info", "Starting Worker (%s)", r.source.Name())
	if err := r.source.Connect(ctx); err != nil {
		return fmt.Errorf("failed to connect to %s: %w", r.source.Name(), err)
	}
	defer r.source.Close()

	// Resolve concurrency
	concurrency := r.maxConcurrency
	if concurrency <= 0 {
		concurrency = 1
	}

	if r.activityFn == nil {
		// Only show verbose startup info if not in TUI mode
		fmt.Printf("   - Worker ID: %s\n", r.config.WorkerID)
		fmt.Printf("   - Source: %s\n", r.source.Name())
		fmt.Printf("   - Handlers: %d registered\n", len(r.handlers))
		fmt.Printf("   - Max Concurrency: %d\n", concurrency)
	}
	r.log("success", "Worker started, listening for jobs...")

	// Semaphore for concurrent job processing
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup

	// Main processing loop with exponential backoff on errors
	backoff := time.Second
	const maxBackoff = 30 * time.Second

	// Job-fetch failures (consume timeouts, transient 5xx during a backend
	// deploy/failover) are normal and self-healing: the loop just backs off and
	// retries. Logging each one as a warning floods the activity panel and reads
	// like the node is broken. Coalesce instead — stay quiet through brief blips,
	// escalate to a single warning only once failures are sustained, then repeat
	// sparingly with a running count, and announce recovery.
	const (
		sustainedFetchErrThreshold = 5  // cycles before a transient blip becomes a warning
		sustainedFetchErrRepeat    = 10 // re-warn every N cycles while still failing
	)
	consecutiveFetchErrs := 0

runLoop:
	for {
		select {
		case sig := <-sigs:
			r.log("info", "Received signal %v, shutting down...", sig)
			cancel()
			break runLoop
		case <-ctx.Done():
			break runLoop
		default:
			// Stop fetching new jobs once draining (e.g. an auto-update is
			// ready to apply). In-flight jobs continue to completion below.
			if r.isDraining() {
				select {
				case <-time.After(200 * time.Millisecond):
				case <-ctx.Done():
					break runLoop
				}
				continue
			}

			// Fetch next job
			job, err := r.source.Next(ctx)
			// Record the poll cycle for introspection regardless of outcome,
			// so the status path can report "last successful poll time" and
			// whether the worker is actively consuming (issue #236).
			r.state.RecordPoll()
			r.recordConsumeStatus(err)
			if err != nil {
				if ctx.Err() != nil {
					break runLoop // Context cancelled
				}
				consecutiveFetchErrs++
				if level, ok := fetchErrLogLevel(consecutiveFetchErrs, sustainedFetchErrThreshold, sustainedFetchErrRepeat); ok {
					if consecutiveFetchErrs == 1 {
						// First blip in a streak: record quietly (persisted to the
						// log, info-level so it doesn't alarm) and let backoff retry.
						r.log(level, "Job fetch retrying (backoff %s): %v", backoff, err)
					} else {
						r.log(level, "Job fetching has failed %d cycles in a row: %v", consecutiveFetchErrs, err)
					}
				}
				select {
				case <-time.After(backoff):
				case <-ctx.Done():
					break runLoop
				}
				// Exponential backoff up to max
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
				continue
			}

			// Reset backoff on success, and announce recovery if we had
			// previously escalated to a sustained-failure warning.
			if consecutiveFetchErrs >= sustainedFetchErrThreshold {
				r.log("success", "Job fetching recovered after %d failed cycles", consecutiveFetchErrs)
			}
			consecutiveFetchErrs = 0
			backoff = time.Second

			if job == nil {
				continue // No job available, loop again
			}

			// CLAIM (synchronous, in this fetch-loop goroutine): publish the
			// claim-ack, run the target-node filter + cancellation check. This
			// is the fast half -- no handler work, no lane wait -- so the
			// backend's short claim-ack window sees a claim the instant a job is
			// read, no matter how long EXECUTION ends up taking or waiting
			// (citadel-cli#908 §2a). A non-proceed result (foreign target, or
			// cancelled) is fully handled inside claimJob (Ack/terminal), so the
			// loop just moves on.
			proceed, stream, startTime := r.claimJob(ctx, job)
			if !proceed {
				continue
			}

			// EXECUTE dispatch. The fetch loop NEVER blocks on execution: it
			// either admits onto a bounded lane (and loops back to source.Next
			// immediately) or hands off to an async goroutine / the semaphore
			// pool / an inline call. All waiting for an execution slot happens
			// OFF this goroutine (citadel-cli#908 §2b).
			_, longSession := longSessionJobTypes[job.Type]
			gpuBound := needsGPUSlot(job.Type) && r.gpuTracker != nil
			switch {
			case r.unboundedLane != nil && needsSerializedLane(job.Type):
				// General unbounded lane (exec-concurrency 1): SERVICE_START,
				// model pulls, builds, MODULE_SET, SERVICE_STOP, ... -- every
				// manifest/lockfile writer, serialized off the fetch loop. This
				// is the direct #908 fix: a long deploy no longer blocks the node
				// from claiming a FILE_READ_BYTES behind it.
				r.dispatchLane(ctx, r.unboundedLane, job, stream, startTime, &wg)
			case r.inferenceLane != nil && gpuBound:
				// Inference admission queue (aceteam#8254): bounded queue-on-full
				// (returns model_warming, never a silent Nack) instead of the
				// bare #825 Nack. The in-executeJob GPU-slot gate is retained;
				// with the lane's exec-concurrency = GPUTracker.Total() it stays
				// in lockstep in production, so the queue-wait is the real
				// backpressure and the #825 Nack becomes effectively unreachable.
				r.dispatchLane(ctx, r.inferenceLane, job, stream, startTime, &wg)
			case longSession || gpuBound:
				// #489 long-session lane (MEETING_JOIN/COBROWSE), plus the
				// degenerate GPU-bound-but-no-inference-lane case (a tracker with
				// Total()<1, test-only) which still reaches the #825 GPU-slot
				// Nack inside executeJob. Unbounded always-async goroutine,
				// independent of the semaphore.
				r.enterJob()
				wg.Add(1)
				go func(j *Job, s StreamWriter, st time.Time) {
					defer wg.Done()
					jobOK := false
					defer func() { r.exitJob(jobOK) }()
					jobOK = r.executeJob(ctx, j, s, st, false, 0)
				}(job, stream, startTime)
			case concurrency > 1:
				// Semaphore-gated concurrent pool.
				r.enterJob()
				sem <- struct{}{} // Acquire semaphore slot
				wg.Add(1)
				go func(j *Job, s StreamWriter, st time.Time) {
					defer wg.Done()
					defer func() { <-sem }() // Release semaphore slot
					jobOK := false
					defer func() { r.exitJob(jobOK) }()
					jobOK = r.executeJob(ctx, j, s, st, false, 0)
				}(job, stream, startTime)
			default:
				// Inline (sequential maxConcurrency=1 node): a non-laned job
				// (shell, file, config, GPU-bound on a tracker-less node) still
				// runs on the fetch-loop goroutine, exactly as before -- the lane
				// fix targets only the manifest-writers and GPU inference that
				// used to block here.
				r.enterJob()
				jobOK := r.executeJob(ctx, job, stream, startTime, false, 0)
				r.exitJob(jobOK)
			}
		}
	}

	// Wait for in-flight jobs to complete
	wg.Wait()

	r.log("info", "Worker shutdown complete")
	return nil
}

// newStreamWriter builds the per-job stream writer, falling back to a no-op
// writer when no factory is configured (e.g. Nexus HTTP source).
func (r *Runner) newStreamWriter(job *Job) StreamWriter {
	if r.streamWriterFactory != nil {
		return r.streamWriterFactory(job)
	}
	return &NoOpStreamWriter{}
}

// streamWriteRetryAttempts / streamWriteRetryBackoff bound the retry
// citadel-cli#985 adds to WriteClaimed and WriteEnd: a single lost publish
// (a transient WS write failure, a flaky HTTP POST) used to be permanent --
// the coordinator's short claim-ack window then fast-fails a job that
// actually succeeded, and for WriteEnd the result is never delivered at all.
// Package vars (not consts) so a test can shrink both without eating the real
// backoff; production defaults are a couple of quick attempts, not a long
// retry campaign -- this runs synchronously in the fetch-loop goroutine for
// WriteClaimed (claimJob), so a wedged transport must not stall job pickup
// for long.
var (
	streamWriteRetryAttempts = 3
	streamWriteRetryBackoff  = []time.Duration{100 * time.Millisecond, 200 * time.Millisecond}
)

// retryStreamWrite calls write up to streamWriteRetryAttempts times, waiting
// streamWriteRetryBackoff[attempt] between attempts, and returns the error
// from the LAST attempt (nil on success). The first attempt is always made
// immediately -- the happy path (success on attempt 1) adds zero latency.
//
// It stops waiting out a backoff early if ctx is cancelled (worker shutdown,
// or a test/job context that has already expired), returning whatever error
// the most recent attempt produced rather than treating cancellation as
// success. Callers treat the returned error as non-fatal either way (log and
// proceed) -- retryStreamWrite only gives the publish more chances, it does
// not change what happens when they are all exhausted.
func retryStreamWrite(ctx context.Context, write func() error) error {
	var err error
	for attempt := 0; attempt < streamWriteRetryAttempts; attempt++ {
		if err = write(); err == nil {
			return nil
		}
		if attempt == streamWriteRetryAttempts-1 {
			break
		}
		backoff := streamWriteRetryBackoff[attempt]
		if attempt >= len(streamWriteRetryBackoff) {
			backoff = streamWriteRetryBackoff[len(streamWriteRetryBackoff)-1]
		}
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return err
		}
	}
	return err
}

// errLaneSaturated is the Nack error when a claimed job cannot be admitted onto
// its lane because the lane is at its admission bound. Like the #825 GPU-slot
// Nack, this is a transparent, non-terminal retry (no stream publish): the job
// was claimed (WriteClaimed already fired in claimJob) but never admitted, so
// nothing decremented and there is nothing to undo (citadel-cli#908 §2b).
var errLaneSaturated = errors.New("execution lane saturated; retry")

// enterJob does the synchronous in-flight accounting for a claimed job that is
// being dispatched for execution: it increments the introspection in-flight
// bracket (RecordJobReceived) and the auto-updater's activeJobs counter. Pair
// with exitJob. Deliberately NOT called for a job rejected at a lane's admission
// bound (that job was claimed but never admitted -- see errLaneSaturated), so
// its counters never move and InFlight can always return to 0.
func (r *Runner) enterJob() {
	r.state.RecordJobReceived()
	atomic.AddInt64(&r.activeJobs, 1)
}

// exitJob is enterJob's terminal counterpart: it releases the activeJobs slot
// and classifies the outcome (RecordJobDone). Counting queued-but-not-yet-
// executing jobs in activeJobs is deliberate -- it keeps the auto-updater from
// swapping the binary out from under a job that has been claimed (WriteClaimed
// published) but is still waiting for a lane slot.
func (r *Runner) exitJob(jobOK bool) {
	atomic.AddInt64(&r.activeJobs, -1)
	r.state.RecordJobDone(jobOK)
}

// claimJob is the fast, synchronous half of the old processJob (citadel-cli#908
// §2a): it runs in the fetch-loop goroutine so the backend's short claim-ack
// window sees a claim the instant a job is read, independent of how long
// EXECUTION takes or waits for a lane slot. It performs the target-node filter,
// publishes the claim-ack, and checks cancellation. It does NOT touch the
// in-flight counters (enterJob owns that at dispatch, so a job rejected at a
// lane's admission bound never increments them). A false `proceed` means claimJob
// fully handled the job (Ack + any terminal event) and the caller must stop.
func (r *Runner) claimJob(ctx context.Context, job *Job) (proceed bool, stream StreamWriter, startTime time.Time) {
	startTime = time.Now()
	r.log("info", "Received job %s (type: %s)", job.ID, job.Type)

	// Target-node filter: when per-node consumer groups are used, every node
	// sees every message on the shared org queue. If the job specifies a
	// target_node that doesn't match this node's Headscale ID, acknowledge
	// and skip it -- the addressed node processes it from its own read
	// position, and our Ack only clears OUR consumer group's pending list.
	//
	// The filter is UNCONDITIONAL, including when this node's own ID is unknown
	// (citadel-cli#654). It used to be gated on NodeID != "", which made an
	// identity failure fail OPEN: a node that could not resolve its Headscale ID
	// matched nothing, so it claimed and executed every addressed job it saw off
	// a shared stream -- including SHELL_COMMAND/terminal_exec work an operator
	// aimed at a DIFFERENT machine -- while the real target sat waiting out its
	// timeout. Failing closed costs a timeout on the addressed job and says so in
	// the log; failing open ran it on the wrong host. The platform's per-node
	// streams (aceteam#6889) remove most of the blast radius, but it still falls
	// back to the shared stream during a mixed-version rollout, where this pin is
	// the only thing routing the job.
	if targetNode, ok := job.Payload["target_node"].(string); ok && targetNode != "" && targetNode != r.config.NodeID {
		if r.config.NodeID == "" {
			r.log("warning", "Declining job %s: addressed to target_node=%s but this node's Headscale ID is unresolved, "+
				"so it cannot claim addressed work (citadel-cli#654)", job.ID, targetNode)
		} else {
			r.log("info", "Skipping job %s: target_node=%s (this node=%s)", job.ID, targetNode, r.config.NodeID)
		}
		r.source.Ack(ctx, job)
		return false, nil, startTime
	}

	// Claim-ack (aceteam#6000): publish a lightweight "claimed" event the moment
	// this node takes ownership of the job -- after the target-node filter (so
	// only the owning node claims a shared-stream message) and before any
	// handler work. The backend dispatcher waits a short window for this event;
	// a wedged or dead-but-heartbeating node never reaches this line, so the
	// dispatcher fast-fails in ~3s instead of burning the full result budget.
	//
	// Best-effort, but retried (citadel-cli#985): a single lost publish here
	// used to be permanent, and the coordinator's fast-fail depends on this
	// event arriving -- a job that actually claimed, ran, and completed in
	// under a second was reported "unreachable" because exactly this one
	// publish never landed. retryStreamWrite gives it a couple of quick extra
	// chances before this still falls back to a log-and-proceed non-fatal
	// warning, same as before.
	stream = r.newStreamWriter(job)
	if err := retryStreamWrite(ctx, func() error { return stream.WriteClaimed(r.agentVersion) }); err != nil {
		r.log("warning", "Failed to publish claimed event for job %s: %v", job.ID, err)
	}

	// JQS-Core Section 5.6: Check cancellation before processing
	if r.source.IsJobCancelled(ctx, job.ID) {
		r.log("info", "Job %s was cancelled before processing", job.ID)
		if err := stream.WriteCancelled("Job cancelled before processing"); err != nil {
			r.log("warning", "Failed to publish cancelled event for job %s: %v", job.ID, err)
		}
		r.recordJob(buildUsageRecord(job, "cancelled", startTime, time.Now(), nil, nil))
		r.source.Ack(ctx, job)
		return false, nil, startTime
	}

	return true, stream, startTime
}

// dispatchLane admits a claimed job onto a bounded lane and spawns the goroutine
// that runs it (citadel-cli#908 §2b). The admission send is NON-BLOCKING: on
// success the fetch loop immediately loops back to source.Next; at the admission
// bound the job is Nacked now (transparent retry) and never enters the lane.
// The spawned goroutine holds the admit slot for its whole life (queued +
// executing) and releases it on return.
func (r *Runner) dispatchLane(ctx context.Context, l *lane, job *Job, stream StreamWriter, startTime time.Time, wg *sync.WaitGroup) {
	if !l.tryAdmit() {
		// Admission bound reached: Nack now (non-terminal, no stream publish),
		// exactly the shape of the #825 GPU-slot-full Nack. enterJob was NOT
		// called, so no counter is left dangling.
		r.log("warning", "%s lane saturated (job %s, type %s); nacking for redelivery", l.name, job.ID, job.Type)
		r.source.Nack(ctx, job, errLaneSaturated)
		return
	}
	r.enterJob()
	wg.Add(1)
	go func(j *Job, s StreamWriter, st time.Time) {
		defer wg.Done()
		jobOK := false
		defer func() {
			r.exitJob(jobOK)
			l.releaseAdmit()
		}()
		jobOK = r.runLaneJob(ctx, l, j, s, st)
	}(job, stream, startTime)
}

// runLaneJob acquires the lane's execution slot (the wait happens here, off the
// fetch-loop goroutine) and then runs executeJob. The exec-acquire discipline
// depends on the lane:
//   - general/unbounded lane (hasExecWait=false): unbounded wait for the single
//     exec slot -- the relocated single-writer serialization; it only unblocks
//     early on shutdown/cancel, which Nacks the still-queued job.
//   - inference lane (hasExecWait=true): BOUNDED wait; on expiry the node returns
//     the existing model_warming backpressure signal (a SUCCESS the platform
//     already retries), never a silent Nack (aceteam#8254 §3a). This is the
//     queue-on-full that replaces the bare #825 Nack for inference under load.
func (r *Runner) runLaneJob(ctx context.Context, l *lane, job *Job, stream StreamWriter, startTime time.Time) (jobOK bool) {
	queueStart := time.Now()

	if l.hasExecWait {
		timer := time.NewTimer(r.inferenceQueueWait)
		defer timer.Stop()
		select {
		case l.exec <- struct{}{}:
			timer.Stop()
		case <-timer.C:
			// Queue-wait exceeded: return the model_warming signal through the
			// SAME success-path terminal publish a normal completion uses, so
			// there is exactly one terminal-publish implementation (never a
			// silent Nack that reintroduces the #559 no-terminal-event bug).
			return r.finishQueueWaitExceeded(ctx, job, stream, queueStart)
		case <-ctx.Done():
			// Cancelled/shutting down while queued: never executed. Nack for
			// redelivery so shutdown can't deadlock on a queued job.
			r.source.Nack(ctx, job, ctx.Err())
			return false
		}
	} else {
		select {
		case l.exec <- struct{}{}:
		case <-ctx.Done():
			r.source.Nack(ctx, job, ctx.Err())
			return false
		}
	}
	defer func() { <-l.exec }()
	l.beginExec()
	defer l.endExec()

	queueWaitMs := time.Since(queueStart).Milliseconds()
	// Only the inference lane emits per-request latency metrics (aceteam#8254 §3c).
	return r.executeJob(ctx, job, stream, startTime, l.hasExecWait, queueWaitMs)
}

// executeJob is the execution half of the old processJob (citadel-cli#908 §2a):
// handler lookup, the #825 GPU-slot gate, the #548 per-job watchdog/deadline,
// and terminal Ack/Nack/Fail + stream publish. It brackets ONLY the actual
// handler execution with the executing counter (RecordJobExecuting/
// RecordJobExecuteDone) -- the self-heal STUCK signal -- so a job that spent
// time queued on a lane before reaching here never looks wedged. The wider
// claimed-to-done in-flight bracket (enterJob/exitJob) is owned by the caller.
//
// The per-job execution deadline (executeWithDeadline) is started HERE, at the
// moment execution begins, never counting lane queue-wait against it (§1e/§2a).
//
// emitLatency and queueWaitMs are set only for the inference lane; every other
// caller passes (false, 0) and executeJob's output is byte-identical to before.
// Returns jobOK true only on a clean success.
func (r *Runner) executeJob(ctx context.Context, job *Job, stream StreamWriter, startTime time.Time, emitLatency bool, queueWaitMs int64) (jobOK bool) {
	r.state.RecordJobExecuting()
	defer r.state.RecordJobExecuteDone()

	// Find handler
	var handler JobHandler
	for _, h := range r.handlers {
		if h.CanHandle(job.Type) {
			handler = h
			break
		}
	}

	if handler == nil {
		r.failUnsupportedJobType(ctx, job, startTime)
		return false
	}

	// GPU tracking: acquire/release GPU slot if tracker is set AND this job
	// type actually contends for a GPU (citadel-cli#825). Unchanged from the
	// pre-#908 processJob. On the inference lane the lane's exec-concurrency
	// already equals GPUTracker.Total(), so this acquire is effectively a
	// no-op in production (they stay in lockstep) and the queue-wait is the
	// real backpressure -- but it is retained so the job-type-scoped gate and
	// specific-GPU assignment still work, and so a degenerate configuration
	// (e.g. an externally-held tracker slot) still Nacks rather than mis-runs.
	gpuIndex := -1
	if r.gpuTracker != nil && needsGPUSlot(job.Type) {
		// Check if job requests a specific GPU
		if targetGpu, ok := job.Payload["targetGpu"]; ok {
			if idx, ok := targetGpu.(float64); ok {
				gpuIdx := int(idx)
				if !r.gpuTracker.AcquireSpecific(gpuIdx) {
					err := fmt.Errorf("requested GPU %d is unavailable", gpuIdx)
					r.log("error", "GPU unavailable: %v", err)
					r.recordJob(buildUsageRecord(job, "failed", startTime, time.Now(), nil, err))
					if werr := stream.WriteError(err, false); werr != nil {
						r.log("warning", "Failed to publish terminal error event for job %s: %v", job.ID, werr)
					}
					r.source.Nack(ctx, job, err)
					return false
				}
				gpuIndex = gpuIdx
			}
		}
		if gpuIndex < 0 {
			// Auto-acquire any available GPU
			idx, ok := r.gpuTracker.Acquire()
			if !ok {
				err := fmt.Errorf("no GPU slots available")
				r.log("warning", "No GPU slots: %v", err)
				r.recordJob(buildUsageRecord(job, "retry", startTime, time.Now(), nil, err))
				// NOTE (citadel-cli#559): this Nack redelivers the SAME job ID, so a
				// terminal event published here would land on stream:v1:{jobId}
				// for an attempt that isn't actually final -- if the backend treats
				// any "error" event as terminal-failure (rather than "recoverable,
				// keep waiting"), this would turn a transparent retry into a
				// reported failure. Left as pre-existing behavior (no publish)
				// pending confirmation of how the backend interprets
				// recoverable=true; see the PR for #559 for the deferred-fix note.
				r.source.Nack(ctx, job, err)
				return false
			}
			gpuIndex = idx
		}
		defer r.gpuTracker.Release(gpuIndex)
		r.log("info", "Job %s assigned to GPU %d", job.ID, gpuIndex)
		// Store GPU index in job payload for handler to use
		job.Payload["_gpuIndex"] = gpuIndex
	}

	// Execute handler (stream writer created at claim time)
	stream.WriteStart("Job processing started")

	// Per-job execution deadline (aceteam#6000). When the payload carries a
	// positive timeout budget, run the handler under a bounded context plus a
	// watchdog so a single hung/blocked handler cannot wedge this node's whole
	// sequential job loop: on expiry executeWithDeadline returns a deadline
	// error and the failure path below publishes the terminal error + Nacks on
	// the live parent ctx, letting the loop advance to the next job. With no
	// budget present (older backend, or a legitimately unbounded job type like
	// model download / build / provision) the call stays exactly as before --
	// synchronous, no timeout, no watchdog goroutine.
	execStart := time.Now()
	var result *JobResult
	var err error
	if timeout, ok := r.resolveJobTimeout(job); ok {
		result, err = r.executeWithDeadline(ctx, handler, job, stream, timeout)
	} else {
		result, err = handler.Execute(ctx, job, stream)
	}

	endTime := time.Now()
	duration := endTime.Sub(startTime)

	if err != nil || (result != nil && result.Status == JobStatusFailure) {
		actualErr := err
		if actualErr == nil && result != nil {
			actualErr = result.Error
		}
		r.log("error", "Job %s failed (%v): %v", job.ID, duration, actualErr)
		r.recordJob(buildUsageRecord(job, "failed", startTime, endTime, result, actualErr))

		// A watchdog abandon (deadline exceeded) is terminal, not a transient
		// failure: the handler goroutine is orphaned and still running, so
		// Nack -> redeliver -> re-execute would just re-wedge the slot for
		// another full budget (and, in sequential mode, block every other job
		// again). Fail (record failed + ACK -> DLQ) instead so a hung job is
		// removed from the pending list rather than retried into a repeated
		// wedge (issue #548). Every other failure keeps the existing Nack/retry
		// semantics.
		// Accepted tradeoff on abandon: like the orphaned handler goroutine, any
		// GPU slot this job holds is released when executeJob returns (the
		// deferred gpuTracker.Release), so a still-running orphan and the next job
		// can briefly share a slot -- unchanged from before #908, since the
		// unbounded lane's exec-concurrency 1 admits the next job only after this
		// one's goroutine returns. In practice the tracker schedules routing
		// fairness rather than exclusive CUDA ownership (inference is an HTTP call
		// into the engine container, which batches concurrent requests), and the
		// orphan self-terminates when its own client/handler timeout fires, so the
		// overlap window is bounded. We prefer this to permanently leaking the slot
		// (which would slowly exhaust GPU capacity across repeated abandons).
		var deadlineErr *deadlineExceededError
		isDeadlineExceeded := errors.As(actualErr, &deadlineErr)

		// Exactly one terminal event per job id (issue #826). A generic failure
		// that will be retried (Nack path, another attempt still within budget)
		// must NOT publish a terminal "error" here -- if the retry then
		// succeeds, executeJob reaches the success path below and publishes an
		// "end" on the SAME stream:v1:{jobId}, so publishing here too would
		// double-report (error, then end) for a job that ultimately succeeded.
		// A deadline-exceeded abandon is always terminal (Fail, never retried
		// by this node), so it always publishes. A generic failure that will
		// NOT be retried -- the final attempt, per willRetry's delivery-count
		// signal, or no signal available at all (see willRetry) -- also
		// publishes, so a job that exhausts its retries still reports failure
		// exactly once. Mirrors the reasoning #822/#559 already applied to the
		// JobStatusRetry and no-GPU-slot Nack paths below/above.
		if isDeadlineExceeded || !willRetry(job) {
			if werr := stream.WriteError(actualErr, false); werr != nil {
				r.log("warning", "Failed to publish terminal error event for job %s: %v", job.ID, werr)
			}
		}

		if isDeadlineExceeded {
			r.source.Fail(ctx, job, actualErr, map[string]any{
				"deadline_exceeded":  true,
				"deadline_seconds":   deadlineErr.timeout.Seconds(),
				"abandoned_by_agent": true,
			})
			return false
		}

		r.source.Nack(ctx, job, actualErr)
		return false
	}

	if result != nil && result.Status == JobStatusRetry {
		r.log("warning", "Job %s needs retry (%v)", job.ID, duration)
		r.recordJob(buildUsageRecord(job, "retry", startTime, endTime, result, result.Error))
		// NOTE (citadel-cli#559): same reasoning as the no-GPU-slots Nack above --
		// this Nack redelivers the same job ID, so left as pre-existing behavior
		// (no publish) pending confirmation of the backend's contract for a
		// non-final attempt. SERVICE_START (routed through LegacyHandlerAdapter)
		// never returns JobStatusRetry, so this branch does not affect it.
		r.source.Nack(ctx, job, result.Error)
		return false
	}

	// Success. Attach per-request latency metrics for the inference lane only
	// (aceteam#8254 §3c) -- tokens_per_second is measured against EXECUTION
	// time (endTime-execStart), never total (which includes lane queue wait),
	// so throughput does not silently under-report on a queued node.
	if emitLatency {
		result = r.attachLatencyMetrics(result, queueWaitMs, startTime, execStart, endTime)
	}
	r.log("success", "Job %s completed (%v)", job.ID, duration)
	r.finishSuccess(ctx, job, stream, result, startTime, endTime)
	return true
}

// finishSuccess is the ONE implementation of the success terminal tail: usage
// record, the stream:v1:{jobId} terminal "end" publish (issue #559 -- the entire
// contract the streaming dispatch path waits on), and the source Ack. Factored
// out of executeJob so the inference queue-wait-exceeded path (finishQueueWait-
// Exceeded) reuses the exact same terminal-publish-then-Ack sequence rather than
// duplicating it -- duplicating it is how a "queued" job would end up Acked with
// no terminal event, reintroducing the #559 bug this design exists to avoid.
func (r *Runner) finishSuccess(ctx context.Context, job *Job, stream StreamWriter, result *JobResult, startTime, endTime time.Time) {
	r.recordJob(buildUsageRecord(job, "success", startTime, endTime, result, nil))
	var output map[string]any
	if result != nil {
		output = result.Output
	}
	// Retried the same way as WriteClaimed (citadel-cli#985): losing the
	// terminal "end" event is worse than losing the claim-ack -- the job
	// genuinely succeeded but the result is never delivered to the waiting
	// caller. Still non-fatal once retries are exhausted, and the Ack below is
	// unconditional either way: a lost publish must not turn a locally
	// successful job into one that's re-delivered and re-run.
	if werr := retryStreamWrite(ctx, func() error { return stream.WriteEnd(output) }); werr != nil {
		r.log("warning", "Failed to publish terminal end event for job %s: %v", job.ID, werr)
	}
	r.source.Ack(ctx, job)
}

// finishQueueWaitExceeded handles an inference job that never got an execution
// slot within the queue-wait budget (aceteam#8254 §3a/§3b). It synthesizes the
// EXISTING model_warming success contract (LLMInferenceHandler.warming's shape),
// which the platform already retries after retry_after seconds -- so this needs
// zero backend change and, crucially, is NOT a Nack: it removes the job from the
// PEL via the normal success terminal (finishSuccess) instead of leaving it
// unacked with no terminal event. queueStart is when the job was admitted (its
// whole in-lane life has been queue wait). Returns true (a warming answer is a
// clean, Acked success from the node's bookkeeping perspective).
func (r *Runner) finishQueueWaitExceeded(ctx context.Context, job *Job, stream StreamWriter, queueStart time.Time) bool {
	model, _ := job.Payload["model"].(string)
	now := time.Now()
	queueWaitMs := now.Sub(queueStart).Milliseconds()
	r.log("info", "Job %s exceeded the inference queue-wait budget (%s); returning model_warming backpressure signal",
		job.ID, r.inferenceQueueWait)
	output := map[string]any{
		"status":        "model_warming",
		"model":         model,
		"eta_seconds":   0,
		"retry_after":   warmingRetryAfter,
		"warming_for":   "queue",
		"queue_wait_ms": queueWaitMs,
		"total_ms":      queueWaitMs,
	}
	result := &JobResult{Status: JobStatusSuccess, Output: output}
	r.finishSuccess(ctx, job, stream, result, queueStart, now)
	return true
}

// attachLatencyMetrics adds per-request timing to an inference job's output
// (aceteam#8254 §3c): total_ms (claim -> terminal, so total_ms - queue_wait_ms
// is execution time), queue_wait_ms (lane admit -> exec slot), and
// tokens_per_second computed from EXECUTION time only. Purely additive to the
// existing JobResult.Output map; no contract change.
func (r *Runner) attachLatencyMetrics(result *JobResult, queueWaitMs int64, startTime, execStart, endTime time.Time) *JobResult {
	if result == nil {
		result = &JobResult{Status: JobStatusSuccess}
	}
	if result.Output == nil {
		result.Output = map[string]any{}
	}
	result.Output["queue_wait_ms"] = queueWaitMs
	result.Output["total_ms"] = endTime.Sub(startTime).Milliseconds()
	if execMs := endTime.Sub(execStart).Milliseconds(); execMs > 0 {
		if completion := intFromOutput(result.Output, "_usage_completion_tokens"); completion > 0 {
			result.Output["tokens_per_second"] = float64(completion) / (float64(execMs) / 1000.0)
		}
	}
	return result
}

// LaneSnapshots returns the current activity of this runner's execution lanes
// for the heartbeat (citadel-cli#908 §4). Nil when no lanes have any relevant
// state to report is not enforced here -- callers project onto omitempty. The
// order is stable: unbounded first, then inference.
func (r *Runner) LaneSnapshots() []LaneSnapshot {
	var out []LaneSnapshot
	if r.unboundedLane != nil {
		out = append(out, r.unboundedLane.snapshot())
	}
	if r.inferenceLane != nil {
		out = append(out, r.inferenceLane.snapshot())
	}
	return out
}

// willRetry reports whether executeJob's generic-failure Nack (issue #826)
// will actually be redelivered to a handler again, so the pre-Nack terminal
// "error" publish in executeJob can be skipped for a job that has not yet
// exhausted its attempts.
//
// The signal is job.Metadata.Attempts/MaxAttempts, populated ONLY by
// RedisSource (internal/worker/redis_source.go's nextSingle/nextMulti) from
// the real per-message Redis delivery count -- the same count RedisSource
// itself uses to decide whether to hand the job to executeJob at all versus
// silently moving it straight to the DLQ. Attempts is this dispatch's
// delivery count (1-indexed, matching Redis XPENDING's RetryCount): a further
// attempt will be dispatched iff the NEXT delivery count (Attempts+1) is
// still under MaxAttempts.
//
// MaxAttempts == 0 means "no signal" -- APISource (the default production
// path per CLAUDE.md; the AceTeam Redis API proxy does not expose a
// per-message delivery count to the node) and any job without populated
// metadata (e.g. a test job). Read as "retry status unknown", not "will not
// retry": in that case we deliberately return false (do not suppress the
// publish), preserving the exact pre-#826 behavior of always publishing a
// terminal error on a generic failure, rather than guessing this is the
// final attempt when it might not be.
func willRetry(job *Job) bool {
	if job == nil || job.Metadata.MaxAttempts <= 0 {
		return false
	}
	return job.Metadata.Attempts+1 < job.Metadata.MaxAttempts
}

// failUnsupportedJobType terminally fails a job whose type has no registered
// handler on this node (issue #382).
//
// Historically this branch called Nack, which set the job status to "failed"
// but never acknowledged the message and never published a terminal stream
// event. For the streaming dispatch path the producer waits on the pub/sub
// terminal event (PublishEnd/PublishError), so a job with no handler produced
// no terminal event and the backend simply timed out after ~30s -- an opaque
// symptom indistinguishable from "node offline/busy". Worse, the un-acked
// message stayed in the consumer group's pending list and was redelivered by
// orphan recovery, re-failing forever.
//
// Instead we (1) publish a structured error event immediately so the backend
// surfaces an actionable "node <ver> doesn't support <TYPE> -- update the node"
// message, and (2) Fail the job (failed status + ACK) so the unsupported
// message is removed from the pending list rather than retried indefinitely.
func (r *Runner) failUnsupportedJobType(ctx context.Context, job *Job, startTime time.Time) {
	agentVersion := r.agentVersion
	if agentVersion == "" {
		agentVersion = "unknown"
	}
	err := fmt.Errorf(
		"unsupported job type %q: node %s has no handler for it (update the node)",
		job.Type, agentVersion,
	)
	r.log("error", "Unsupported job type: %v", err)

	data := map[string]any{
		"unsupported_job_type": true,
		"job_type":             job.Type,
		"agent_version":        agentVersion,
		"supported_types":      r.supportedJobTypes(),
	}

	r.recordJob(buildUsageRecord(job, "failed", startTime, time.Now(), nil, err))

	// Publish a terminal error event so the streaming dispatch path stops
	// waiting on a terminal event that would otherwise never arrive. Marked
	// non-recoverable: retrying an unsupported type on the same node is futile.
	var stream StreamWriter
	if r.streamWriterFactory != nil {
		stream = r.streamWriterFactory(job)
	} else {
		stream = &NoOpStreamWriter{}
	}
	if werr := stream.WriteError(err, false); werr != nil {
		r.log("warning", "Failed to publish unsupported-type error for job %s: %v", job.ID, werr)
	}

	// Fail (failed status + ACK) so the message is not redelivered forever.
	if ferr := r.source.Fail(ctx, job, err, data); ferr != nil {
		r.log("warning", "Failed to ack unsupported job %s: %v", job.ID, ferr)
	}
}

// supportedJobTypes returns the sorted set of job types this node's registered
// handlers can process. It is included in the unsupported-type failure so the
// backend (and operators) can see exactly what the node build supports.
func (r *Runner) supportedJobTypes() []string {
	seen := make(map[string]struct{})
	for _, jt := range allKnownJobTypes {
		for _, h := range r.handlers {
			if h.CanHandle(jt) {
				seen[jt] = struct{}{}
				break
			}
		}
	}
	types := make([]string, 0, len(seen))
	for jt := range seen {
		types = append(types, jt)
	}
	sort.Strings(types)
	return types
}

// buildUsageRecord constructs a UsageRecord from job execution context.
func buildUsageRecord(job *Job, status string, started, completed time.Time, result *JobResult, err error) usage.UsageRecord {
	r := usage.UsageRecord{
		JobID:       job.ID,
		JobType:     job.Type,
		Status:      status,
		StartedAt:   started,
		CompletedAt: completed,
		DurationMs:  completed.Sub(started).Milliseconds(),
	}

	// Extract backend and model from job payload
	if v, ok := job.Payload["backend"]; ok {
		if s, ok := v.(string); ok {
			r.Backend = s
		}
	}
	if v, ok := job.Payload["model"]; ok {
		if s, ok := v.(string); ok {
			r.Model = s
		}
	}

	// Extract usage metrics from result output (_usage_* keys)
	if result != nil && result.Output != nil {
		r.PromptTokens = intFromOutput(result.Output, "_usage_prompt_tokens")
		r.CompletionTokens = intFromOutput(result.Output, "_usage_completion_tokens")
		r.TotalTokens = intFromOutput(result.Output, "_usage_total_tokens")
		r.RequestBytes = intFromOutput(result.Output, "_usage_request_bytes")
		r.ResponseBytes = intFromOutput(result.Output, "_usage_response_bytes")
	}

	if err != nil {
		msg := err.Error()
		if len(msg) > 1024 {
			msg = msg[:1024]
		}
		r.ErrorMessage = msg
	}

	return r
}

// intFromOutput extracts an int64 value from a map[string]any.
func intFromOutput(m map[string]any, key string) int64 {
	v, ok := m[key]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case int:
		return int64(n)
	case int64:
		return n
	case float64:
		if n != n || n > float64(math.MaxInt64) || n < float64(math.MinInt64) { // NaN or overflow
			return 0
		}
		return int64(n)
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return i
		}
	}
	return 0
}

// RegisterHandler adds a handler to the runner.
func (r *Runner) RegisterHandler(handler JobHandler) {
	r.handlers = append(r.handlers, handler)
}

// CanHandle reports whether any registered handler can process jobType. It is the
// runner-level view of dispatchability, used by callers and tests to assert the
// registered handler set covers a given job type (e.g. WHATSAPP_PROVISION /
// AGENT_UPDATE must be present on both the dedicated worker and a control-center-only
// worker).
func (r *Runner) CanHandle(jobType string) bool {
	for _, h := range r.handlers {
		if h.CanHandle(jobType) {
			return true
		}
	}
	return false
}
