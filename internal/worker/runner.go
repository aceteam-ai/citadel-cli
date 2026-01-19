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
}

// RunnerConfig holds configuration for the runner.
type RunnerConfig struct {
	// WorkerID identifies this worker instance
	WorkerID string

	// Verbose enables detailed logging
	Verbose bool
}

// NewRunner creates a new job runner.
func NewRunner(source JobSource, handlers []JobHandler, config RunnerConfig) *Runner {
	return &Runner{
		source:   source,
		handlers: handlers,
		config:   config,
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
	fmt.Printf("--- 🚀 Starting Worker (%s) ---\n", r.source.Name())
	if err := r.source.Connect(ctx); err != nil {
		return fmt.Errorf("failed to connect to %s: %w", r.source.Name(), err)
	}
	defer r.source.Close()

	fmt.Printf("   - Worker ID: %s\n", r.config.WorkerID)
	fmt.Printf("   - Source: %s\n", r.source.Name())
	fmt.Printf("   - Handlers: %d registered\n", len(r.handlers))
	fmt.Println("   - ✅ Worker started. Listening for jobs...")

	// Main processing loop with exponential backoff on errors
	backoff := time.Second
	const maxBackoff = 30 * time.Second

runLoop:
	for {
		select {
		case sig := <-sigs:
			fmt.Printf("\n   - Received signal %v, initiating graceful shutdown...\n", sig)
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
				fmt.Fprintf(os.Stderr, "   - ⚠️ Error fetching job: %v (retry in %s)\n", err, backoff)
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

	fmt.Println("--- 🛑 Worker shutdown complete ---")
	return nil
}

// processJob dispatches a job to the appropriate handler.
func (r *Runner) processJob(ctx context.Context, job *Job) {
	fmt.Printf("   - 📥 Received job %s (type: %s)\n", job.ID, job.Type)
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
		fmt.Printf("   - ❌ %v\n", err)
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

	duration := time.Since(startTime)

	if err != nil || (result != nil && result.Status == JobStatusFailure) {
		actualErr := err
		if actualErr == nil && result != nil {
			actualErr = result.Error
		}
		fmt.Printf("   - ❌ Job %s failed (%v): %v\n", job.ID, duration, actualErr)
		stream.WriteError(actualErr, false)
		r.source.Nack(ctx, job, actualErr)
		return
	}

	if result != nil && result.Status == JobStatusRetry {
		fmt.Printf("   - 🔄 Job %s needs retry (%v)\n", job.ID, duration)
		r.source.Nack(ctx, job, result.Error)
		return
	}

	// Success
	fmt.Printf("   - ✅ Job %s completed (%v)\n", job.ID, duration)
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
