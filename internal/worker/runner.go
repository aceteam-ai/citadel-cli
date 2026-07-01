package worker

import (
	"context"
	"encoding/json"
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

	// NodeID is this node's Headscale numeric node ID (e.g. "758").
	// Used to filter jobs with a target_node field: if set and the job's
	// target_node doesn't match, the job is acknowledged and skipped.
	// When empty, target_node filtering is disabled (all jobs are processed).
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
	return &Runner{
		source:         source,
		handlers:       handlers,
		config:         config,
		agentVersion:   config.AgentVersion,
		activityFn:     config.ActivityFn,
		jobRecordFn:    config.JobRecordFn,
		maxConcurrency: config.MaxConcurrency,
		gpuTracker:     config.GPUTracker,
		state:          config.State,
	}
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

			// Process the job (concurrently if maxConcurrency > 1)
			if concurrency > 1 {
				sem <- struct{}{} // Acquire semaphore slot
				wg.Add(1)
				go func(j *Job) {
					defer wg.Done()
					defer func() { <-sem }() // Release semaphore slot
					r.processJob(ctx, j)
				}(job)
			} else {
				r.processJob(ctx, job)
			}
		}
	}

	// Wait for in-flight jobs to complete
	wg.Wait()

	r.log("info", "Worker shutdown complete")
	return nil
}

// processJob dispatches a job to the appropriate handler.
func (r *Runner) processJob(ctx context.Context, job *Job) {
	atomic.AddInt64(&r.activeJobs, 1)
	defer atomic.AddInt64(&r.activeJobs, -1)

	// Track job in the introspection state. jobOK is flipped to true only on a
	// clean success; the deferred RecordJobDone classifies the outcome (issue
	// #236). Covers every return path of this function.
	r.state.RecordJobReceived()
	jobOK := false
	defer func() { r.state.RecordJobDone(jobOK) }()

	r.log("info", "Received job %s (type: %s)", job.ID, job.Type)
	startTime := time.Now()

	// Target-node filter: when per-node consumer groups are used, every node
	// sees every message on the shared org queue. If the job specifies a
	// target_node that doesn't match this node's Headscale ID, acknowledge
	// and skip it silently -- the correct node will process it from its own
	// read position. When this node's ID is unknown (empty), skip filtering
	// to preserve pre-filter behavior and avoid dropping jobs.
	if r.config.NodeID != "" {
		if targetNode, ok := job.Payload["target_node"].(string); ok && targetNode != "" && targetNode != r.config.NodeID {
			r.log("info", "Skipping job %s: target_node=%s (this node=%s)", job.ID, targetNode, r.config.NodeID)
			r.source.Ack(ctx, job)
			return
		}
	}

	// JQS-Core Section 5.6: Check cancellation before processing
	if r.source.IsJobCancelled(ctx, job.ID) {
		r.log("info", "Job %s was cancelled before processing", job.ID)
		var stream StreamWriter
		if r.streamWriterFactory != nil {
			stream = r.streamWriterFactory(job)
		} else {
			stream = &NoOpStreamWriter{}
		}
		if err := stream.WriteCancelled("Job cancelled before processing"); err != nil {
			r.log("warning", "Failed to publish cancelled event for job %s: %v", job.ID, err)
		}
		r.recordJob(buildUsageRecord(job, "cancelled", startTime, time.Now(), nil, nil))
		jobOK = true // cleanly acked, not a processing failure
		r.source.Ack(ctx, job)
		return
	}

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
		return
	}

	// GPU tracking: acquire/release GPU slot if tracker is set
	gpuIndex := -1
	if r.gpuTracker != nil {
		// Check if job requests a specific GPU
		if targetGpu, ok := job.Payload["targetGpu"]; ok {
			if idx, ok := targetGpu.(float64); ok {
				gpuIdx := int(idx)
				if !r.gpuTracker.AcquireSpecific(gpuIdx) {
					err := fmt.Errorf("requested GPU %d is unavailable", gpuIdx)
					r.log("error", "GPU unavailable: %v", err)
					r.recordJob(buildUsageRecord(job, "failed", startTime, time.Now(), nil, err))
					var stream StreamWriter
					if r.streamWriterFactory != nil {
						stream = r.streamWriterFactory(job)
					} else {
						stream = &NoOpStreamWriter{}
					}
					stream.WriteError(err, false)
					r.source.Nack(ctx, job, err)
					return
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
				r.source.Nack(ctx, job, err)
				return
			}
			gpuIndex = idx
		}
		defer r.gpuTracker.Release(gpuIndex)
		r.log("info", "Job %s assigned to GPU %d", job.ID, gpuIndex)
		// Store GPU index in job payload for handler to use
		job.Payload["_gpuIndex"] = gpuIndex
	}

	// Create stream writer
	var stream StreamWriter
	if r.streamWriterFactory != nil {
		stream = r.streamWriterFactory(job)
	} else {
		stream = &NoOpStreamWriter{}
	}

	// Execute handler
	stream.WriteStart("Job processing started")
	result, err := handler.Execute(ctx, job, stream)

	endTime := time.Now()
	duration := endTime.Sub(startTime)

	if err != nil || (result != nil && result.Status == JobStatusFailure) {
		actualErr := err
		if actualErr == nil && result != nil {
			actualErr = result.Error
		}
		r.log("error", "Job %s failed (%v): %v", job.ID, duration, actualErr)
		r.recordJob(buildUsageRecord(job, "failed", startTime, endTime, result, actualErr))
		stream.WriteError(actualErr, false)
		r.source.Nack(ctx, job, actualErr)
		return
	}

	if result != nil && result.Status == JobStatusRetry {
		r.log("warning", "Job %s needs retry (%v)", job.ID, duration)
		r.recordJob(buildUsageRecord(job, "retry", startTime, endTime, result, result.Error))
		r.source.Nack(ctx, job, result.Error)
		return
	}

	// Success
	jobOK = true
	r.log("success", "Job %s completed (%v)", job.ID, duration)
	r.recordJob(buildUsageRecord(job, "success", startTime, endTime, result, nil))
	if result != nil {
		stream.WriteEnd(result.Output)
	} else {
		stream.WriteEnd(nil)
	}
	r.source.Ack(ctx, job)
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
