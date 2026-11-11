package worker

import (
	"context"
	"fmt"
	"log"
	"time"
)

// TerminalResultTTL is how long a retained terminal MEETING_JOIN event lives
// in Redis (citadel-cli#1553). The coordinator reads it at startup to recover
// a result that was published while no replica was subscribed. API mode stores
// it through apiTerminalRetainer.
const TerminalResultTTL = 72 * time.Hour

// terminalRetainTimeout bounds the retained write so it cannot stall the
// terminal path behind a slow Redis or proxy.
const terminalRetainTimeout = 5 * time.Second

// TerminalResultKey is the Redis key holding a job's retained terminal event:
// the exact stream:v1 envelope published on its end, error or cancelled event.
func TerminalResultKey(jobID string) string {
	return fmt.Sprintf("job:%s:result", jobID)
}

// retainsTerminalResult reports whether a job type's terminal event is also
// retained. Only MEETING_JOIN: its result (a transcript) is published once,
// hours after dispatch, and is otherwise lost if no coordinator is subscribed.
func retainsTerminalResult(jobType string) bool {
	return jobType == JobTypeMeetingJoin
}

// retentionLogf reports a failed retained write. A package var so tests can
// capture it.
var retentionLogf = log.Printf

// terminalRetainer is the client half of retention. The direct Redis client
// implements it; an API-proxy implementation plugs in via apiTerminalRetainer.
type terminalRetainer interface {
	RetainTerminalEvent(ctx context.Context, key string, eventJSON []byte, ttl time.Duration) (bool, error)
}

// retainTerminalEvent stores eventJSON under the job's result key. It never
// returns an error: retention is a recovery copy, and the terminal publish
// (with its own retry) must not depend on it. The write uses a context
// detached from cancellation so a shutdown that ends the meeting still leaves
// the result behind.
func retainTerminalEvent(ctx context.Context, client terminalRetainer, jobID string, eventJSON []byte) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), terminalRetainTimeout)
	defer cancel()
	if _, err := client.RetainTerminalEvent(rctx, TerminalResultKey(jobID), eventJSON, TerminalResultTTL); err != nil {
		retentionLogf("job %s: retaining terminal result failed (publish unaffected): %v", jobID, err)
	}
}
