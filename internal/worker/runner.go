package worker

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// Runner orchestrates job processing from a source through handlers.
type Runner struct {
	source   JobSource
	handlers []JobHandler
	config   RunnerConfig

	// Optional integrations (set via WithXxx methods)
	streamWriterFactory func(jobID string) StreamWriter
	activityFn          func(level, msg string)
	jobRecordFn         func(id, jobType, status string, started, completed time.Time, err error)
}

// RunnerConfig holds configuration for the runner.
type RunnerConfig struct {
	// WorkerID identifies this worker instance
	WorkerID string

	// Verbose enables detailed logging
	Verbose bool

	// ActivityFn is called for log messages (if set, suppresses stdout)
	ActivityFn func(level, msg string)

	// JobRecordFn is called when a job completes (for history tracking)
	JobRecordFn func(id, jobType, status string, started, completed time.Time, err error)
}

// NewRunner creates a new job runner.
func NewRunner(source JobSource, handlers []JobHandler, config RunnerConfig) *Runner {
	return &Runner{
		source:      source,
		handlers:    handlers,
		config:      config,
		activityFn:  config.ActivityFn,
		jobRecordFn: config.JobRecordFn,
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

// recordJob records a job completion for history tracking
func (r *Runner) recordJob(id, jobType, status string, started, completed time.Time, err error) {
	if r.jobRecordFn != nil {
		r.jobRecordFn(id, jobType, status, started, completed, err)
	}
}

// WithStreamWriterFactory sets a factory for creating stream writers.
// If not set, a NoOpStreamWriter is used.
func (r *Runner) WithStreamWriterFactory(factory func(jobID string) StreamWriter) *Runner {
	r.streamWriterFactory = factory
	return r
}

// Run starts the job processing loop.
// This method blocks until the context is cancelled or a signal is received.
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

	if r.activityFn == nil {
		// Only show verbose startup info if not in TUI mode
		fmt.Printf("   - Worker ID: %s\n", r.config.WorkerID)
		fmt.Printf("   - Source: %s\n", r.source.Name())
		fmt.Printf("   - Handlers: %d registered\n", len(r.handlers))
	}
	r.log("success", "Worker started, listening for jobs...")

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

			// Process the job
			r.processJob(ctx, job)
		}
	}

	r.log("info", "Worker shutdown complete")
	return nil
}

// processJob dispatches a job to the appropriate handler.
func (r *Runner) processJob(ctx context.Context, job *Job) {
	r.log("info", "Received job %s (type: %s)", job.ID, job.Type)
	startTime := time.Now()

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
		r.recordJob(job.ID, job.Type, "failed", startTime, time.Now(), err)
		r.source.Nack(ctx, job, err)
		return
	}

	// Create stream writer
	var stream StreamWriter
	if r.streamWriterFactory != nil {
		stream = r.streamWriterFactory(job.ID)
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
		r.recordJob(job.ID, job.Type, "failed", startTime, endTime, actualErr)
		stream.WriteError(actualErr, false)
		r.source.Nack(ctx, job, actualErr)
		return
	}

	if result != nil && result.Status == JobStatusRetry {
		r.log("warning", "Job %s needs retry (%v)", job.ID, duration)
		r.recordJob(job.ID, job.Type, "retry", startTime, endTime, result.Error)
		r.source.Nack(ctx, job, result.Error)
		return
	}

	// Success
	r.log("success", "Job %s completed (%v)", job.ID, duration)
	r.recordJob(job.ID, job.Type, "success", startTime, endTime, nil)
	if result != nil {
		stream.WriteEnd(result.Output)
	} else {
		stream.WriteEnd(nil)
	}
	r.source.Ack(ctx, job)
}

// RegisterHandler adds a handler to the runner.
func (r *Runner) RegisterHandler(handler JobHandler) {
	r.handlers = append(r.handlers, handler)
}
