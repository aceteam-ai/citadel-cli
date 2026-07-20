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
	// WriteClaimed signals that this worker has read the job off the queue and
	// committed to running it, published BEFORE handler execution. The backend
	// dispatcher (aceteam#6000) waits a short window for this event before
	// committing the full result budget: a wedged/dead-but-heartbeating node
	// never emits it, so the dispatcher fast-fails in ~3s instead of burning the
	// whole deadline. agentVersion lets the backend attribute the claim.
	WriteClaimed(agentVersion string) error

	// WriteStart signals the beginning of job processing.
	WriteStart(message string) error

	// WriteChunk sends an incremental output chunk (e.g., LLM token).
	WriteChunk(content string, index int) error

	// WriteEnd signals successful job completion with final result.
	WriteEnd(result map[string]any) error

	// WriteError signals job failure.
	WriteError(err error, recoverable bool) error

	// WriteCancelled signals job cancellation (JQS-Core terminal event).
	WriteCancelled(reason string) error
}

// NoOpStreamWriter is a StreamWriter that does nothing.
// Used when streaming is not supported or needed.
type NoOpStreamWriter struct{}

func (n *NoOpStreamWriter) WriteClaimed(agentVersion string) error       { return nil }
func (n *NoOpStreamWriter) WriteStart(message string) error              { return nil }
func (n *NoOpStreamWriter) WriteChunk(content string, index int) error   { return nil }
func (n *NoOpStreamWriter) WriteEnd(result map[string]any) error         { return nil }
func (n *NoOpStreamWriter) WriteError(err error, recoverable bool) error { return nil }
func (n *NoOpStreamWriter) WriteCancelled(reason string) error           { return nil }

// Ensure NoOpStreamWriter implements StreamWriter
var _ StreamWriter = (*NoOpStreamWriter)(nil)
