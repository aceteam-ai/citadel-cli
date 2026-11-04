package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/platformhttp"
)

func isolateSelfHealHome(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	for _, key := range []string{"HOME", "USERPROFILE", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME"} {
		path := filepath.Join(root, key)
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
		t.Setenv(key, path)
	}
}

func TestPlatformMonitorRedialsThenGuardedExit(t *testing.T) {
	isolateSelfHealHome(t)
	base := time.Unix(1700000000, 0)
	s := NewWorkerState()
	m := newTestMonitor(s, false)
	m.startedAt = base.Add(-time.Hour)
	m.stallTimeout, m.stuckTimeout = 0, 0
	m.platformWindow, m.platformBackoff = time.Minute, time.Minute
	var redials, exits int
	m.onWedge = func(string) { exits++ }
	m.SetPlatformRecovery(func() { redials++ }, func(exit func()) bool { exit(); return true })
	step := func(elapsed time.Duration) (string, bool) {
		now := base.Add(elapsed)
		s.ObservePlatform(platformhttp.Observation{At: now, Outcome: platformhttp.HTTPWrongAnswer, Status: 404})
		return m.check(now)
	}
	step(0)
	if _, w := step(time.Minute); w || redials != 1 {
		t.Fatal("first failure window must redial without exit")
	}
	if _, w := step(2 * time.Minute); w {
		t.Fatal("second failure window skipped backoff")
	}
	reason, w := step(3 * time.Minute)
	if !w || !m.handleWedge(reason, base.Add(3*time.Minute)) || exits != 1 {
		t.Fatal("bounded recovery did not use guarded exit")
	}
	snap := s.Snapshot()
	if snap.PlatformFailingSince == nil || snap.PlatformLastHTTPStatus != 404 || snap.PlatformLastOutcome != platformhttp.HTTPWrongAnswer {
		t.Fatal("worker snapshot lost HTTP provenance")
	}
}

func TestPlatformMonitorBusyDrainAndExitVeto(t *testing.T) {
	isolateSelfHealHome(t)
	base := time.Unix(1700000000, 0)
	s := NewWorkerState()
	m := newTestMonitor(s, false)
	m.startedAt = base.Add(-time.Hour)
	m.stallTimeout, m.stuckTimeout = 0, 0
	m.platformWindow, m.platformBackoff = time.Minute, time.Minute
	var redials, exits int
	m.onWedge = func(string) { exits++ }
	m.SetPlatformRecovery(func() { redials++ }, func(func()) bool { return false })
	s.ObservePlatform(platformhttp.Observation{At: base, Outcome: platformhttp.HTTPWrongAnswer, Status: 503})
	s.inFlight = 1
	s.ObservePlatform(platformhttp.Observation{At: base.Add(time.Hour), Outcome: platformhttp.HTTPWrongAnswer, Status: 503})
	m.check(base.Add(time.Hour))
	if redials != 0 || exits != 0 {
		t.Fatal("long legitimate job triggered recovery")
	}
	s.inFlight = 0
	m.isDraining = func() bool { return true }
	m.check(base.Add(2 * time.Hour))
	m.isDraining = func() bool { return false }
	for _, elapsed := range []time.Duration{2*time.Hour + time.Second, 2*time.Hour + time.Minute + time.Second, 2*time.Hour + 2*time.Minute + time.Second, 2*time.Hour + 3*time.Minute + time.Second} {
		now := base.Add(elapsed)
		s.ObservePlatform(platformhttp.Observation{At: now, Outcome: platformhttp.HTTPWrongAnswer, Status: 503})
		if reason, w := m.check(now); w {
			if m.handleWedge(reason, now) {
				t.Fatal("idle fence veto was ignored")
			}
		}
	}
	if redials != 1 || exits != 0 {
		t.Fatal("busy/drain reset or final idle veto failed")
	}
}

