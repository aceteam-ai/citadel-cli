package platformhttp

import (
	"sync"
	"time"
)

// Outcome describes observed HTTP evidence, never a cached/method-level result.
type Outcome string

const (
	HTTPHealthy     Outcome = "http_2xx"
	HTTPWrongAnswer Outcome = "wrong_answer"
	HTTPUnavailable Outcome = "cannot_connect"
	HTTPOther       Outcome = "other_http"
)

type Observation struct {
	At      time.Time
	Outcome Outcome
	Status  int
}

type Observer func(Observation)

type RecoveryAction int

const (
	NoRecovery RecoveryAction = iota
	RedialPlatform
	RestartWorker
)

type HealthSnapshot struct {
	OKAt         *time.Time
	FailingSince *time.Time
	LastOutcome  Outcome
	LastStatus   int
}

// Health is zero-value ready. It holds only times, counters and closed labels;
// no credentials, response bodies, URLs or persistent host-state files.
type Health struct {
	mu             sync.Mutex
	okAt           time.Time
	lastAt         time.Time
	lastOutcome    Outcome
	lastStatus     int
	failingSince   time.Time
	eligibleSince  time.Time
	suspendedAt    time.Time
	redialAt       time.Time
	restartAfter   time.Time
	sequence       uint64
	actionSequence uint64
	finished       bool
	committed      bool
}

func (h *Health) clearEscalation() {
	h.eligibleSince = time.Time{}
	h.redialAt = time.Time{}
	h.restartAfter = time.Time{}
	h.finished = false
	h.committed = false
}

func (h *Health) Observe(o Observation) {
	if o.At.IsZero() {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if o.Outcome == HTTPHealthy && (h.okAt.IsZero() || o.At.After(h.okAt)) {
		h.okAt = o.At
	}
	if o.At.Before(h.lastAt) {
		// A delayed older success/non-outage response still breaks a streak
		// that straddled it. Restart timing resumes conservatively at the
		// newest known bad answer, never at an earlier failed response.
		if o.Outcome != HTTPWrongAnswer && !h.failingSince.IsZero() && !o.At.Before(h.failingSince) {
			h.clearEscalation()
			h.failingSince = h.lastAt
			if h.lastAt.After(h.suspendedAt) {
				h.eligibleSince = h.lastAt
			}
		}
		return
	}
	h.lastAt, h.lastOutcome, h.lastStatus = o.At, o.Outcome, o.Status
	h.sequence++
	if o.Outcome != HTTPWrongAnswer {
		h.failingSince = time.Time{}
		h.clearEscalation()
		return
	}
	if h.failingSince.IsZero() {
		h.failingSince = o.At
	}
	if h.eligibleSince.IsZero() && o.At.After(h.suspendedAt) {
		h.eligibleSince = o.At
	}
}

func (h *Health) Snapshot() HealthSnapshot {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := HealthSnapshot{LastOutcome: h.lastOutcome, LastStatus: h.lastStatus}
	if !h.okAt.IsZero() {
		at := h.okAt
		s.OKAt = &at
	}
	if !h.failingSince.IsZero() {
		at := h.failingSince
		s.FailingSince = &at
	}
	return s
}

// Suspend preserves operator-visible evidence but requires a fresh idle streak
// before any recovery action after draining or admitted/executing work.
func (h *Health) Suspend(now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clearEscalation()
	if now.After(h.suspendedAt) {
		h.suspendedAt = now
	}
}

// NextAction requires fresh wrong answers, not just elapsed wall time. Each
// process earns two complete windows again after startup, followed by bounded
// backoff. A dead network, auth/quota refusal, success or busy/drain suspension
// cancels escalation. No restart state is inherited from another process.
func (h *Health) NextAction(now time.Time, window, backoff time.Duration) RecoveryAction {
	h.mu.Lock()
	defer h.mu.Unlock()
	if window <= 0 || h.finished || h.lastOutcome != HTTPWrongAnswer || h.eligibleSince.IsZero() || now.Before(h.lastAt) {
		return NoRecovery
	}
	if h.redialAt.IsZero() {
		if h.lastAt.Sub(h.eligibleSince) < window {
			return NoRecovery
		}
		h.redialAt = now
		h.actionSequence = h.sequence
		return RedialPlatform
	}
	if h.restartAfter.IsZero() {
		if h.sequence <= h.actionSequence || h.lastAt.Sub(h.redialAt) < window {
			return NoRecovery
		}
		backoff = max(time.Minute, min(backoff, 5*time.Minute))
		h.restartAfter = now.Add(backoff)
		h.actionSequence = h.sequence
		return NoRecovery
	}
	if h.sequence <= h.actionSequence || h.lastAt.Before(h.restartAfter) || now.Before(h.restartAfter) {
		return NoRecovery
	}
	h.finished = true
	return RestartWorker
}

// CommitRestart revalidates evidence inside the final idle/admission fence.
// A success or offline/refusal response after the decision vetoes exit. The
// action must only exit (or be an inert test callback); it cannot call Health.
func (h *Health) CommitRestart(now time.Time, exit func()) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.finished || h.committed || h.lastOutcome != HTTPWrongAnswer || h.restartAfter.IsZero() || h.lastAt.Before(h.restartAfter) || now.Before(h.lastAt) {
		return false
	}
	h.committed = true
	exit()
	return true
}
