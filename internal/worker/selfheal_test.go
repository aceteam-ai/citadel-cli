package worker

import (
	"strings"
	"testing"
	"time"
)

// newTestMonitor builds a monitor with fixed thresholds and no real exit, so
// check() can be exercised deterministically.
func newTestMonitor(state *WorkerState, draining bool) *LivenessMonitor {
	return &LivenessMonitor{
		state:        state,
		stallTimeout: 10 * time.Minute,
		stuckTimeout: 5 * time.Hour,
		graceStart:   2 * time.Minute,
		// Default: monitor started long ago so grace has passed. The startup-grace
		// case overrides this to a recent value.
		startedAt:  time.Now().Add(-time.Hour),
		isDraining: func() bool { return draining },
		log:        func(string, string) {},
		onWedge:    func(string) {},
	}
}

// stateWith stamps a WorkerState with a chosen startedAt, lastPoll, lastJob and
// in-flight count so check() sees a specific liveness snapshot. lastJob also
// seeds oldestInFlightUnixNano AND the executing streak (oldestExecutingUnixNano
// + executing) when inFlight > 0 -- every existing caller of this helper models
// a single job that is BOTH in flight and executing (the pre-#908 world where
// claim and execute were one span), where "when it started" and "when the
// executing streak began" are the same instant. That keeps their intent
// unchanged after the STUCK check switched to reading OldestExecutingAt/Executing
// (citadel-cli#908). See TestLivenessMonitorCheck_StuckUsesOldestInFlightNotLastJob
// (executing) and TestLivenessMonitorCheck_LongQueuedWaitDoesNotTripStuck (queued
// but not executing) below for the cases these now diverge on.
func stateWith(started, lastPoll, lastJob time.Time, inFlight int64) *WorkerState {
	s := NewWorkerState()
	s.startedAt = started
	if !lastPoll.IsZero() {
		s.lastPollUnixNano = lastPoll.UnixNano()
	}
	if !lastJob.IsZero() {
		s.lastJobUnixNano = lastJob.UnixNano()
		if inFlight > 0 {
			s.oldestInFlightUnixNano = lastJob.UnixNano()
			// These callers model an EXECUTING job, so the executing streak
			// began at the same instant.
			s.executing = inFlight
			s.oldestExecutingUnixNano = lastJob.UnixNano()
		}
	}
	s.inFlight = inFlight
	return s
}

func TestLivenessMonitorCheck(t *testing.T) {
	now := time.Now()

	t.Run("healthy: polling recently, nothing in flight", func(t *testing.T) {
		s := stateWith(now.Add(-time.Hour), now.Add(-3*time.Second), time.Time{}, 0)
		if reason, wedged := newTestMonitor(s, false).check(now); wedged {
			t.Fatalf("healthy node flagged wedged: %s", reason)
		}
	})

	t.Run("stall: no poll for > threshold with nothing in flight -> wedged", func(t *testing.T) {
		s := stateWith(now.Add(-time.Hour), now.Add(-15*time.Minute), time.Time{}, 0)
		if _, wedged := newTestMonitor(s, false).check(now); !wedged {
			t.Fatal("stalled loop (no poll 15m, in_flight 0) not flagged wedged")
		}
	})

	t.Run("busy long job: no poll but a job is in flight -> NOT wedged (stuck ceiling not reached)", func(t *testing.T) {
		// A legitimate long job: last poll is old (loop is inside the handler),
		// in_flight==1, but the job has only been running 15m -- far under the
		// stuck ceiling. Must not restart.
		s := stateWith(now.Add(-time.Hour), now.Add(-15*time.Minute), now.Add(-15*time.Minute), 1)
		if reason, wedged := newTestMonitor(s, false).check(now); wedged {
			t.Fatalf("busy node with a legitimate long job flagged wedged: %s", reason)
		}
	})

	t.Run("stuck: a job in flight past the stuck ceiling -> wedged", func(t *testing.T) {
		s := stateWith(now.Add(-10*time.Hour), now.Add(-6*time.Hour), now.Add(-6*time.Hour), 1)
		if _, wedged := newTestMonitor(s, false).check(now); !wedged {
			t.Fatal("job in flight 6h (> 5h ceiling) not flagged wedged")
		}
	})

	t.Run("draining: intentional pause is never a wedge", func(t *testing.T) {
		s := stateWith(now.Add(-time.Hour), now.Add(-15*time.Minute), time.Time{}, 0)
		if _, wedged := newTestMonitor(s, true).check(now); wedged {
			t.Fatal("draining worker flagged wedged")
		}
	})

	t.Run("startup grace: a just-started monitor with no poll yet is not wedged", func(t *testing.T) {
		s := stateWith(now.Add(-30*time.Second), time.Time{}, time.Time{}, 0)
		m := newTestMonitor(s, false)
		m.startedAt = now.Add(-30 * time.Second) // monitor within its 2min grace
		if _, wedged := m.check(now); wedged {
			t.Fatal("monitor inside startup grace flagged wedged")
		}
	})

	t.Run("never polled past grace with nothing in flight -> wedged", func(t *testing.T) {
		s := stateWith(now.Add(-10*time.Minute), time.Time{}, time.Time{}, 0)
		if _, wedged := newTestMonitor(s, false).check(now); !wedged {
			t.Fatal("worker up 10m that never polled not flagged wedged")
		}
	})
}

