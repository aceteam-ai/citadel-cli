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
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/aep"
	"github.com/aceteam-ai/citadel-cli/internal/clockstate"
	"github.com/aceteam-ai/citadel-cli/internal/jobprivacy"
	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/meetingpolicy"
	"github.com/aceteam-ai/citadel-cli/internal/usage"
)

// Runner orchestrates job processing from a source through handlers.
type Runner struct {
	source       JobSource
	handlers     []JobHandler
	config       RunnerConfig
	agentVersion string
	clockState   clockstate.Provider
	wallNow      func() time.Time

	// Optional integrations (set via WithXxx methods)
	streamWriterFactory func(job *Job) StreamWriter
	activityFn          func(level, msg string)
	jobRecordFn         func(record usage.UsageRecord)

	// Concurrency support
	maxConcurrency int
	gpuTracker     *GPUTracker

	// Work-receipt signing for RUNNER-signed dispatched work (citadel-cli#1287).
	// When a handler DECLARES ReceiptOwner==ReceiptOwnerRunner and this signer is
	// set, the runner signs a work receipt for every dispatched success (default
	// on, independent of CITADEL_SIGN_AEP_RECEIPTS) and FAILS CLOSED when signing
	// is unavailable. nil leaves such jobs unsigned and never fails them closed —
	// the same posture a handler-signed path has with no signer wired, so existing
	// tests that build a runner without a signer are unaffected. Handler-signed
	// job types (FILE_INDEX, embedding) are untouched by this; they sign inside
	// the handler via the LegacyHandlerOpts.WorkReceiptSigner path.
	workReceiptSigner aep.Signer
	workReceiptNodeID string

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
	heavyLane          *lane
	inferenceQueueWait time.Duration
	huddleCancelPoll   time.Duration
	// coopCancelPoll is the mid-job cooperative-cancel poll interval for
	// cooperativeCancelJobTypes (FILE_INDEX). Defaults to 1s; tests shorten it.
	coopCancelPoll time.Duration

	// state, when set, records live introspection metrics (poll time, job
	// counts) for the out-of-band status/control path (issue #236).
	state *WorkerState

	// Lifecycle observability for safe self-update.
	// activeJobs counts admitted jobs, including lane and semaphore waiters.
	// A permanent drain and independently owned temporary drains both stop
	// pickup. The mutex makes acquiring and releasing scopes atomic with the
	// consume loop's drain checks.
	activeJobs     int64
	drainMu        sync.Mutex
	permanentDrain bool
	drainScopes    int
	pollCancel     context.CancelFunc
	pollDone       chan struct{}
	pendingJob     *Job
	// A claim remains in-flight until enterJob accounts for its admission or
	// a saturated lane has Nacked it. This closes the claim-to-dispatch idle gap.
	dispatchingJobs int
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

	// WorkReceiptSigner signs RUNNER-signed dispatched-work receipts
	// (citadel-cli#1287). Injected explicitly (NOT resolved inside this package)
	// so a test never signs with a real node identity: cmd/work.go passes
	// nodeidentity.Convergent(network.GetNodeConfigDir()), the SAME signer it
	// gives LegacyHandlerOpts.WorkReceiptSigner. nil disables runner-signing (and
	// its fail-closed posture) entirely.
	WorkReceiptSigner aep.Signer
	// WorkReceiptNodeID is the preferred receipt node_id (the fabric node id);
	// empty falls back to the signer's own public-key fingerprint.
	WorkReceiptNodeID string
	// ClockState is a cheap cached observation, not a probe or execution gate.
	ClockState clockstate.Provider
}

// NewRunner creates a new job runner.
func NewRunner(source JobSource, handlers []JobHandler, config RunnerConfig) *Runner {
	r := &Runner{
		clockState:         config.ClockState,
		wallNow:            time.Now,
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
		workReceiptSigner:  config.WorkReceiptSigner,
		workReceiptNodeID:  config.WorkReceiptNodeID,
	}

	// The general unbounded lane always exists (exec-concurrency 1): it is the
	// relocated single-writer lock over the unlocked manifest/lockfile paths.
	r.unboundedLane = newLane("unbounded", resolveUnboundedLaneQueue(), 1, false)

	// The heavy lane always exists (exec 1, admit 2): FILE_INDEX and future heavy
	// index/extract work. exec 1 keeps a single DISPATCHED index-DB writer; admit 2
	// lets one more be claimed+queued so the fetch loop never blocks. It is kept
	// SEPARATE from the unbounded manifest-writer lane so a multi-hour index never
	// queues a deploy (aceteam#10876 C1). hasExecWait=false: a queued heavy job
	// waits (unbounded) for the single exec slot, like the unbounded lane.
	r.heavyLane = newLane("heavy", heavyLaneAdmitDepth, 1, false)

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

// ActiveJobs includes jobs claimed but not yet dispatched, as well as jobs
// admitted for execution. It is safe to call concurrently and is used by the
// auto-updater to find an idle moment before swapping the binary.
func (r *Runner) ActiveJobs() int {
	r.drainMu.Lock()
	defer r.drainMu.Unlock()
	active := int(atomic.LoadInt64(&r.activeJobs)) + r.dispatchingJobs
	if r.pendingJob != nil {
		active++
	}
	return active
}

// Drain permanently stops fetching new jobs. In-flight jobs may finish.
// Shutdown and remote updates use this fail-closed drain.
func (r *Runner) Drain() {
	r.drainMu.Lock()
	r.permanentDrain = true
	cancel, done := r.pollCancel, r.pollDone
	r.drainMu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

// NodeID is the local identity used by the runner's target filter.
func (r *Runner) NodeID() string { return r.config.NodeID }

// FenceIntakeForRecovery stops pickup before STUCK restart without waiting for
// the possibly uncooperative poll that recovery is intended to terminate.
func (r *Runner) FenceIntakeForRecovery() {
	r.drainMu.Lock()
	r.permanentDrain = true
	cancel := r.pollCancel
	r.drainMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// BeginDrain stops fetching jobs until its returned release function is called.
// Each scope owns only its own pause; release is safe to call more than once.
// A permanent Drain always takes precedence over released scopes.
func (r *Runner) BeginDrain() func() {
	r.drainMu.Lock()
	r.drainScopes++
	cancel, done := r.pollCancel, r.pollDone
	r.drainMu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			r.drainMu.Lock()
			r.drainScopes--
			r.drainMu.Unlock()
		})
	}
}

// TryIdleRestart fences pickup and waits for the current poll/claim to settle
// before checking all admitted work. Another drain owner or active job vetoes
// the exit. action must only exit the process (or be an inert test callback).
func (r *Runner) TryIdleRestart(action func()) bool {
	release := r.BeginDrain()
	defer release()
	r.drainMu.Lock()
	defer r.drainMu.Unlock()
	if r.permanentDrain || r.drainScopes != 1 || atomic.LoadInt64(&r.activeJobs) != 0 || r.dispatchingJobs != 0 || r.pendingJob != nil {
		return false
	}
	action()
	return true
}

