package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/usage"
)

// Runner orchestrates job processing from a source through handlers.
type Runner struct {
	source   JobSource
	handlers []JobHandler
	config   RunnerConfig

	// Optional integrations (set via WithXxx methods)
	streamWriterFactory func(job *Job) StreamWriter
	activityFn          func(level, msg string)
	jobRecordFn         func(record usage.UsageRecord)

	// Concurrency support
	maxConcurrency int
	gpuTracker     *GPUTracker
}

// RunnerConfig holds configuration for the runner.
type RunnerConfig struct {
	// WorkerID identifies this worker instance
	WorkerID string

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
}

// NewRunner creates a new job runner.
func NewRunner(source JobSource, handlers []JobHandler, config RunnerConfig) *Runner {
	return &Runner{
		source:         source,
		handlers:       handlers,
		config:         config,
		activityFn:     config.ActivityFn,
		jobRecordFn:    config.JobRecordFn,
		maxConcurrency: config.MaxConcurrency,
		gpuTracker:     config.GPUTracker,
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

// recordJob records a job completion for usage tracking
func (r *Runner) recordJob(record usage.UsageRecord) {
	if r.jobRecordFn != nil {
		r.jobRecordFn(record)
	}
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
			// Fetch next job
			job, err := r.source.Next(ctx)
			if err != nil {
				if ctx.Err() != nil {
					break runLoop // Context cancelled
				}
				r.log("warning", "Error fetching job: %v (retry in %s)", err, backoff)
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

			// Reset backoff on success
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
	r.log("info", "Received job %s (type: %s)", job.ID, job.Type)
	startTime := time.Now()

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
		err := fmt.Errorf("no handler for job type: %s", job.Type)
		r.log("error", "No handler: %v", err)
		r.recordJob(buildUsageRecord(job, "failed", startTime, time.Now(), nil, err))
		r.source.Nack(ctx, job, err)
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
	r.log("success", "Job %s completed (%v)", job.ID, duration)
	r.recordJob(buildUsageRecord(job, "success", startTime, endTime, result, nil))
	if result != nil {
		stream.WriteEnd(result.Output)
	} else {
		stream.WriteEnd(nil)
	}
	r.source.Ack(ctx, job)
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