// TestLivenessMonitorCheck_StuckUsesOldestInFlightNotLastJob is the regression
// test for the citadel-cli#489 review WANT, updated for citadel-cli#908: on a
// maxConcurrency=1 node with an async lane, a wedged MEETING_JOIN can sit
// EXECUTING for hours while ordinary SHELL_COMMAND jobs keep completing beside
// it. Before #489, the STUCK check measured time since WorkerState.LastJobAt,
// which every one of those short jobs resets -- so as long as short jobs kept
// arriving, the check would never see the meeting as stuck even after it blew
// past the ceiling. Post-#908 the STUCK check reads OldestExecutingAt/Executing,
// so this drives the real executing bracket (RecordJobExecuting/
// RecordJobExecuteDone) for the interleaved short job to prove the fix still
// holds against the field the check now reads.
func TestLivenessMonitorCheck_StuckUsesOldestInFlightNotLastJob(t *testing.T) {
	now := time.Now()
	s := NewWorkerState()
	s.startedAt = now.Add(-10 * time.Hour)
	s.lastPollUnixNano = now.Add(-1 * time.Second).UnixNano()

	// The wedged long-session job started EXECUTING 6h ago (past the 5h test
	// ceiling). Model it as one in-flight-and-executing job whose streaks began
	// then.
	sixHoursAgo := now.Add(-6 * time.Hour).UnixNano()
	s.inFlight = 1
	s.oldestInFlightUnixNano = sixHoursAgo
	s.executing = 1
	s.oldestExecutingUnixNano = sixHoursAgo
	// ...and LastJobAt is kept FRESH by a stream of short jobs completing beside
	// it, exactly as concurrent SHELL_COMMAND traffic would. Drive one such job
	// through the real API (both the received AND executing brackets) so every
	// field updates the way production code actually updates them.
	s.RecordJobReceived()    // in_flight now 2 (meeting + this short job)
	s.RecordJobExecuting()   // executing now 2
	s.RecordJobExecuteDone() // executing back to 1 (only the meeting executes)
	s.RecordJobDone(true)    // in_flight back to 1

	if held := now.Sub(time.Unix(0, s.lastJobUnixNano)); held > time.Minute {
		t.Fatalf("precondition: LastJobAt should be fresh (just stamped), got %s old", held)
	}
	if held := now.Sub(time.Unix(0, s.oldestExecutingUnixNano)); held < 5*time.Hour {
		t.Fatalf("precondition: OldestExecutingAt should still reflect the 6h-old meeting, got %s old", held)
	}

	reason, wedged := newTestMonitor(s, false).check(now)
	if !wedged {
		t.Fatal("a job stuck 6h (> 5h ceiling) was not flagged wedged, even though short jobs kept LastJobAt fresh")
	}
	if !strings.Contains(reason, "stuck") {
		t.Errorf("expected a 'stuck' reason, got: %q", reason)
	}
}

// TestLivenessMonitorCheck_LongQueuedWaitDoesNotTripStuck is the companion the
// citadel-cli#908 design (§2d) asks for: a job that has been legitimately
// QUEUED for hours (in_flight, but NOT executing -- e.g. behind a large model
// pull on the exec-concurrency-1 general lane) must NOT trip the STUCK check,
// while a job that has been EXECUTING that long still does. This is the whole
// reason STUCK reads OldestExecutingAt/Executing rather than the in-flight
// streak: measuring in-flight would restart the node for a perfectly healthy
// backlog.
func TestLivenessMonitorCheck_LongQueuedWaitDoesNotTripStuck(t *testing.T) {
	now := time.Now()
	sixHoursAgo := now.Add(-6 * time.Hour).UnixNano()

	// A job claimed and admitted 6h ago but still QUEUED: in_flight=1 and the
	// in-flight streak is 6h old, but executing=0 (nothing is in a handler).
	s := NewWorkerState()
	s.startedAt = now.Add(-10 * time.Hour)
	s.lastPollUnixNano = now.Add(-1 * time.Second).UnixNano()
	s.inFlight = 1
	s.oldestInFlightUnixNano = sixHoursAgo
	// executing stays 0, oldestExecutingUnixNano stays 0.

	if reason, wedged := newTestMonitor(s, false).check(now); wedged {
		t.Fatalf("a job QUEUED (not executing) for 6h must not trip STUCK, got wedged: %q", reason)
	}

	// Now promote that same job to EXECUTING, with the executing streak 6h old:
	// it must trip STUCK, proving the check reads the executing streak, not a
	// blanket "queued or executing" in-flight one.
	s.executing = 1
	s.oldestExecutingUnixNano = sixHoursAgo
	if _, wedged := newTestMonitor(s, false).check(now); !wedged {
		t.Fatal("a job EXECUTING for 6h (> 5h ceiling) must trip STUCK")
	}
}

func TestSelfHealEnabled(t *testing.T) {
	t.Run("default on", func(t *testing.T) {
		t.Setenv(selfHealEnabledEnvVar, "")
		if !selfHealEnabled() {
			t.Fatal("self-heal should default ON")
		}
	})
	for _, v := range []string{"0", "false", "no", "off", "FALSE"} {
		t.Run("disabled by "+v, func(t *testing.T) {
			t.Setenv(selfHealEnabledEnvVar, v)
			if selfHealEnabled() {
				t.Fatalf("%q should disable self-heal", v)
			}
		})
	}
}

func TestNewLivenessMonitorDisabled(t *testing.T) {
	t.Setenv(selfHealEnabledEnvVar, "false")
	if m := NewLivenessMonitor(NewWorkerState(), nil, nil); m != nil {
		t.Fatal("NewLivenessMonitor should return nil when disabled")
	}
	// A nil monitor's Run is a safe no-op.
	var nilMon *LivenessMonitor
	nilMon.Run(nil)
}