func TestPlatformMonitorLateSuccessVetoKeepsStallAndStuckChecksAlive(t *testing.T) {
	for _, nextWedge := range []string{"stall", "stuck"} {
		t.Run(nextWedge, func(t *testing.T) {
			isolateSelfHealHome(t)
			base := time.Unix(1700000000, 0)
			s := NewWorkerState()
			m := newTestMonitor(s, false)
			m.startedAt = base.Add(-time.Hour)
			m.stallTimeout, m.stuckTimeout = 0, 0
			m.platformWindow, m.platformBackoff = time.Minute, time.Minute
			now := base
			m.now = func() time.Time { return now }
			var redials, exits, fenceCalls int
			m.onWedge = func(string) { exits++ }
			m.SetPlatformRecovery(func() { redials++ }, func(action func()) bool {
				fenceCalls++
				// The runner DID execute the action and reports success. Only
				// CommitRestart can veto after this last-moment HTTP success.
				now = now.Add(time.Nanosecond)
				s.ObservePlatform(platformhttp.Observation{At: now, Outcome: platformhttp.HTTPHealthy, Status: 200})
				action()
				return true
			})
			for _, elapsed := range []time.Duration{0, time.Minute, 2 * time.Minute} {
				now = base.Add(elapsed)
				s.ObservePlatform(platformhttp.Observation{At: now, Outcome: platformhttp.HTTPWrongAnswer, Status: 503})
				if reason, wedged := m.check(now); wedged {
					t.Fatalf("fixture escalated early: %s", reason)
				}
			}
			now = base.Add(3 * time.Minute)
			s.ObservePlatform(platformhttp.Observation{At: now, Outcome: platformhttp.HTTPWrongAnswer, Status: 503})
			reason, wedged := m.check(now)
			if !wedged || !m.platformRestart || redials != 1 {
				t.Fatal("fixture did not reserve platform restart")
			}
			// Run terminates only when handleWedge returns true. Returning
			// true here would permanently disable ALL subsequent checks.
			if m.handleWedge(reason, now) {
				t.Fatal("late-success commit veto would stop the monitor loop")
			}
			if fenceCalls != 1 || exits != 0 {
				t.Fatalf("fence calls = %d, exits = %d; want 1, 0", fenceCalls, exits)
			}
			now = now.Add(10 * time.Minute)
			wantReason := "consume loop stalled"
			if nextWedge == "stall" {
				m.stallTimeout = time.Minute
				s.lastPollUnixNano = now.Add(-2 * time.Minute).UnixNano()
			} else {
				m.stuckTimeout = time.Minute
				s.inFlight, s.executing = 1, 1
				s.oldestExecutingUnixNano = now.Add(-2 * time.Minute).UnixNano()
				wantReason = "job stuck in handler"
			}
			reason, wedged = m.check(now)
			if !wedged || m.platformRestart || !strings.Contains(reason, wantReason) {
				t.Fatalf("later %s check lost: reason=%q wedged=%t platform=%t", nextWedge, reason, wedged, m.platformRestart)
			}
			if !m.handleWedge(reason, now) || exits != 1 || fenceCalls != 1 {
				t.Fatal("monitor failed to handle a subsequent ordinary wedge")
			}
		})
	}
}

func TestPlatformMonitorDrainAloneRequiresNewFailureWindows(t *testing.T) {
	isolateSelfHealHome(t)
	base := time.Unix(1700000000, 0)
	s := NewWorkerState()
	m := newTestMonitor(s, false)
	m.startedAt = base.Add(-time.Hour)
	m.stallTimeout, m.stuckTimeout = 0, 0
	m.platformWindow, m.platformBackoff = time.Minute, time.Minute
	draining := false
	m.isDraining = func() bool { return draining }
	redials := 0
	m.SetPlatformRecovery(func() { redials++ }, func(func()) bool {
		t.Fatal("drain-only reset must not reach the restart fence")
		return false
	})
	step := func(elapsed time.Duration) {
		t.Helper()
		now := base.Add(elapsed)
		s.ObservePlatform(platformhttp.Observation{At: now, Outcome: platformhttp.HTTPWrongAnswer, Status: 503})
		if reason, wedged := m.check(now); wedged {
			t.Fatalf("at %s: unexpected restart after drain: %s", elapsed, reason)
		}
	}
	// Nothing is busy and startup grace has already passed: only draining
	// can clear the fully armed redial/backoff state in this fixture.
	step(0)
	step(time.Minute)
	step(2 * time.Minute)
	if redials != 1 {
		t.Fatal("fixture did not redial before draining")
	}
	draining = true
	step(2*time.Minute + 30*time.Second)
	draining = false
	start := 3*time.Minute + time.Second
	step(start)
	step(start + time.Minute - time.Nanosecond)
	if redials != 1 {
		t.Fatal("partial post-drain window redialed early")
	}
	step(start + time.Minute)
	if redials != 2 {
		t.Fatal("full fresh post-drain window did not redial again")
	}
}

func TestPlatformMonitorDefaultWindowAndJitterBounds(t *testing.T) {
	isolateSelfHealHome(t)
	t.Setenv(selfHealEnabledEnvVar, "true")
	t.Setenv(selfHealPlatformEnvVar, "")
	m := NewLivenessMonitor(NewWorkerState(), nil, nil)
	if m.platformWindow != 900*time.Second || m.platformBackoff < time.Minute || m.platformBackoff > 5*time.Minute {
		t.Fatal("unsafe platform recovery defaults")
	}
	t.Setenv(selfHealPlatformEnvVar, "0")
	if NewLivenessMonitor(NewWorkerState(), nil, nil).platformWindow != 0 {
		t.Fatal("platform-only disable knob ignored")
	}
}
