package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// jobTimeoutPayloadKey is the wire field the backend dispatcher injects to give
// a job a per-execution budget (aceteam#6000). It is a RELATIVE duration in
// milliseconds measured from the moment the worker begins executing the job --
// NOT an absolute epoch deadline. A relative duration is deliberate: nodes are
// user-owned hardware, so an absolute deadline would be hostage to clock skew
// between the backend and the node. Keep this string in sync with the backend
// (`python-backend/routes/aceteam_mcp_code.py`).
const jobTimeoutPayloadKey = "timeout_ms"

// jobExecTimeout extracts the optional per-job execution budget from a job
// payload. It returns (duration, true) only when jobTimeoutPayloadKey is present
// and strictly positive; every other case returns ok=false so the caller
// preserves the pre-existing no-timeout behavior exactly.
//
// This keeps the timeout strictly opt-in. Older backends that never set the
// field, and job types that are legitimately unbounded (model download, build,
// provision), are never capped -- there is deliberately no blanket ceiling.
func jobExecTimeout(job *Job) (time.Duration, bool) {
	if job == nil || job.Payload == nil {
		return 0, false
	}
	raw, ok := job.Payload[jobTimeoutPayloadKey]
	if !ok {
		return 0, false
	}
	ms, ok := coerceToInt64(raw)
	if !ok || ms <= 0 {
		return 0, false
	}
	return time.Duration(ms) * time.Millisecond, true
}

// coerceToInt64 best-effort converts a JSON-decoded payload value to an int64.
// Redis payloads reach the worker via json.Unmarshal, so a numeric field is a
// float64; a payload assembled directly may carry int/int64/json.Number, and a
// stringly-typed transport may carry a decimal string. Anything else fails.
func coerceToInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case float32:
		return int64(n), true
	case int:
		return int64(n), true
	case int64:
		return n, true
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return i, true
		}
		if f, err := n.Float64(); err == nil {
			return int64(f), true
		}
		return 0, false
	case string:
		if i, err := strconv.ParseInt(n, 10, 64); err == nil {
			return i, true
		}
		if f, err := strconv.ParseFloat(n, 64); err == nil {
			return int64(f), true
		}
		return 0, false
	default:
		return 0, false
	}
}

// deadlineExceededError marks a handler that was abandoned because it exceeded
// its per-job execution budget. It flows through the SAME terminal-error path
// as any other handler failure, so the backend dispatcher (subscribed to
// stream:v1:{jobId}) receives a fast, honest error instead of hanging until its
// own wait deadline.
type deadlineExceededError struct {
	timeout time.Duration
}

func (e *deadlineExceededError) Error() string {
	return fmt.Sprintf(
		"job exceeded its execution deadline of %s and was abandoned by the worker",
		e.timeout,
	)
}

// executeWithDeadline runs handler.Execute under a child context bounded by
// timeout, but never blocks the job loop past that deadline (aceteam#6000).
//
// The handler runs in its own goroutine. If it honors context cancellation
// (e.g. SHELL_COMMAND via exec.CommandContext) the underlying child process is
// terminated; if it ignores cancellation the goroutine keeps running in the
// background while this function returns and the loop advances. Either way one
// wedged handler can no longer stall every subsequent job on the node.
//
// On timeout it returns a *deadlineExceededError; the caller's existing failure
// path publishes the terminal error event and Nacks on the LIVE parent context
// (never the expired child) so the dispatcher receives a real error event.
func (r *Runner) executeWithDeadline(
	ctx context.Context,
	handler JobHandler,
	job *Job,
	stream StreamWriter,
	timeout time.Duration,
) (*JobResult, error) {
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	type handlerResult struct {
		result *JobResult
		err    error
	}
	// Buffered (size 1) so a handler that ignores cancellation and finishes
	// AFTER the deadline can still send its result and exit, rather than leaking
	// blocked on the channel forever.
	done := make(chan handlerResult, 1)
	go func() {
		result, err := handler.Execute(execCtx, job, stream)
		done <- handlerResult{result: result, err: err}
	}()

	select {
	case hr := <-done:
		// If the handler returned an error exactly as the deadline elapsed (e.g.
		// exec.CommandContext killed the child, yielding "signal: killed"),
		// prefer the clear deadline message over the incidental one.
		if hr.err != nil && errors.Is(execCtx.Err(), context.DeadlineExceeded) {
			return nil, &deadlineExceededError{timeout: timeout}
		}
		return hr.result, hr.err
	case <-execCtx.Done():
		if errors.Is(execCtx.Err(), context.DeadlineExceeded) {
			r.log("error", "Job %s abandoned: exceeded execution deadline of %s", job.ID, timeout)
			return nil, &deadlineExceededError{timeout: timeout}
		}
		// Parent context cancelled (worker shutdown): surface the raw error so
		// the loop unwinds without misreporting a deadline breach.
		return nil, execCtx.Err()
	}
}
