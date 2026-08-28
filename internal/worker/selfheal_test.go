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
// seeds oldestInFlightUnixNano when inFlight > 0 -- every existing caller of
// this helper models a single job in flight, where "when it started" and
// "when the in-flight streak began" are the same instant, so this keeps their
// intent unchanged after the STUCK check switched to OldestInFlightAt (see
// TestLivenessMonitorCheck_StuckUsesOldestInFlightNotLastJob below for a case
// where the two diverge).
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
// test for the citadel-cli#489 review WANT: on a maxConcurrency=1 node with
// the #489 long-session async lane, a wedged MEETING_JOIN can sit in_flight
// for hours while ordinary SHELL_COMMAND jobs keep completing beside it on
// the sequential lane. Before this fix, the STUCK check measured time since
// WorkerState.LastJobAt, which every one of those short jobs resets -- so as
// long as short jobs kept arriving, the check would never see the meeting as
// stuck even after it blew well past the ceiling. This drives real
// WorkerState transitions (not the stateWith fixture, which cannot represent
// "two overlapping jobs with different start times") through
// RecordJobReceived/RecordJobDone to prove the fix.
func TestLivenessMonitorCheck_StuckUsesOldestInFlightNotLastJob(t *testing.T) {
	now := time.Now()
	s := NewWorkerState()
	s.startedAt = now.Add(-10 * time.Hour)
	s.lastPollUnixNano = now.Add(-1 * time.Second).UnixNano()

	// The wedged long-session job starts 6h ago (past the 5h test ceiling)...
	s.oldestInFlightUnixNano = now.Add(-6 * time.Hour).UnixNano()
	s.inFlight = 1
	// ...and LastJobAt is kept FRESH by a stream of short jobs completing
	// beside it, exactly as concurrent SHELL_COMMAND traffic would. Simulate
	// one such job via the real API so both fields update the way production
	// code actually updates them.
	s.RecordJobReceived() // in_flight now 2 (meeting + this short job)
	s.RecordJobDone(true) // in_flight back to 1 (only the meeting remains)

	if held := now.Sub(time.Unix(0, s.lastJobUnixNano)); held > time.Minute {
		t.Fatalf("precondition: LastJobAt should be fresh (just stamped), got %s old", held)
	}
	if held := now.Sub(time.Unix(0, s.oldestInFlightUnixNano)); held < 5*time.Hour {
		t.Fatalf("precondition: OldestInFlightAt should still reflect the 6h-old meeting, got %s old", held)
	}

	reason, wedged := newTestMonitor(s, false).check(now)
	if !wedged {
		t.Fatal("a job stuck 6h (> 5h ceiling) was not flagged wedged, even though short jobs kept LastJobAt fresh")
	}
	if !strings.Contains(reason, "stuck") {
		t.Errorf("expected a 'stuck' reason, got: %q", reason)
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
