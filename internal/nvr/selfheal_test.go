package nvr

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeEnv is the injectable, race-safe backing for a Monitor under test: a
// controllable clock, a configurable fps reading (or fetch error), a
// bridge-running answer, and a counting restart that never shells out.
type fakeEnv struct {
	mu            sync.Mutex
	now           time.Time
	fps           map[string]float64
	fetchErr      error
	bridgeRunning bool
	bridgeErr     error
	restarts      int
	restartErr    error
	reasons       []string
	logs          []string
}

func newFakeEnv(start time.Time) *fakeEnv {
	return &fakeEnv{now: start, bridgeRunning: true, fps: map[string]float64{}}
}

func (e *fakeEnv) advance(d time.Duration) {
	e.mu.Lock()
	e.now = e.now.Add(d)
	e.mu.Unlock()
}

func (e *fakeEnv) setFPS(m map[string]float64) {
	e.mu.Lock()
	e.fps = m
	e.mu.Unlock()
}

func (e *fakeEnv) restartCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.restarts
}

func (e *fakeEnv) monitor(threshold time.Duration) *Monitor {
	return NewMonitor(MonitorConfig{
		StallThreshold: threshold,
		Now: func() time.Time {
			e.mu.Lock()
			defer e.mu.Unlock()
			return e.now
		},
		Fetch: func(ctx context.Context) (map[string]float64, error) {
			e.mu.Lock()
			defer e.mu.Unlock()
			if e.fetchErr != nil {
				return nil, e.fetchErr
			}
			out := make(map[string]float64, len(e.fps))
			for k, v := range e.fps {
				out[k] = v
			}
			return out, nil
		},
		BridgeRunning: func(ctx context.Context) (bool, error) {
			e.mu.Lock()
			defer e.mu.Unlock()
			return e.bridgeRunning, e.bridgeErr
		},
		RestartBridge: func(ctx context.Context, reason string) error {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.restarts++
			e.reasons = append(e.reasons, reason)
			return e.restartErr
		},
		Log: func(format string, args ...any) {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.logs = append(e.logs, fmt.Sprintf(format, args...))
		},
	})
}

func stateOf(m *Monitor, name string) CameraState {
	for _, c := range m.Snapshot().Cameras {
		if c.Name == name {
			return c.State
		}
	}
	return ""
}

func TestSelfHealDefaults(t *testing.T) {
	if DefaultStallThreshold != 5*time.Minute {
		t.Errorf("DefaultStallThreshold = %v, want 5m", DefaultStallThreshold)
	}
	if PostRestartGrace != 2*time.Minute {
		t.Errorf("PostRestartGrace = %v, want 2m", PostRestartGrace)
	}
	want := []time.Duration{5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 40 * time.Minute, 60 * time.Minute}
	if !reflect.DeepEqual(BackoffSchedule, want) {
		t.Errorf("BackoffSchedule = %v, want %v", BackoffSchedule, want)
	}
}

func TestBackoffFor(t *testing.T) {
	cases := []struct {
		attempts int
		want     time.Duration
	}{
		{0, 0},
		{1, 5 * time.Minute},
		{2, 10 * time.Minute},
		{3, 20 * time.Minute},
		{4, 40 * time.Minute},
		{5, 60 * time.Minute},
		{6, 60 * time.Minute}, // capped
		{100, 60 * time.Minute},
	}
	for _, c := range cases {
		if got := backoffFor(c.attempts); got != c.want {
			t.Errorf("backoffFor(%d) = %v, want %v", c.attempts, got, c.want)
		}
	}
}

