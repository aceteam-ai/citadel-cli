package worker

import "context"

// JobHandler processes jobs of a specific type.
// Handlers are registered with the Runner and dispatched based on CanHandle().
type JobHandler interface {
	// CanHandle returns true if this handler can process the given job type.
	CanHandle(jobType string) bool

	// Execute processes the job.
	// The stream parameter allows streaming responses (can be nil for non-streaming).
	// Returns a JobResult with status and output.
	Execute(ctx context.Context, job *Job, stream StreamWriter) (*JobResult, error)
}

// StreamWriter enables streaming job output to subscribers.
// For Redis sources, this publishes to Redis Pub/Sub.
// For Nexus sources, this may be a no-op.
type StreamWriter interface {
	// WriteStart signals the beginning of job processing.
	WriteStart(message string) error

	// WriteChunk sends an incremental output chunk (e.g., LLM token).
	WriteChunk(content string, index int) error

	// WriteEnd signals successful job completion with final result.
	WriteEnd(result map[string]any) error

	// WriteError signals job failure.
	WriteError(err error, recoverable bool) error
}

// NoOpStreamWriter is a StreamWriter that does nothing.
// Used when streaming is not supported or needed.
type NoOpStreamWriter struct{}

func (n *NoOpStreamWriter) WriteStart(message string) error       { return nil }
func (n *NoOpStreamWriter) WriteChunk(content string, index int) error { return nil }
func (n *NoOpStreamWriter) WriteEnd(result map[string]any) error  { return nil }
func (n *NoOpStreamWriter) WriteError(err error, recoverable bool) error { return nil }

// Ensure NoOpStreamWriter implements StreamWriter
var _ StreamWriter = (*NoOpStreamWriter)(nil)
