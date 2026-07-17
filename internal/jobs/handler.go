// internal/jobs/handler.go
package jobs

import (
	"context"
	"fmt"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

// JobContext can hold shared resources like a logger, config, etc.
type JobContext struct {
	// LogFn is an optional callback for logging (if nil, prints to stdout)
	LogFn func(level, msg string)

	// Ctx is the per-job execution context threaded from the worker runner.
	// Handlers that shell out or perform cancellable I/O should honor it (e.g.
	// exec.CommandContext) so a per-job deadline or cancellation actually
	// terminates in-flight work (aceteam#6000). It may be nil for callers that
	// predate deadline propagation; use Context() to read it safely.
	Ctx context.Context
}

// Context returns the job's execution context, falling back to
// context.Background() when unset so handlers can pass it to exec.CommandContext
// unconditionally.
func (c *JobContext) Context() context.Context {
	if c.Ctx != nil {
		return c.Ctx
	}
	return context.Background()
}

// Log outputs a message - uses LogFn callback if set, otherwise prints to stdout.
func (c *JobContext) Log(level, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if c.LogFn != nil {
		c.LogFn(level, msg)
	} else {
		fmt.Printf("%s\n", msg)
	}
}

// JobHandler is the interface that all job executors must implement.
type JobHandler interface {
	Execute(ctx JobContext, job *nexus.Job) (output []byte, err error)
}