// isDraining reports whether any drain remains active.
func (r *Runner) isDraining() bool {
	r.drainMu.Lock()
	defer r.drainMu.Unlock()
	return r.permanentDrain || r.drainScopes != 0
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
	defer func() {
		r.Drain()
		r.drainMu.Lock()
		r.pendingJob = nil
		r.drainMu.Unlock()
	}()
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
	for _, handler := range r.handlers {
		if background, ok := handler.(interface{ RunBackground(context.Context) }); ok {
			wg.Add(1)
			go func() {
				defer wg.Done()
				background.RunBackground(ctx)
			}()
		}
	}

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

			// A poll already in progress when a drain begins may still return a
			// job. Hold it locally until this worker may claim work again.
			r.drainMu.Lock()
			job := r.pendingJob
			r.drainMu.Unlock()
			var err error
			var pollCtx context.Context
			if job == nil {
				var cancelPoll context.CancelFunc
				pollCtx, cancelPoll = context.WithCancel(ctx)
				r.drainMu.Lock()
				if r.permanentDrain || r.drainScopes != 0 {
					r.drainMu.Unlock()
					cancelPoll()
					continue
				}
				r.pollCancel = cancelPoll
				r.pollDone = make(chan struct{})
				r.drainMu.Unlock()
				job, err = r.source.Next(pollCtx)
				cancelPoll()
				r.drainMu.Lock()
				if err == nil {
					r.pendingJob = job
				}
				close(r.pollDone)
				r.pollDone = nil
				r.pollCancel = nil
				r.drainMu.Unlock()
				// A pending job below was already polled; count only actual
				// source calls in the worker's consume metrics.
				r.state.RecordPoll()
				r.recordConsumeStatus(err)
			}
			if err != nil {
				if ctx.Err() != nil {
					break runLoop // Context cancelled
				}
				if pollCtx != nil && pollCtx.Err() != nil && r.isDraining() {
					continue // intentional drain interrupted the source poll
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
			r.drainMu.Lock()
			if ctx.Err() != nil {
				r.drainMu.Unlock()
				break runLoop
			}
			if r.permanentDrain || r.drainScopes != 0 {
				r.drainMu.Unlock()
				continue
			}
			// Claim and drain acquisition share this lock: once BeginDrain
			// returns, no subsequent claim can begin until its release.
			r.dispatchingJobs++
			r.pendingJob = nil
			proceed, stream, startTime := r.claimJob(ctx, job)
			if !proceed {
				r.dispatchingJobs--
			}
			r.drainMu.Unlock()
			if !proceed {
				continue
			}
			if yieldsFineTune(job.Type) {
				if !r.yieldFineTuneForDemand(ctx, job, stream) {
					continue
				}
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
			case r.heavyLane != nil && r.capabilitiesFor(job.Type).Lane == jobs.LaneHeavy:
				// Dedicated heavy lane (exec 1, admit 2): any handler that DECLARES
				// Lane==LaneHeavy (FILE_INDEX today). A single DISPATCHED index-DB writer off
				// the fetch loop, isolated from the manifest-writer lane so a
				// multi-hour index never queues a deploy (aceteam#10876 C1).
				r.dispatchLane(ctx, r.heavyLane, job, stream, startTime, &wg)
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

// A claimed demand job must never enter any execution lane until every active
// fine-tune handler confirms that its trainer and reservation are gone. A
// failed preemption is a terminal refusal, not a speculative admission.
func (r *Runner) yieldFineTuneForDemand(ctx context.Context, job *Job, stream StreamWriter) bool {
	for _, handler := range r.handlers {
		yielder, ok := handler.(interface{ YieldToDemand() error })
		if !ok {
			continue
		}
		if err := yielder.YieldToDemand(); err != nil {
			r.log("error", "Refusing job %s: fine-tune preemption unconfirmed: %v", job.ID, err)
			if stream != nil {
				if writeErr := stream.WriteError(err, false); writeErr != nil {
					r.log("warning", "Failed to publish demand refusal for job %s: %v", job.ID, writeErr)
				}
			}
			if failErr := r.source.Fail(ctx, job, err, map[string]any{"fine_tune_preemption_unconfirmed": true}); failErr != nil {
				r.log("error", "Failed to record demand refusal for job %s: %v", job.ID, failErr)
			}
			return false
		}
	}
	return true
}

func yieldsFineTune(t string) bool {
	if needsGPUSlot(t) {
		return true
	}
	switch t {
	case JobTypeShellCommand, JobTypeTmuxSession, JobTypeCobrowse, JobTypeCobrowseSession,
		JobTypeVNCType, JobTypeVNCKeys, JobTypeVNCActions, JobTypeServiceStart,
		JobTypeModuleSet, JobTypeExtraction, JobTypeTranscribeAudio, JobTypeMediaGenerate:
		return true
	}
	return false
}

func (r *Runner) clockNow() time.Time {
	if r.wallNow != nil {
		return r.wallNow()
	}
	return time.Now()
}

// newStreamWriter builds the per-job stream writer, falling back to a no-op
// writer when no factory is configured (e.g. Nexus HTTP source).
func (r *Runner) newStreamWriter(job *Job) StreamWriter {
	var raw StreamWriter = &NoOpStreamWriter{}
	if r.streamWriterFactory != nil {
		raw = r.streamWriterFactory(job)
	}
	if job.delivery != nil {
		if api, ok := raw.(*APIStreamWriter); ok {
			copy := *api
			copy.ctx, copy.durable = job.delivery().lost, true
			raw = &copy
		}
	}
	raw = observeStream(raw, job.clock)
	if job.delivery == nil {
		return raw
	}
	job.delivery().mu.Lock()
	if job.delivery().raw == nil {
		job.delivery().raw = raw
	}
	job.delivery().mu.Unlock()
	return &durableStream{StreamWriter: raw, delivery: job.delivery()}
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
//
// This makes delivery AT-LEAST-ONCE, not exactly-once: redisapi.Client.Publish
// only guarantees no double-publish WITHIN a single call (see its doc
// comment), so a `write` that reports an error after the message actually
// reached the coordinator (a lost 200, a 502 from a proxy sitting in front of
// an otherwise-successful publish) gets retried here and can be published
// twice. That is the deliberately benign direction for both call sites: a
// duplicate `claimed` event is idempotent (the coordinator only cares that
// one arrived within the window), and a duplicate `end` event carries
// identical output and re-runs nothing -- unlike a truly LOST event, neither
// triggers #985's fast-fail/withdrawal hazard. The alternative (treat any
// error as possibly-already-delivered and never retry) reintroduces the exact
// bug this issue fixes.
func retryStreamWrite(ctx context.Context, write func() error) error {
	var err error
	for attempt := 0; attempt < streamWriteRetryAttempts; attempt++ {
		if err = write(); err == nil {
			return nil
		}
		if attempt == streamWriteRetryAttempts-1 {
			break
		}
		// Clamp BEFORE indexing: streamWriteRetryAttempts and
		// streamWriteRetryBackoff are independent package vars (retuned
		// separately, e.g. by a test), so attempt can exceed the backoff
		// slice's length. Indexing first and clamping after is dead code --
		// it only "worked" because production defaults (3 attempts, 2
		// backoff entries) sit exactly at the boundary. min() here is what
		// actually prevents an out-of-range panic in the fetch-loop/lane
		// goroutine (no recover -> worker crash) the moment either var is
		// retuned independently.
		backoff := streamWriteRetryBackoff[min(attempt, len(streamWriteRetryBackoff)-1)]
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
	r.drainMu.Lock()
	atomic.AddInt64(&r.activeJobs, 1)
	r.dispatchingJobs--
	r.drainMu.Unlock()
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
	if job.delivery != nil {
		job.delivery().followShutdown(ctx)
		if ctx.Err() != nil {
			_ = r.source.Nack(ctx, job, ctx.Err())
			return false, nil, startTime
		}
		job.clock = newDeliveryClock(r.clockNow, r.clockState)
		stream = r.newStreamWriter(job)
	}

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
	if targetNode, ok := job.Payload["target_node"].(string); job.Type != JobTypeWorkerControl && ok && targetNode != "" && targetNode != r.config.NodeID {
		if r.config.NodeID == "" {
			r.log("warning", "Declining job %s: addressed to target_node=%s but this node's Headscale ID is unresolved, "+
				"so it cannot claim addressed work", job.ID, targetNode)
		} else {
			r.log("info", "Skipping job %s: target_node=%s (this node=%s)", job.ID, targetNode, r.config.NodeID)
		}
		if job.delivery != nil {
			if r.config.NodeID == "" {
				_ = r.source.Fail(ctx, job, errors.New("target_unresolved"), map[string]any{"reason": "target_unresolved"})
			} else {
				_ = r.source.Nack(ctx, job, errors.New("target mismatch; awaiting claim-time refusal"))
			}
		} else {
			r.source.Ack(ctx, job)
		}
		return false, nil, startTime
	}
	if job.delivery != nil && r.config.NodeID == "" {
		_ = r.source.Fail(ctx, job, errors.New("target_unresolved"), map[string]any{"reason": "target_unresolved"})
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
	if stream == nil {
		job.clock = newDeliveryClock(r.clockNow, r.clockState)
		stream = r.newStreamWriter(job)
	}
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
		r.drainMu.Lock()
		r.dispatchingJobs--
		r.drainMu.Unlock()
		return
	}
	// Claim dispatch is synchronous. Reserve ordering before spawning, so the
	// scheduler cannot let a later goroutine enter the execution slot first.
	ticket := l.reserveExecTicket()
	if job.Type == JobTypeFineTuneStart {
		for _, handler := range r.handlers {
			if pending, ok := handler.(interface{ MarkPendingFineTune(string) }); ok {
				pending.MarkPendingFineTune(job.ID)
			}
		}
	}
	r.enterJob()
	wg.Add(1)
	go func(j *Job, s StreamWriter, st time.Time) {
		defer wg.Done()
		if j.Type == JobTypeFineTuneStart {
			defer func() {
				for _, handler := range r.handlers {
					if pending, ok := handler.(interface{ ClearPendingFineTune(string) }); ok {
						pending.ClearPendingFineTune(j.ID)
					}
				}
			}()
		}
		jobOK := false
		defer func() {
			r.exitJob(jobOK)
			l.releaseAdmit()
		}()
		jobOK = r.runLaneJob(ctx, l, j, s, st, ticket)
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
func (r *Runner) runLaneJob(ctx context.Context, l *lane, job *Job, stream StreamWriter, startTime time.Time, ticket *laneExecTicket) (jobOK bool) {
	if job.delivery != nil {
		var release func()
		ctx, release = job.delivery().bind(ctx)
		defer release()
	}
	ownsExec := false
	releaseWaitingAuthority := func() {
		if ownsExec {
			<-l.exec
			ownsExec = false
		}
		l.releaseExecTicket(ticket)
	}
	defer releaseWaitingAuthority()
	queueStart := time.Now()
	var timer *time.Timer
	var expiry <-chan time.Time
	var deadline time.Time
	if l.hasExecWait {
		deadline = queueStart.Add(r.inferenceQueueWait)
		timer = time.NewTimer(r.inferenceQueueWait)
		expiry = timer.C
		defer timer.Stop()
	}
	finishWait := func(expired bool) (bool, bool) {
		// Cancellation wins when multiple readiness signals are already set.
		if err := ctx.Err(); err != nil {
			releaseWaitingAuthority()
			r.cancelWaitingDelivery(ctx, job, stream, err)
			return true, false
		}
		if expired || (l.hasExecWait && !time.Now().Before(deadline)) {
			releaseWaitingAuthority()
			// Keep the existing model_warming terminal path, never a silent Nack.
			return true, r.finishQueueWaitExceeded(ctx, job, stream, queueStart)
		}
		return false, false
	}
	if finished, ok := finishWait(false); finished {
		return ok
	}
	expired := false
	select {
	case <-ticket.ready:
	case <-expiry:
		expired = true
	case <-ctx.Done():
		releaseWaitingAuthority()
		r.cancelWaitingDelivery(ctx, job, stream, ctx.Err())
		return false
	}
	if finished, ok := finishWait(expired); finished {
		return ok
	}
	select {
	case l.exec <- struct{}{}:
		ownsExec = true
	case <-expiry:
		expired = true
	case <-ctx.Done():
		releaseWaitingAuthority()
		r.cancelWaitingDelivery(ctx, job, stream, ctx.Err())
		return false
	}
	// A ready slot can win select alongside cancel/expiry. Return it before
	// beginExec in that case; an expired or canceled waiter never runs a handler.
	if finished, ok := finishWait(expired); finished {
		return ok
	}
	if timer != nil {
		timer.Stop()
	}
	l.releaseExecTicket(ticket)
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
	if job.delivery != nil {
		var release func()
		ctx, release = job.delivery().bind(ctx)
		defer release()
		defer job.delivery().stop()
		if !job.delivery().valid() {
			return false
		}
		if ctx.Err() != nil {
			_ = r.source.Nack(ctx, job, ctx.Err())
			return false
		}
		// A job may have been cancelled while waiting for its execution slot.
		if r.source.IsJobCancelled(ctx, job.ID) {
			_ = stream.WriteCancelled("Job cancelled before execution")
			_ = r.source.Ack(ctx, job)
			return false
		}
	}
	r.state.RecordJobExecuting()
	defer r.state.RecordJobExecuteDone()

	// Find handler (shared with the lane-routing lookup via findHandler).
	handler := r.findHandler(job.Type)
	if handler == nil {
		r.failUnsupportedJobType(ctx, job, startTime)
		return false
	}
	caps := capabilitiesOf(handler, job.Type)

	// Preflight RUNNER-signed dispatched work BEFORE executing: a keyless node
	// fails in milliseconds rather than after a full inference (citadel-cli#1287,
	// mirroring the handler-signed Preflight). Only a handler that DECLARED
	// ReceiptOwnerRunner with a signer wired reaches this — never a no-action job.
	if r.runnerSigns(caps) {
		if perr := r.newRunnerWorkReceiptConfig(job).Preflight(); perr != nil {
			r.log("error", "Job %s refused: %v", job.ID, perr)
			r.recordJob(buildUsageRecord(job, "failed", startTime, time.Now(), nil, perr))
			if werr := stream.WriteError(perr, false); werr != nil {
				r.log("warning", "Failed to publish receipt-signing-unavailable error for job %s: %v", job.ID, werr)
			}
			if ferr := r.source.Fail(ctx, job, perr, map[string]any{"reason": "receipt_signing_unavailable"}); ferr != nil {
				r.log("warning", "Failed to ack receipt-signing-unavailable job %s: %v", job.ID, ferr)
			}
			return false
		}
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

	handlerCtx, timeout, bounded, budgetErr := r.resolveExecutionBudget(ctx, handler, job)
	var ticket *executionTicket
	if budgetErr == nil {
		executionStarted := time.Now()
		if job.Type == JobTypeMeetingJoin {
			deadlineCtx, cancelDeadline := context.WithDeadline(handlerCtx, executionStarted.Add(timeout))
			defer cancelDeadline()
			handlerCtx = jobs.WithMeetingExecutionStart(deadlineCtx, executionStarted)
		}
		var specific time.Duration
		if b, ok := handlerCtx.Value(meetingBudgetKey{}).(meetingpolicy.Budget); ok {
			specific = b.StuckAfter()
		}
		ticket = r.state.beginExecution(job.Type, executionStarted, specific)
		handlerCtx = context.WithValue(handlerCtx, executionTicketKey{}, ticket)
		defer ticket.pipelineFinished()
		stream.WriteStart("Job processing started")
	}

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
	handlerCtx, stopCancellationWatch := r.watchHuddleCancellation(handlerCtx, job)
	// Cooperative (graceful) mid-job cancel watch for cooperativeCancelJobTypes
	// (FILE_INDEX): polls job:cancelled:<id> and sets a flag the handler reads via
	// JobContext.CancelRequested, WITHOUT cancelling handlerCtx — so the current
	// file finishes and commits before the walk stops (aceteam#10876 C1). A no-op
	// for every other job type, and harmless alongside the huddle watch (their
	// job-type sets are disjoint).
	handlerCtx, stopCoopCancelWatch := r.watchCooperativeCancellation(handlerCtx, job)
	if budgetErr != nil {
		err = budgetErr
	} else if bounded {
		result, err = r.executeWithDeadline(handlerCtx, handler, job, handlerDeliveryStream(job, stream), timeout)
	} else {
		result, err = r.executeObserved(handlerCtx, handler, job, handlerDeliveryStream(job, stream))
	}
	stopCoopCancelWatch()
	cancelled := stopCancellationWatch()
	// Cooperative legacy handlers finish their current unit and return partial
	// success-shaped bytes. Use the runner-owned cancellation observation, never
	// payload/output text, to preserve the v1 cancelled terminal mapping.
	if job.delivery != nil {
		if requested := jobs.CancelRequestedFrom(handlerCtx); requested != nil && requested() {
			cancelled = true
		}
	}
	if job.delivery != nil && !job.delivery().valid() {
		return false
	}
	if job.Type == JobTypeMeetingJoin && errors.Is(err, jobs.ErrMeetingTeardownUnconfirmed) {
		r.log("warning", "Meeting teardown remains unconfirmed; retaining source ownership")
		return false
	}
	if cleanup := jobs.MeetingCleanupFrom(handlerCtx); cleanup != nil && budgetErr == nil {
		publicationCtx, finish, e := cleanup.Begin(ctx)
		if e != nil {
			ticket.handlerFinished(true)
			r.log("warning", "Meeting publication allowance exhausted; retaining source ownership")
			return false
		}
		ctx = publicationCtx
		defer func() {
			if finish() != nil {
				ticket.handlerFinished(true)
			}
		}()
	}
	// Sanitize before any generic failure logging, usage record or retry/status
	// publication. The handler still receives the original in-flight payload.
	err = jobprivacy.Error(job.Type, job.Payload, err)
	if result != nil {
		result.Error = jobprivacy.Error(job.Type, job.Payload, result.Error)
	}

	endTime := time.Now()
	// Capture after execution using the separate wall seam, never for duration
	// arithmetic. Handler-owned early terminal observations remain immutable.
	job.clock.finish(r.clockNow())
	job.clock.populate(result)
	duration := endTime.Sub(startTime)
	if job.delivery != nil && (cancelled || (result != nil && result.Status == JobStatusCancelled)) {
		_ = stream.WriteCancelled("Job cancelled")
		_ = r.source.Ack(ctx, job)
		return false
	}
	var hardDeadline *deadlineExceededError
	if job.delivery != nil && ctx.Err() != nil && !errors.As(err, &hardDeadline) && (err != nil || result == nil || result.Status != JobStatusSuccess) {
		_ = r.source.Nack(ctx, job, ctx.Err())
		return false
	}
	// Caller cancellation (the platform's durable marker, or a handler-local
	// cancel while the worker is alive) is distinct from worker shutdown: a
	// shutdown-interrupted meeting falls through to the ordinary terminal
	// failure path below, never "Meeting cancelled".
	if cancelled || job.Type == JobTypeMeetingJoin && errors.Is(err, context.Canceled) && ctx.Err() == nil {
		if job.Type == JobTypeMeetingJoin {
			r.recordJob(buildUsageRecord(job, "cancelled", startTime, endTime, result, context.Canceled))
			if e := stream.WriteCancelled("Meeting cancelled"); e != nil {
				r.log("warning", "Failed to publish meeting cancellation")
			}
			r.source.Ack(ctx, job)
			return false
		}
		if errors.Is(err, jobs.ErrHuddleTerminalOwned) {
			r.log("info", "Huddle job %s terminal is coordinator-owned", job.ID)
			r.ackHuddleCoordinatorTerminal(ctx, job)
			return false
		}
		if errors.Is(err, jobs.ErrHuddleLifecycleOutboxUnavailable) {
			r.log("warning", "Huddle job %s cannot persist lifecycle recovery debt", job.ID)
			r.source.Nack(ctx, job, err)
			return false
		}
		if errors.Is(err, jobs.ErrHuddleTerminalIntentPending) {
			r.log("warning", "Huddle job %s cancellation is waiting for durable terminal intent", job.ID)
			r.recordJob(buildUsageRecord(job, "retry", startTime, endTime, result, err))
			r.deferHuddleTerminalIntent(ctx, job, err)
			return false
		}
		// Cancellation does not erase node teardown debt.  The exact huddle job
		// must be redelivered until its deterministic meetingd session is proven
		// gone; ACKing here would strand the coordinator's cancelling lifecycle.
		if errors.Is(err, jobs.ErrHuddleTeardownPending) {
			r.log("warning", "Huddle job %s cancellation is waiting for node teardown", job.ID)
			r.recordJob(buildUsageRecord(job, "retry", startTime, endTime, result, err))
			r.deferHuddleTeardown(ctx, job, err)
			return false
		}
		r.log("info", "Huddle job %s cancelled during execution", job.ID)
		r.recordJob(buildUsageRecord(job, "cancelled", startTime, endTime, result, context.Canceled))
		if werr := stream.WriteCancelled("Huddle lifecycle cancelled"); werr != nil {
			r.log("warning", "Failed to publish cancelled event for job %s: %v", job.ID, werr)
		}
		r.source.Ack(ctx, job)
		return false
	}

	if result != nil && result.Status == JobStatusCancelled {
		r.recordJob(buildUsageRecord(job, "cancelled", startTime, endTime, result, nil))
		r.source.Ack(ctx, job)
		return false
	}

	// Runner-signed work receipt (citadel-cli#1287): sign a receipt for every
	// dispatched success of a job that DECLARED ReceiptOwnerRunner (LLM_INFERENCE).
	// A sign failure becomes `err` here and flows into the isReceiptSigningUnavailable
	// fail-closed branch below (terminal, Fail/DLQ, one terminal error) — never a
	// silent unsigned success. model_warming is skipped inside (no inference ran).
	if err == nil && result != nil && result.Status == JobStatusSuccess && r.runnerSigns(caps) {
		if signErr := r.attachRunnerWorkReceipt(job, result); signErr != nil {
			err = signErr
		}
	}

	if result != nil && result.Status == JobStatusTerminalFailure {
		actualErr := result.Error
		if actualErr == nil {
			actualErr = errors.New("terminal job failure")
		}
		r.recordJob(buildUsageRecord(job, "failed", startTime, endTime, result, actualErr))
		_ = stream.WriteError(actualErr, false)
		_ = r.source.Fail(ctx, job, actualErr, map[string]any{"terminal_failure": true})
		return false
	}

	if err != nil || (result != nil && result.Status == JobStatusFailure) {
		actualErr := err
		if actualErr == nil && result != nil {
			actualErr = result.Error
		}
		if errors.Is(actualErr, jobs.ErrHuddleTerminalOwned) {
			r.log("info", "Huddle job %s terminal is coordinator-owned", job.ID)
			r.recordJob(buildUsageRecord(job, "failed", startTime, endTime, result, actualErr))
			r.ackHuddleCoordinatorTerminal(ctx, job)
			return false
		}
		if errors.Is(actualErr, jobs.ErrHuddleLifecycleOutboxUnavailable) {
			r.log("warning", "Huddle job %s cannot persist lifecycle recovery debt: %v", job.ID, actualErr)
			r.source.Nack(ctx, job, actualErr)
			return false
		}
		if errors.Is(actualErr, jobs.ErrHuddleTerminalIntentPending) {
			r.log("warning", "Huddle job %s is waiting for durable terminal intent (%v): %v", job.ID, duration, actualErr)
			r.recordJob(buildUsageRecord(job, "retry", startTime, endTime, result, actualErr))
			r.deferHuddleTerminalIntent(ctx, job, actualErr)
			return false
		}
		// This is not a terminal huddle failure: the same exact attempt owns
		// durable teardown debt and must be retried without an `error` stream
		// event. A later delivery reports terminal only after DELETE succeeds.
		if errors.Is(actualErr, jobs.ErrHuddleTeardownPending) {
			r.log("warning", "Huddle job %s is waiting for node teardown (%v): %v", job.ID, duration, actualErr)
			r.recordJob(buildUsageRecord(job, "retry", startTime, endTime, result, actualErr))
			r.deferHuddleTeardown(ctx, job, actualErr)
			return false
		}
		r.log("error", "Job %s failed (%v): %v", job.ID, duration, actualErr)
		r.recordJob(buildUsageRecord(job, "failed", startTime, endTime, result, actualErr))

		// A watchdog abandon (deadline exceeded) is terminal, not a transient
		// failure: the handler goroutine is orphaned and still running, so
		// Nack -> redeliver -> re-execute would just re-wedge the slot for
		// another full budget (and, in sequential mode, block every other job
		// again). Fail (record failed + ACK -> DLQ) instead so a hung job is
		// removed from the pending list rather than retried into a repeated
		// wedge (issue #548). Apart from Files permission refusals below, other
		// failures keep the existing Nack/retry semantics.
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
		// Files permission is captured when handlers are built. A refusal is
		// terminal even on the first delivery: retries cannot enable Files.
		isFilesDisabled := errors.Is(actualErr, jobs.ErrFilesDisabled)
		// A dispatched work job that cannot sign its receipt is terminal on the
		// first delivery too (aceteam#10876 C7): a retry cannot make a missing node
		// identity key appear. Fail (DLQ) with reason receipt_signing_unavailable,
		// never silently succeed unsigned. Same shape as the Files refusal.
		isReceiptSigningUnavailable := errors.Is(actualErr, jobs.ErrReceiptSigningUnavailable)
		// A job that needs a sandboxed helper module this node cannot run
		// (no runtime able to enforce the sandbox isolation, or the module
		// image absent) is terminal on first delivery too (citadel-cli#1288):
		// a sandboxed module runs with no network and cannot pull its own
		// image, so a retry cannot make it appear. Same shape as the Files /
		// receipt-signing refusals.
		isSandboxedModuleUnavailable := errors.Is(actualErr, jobs.ErrSandboxedModuleUnavailable)
		// A gate refusal returned as a *jobs.ShellRefusal is terminal on the first
		// delivery: a retry presents the same payload, so it cannot pass. That is
		// SHELL_COMMAND, the desktop handlers, and WORKFLOW_RUN when its Shell gate
		// refuses at submit. A Shell node refused later, mid-run, fails the
		// workflow as an ordinary failure instead.
		var gateRefusal *jobs.ShellRefusal
		isGateRefusal := errors.As(actualErr, &gateRefusal)
		isHuddleJoin := job.Type == JobTypeHuddleJoin
		isMeetingJoin := job.Type == JobTypeMeetingJoin

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
		if isDeadlineExceeded || isFilesDisabled || isReceiptSigningUnavailable || isSandboxedModuleUnavailable || isGateRefusal || isHuddleJoin || isMeetingJoin || !willRetry(job) {
			if werr := stream.WriteError(actualErr, false); werr != nil {
				r.log("warning", "Failed to publish terminal error event for job %s: %v", job.ID, werr)
			}
		}

		if isMeetingJoin && !isGateRefusal {
			r.source.Fail(ctx, job, actualErr, map[string]any{"reason": "meeting_failed_no_automatic_rejoin"})
			return false
		}
		if isDeadlineExceeded {
			r.source.Fail(ctx, job, actualErr, map[string]any{
				"deadline_exceeded":  true,
				"deadline_seconds":   deadlineErr.timeout.Seconds(),
				"abandoned_by_agent": true,
			})
			return false
		}

		if isFilesDisabled {
			if ferr := r.source.Fail(ctx, job, actualErr, map[string]any{"reason": "files_disabled"}); ferr != nil {
				r.log("warning", "Failed to ack Files-disabled job %s: %v", job.ID, ferr)
			}
			return false
		}

		if isReceiptSigningUnavailable {
			if ferr := r.source.Fail(ctx, job, actualErr, map[string]any{"reason": "receipt_signing_unavailable"}); ferr != nil {
				r.log("warning", "Failed to ack receipt-signing-unavailable job %s: %v", job.ID, ferr)
			}
			return false
		}

		if isSandboxedModuleUnavailable {
			if ferr := r.source.Fail(ctx, job, actualErr, map[string]any{"reason": "sandboxed_module_unavailable"}); ferr != nil {
				r.log("warning", "Failed to ack sandboxed-module-unavailable job %s: %v", job.ID, ferr)
			}
			return false
		}

		if isGateRefusal {
			if ferr := r.source.Fail(ctx, job, actualErr, map[string]any{"reason": gateRefusal.Reason}); ferr != nil {
				r.log("warning", "Failed to ack refused job %s: %v", job.ID, ferr)
			}
			return false
		}

		// A HUDDLE_JOIN lifecycle attempt is itself the retry boundary. The
		// platform owns successor-attempt creation, so redelivering the same job
		// after an exact terminal report would only replay stale authority.
		if isHuddleJoin {
			if ferr := r.source.Fail(ctx, job, actualErr, map[string]any{"reason": "huddle_lifecycle_failed"}); ferr != nil {
				r.log("warning", "Failed to ack terminal huddle job %s: %v", job.ID, ferr)
			}
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
	if job.Type == JobTypeHuddleJoin && !huddleTeardownOnly(job) {
		// The handler returned only after AceTeam committed participant cleanup,
		// durable status, and the original terminal event. Never publish or write
		// status a second time from the disposable delivery.
		r.recordJob(buildUsageRecord(job, "completed", startTime, endTime, result, nil))
		r.ackHuddleCoordinatorTerminal(ctx, job)
		return true
	}
	if job.Type == JobTypeWorkerControl {
		return r.finishWorkerControl(ctx, job, stream, handler, result, startTime, endTime)
	}
	r.finishSuccess(ctx, job, stream, result, startTime, endTime)
	return job.delivery == nil || job.delivery().isCommitted()
}

type huddleTeardownDeferrer interface {
	DeferHuddleTeardown(context.Context, *Job, error) error
}

type huddleTerminalIntentDeferrer interface {
	DeferHuddleTerminalIntent(context.Context, *Job, error) error
}

type huddleCoordinatorTerminalAcker interface {
	AckHuddleCoordinatorTerminal(context.Context, *Job) error
}

func huddleTeardownOnly(job *Job) bool {
	if job == nil || job.Payload == nil {
		return false
	}
	switch value := job.Payload["teardownOnly"].(type) {
	case bool:
		return value
	case string:
		parsed, err := strconv.ParseBool(value)
		return err == nil && parsed
	default:
		return false
	}
}

func (r *Runner) ackHuddleCoordinatorTerminal(ctx context.Context, job *Job) {
	if source, ok := r.source.(huddleCoordinatorTerminalAcker); ok {
		if err := source.AckHuddleCoordinatorTerminal(ctx, job); err != nil {
			r.log("warning", "Failed to acknowledge coordinator-owned huddle job %s: %v", job.ID, err)
		}
		return
	}
	if err := r.source.Ack(ctx, job); err != nil {
		r.log("warning", "Failed to acknowledge coordinator-owned huddle job %s: %v", job.ID, err)
	}
}

func (r *Runner) deferHuddleTerminalIntent(ctx context.Context, job *Job, err error) {
	if source, ok := r.source.(huddleTerminalIntentDeferrer); ok {
		if deferErr := source.DeferHuddleTerminalIntent(ctx, job, err); deferErr != nil {
			r.log("warning", "Failed to defer huddle terminal intent job %s: %v", job.ID, deferErr)
		}
		return
	}
	r.source.Nack(ctx, job, err)
}

func (r *Runner) deferHuddleTeardown(ctx context.Context, job *Job, err error) {
	if source, ok := r.source.(huddleTeardownDeferrer); ok {
		if deferErr := source.DeferHuddleTeardown(ctx, job, err); deferErr != nil {
			r.log("warning", "Failed to defer huddle teardown job %s: %v", job.ID, deferErr)
		}
		return
	}
	// Test/custom sources retain the legacy unacked retry behavior. All three
	// production sources implement the durable server-owned deferral above.
	r.source.Nack(ctx, job, err)
}

// watchHuddleCancellation turns the platform's durable cancellation marker
// into hard execution cancellation for HUDDLE_JOIN and MEETING_JOIN. Only the
// observation is shared; their terminal authority and teardown handling differ.
// Other job types retain the existing pre-execution-only cancellation check.
func (r *Runner) watchHuddleCancellation(ctx context.Context, job *Job) (context.Context, func() bool) {
	if job.Type != JobTypeHuddleJoin && job.Type != JobTypeMeetingJoin {
		return ctx, func() bool { return false }
	}
	execCtx, cancelExec := context.WithCancel(ctx)
	watchCtx, cancelWatch := context.WithCancel(ctx)
	stop := make(chan struct{})
	done := make(chan struct{})
	var cancelled atomic.Bool
	interval := r.huddleCancelPoll
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if r.source.IsJobCancelled(watchCtx, job.ID) {
					cancelled.Store(true)
					cancelExec()
					return
				}
			}
		}
	}()
	return execCtx, func() bool {
		cancelWatch()
		close(stop)
		<-done
		cancelExec()
		return cancelled.Load()
	}
}

// watchCooperativeCancellation turns the platform's durable cancellation marker
// into a GRACEFUL stop signal for cooperativeCancelJobTypes (FILE_INDEX). Unlike
// watchHuddleCancellation it does NOT cancel the handler context: it polls
// job:cancelled:<id> and, on cancel, sets a flag the handler reads via
// JobContext.CancelRequested (threaded through the returned context). The handler
// finishes the current unit (file), commits it, and returns `cancelled` with
// partial counts — "cancel commits the current file" (aceteam#10876 C1).
//
// For any job type NOT in cooperativeCancelJobTypes this returns ctx unchanged
// and a no-op stop, so it is inert for every other job. The returned stop func
// halts the poll goroutine (call it once execution is done).
func (r *Runner) watchCooperativeCancellation(ctx context.Context, job *Job) (context.Context, func()) {
	if !r.capabilitiesFor(job.Type).CooperativeCancel {
		return ctx, func() {}
	}
	var requested atomic.Bool
	pollCtx, cancelPoll := context.WithCancel(ctx)
	// The predicate reads the live flag, so setting it below is observed by the
	// handler on its next poll. Thread it to the adapter via the context.
	ctx = jobs.WithCancelRequested(ctx, requested.Load)
	done := make(chan struct{})
	interval := r.coopCancelPoll
	if interval <= 0 {
		interval = time.Second
	}
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-pollCtx.Done():
				return
			case <-ticker.C:
				if r.source.IsJobCancelled(pollCtx, job.ID) {
					requested.Store(true)
					return // once requested, stop polling; the handler stops gracefully
				}
			}
		}
	}()
	var stopOnce sync.Once
	return ctx, func() {
		stopOnce.Do(cancelPoll)
		<-done
	}
}

// finishWorkerControl ACKs before reporting acceptance. Scheduling is prepared
// behind a gate before the accepted event, and the gate opens only after that
// event has been published. Thus neither an ACK nor scheduling failure can
// produce an accepted result or initiate a restart.
func (r *Runner) finishWorkerControl(ctx context.Context, job *Job, stream StreamWriter, handler JobHandler, result *JobResult, startTime, endTime time.Time) bool {
	if result == nil {
		r.source.Nack(ctx, job, errors.New("WORKER_CONTROL returned no result"))
		return false
	}
	accepted, _ := result.Output["accepted"].(bool)
	control, ok := handler.(interface {
		PrepareAfterAck(*Job) (func(), func(), error)
		Abort(*Job) error
	})
	if err := r.source.Ack(ctx, job); err != nil {
		r.log("error", "WORKER_CONTROL %s queue ack failed: %v", job.ID, err)
		if accepted && ok {
			if abortErr := control.Abort(job); abortErr != nil {
				r.log("error", "WORKER_CONTROL %s abort failed: %v", job.ID, abortErr)
				return false
			}
		}
		r.publishWorkerControlRefusal(ctx, stream, "ack_failed", "worker restart queue acknowledgment failed")
		return false
	}
	var commit func()
	if accepted {
		if !ok {
			r.publishWorkerControlRefusal(ctx, stream, "schedule_failed", "worker restart scheduler is unavailable")
			return false
		}
		var err error
		commit, _, err = control.PrepareAfterAck(job)
		if err != nil {
			r.log("error", "WORKER_CONTROL %s restart preparation failed: %v", job.ID, err)
			if abortErr := control.Abort(job); abortErr != nil {
				r.log("error", "WORKER_CONTROL %s abort failed: %v", job.ID, abortErr)
				return false
			}
			r.publishWorkerControlRefusal(ctx, stream, "schedule_failed", "worker restart could not be scheduled")
			return false
		}
	}
	if err := retryStreamWrite(ctx, func() error { return stream.WriteEnd(result.Output) }); err != nil {
		r.log("error", "WORKER_CONTROL %s result publish failed with uncertain delivery: %v", job.ID, err)
		if accepted {
			// A failed publish call may still have delivered the accepted event.
			// Once publication has been attempted, cancelling the exit would
			// leave that caller with accepted=true but no restart. Keep the
			// prepared restart; an unseen result remains unconfirmed upstream.
			commit()
		}
		return false
	}
	if accepted {
		// Acceptance is already visible to the caller. Release the prepared
		// restart gate before optional usage accounting, which may block or
		// panic and must not strand an accepted restart.
		commit()
	}
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				r.log("error", "WORKER_CONTROL %s optional usage accounting panicked: %v", job.ID, recovered)
			}
		}()
		r.recordJob(buildUsageRecord(job, "success", startTime, endTime, result, nil))
	}()
	return true
}

func (r *Runner) publishWorkerControlRefusal(ctx context.Context, stream StreamWriter, code, message string) {
	output := map[string]any{"action": "restart", "accepted": false, "restarting": false, "code": code, "message": message}
	if err := retryStreamWrite(ctx, func() error { return stream.WriteEnd(output) }); err != nil {
		r.log("error", "WORKER_CONTROL refusal publish failed: %v", err)
	}
}

// finishSuccess is the ONE implementation of the success terminal tail: usage
// record, the stream:v1:{jobId} terminal "end" publish (issue #559 -- the entire
// contract the streaming dispatch path waits on), and the source Ack. Factored
// out of executeJob so the inference queue-wait-exceeded path (finishQueueWait-
// Exceeded) reuses the exact same terminal-publish-then-Ack sequence rather than
// duplicating it -- duplicating it is how a "queued" job would end up Acked with
// no terminal event, reintroducing the #559 bug this design exists to avoid.
func (r *Runner) finishSuccess(ctx context.Context, job *Job, stream StreamWriter, result *JobResult, startTime, endTime time.Time) {
	if job.delivery != nil && r.durableModelWarming(job, result) {
		_ = r.source.Nack(ctx, job, errors.New("model_warming"))
		return
	}
	job.clock.finish(r.clockNow())
	job.clock.populate(result)
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
	return job.delivery == nil || job.delivery().isCommitted()
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
	if r.heavyLane != nil {
		out = append(out, r.heavyLane.snapshot())
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
// surfaces an actionable message, and (2) Fail the job (failed status + ACK)
// so the unsupported message is removed from the pending list rather than
// retried indefinitely.
//
// The error text distinguishes two causes that used to collapse into one
// misleading "(update the node)" message (aceteam#9962): a type this build
// genuinely does not know (allKnownJobTypes) really does need an update, but
// a known type is refused with current registration-gate evidence and simply not
// registered this run -- typically a default-DENY permission (files/desktop)
// or a missing config directory. Telling an operator to "update the node" for
// the second case sends them chasing a binary upgrade that fixes nothing; the
// real fix is a permission toggle or provisioning step named in the message.
// Refusal evidence is not a capability grant and never makes a handler runnable.
func (r *Runner) failUnsupportedJobType(ctx context.Context, job *Job, startTime time.Time) {
	agentVersion := r.agentVersion
	if agentVersion == "" {
		agentVersion = "unknown"
	}

	var err error
	refusal := r.unavailableJobRefusal(job.Type)
	gated := refusal != nil
	if gated {
		err = refusal
	} else {
		err = fmt.Errorf(
			"unsupported job type %q: node %s has no handler for it (update the node)",
			job.Type, agentVersion,
		)
	}
	if gated {
		r.log("warning", "Refused job type %s: %v", job.Type, err)
	} else {
		r.log("error", "Unsupported job type: %v", err)
	}

	data := map[string]any{
		"unsupported_job_type": true,
		"job_type":             job.Type,
		"agent_version":        agentVersion,
		"supported_types":      r.SupportedJobTypes(),
	}
	if gated {
		data["unsupported_job_type"] = false
		data["refused"] = true
		data["reason"] = refusal.Reason
		data["known_to_build"] = true
		data["unregistered_reason"] = refusal.Message
	}

	r.recordJob(buildUsageRecord(job, "failed", startTime, time.Now(), nil, err))

	// Publish a terminal error event so the streaming dispatch path stops
	// waiting on a terminal event that would otherwise never arrive. Marked
	// non-recoverable: retrying an unsupported type on the same node is futile.
	stream := r.newStreamWriter(job)
	if werr := stream.WriteError(err, false); werr != nil {
		r.log("warning", "Failed to publish unsupported-type error for job %s: %v", job.ID, werr)
	}

	// Fail (failed status + ACK) so the message is not redelivered forever.
	if ferr := r.source.Fail(ctx, job, err, data); ferr != nil {
		r.log("warning", "Failed to ack unsupported job %s: %v", job.ID, ferr)
	}
}

// SupportedJobTypes returns the sorted set of job types this node's registered
// handlers can process. It is the canonical live capability set used by both
// unsupported-type failures and heartbeat advertisement.
func (r *Runner) SupportedJobTypes() []string {
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

	// Flow attribution from the job envelope (aceteam#10876 / citadel-cli#1287),
	// so central fabric_run_work can join this node's work to a flow run. The
	// producer spellings match the aceteam backend (flow_run_id / flow_node_id /
	// delegated_node_id / node_uid / receipt_ids). These are read from the decoded
	// payload; absent keys leave the record field empty.
	r.FlowRunID = payloadString(job.Payload, "flow_run_id")
	r.FlowNodeID = payloadString(job.Payload, "flow_node_id")
	r.DelegatedNodeID = payloadString(job.Payload, "delegated_node_id")
	r.NodeUID = payloadString(job.Payload, "node_uid")
	r.ReceiptIDs = payloadStringSlice(job.Payload, "receipt_ids")

	// Extract usage metrics from result output (_usage_* keys). These are set
	// directly on Output by NATIVE worker handlers (e.g. llm_inference).
	if result != nil && result.Output != nil {
		r.PromptTokens = intFromOutput(result.Output, "_usage_prompt_tokens")
		r.CompletionTokens = intFromOutput(result.Output, "_usage_completion_tokens")
		r.TotalTokens = intFromOutput(result.Output, "_usage_total_tokens")
		r.RequestBytes = intFromOutput(result.Output, "_usage_request_bytes")
		r.ResponseBytes = intFromOutput(result.Output, "_usage_response_bytes")

		// A LEGACY handler's whole JSON output is wrapped as a string under the
		// "output" key by LegacyHandlerAdapter, so the _usage_* top-level read
		// above never sees its metrics. FILE_INDEX / embedding emit a nested
		// `usage` object there (work attribution + typed units + receipt binding,
		// aceteam#10876 C7); decode it and carry it onto the record. This is the
		// "usage key fix": without it a FILE_INDEX/embedding job records zero
		// tokens/bytes and no work attribution.
		applyNestedUsage(&r, result.Output)
		// A NATIVE handler (llm_inference, citadel-cli#1287) carries its `usage`
		// object as a TOP-LEVEL map (not wrapped as a string). The runner-signed
		// work-receipt path merges attribution into that same map, so decode it
		// here too to carry origin/action/org_id/units/manifest onto the record.
		if usg, ok := result.Output["usage"].(map[string]any); ok {
			applyUsageMap(&r, usg)
		}

		// Signed work-receipt BODIES (aceteam#11146 / citadel-cli#1291): carry the
		// full v2 AEP receipt maps the handler/runner already produced onto the
		// record so central's verify_and_store_work_receipt() has the bytes to
		// attest. Collected here (inside buildUsageRecord) so EVERY terminal branch
		// — success, failed, retry, cancelled — is covered uniformly, with no
		// per-branch wiring; in practice only a success carries receipts.
		receipts := collectWorkReceipts(result.Output)
		r.Receipts, r.ReceiptsTruncated = boundWorkReceipts(receipts)
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

// applyNestedUsage decodes a legacy handler's JSON output (wrapped as a string
// under Output["output"]) and, when it carries a nested `usage` object, carries
// the work attribution onto the usage record (aceteam#10876 C7). It is a no-op
// for an output that is not a JSON object or has no `usage` map, so non-work legacy
// jobs are unaffected. A NATIVE handler (e.g. llm_inference, citadel-cli#1287)
// carries its `usage` object as a top-level map instead; buildUsageRecord calls
// applyUsageMap directly for that case.
func applyNestedUsage(r *usage.UsageRecord, output map[string]any) {
	raw, ok := output["output"].(string)
	if !ok || raw == "" {
		return
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		return
	}
	usg, ok := decoded["usage"].(map[string]any)
	if !ok {
		return
	}
	applyUsageMap(r, usg)
}

// applyUsageMap carries work attribution, typed units, receipt binding, and
// token/byte metrics from a decoded `usage` object onto the record. Token/byte
// fields are only taken from the object when the native _usage_* read did not
// already set them (native handlers win). Shared by the legacy nested-string
// path (applyNestedUsage) and the native top-level-map path.
func applyUsageMap(r *usage.UsageRecord, usg map[string]any) {
	if usg == nil {
		return
	}
	if s, ok := usg["origin"].(string); ok {
		r.Origin = s
	}
	if s, ok := usg["org_id"].(string); ok {
		r.OrgID = s
	}
	if s, ok := usg["action"].(string); ok {
		r.WorkAction = s
	}
	if s, ok := usg["manifest_sha256"].(string); ok {
		r.ReceiptManifestSHA256 = s
	}
	if b, ok := usg["signed"].(bool); ok {
		r.ReceiptSigned = b
	}
	// Peer-service borrowing attribution (citadel-cli#1289). When a REQUESTER
	// borrows a unit of work from a peer at RUNTIME (a decision the node makes, not
	// the producer), the handler sets these in its output usage map so central
	// fabric_run_work can join this node's work to the peer that served it. They
	// OVERRIDE the envelope-carried values buildUsageRecord read earlier (the
	// runtime decision is more specific than the dispatch-time envelope).
	if s, ok := usg["delegated_node_id"].(string); ok && s != "" {
		r.DelegatedNodeID = s
	}
	if ids := payloadStringSlice(usg, "receipt_ids"); ids != nil {
		r.ReceiptIDs = ids
	}
	if units, ok := usg["units"].(map[string]any); ok {
		r.Units = unitsFromAny(units)
	}
	// Token/byte metrics: prefer a value already set from the native _usage_*
	// path; otherwise take the object's.
	if r.PromptTokens == 0 {
		r.PromptTokens = intFromOutput(usg, "prompt_tokens")
	}
	if r.CompletionTokens == 0 {
		r.CompletionTokens = intFromOutput(usg, "completion_tokens")
	}
	if r.TotalTokens == 0 {
		r.TotalTokens = intFromOutput(usg, "total_tokens")
	}
	if r.RequestBytes == 0 {
		r.RequestBytes = intFromOutput(usg, "request_bytes")
	}
	if r.ResponseBytes == 0 {
		r.ResponseBytes = intFromOutput(usg, "response_bytes")
	}
}

// usageReceiptsMaxBytes bounds the JSON size of the receipts[] array carried on a
// single usage record. Central imposes no hard cap but the record rides a Redis
// stream (direct XAdd) OR an HTTP StreamAdd through the API proxy (whose body cap
// is unknown to the node), so we keep it WELL under central's documented ~1 MB
// guidance. A v2 work receipt is ~700 bytes, so this fits hundreds of receipts —
// far more than any real job signs (one today). When a job somehow exceeds it,
// receipts are dropped from the TAIL and the count is reported as
// ReceiptsTruncated (never a silent drop); central marks such a record partially
// attested.
const usageReceiptsMaxBytes = 768 * 1024

// collectWorkReceipts gathers the signed v2 work-receipt maps a handler/runner
// placed in job output under `work_receipts`. The two producer shapes are
// mutually exclusive per job: a NATIVE runner-signed handler (LLM_INFERENCE,
// citadel-cli#1287) sets a TOP-LEVEL `work_receipts` ([]map[string]any, in-memory,
// never crossing a JSON boundary before this read), while a LEGACY handler
// (FILE_INDEX / embedding, aceteam#10876 C7) has its whole output marshaled to a
// JSON string under Output["output"], so its `work_receipts` is nested there
// (decoded as []any of map[string]any). Top-level wins when present (it is the
// only producer for that job); otherwise the nested legacy string is decoded.
// This mirrors the existing usage-map dual handling (applyUsageMap vs
// applyNestedUsage). Returns nil when a job produced no receipts.
func collectWorkReceipts(output map[string]any) []map[string]any {
	if output == nil {
		return nil
	}
	if rs := receiptsFromAny(output["work_receipts"]); len(rs) > 0 {
		return rs
	}
	raw, ok := output["output"].(string)
	if !ok || raw == "" {
		return nil
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		return nil
	}
	return receiptsFromAny(decoded["work_receipts"])
}

// receiptsFromAny normalizes a `work_receipts` value to []map[string]any. It
// accepts the native in-memory shape ([]map[string]any) and the
// JSON-round-tripped legacy shape ([]any whose elements are map[string]any),
// skipping any element that is not a map. Any other type yields nil.
func receiptsFromAny(v any) []map[string]any {
	switch rs := v.(type) {
	case []map[string]any:
		out := make([]map[string]any, 0, len(rs))
		for _, m := range rs {
			if m != nil {
				out = append(out, m)
			}
		}
		return out
	case []any:
		out := make([]map[string]any, 0, len(rs))
		for _, e := range rs {
			if m, ok := e.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	default:
		return nil
	}
}

// boundWorkReceipts keeps as many receipts as fit (from the front, in order)
// under usageReceiptsMaxBytes and returns the kept slice plus the count dropped.
// It NEVER silently drops: whatever does not fit is counted in the returned
// truncated value so the payload can carry receiptsTruncated and central marks
// the record partially attested. A receipt that cannot marshal is also counted as
// dropped rather than kept corrupt. The first receipt is always kept (a single
// receipt is tiny relative to the budget), so a job that signed at least one
// never reports "attested nothing" to central. Returns (nil, 0) for no receipts.
func boundWorkReceipts(receipts []map[string]any) ([]map[string]any, int) {
	if len(receipts) == 0 {
		return nil, 0
	}
	kept := make([]map[string]any, 0, len(receipts))
	total := 2 // the enclosing "[]"
	full := false
	for _, m := range receipts {
		if full {
			break
		}
		b, err := json.Marshal(m)
		if err != nil {
			continue // unmarshalable: drop (counted via len(receipts)-len(kept))
		}
		size := len(b) + 1 // + a comma separator; a slight over-count is safe
		if len(kept) > 0 && total+size > usageReceiptsMaxBytes {
			full = true
			break
		}
		total += size
		kept = append(kept, m)
	}
	truncated := len(receipts) - len(kept)
	if len(kept) == 0 {
		return nil, truncated
	}
	return kept, truncated
}

// payloadStringSlice reads a []string from a decoded job payload. It accepts a
// JSON array (decoded as []any of strings); a bare string yields a single-element
// slice. Absent or malformed values yield nil.
func payloadStringSlice(payload map[string]any, key string) []string {
	if payload == nil {
		return nil
	}
	switch v := payload[key].(type) {
	case []string:
		if len(v) == 0 {
			return nil
		}
		return append([]string(nil), v...)
	case []any:
		out := make([]string, 0, len(v))
		for _, e := range v {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	case string:
		if v == "" {
			return nil
		}
		return []string{v}
	default:
		return nil
	}
}

// unitsFromAny converts a decoded JSON units object to map[string]int64,
// dropping any non-numeric entry.
func unitsFromAny(m map[string]any) map[string]int64 {
	out := make(map[string]int64, len(m))
	for k := range m {
		out[k] = intFromOutput(m, k)
	}
	return out
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
