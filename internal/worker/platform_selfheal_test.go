package worker

import (
	"os"
	"path/filepath"
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
