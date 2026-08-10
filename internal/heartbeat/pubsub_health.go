package heartbeat

import (
	"fmt"
	"sync"
	"time"
)

// pubSubWarnInterval bounds how often a sustained best-effort pub/sub failure
// is escalated to a warning.
//
// Heartbeats publish every 30s, so warning on every failure produced ~120
// identical lines an hour. That is why the outage in citadel-cli#722 stayed
// unnoticed for 12 hours: the signal was present but indistinguishable from
// background noise, and nothing marked the transition into failure. Silence is
// the other failure mode, so this rate-limits rather than demotes: the first
// failure of a run is always reported, and a sustained outage keeps reporting
// on a slow cadence.
const pubSubWarnInterval = 15 * time.Minute

// pubSubHealth tracks consecutive failures of the best-effort pub/sub publish.
//
// Policy: report the first failure of a run, then at most once per
// pubSubWarnInterval, then once more when publishing recovers. Every reported
// line carries the consecutive failure count and how long the channel has been
// down, so one log entry answers "how long has this been broken".
//
// Safe for concurrent use; the publishers call it from a single goroutine
// today, but the zero value must be usable and the counters are shared state.
type pubSubHealth struct {
	mu       sync.Mutex
	failures int
	since    time.Time
	lastWarn time.Time
}

// pubSubReport is the detail attached to a reported failure or recovery.
type pubSubReport struct {
	// Failures is the number of consecutive failures, including this one (for
	// a failure report) or the run that just ended (for a recovery report).
	Failures int
	// Duration is how long the channel has been failing.
	Duration time.Duration
}

// describe renders the run for an operator. The first failure of a run has no
// elapsed time worth printing: "failing for 0s" reads as a rounding artifact
// and undercuts the one line an operator is most likely to act on.
func (r pubSubReport) describe() string {
	if r.Failures <= 1 {
		return "1st consecutive failure"
	}
	return fmt.Sprintf("%d consecutive failures over %s", r.Failures, r.Duration.Round(time.Second))
}

// recordFailure notes a failed pub/sub publish. The second return value is
// true when the caller should surface this failure rather than suppress it.
func (h *pubSubHealth) recordFailure(now time.Time) (pubSubReport, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.failures == 0 {
		h.since = now
	}
	h.failures++
	report := pubSubReport{Failures: h.failures, Duration: now.Sub(h.since)}

	// First failure of a run is always reported; after that, at most once per
	// interval.
	if h.failures == 1 || now.Sub(h.lastWarn) >= pubSubWarnInterval {
		h.lastWarn = now
		return report, true
	}
	return report, false
}

// recordSuccess notes a successful pub/sub publish. The second return value is
// true when this success ended a run of failures and the caller should say so.
func (h *pubSubHealth) recordSuccess(now time.Time) (pubSubReport, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.failures == 0 {
		return pubSubReport{}, false
	}
	report := pubSubReport{Failures: h.failures, Duration: now.Sub(h.since)}
	h.failures = 0
	h.since = time.Time{}
	h.lastWarn = time.Time{}
	return report, true
}

// consecutiveFailures returns the current run length of pub/sub failures
// (0 when the channel is healthy).
func (h *pubSubHealth) consecutiveFailures() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.failures
}
