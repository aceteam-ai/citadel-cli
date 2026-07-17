package worker

import (
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
// in-flight count so check() sees a specific liveness snapshot.
func stateWith(started, lastPoll, lastJob time.Time, inFlight int64) *WorkerState {
	s := NewWorkerState()
	s.startedAt = started
	if !lastPoll.IsZero() {
		s.lastPollUnixNano = lastPoll.UnixNano()
	}
	if !lastJob.IsZero() {
		s.lastJobUnixNano = lastJob.UnixNano()
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