func TestParseFrigateCameraFPS(t *testing.T) {
	t.Run("wrapped cameras key with mixed siblings", func(t *testing.T) {
		body := []byte(`{
			"cameras": {
				"front_door": {"camera_fps": 5.0, "detection_fps": 1.2},
				"garage": {"camera_fps": 0.0, "expected_fps": 5}
			},
			"detectors": {"openvino": {"inference_speed": 12.3}},
			"detection_fps": 7.5,
			"service": {"uptime": 3600}
		}`)
		got, err := ParseFrigateCameraFPS(body)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := map[string]float64{"front_door": 5.0, "garage": 0.0}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("legacy top-level shape", func(t *testing.T) {
		body := []byte(`{
			"front_door": {"camera_fps": 4.0},
			"detectors": {"cpu": {"inference_speed": 9}},
			"detection_fps": 3.3,
			"service": {"uptime": 10}
		}`)
		got, err := ParseFrigateCameraFPS(body)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := map[string]float64{"front_door": 4.0}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("no cameras is an error (fail open)", func(t *testing.T) {
		if _, err := ParseFrigateCameraFPS([]byte(`{"detectors": {}, "service": {}}`)); err == nil {
			t.Error("expected error for cameraless stats, got nil")
		}
	})

	t.Run("malformed json is an error", func(t *testing.T) {
		if _, err := ParseFrigateCameraFPS([]byte(`not json`)); err == nil {
			t.Error("expected error for malformed json, got nil")
		}
	})
}

func TestMonitor_HealthyNeverRestarts(t *testing.T) {
	e := newFakeEnv(time.Unix(0, 0))
	e.setFPS(map[string]float64{"cam": 5.0})
	m := e.monitor(5 * time.Minute)

	for i := 0; i < 60; i++ { // 60 ticks across an hour
		m.Reconcile(context.Background())
		e.advance(time.Minute)
	}
	if e.restartCount() != 0 {
		t.Fatalf("healthy camera caused %d restart(s), want 0", e.restartCount())
	}
	if got := stateOf(m, "cam"); got != CameraRecording {
		t.Errorf("state = %q, want recording", got)
	}
}

func TestMonitor_ZeroBelowDwellNoRestart(t *testing.T) {
	e := newFakeEnv(time.Unix(0, 0))
	e.setFPS(map[string]float64{"cam": 0})
	m := e.monitor(5 * time.Minute)

	// 4 minutes of zero fps -- under the 5m dwell.
	for i := 0; i < 4; i++ {
		m.Reconcile(context.Background())
		e.advance(time.Minute)
	}
	if e.restartCount() != 0 {
		t.Fatalf("restart fired before dwell elapsed: %d", e.restartCount())
	}
	if got := stateOf(m, "cam"); got != CameraStalled {
		t.Errorf("state = %q, want stalled", got)
	}
}

func TestMonitor_StalledTriggersExactlyOneRestartThenBacksOff(t *testing.T) {
	e := newFakeEnv(time.Unix(0, 0))
	e.setFPS(map[string]float64{"cam": 0})
	m := e.monitor(5 * time.Minute)

	// t0: first observation.
	m.Reconcile(context.Background())
	// Advance to exactly the dwell and reconcile -> exactly one restart.
	e.advance(5 * time.Minute)
	m.Reconcile(context.Background())
	if e.restartCount() != 1 {
		t.Fatalf("after dwell: restarts = %d, want 1", e.restartCount())
	}

	// Walk forward minute-by-minute to just before the first backoff window
	// (5m) elapses -> still exactly one restart.
	for i := 0; i < 4; i++ {
		e.advance(time.Minute)
		m.Reconcile(context.Background())
	}
	if e.restartCount() != 1 {
		t.Fatalf("within backoff window: restarts = %d, want 1", e.restartCount())
	}
	// During grace it is recovering; after grace, still dead -> offline.
	if got := stateOf(m, "cam"); got != CameraOffline {
		t.Errorf("state after grace = %q, want offline", got)
	}
}

func TestMonitor_PersistentlyOfflineOneRestartPerWindow(t *testing.T) {
	e := newFakeEnv(time.Unix(0, 0))
	e.setFPS(map[string]float64{"cam": 0})
	m := e.monitor(5 * time.Minute)

	// Drive 2 hours, one tick per minute. Expected restart instants (minutes):
	// 5 (dwell), then +5 -> 10, +10 -> 20, +20 -> 40, +40 -> 80, +60 -> 140(>120).
	// Within 120 minutes: restarts at 5, 10, 20, 40, 80 = 5 restarts.
	restartMinutes := map[int]bool{}
	for min := 0; min <= 120; min++ {
		before := e.restartCount()
		m.Reconcile(context.Background())
		if e.restartCount() > before {
			restartMinutes[min] = true
		}
		e.advance(time.Minute)
	}

	wantAt := []int{5, 10, 20, 40, 80}
	for _, w := range wantAt {
		if !restartMinutes[w] {
			t.Errorf("expected a restart at minute %d, got none (restarts at %v)", w, restartMinutes)
		}
	}
	if len(restartMinutes) != len(wantAt) {
		t.Errorf("restart minutes = %v, want exactly %v (one per backoff window, no loop)", restartMinutes, wantAt)
	}
	if got := stateOf(m, "cam"); got != CameraOffline {
		t.Errorf("state = %q, want offline", got)
	}
}

func TestMonitor_RecoveredResetsBackoffAndShowsRecording(t *testing.T) {
	e := newFakeEnv(time.Unix(0, 0))
	e.setFPS(map[string]float64{"cam": 0})
	m := e.monitor(5 * time.Minute)

	m.Reconcile(context.Background())
	e.advance(5 * time.Minute)
	m.Reconcile(context.Background()) // restart #1
	if e.restartCount() != 1 {
		t.Fatalf("restarts = %d, want 1", e.restartCount())
	}

	// Camera comes back.
	e.advance(time.Minute)
	e.setFPS(map[string]float64{"cam": 6.0})
	m.Reconcile(context.Background())
	if got := stateOf(m, "cam"); got != CameraRecording {
		t.Fatalf("after recovery state = %q, want recording", got)
	}

	// It dies AGAIN: because recovery reset the dead-streak, it must re-wait
	// the full dwell (not jump straight to a backed-off restart).
	e.setFPS(map[string]float64{"cam": 0})
	e.advance(time.Minute)
	m.Reconcile(context.Background()) // first zero obs of the new streak
	e.advance(4 * time.Minute)        // 4m < dwell
	m.Reconcile(context.Background())
	if e.restartCount() != 1 {
		t.Fatalf("new dead streak restarted before dwell: restarts = %d, want still 1", e.restartCount())
	}
	e.advance(time.Minute) // now 5m into the new streak
	m.Reconcile(context.Background())
	if e.restartCount() != 2 {
		t.Fatalf("new dead streak did not restart after a fresh dwell: restarts = %d, want 2", e.restartCount())
	}
}

func TestMonitor_ProbeErrorUnknownNoRestart(t *testing.T) {
	e := newFakeEnv(time.Unix(0, 0))
	e.fetchErr = context.DeadlineExceeded
	m := e.monitor(5 * time.Minute)

	for i := 0; i < 20; i++ {
		m.Reconcile(context.Background())
		e.advance(time.Minute)
	}
	if e.restartCount() != 0 {
		t.Fatalf("probe error caused %d restart(s), want 0 (fail open)", e.restartCount())
	}
	snap := m.Snapshot()
	if snap.LastError == "" {
		t.Error("expected LastError to be populated on probe failure")
	}
}

func TestMonitor_BridgeNotRunningNoRestart(t *testing.T) {
	e := newFakeEnv(time.Unix(0, 0))
	e.bridgeRunning = false // module stopped by operator
	e.setFPS(map[string]float64{"cam": 0})
	m := e.monitor(5 * time.Minute)

	for i := 0; i < 20; i++ {
		m.Reconcile(context.Background())
		e.advance(time.Minute)
	}
	if e.restartCount() != 0 {
		t.Fatalf("restarted a stopped bridge %d time(s), want 0", e.restartCount())
	}
	if snap := m.Snapshot(); snap.BridgeRunning {
		t.Error("snapshot reports bridge running, want not running")
	}
}

func TestMonitor_TwoStalledCamerasCauseOneRestart(t *testing.T) {
	e := newFakeEnv(time.Unix(0, 0))
	e.setFPS(map[string]float64{"a": 0, "b": 0})
	m := e.monitor(5 * time.Minute)

	m.Reconcile(context.Background())
	e.advance(5 * time.Minute)
	m.Reconcile(context.Background())

	if e.restartCount() != 1 {
		t.Fatalf("two stalled cameras caused %d restart(s), want exactly 1 (coalesced)", e.restartCount())
	}
	// The single restart was attributed to both cameras (both reason-named).
	e.mu.Lock()
	reason := strings.Join(e.reasons, ";")
	e.mu.Unlock()
	if !strings.Contains(reason, "a") || !strings.Contains(reason, "b") {
		t.Errorf("restart reason %q does not name both cameras", reason)
	}
}

func TestMonitor_RestartDoesNotCascadeOntoHealthySibling(t *testing.T) {
	e := newFakeEnv(time.Unix(0, 0))
	e.setFPS(map[string]float64{"dead": 0, "healthy": 5})
	m := e.monitor(5 * time.Minute)

	m.Reconcile(context.Background()) // t0
	e.advance(5 * time.Minute)        //
	m.Reconcile(context.Background()) // t5: restart #1 for "dead"
	if e.restartCount() != 1 {
		t.Fatalf("restarts = %d, want 1", e.restartCount())
	}

	// t6 (inside grace): the restart blipped BOTH cameras to 0.
	e.advance(time.Minute)
	e.setFPS(map[string]float64{"dead": 0, "healthy": 0})
	m.Reconcile(context.Background())
	if got := stateOf(m, "healthy"); got != CameraRecovering {
		t.Errorf("healthy during grace = %q, want recovering", got)
	}

	// t8 (past grace): healthy reconnected; dead still dead.
	e.advance(2 * time.Minute)
	e.setFPS(map[string]float64{"dead": 0, "healthy": 5})
	m.Reconcile(context.Background())
	if got := stateOf(m, "healthy"); got != CameraRecording {
		t.Errorf("healthy after grace = %q, want recording", got)
	}

	// Through the rest of the first backoff window (until t10) the healthy
	// camera's grace-blip must NOT have driven any extra restart.
	e.advance(time.Minute) // t9
	m.Reconcile(context.Background())
	if e.restartCount() != 1 {
		t.Fatalf("healthy sibling's grace-blip cascaded into %d restarts, want 1", e.restartCount())
	}
}

func TestMonitor_RestartFailureConsumesWindowNoStorm(t *testing.T) {
	e := newFakeEnv(time.Unix(0, 0))
	e.setFPS(map[string]float64{"cam": 0})
	e.restartErr = context.DeadlineExceeded // restart keeps failing
	m := e.monitor(5 * time.Minute)

	m.Reconcile(context.Background())
	e.advance(5 * time.Minute)
	m.Reconcile(context.Background()) // attempt #1 (fails)
	if e.restartCount() != 1 {
		t.Fatalf("restarts = %d, want 1 attempt", e.restartCount())
	}
	// A FAILED restart must not read as "offline, recovered-and-still-dead":
	// the camera stays stalled and last_restart_at stays zero, with the
	// failure surfaced on the snapshot's LastError.
	if got := stateOf(m, "cam"); got != CameraStalled {
		t.Errorf("state after failed restart = %q, want stalled (not offline)", got)
	}
	for _, c := range m.Snapshot().Cameras {
		if c.Name == "cam" && !c.LastRestartAt.IsZero() {
			t.Errorf("last_restart_at = %v after a FAILED restart, want zero", c.LastRestartAt)
		}
	}
	if snap := m.Snapshot(); snap.LastError == "" {
		t.Error("expected snapshot LastError to carry the restart failure")
	}
	// Next few ticks must NOT storm: backoff consumed even though it failed.
	for i := 0; i < 4; i++ {
		e.advance(time.Minute)
		m.Reconcile(context.Background())
	}
	if e.restartCount() != 1 {
		t.Fatalf("failing restart stormed to %d attempts within the window, want 1", e.restartCount())
	}
	// After the 5m window, it retries exactly once more.
	e.advance(time.Minute)
	m.Reconcile(context.Background())
	if e.restartCount() != 2 {
		t.Fatalf("restarts after window = %d, want 2", e.restartCount())
	}
}

func TestMonitor_LogsRestartWithCameraAndReason(t *testing.T) {
	e := newFakeEnv(time.Unix(0, 0))
	e.setFPS(map[string]float64{"front_door": 0})
	m := e.monitor(5 * time.Minute)
	m.Reconcile(context.Background())
	e.advance(5 * time.Minute)
	m.Reconcile(context.Background())

	e.mu.Lock()
	joined := strings.Join(e.logs, "\n")
	e.mu.Unlock()
	if !strings.Contains(joined, "front_door") {
		t.Errorf("restart log does not name the camera: %q", joined)
	}
	if !strings.Contains(joined, "restarting wyze-bridge") {
		t.Errorf("restart log does not state the action: %q", joined)
	}
}

func TestMonitor_ConcurrentReconcileAndSnapshot(t *testing.T) {
	e := newFakeEnv(time.Unix(0, 0))
	e.setFPS(map[string]float64{"a": 0, "b": 5})
	m := e.monitor(time.Nanosecond) // trivially past dwell so restarts happen

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				m.Reconcile(context.Background())
				_ = m.Snapshot()
				e.advance(time.Second) // exercise dwell/grace/backoff paths concurrently (mutex-guarded)
			}
		}()
	}
	wg.Wait()
	// No assertion beyond "did not race / panic"; run under -race.
}
