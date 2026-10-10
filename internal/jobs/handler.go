// internal/jobs/handler.go
package jobs

import (
	"context"
	"fmt"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

// ProgressEvent is a throttled, non-terminal progress update emitted by a
// long-running job handler (FILE_INDEX today; aceteam#10876 C1, design 2a.3).
// It carries counts and BASENAMES only — never full paths or content — so it is
// safe to publish on the job's stream and mirror into Redis.
type ProgressEvent struct {
	// Stage is the coarse phase. Only the approved ProgressStage constants are
	// emitted by FILE_INDEX.
	Stage ProgressStage
	// Done / Total measure progress in Unit terms. Total == 0 means unknown.
	Done  int
	Total int
	// Unit is what Done / Total count: "files" | "pages" | "chunks".
	Unit string
	// Current is the BASENAME of the item being processed (never a full path).
	Current string
	// Counts carries optional additive per-stage tallies (indexed/skipped/...).
	Counts map[string]int
	// InFlight is internal accounting for candidates that have begun but do not
	// yet have a terminal disposition. It is never projected onto the wire.
	InFlight int
	// Final marks a successfully completed full walk. It is internal/non-wire
	// state and is the only condition that permits a 100 percent projection.
	Final bool
	// Flush forces a non-final snapshot through throttling (for example a
	// cooperative partial stop). It is internal and never projected onto wire.
	Flush bool
}

// ProgressStage is the closed FILE_INDEX progress vocabulary.
type ProgressStage string

const (
	ProgressStagePlan    ProgressStage = "plan"
	ProgressStageExtract ProgressStage = "extract"
	ProgressStageEmbed   ProgressStage = "embed"
	ProgressStageUpsert  ProgressStage = "upsert"
	ProgressStagePrune   ProgressStage = "prune"
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
	//
	// Ctx cancellation is a HARD abort (a watchdog deadline or worker shutdown):
	// in-flight work is terminated. It is distinct from CancelRequested below,
	// which is a cooperative, graceful stop.
	Ctx context.Context

	// Progress, when set, receives throttled progress updates from a long-running
	// handler. nil for callers that do not surface progress; handlers call
	// EmitProgress to invoke it safely.
	Progress func(ProgressEvent)

	// CancelRequested, when set, reports whether a COOPERATIVE (graceful) cancel
	// has been requested for this job — distinct from Ctx cancellation. A handler
	// polls it at safe unit boundaries (e.g. between files) and stops gracefully,
	// committing the current unit, rather than aborting in-flight work. nil means
	// "never cooperatively cancelled"; handlers call CancelRequestedNow to read it
	// safely (aceteam#10876 C1).
	CancelRequested func() bool
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

// EmitProgress forwards ev to the Progress callback when one is wired, and is a
// no-op otherwise, so handlers can report progress unconditionally.
func (c *JobContext) EmitProgress(ev ProgressEvent) {
	if c.Progress != nil {
		c.Progress(ev)
	}
}

// CancelRequestedNow reports whether a cooperative (graceful) cancel has been
// requested. Safe to call when no cancel source is wired (returns false).
func (c *JobContext) CancelRequestedNow() bool {
	return c.CancelRequested != nil && c.CancelRequested()
}

// cancelRequestedCtxKey carries a cooperative-cancel predicate across the
// worker -> LegacyHandlerAdapter -> JobContext boundary without widening the
// JobHandler signature. The worker runner sets it on the handler context (for
// the job types that get a mid-job cancel watch); the adapter reads it into
// JobContext.CancelRequested.
type cancelRequestedCtxKey struct{}

// WithCancelRequested returns a context carrying a cooperative-cancel predicate.
// A nil predicate returns ctx unchanged.
func WithCancelRequested(ctx context.Context, fn func() bool) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, cancelRequestedCtxKey{}, fn)
}

// CancelRequestedFrom extracts a cooperative-cancel predicate from ctx, or nil
// when none was set.
func CancelRequestedFrom(ctx context.Context) func() bool {
	if ctx == nil {
		return nil
	}
	fn, _ := ctx.Value(cancelRequestedCtxKey{}).(func() bool)
	return fn
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
