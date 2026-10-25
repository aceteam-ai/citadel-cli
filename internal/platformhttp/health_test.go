package platformhttp

import (
	"testing"
	"time"
)

func observeAt(h *Health, at time.Time, outcome Outcome) {
	status := 404
	if outcome == HTTPHealthy {
		status = 200
	}
	if outcome == HTTPUnavailable {
		status = 0
	}
	if outcome == HTTPOther {
		status = 429
	}
	h.Observe(Observation{At: at, Outcome: outcome, Status: status})
}

func TestHealthRequiresTwoFreshWindowsAndBoundedBackoff(t *testing.T) {
	base := time.Unix(1700000000, 0)
	for _, delay := range []time.Duration{0, time.Minute, 10 * time.Minute} {
		t.Run(delay.String(), func(t *testing.T) {
			var h Health
			window := 15 * time.Minute
			observeAt(&h, base, HTTPWrongAnswer)
			if h.NextAction(base.Add(2*window), window, delay) != NoRecovery {
				t.Fatal("elapsed time alone fabricated ongoing failed attempts")
			}
			observeAt(&h, base.Add(window), HTTPWrongAnswer)
			if h.NextAction(base.Add(window), window, delay) != RedialPlatform {
				t.Fatal("first fresh-failure window did not request redial")
			}
			if h.NextAction(base.Add(3*window), window, delay) != NoRecovery {
				t.Fatal("pre-redial failure evidence counted as a second window")
			}
			observeAt(&h, base.Add(2*window), HTTPWrongAnswer)
			if h.NextAction(base.Add(2*window), window, delay) != NoRecovery {
				t.Fatal("restart skipped bounded backoff")
			}
			backoff := max(time.Minute, min(delay, 5*time.Minute))
			due := base.Add(2*window + backoff)
			observeAt(&h, due.Add(-time.Nanosecond), HTTPWrongAnswer)
			if h.NextAction(due, window, delay) != NoRecovery {
				t.Fatal("restart lacked fresh evidence at the backoff boundary")
			}
			observeAt(&h, due, HTTPWrongAnswer)
			if h.NextAction(due, window, delay) != RestartWorker || h.NextAction(due, window, delay) != NoRecovery {
				t.Fatal("restart did not fire exactly once after full backoff")
			}
			// A new process cannot inherit an old restart timer during an outage.
			var next Health
			observeAt(&next, due, HTTPWrongAnswer)
			if next.NextAction(due, window, delay) != NoRecovery {
				t.Fatal("fresh process inherited restart authority")
			}
		})
	}
}

func TestHealthSuccessOfflineAndOtherHTTPClearEscalation(t *testing.T) {
	base := time.Unix(1700000000, 0)
	window := 15 * time.Minute
	for _, outcome := range []Outcome{HTTPHealthy, HTTPUnavailable, HTTPOther} {
		t.Run(string(outcome), func(t *testing.T) {
			var h Health
			observeAt(&h, base, HTTPWrongAnswer)
			observeAt(&h, base.Add(window), HTTPWrongAnswer)
			h.NextAction(base.Add(window), window, time.Minute)
			reset := base.Add(window + time.Second)
			observeAt(&h, reset, outcome)
			if h.Snapshot().FailingSince != nil || h.NextAction(reset.Add(24*time.Hour), window, time.Minute) != NoRecovery {
				t.Fatal("non-outage response left a restart armed")
			}
			if outcome == HTTPHealthy && (h.Snapshot().OKAt == nil || !h.Snapshot().OKAt.Equal(reset)) {
				t.Fatal("actual 2xx success was not surfaced")
			}
			observeAt(&h, reset.Add(time.Second), HTTPWrongAnswer)
			if h.NextAction(reset.Add(time.Second), window, time.Minute) != NoRecovery {
				t.Fatal("new failure inherited the previous streak")
			}
		})
	}
}

func TestHealthSuspensionClockRollbackAndLateEvidence(t *testing.T) {
	base := time.Unix(1700000000, 0)
	window := time.Minute
	var h Health
	observeAt(&h, base, HTTPWrongAnswer)
	observeAt(&h, base.Add(window), HTTPWrongAnswer)
	if h.NextAction(base.Add(-time.Second), window, time.Minute) != NoRecovery {
		t.Fatal("backwards check clock escalated")
	}
	h.Suspend(base.Add(window))
	if h.NextAction(base.Add(4*window), window, time.Minute) != NoRecovery {
		t.Fatal("busy/drain pause retained escalation")
	}
	observeAt(&h, base.Add(2*window), HTTPWrongAnswer)
	if h.NextAction(base.Add(2*window), window, time.Minute) != NoRecovery {
		t.Fatal("pause did not require a new idle window")
	}
	observeAt(&h, base.Add(3*window), HTTPWrongAnswer)
	// A delayed success inside the old streak moves its start forward.
	observeAt(&h, base.Add(2*window+time.Second), HTTPHealthy)
	if h.NextAction(base.Add(3*window), window, time.Minute) != NoRecovery {
		t.Fatal("delayed success failed to break old evidence")
	}
	observeAt(&h, base.Add(window), HTTPWrongAnswer)
	if h.Snapshot().OKAt == nil || !h.Snapshot().OKAt.Equal(base.Add(2*window+time.Second)) {
		t.Fatal("late failure overwrote success provenance")
	}
}

func TestHealthSuccessAfterDecisionVetoesExitAndCommitIsOnce(t *testing.T) {
	base := time.Unix(1700000000, 0)
	for _, reset := range []bool{false, true} {
		var h Health
		observeAt(&h, base, HTTPWrongAnswer)
		observeAt(&h, base.Add(time.Minute), HTTPWrongAnswer)
		h.NextAction(base.Add(time.Minute), time.Minute, time.Minute)
		observeAt(&h, base.Add(2*time.Minute), HTTPWrongAnswer)
		h.NextAction(base.Add(2*time.Minute), time.Minute, time.Minute)
		due := base.Add(3 * time.Minute)
		observeAt(&h, due, HTTPWrongAnswer)
		if h.NextAction(due, time.Minute, time.Minute) != RestartWorker {
			t.Fatal("fixture did not reserve restart")
		}
		if reset {
			observeAt(&h, due.Add(time.Nanosecond), HTTPHealthy)
		}
		calls := 0
		committed := h.CommitRestart(due.Add(time.Nanosecond), func() { calls++ })
		if committed == reset || calls != map[bool]int{false: 1, true: 0}[reset] {
			t.Fatal("success/commit fence did not linearize exit")
		}
		if h.CommitRestart(due.Add(time.Nanosecond), func() { calls++ }) {
			t.Fatal("one decision committed twice")
		}
	}
}
