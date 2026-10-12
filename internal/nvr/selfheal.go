// internal/nvr/selfheal.go
//
// Per-camera stall detection and automatic recovery for the `nvr` catalog
// module (aceteam-ai/citadel-cli#1295).
//
// The failure this fixes: when a Wyze camera power-cycles, the wyze-bridge
// container stays wedged on the dead TUTK P2P session forever. go2rtc logs
// `discovery timeout` for that stream every 10s, Frigate's ffmpeg gets a 404
// from the RTSP URL and crash-loops, and NOTHING records -- while the bridge
// container stays "Up", so no container healthcheck ever reports unhealthy.
// Today the only recovery is an admin running `docker restart
// citadel-nvr-wyze-bridge` by hand (which blips every camera ~30s). One node
// had a camera down 20 days before anyone noticed.
//
// This Monitor is a WORKER-SIDE check (no Docker socket handed to any
// container): it polls Frigate's /api/stats over the module's loopback host
// port, treats a camera reporting camera_fps == 0 as stalled, and -- once a
// camera has been stalled past a dwell threshold -- restarts the wyze-bridge
// container through the caller-injected runtime seam. The load-bearing safety
// is per-camera EXPONENTIAL BACKOFF: a genuinely-offline/unplugged camera
// triggers at most ONE bridge restart per backoff window (5/10/20/40 min, cap
// 1h) rather than a restart loop that blips every other camera every few
// minutes.
//
// This package is stdlib-only on purpose: internal/nvr is compiled into the
// nvr-config init-container binary (cmd/nvrconfig), so it must not drag in the
// container runtime, HTTP client, or status collector. Every side effect
// (fetching Frigate stats, inspecting/restarting the bridge, the clock) is an
// injected func, which also makes the whole state machine testable with no
// Docker, no HTTP, and a fake clock. The production wiring lives in cmd
// (cmd/nvr_monitor.go).
package nvr

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// CameraState is the per-camera self-heal state surfaced on the heartbeat.
type CameraState string

const (
	// CameraRecording: the camera reported camera_fps > 0 on the last probe.
	CameraRecording CameraState = "recording"
	// CameraStalled: camera_fps == 0 and no restart has been issued yet in the
	// current dead streak (the degraded-but-not-yet-acted-on window).
	CameraStalled CameraState = "stalled"
	// CameraRecovering: a bridge restart was just issued; the camera is inside
	// the post-restart grace window while streams reconnect.
	CameraRecovering CameraState = "recovering"
	// CameraOffline: camera_fps == 0 and at least one bridge restart in this
	// dead streak has not brought it back -- it is probably genuinely
	// offline/unplugged.
	CameraOffline CameraState = "offline"
	// CameraUnknown: the last probe could not determine this camera's state
	// (Frigate unreachable, or the wyze-bridge container not running). Never
	// treated as stalled -- "we don't know" is never "no frames".
	CameraUnknown CameraState = "unknown"
)

// CameraHealth is one camera's self-heal snapshot. JSON tags mirror
// status.NVRCameraHealth; cmd's TestNVRHealthShapeParity pins the parity.
type CameraHealth struct {
	Name          string      `json:"name"`
	State         CameraState `json:"state"`
	FPS           float64     `json:"fps"`
	RestartCount  int         `json:"restart_count,omitempty"`
	StalledSince  time.Time   `json:"stalled_since,omitempty"`
	LastRestartAt time.Time   `json:"last_restart_at,omitempty"`
}

// Snapshot is the Monitor's latest view. JSON tags mirror status.NVRHealth.
type Snapshot struct {
	Cameras       []CameraHealth `json:"cameras,omitempty"`
	BridgeRunning bool           `json:"bridge_running"`
	LastRestartAt time.Time      `json:"last_restart_at,omitempty"`
	LastError     string         `json:"last_error,omitempty"`
	UpdatedAt     time.Time      `json:"updated_at,omitempty"`
}

// Tunables. Package vars (not consts) so a test can shorten them without
// sleeping real minutes; TestSelfHealDefaults pins the shipped values.
var (
	// DefaultStallThreshold is the dwell: a camera must report zero frames
	// continuously for this long before the FIRST bridge restart. 5 min + a
	// ~30s restart + stream reconnect lands comfortably inside the issue's
	// "~10 minutes" recovery acceptance, while filtering transient blips.
	DefaultStallThreshold = 5 * time.Minute

	// PostRestartGrace is how long after a bridge restart the monitor treats
	// every camera as "recovering" and ignores zero-fps readings. A bridge
	// restart blips ALL cameras to 0 fps for ~30s while streams reconnect;
	// without this grace, the restart issued for a dead camera would make its
	// healthy siblings look stalled on the very next tick and cascade into
	// more restarts.
	PostRestartGrace = 2 * time.Minute

	// BackoffSchedule is the minimum gap between consecutive bridge restarts
	// for a camera that stays dead: 5, 10, 20, 40 min, capped at 1h. This is
	// the load-bearing anti-loop guarantee -- an unplugged camera triggers at
	// most one restart per window, not one every few minutes.
	BackoffSchedule = []time.Duration{
		5 * time.Minute,
		10 * time.Minute,
		20 * time.Minute,
		40 * time.Minute,
		60 * time.Minute,
	}
)

// backoffFor returns the backoff window after `attempts` restarts have already
// been issued in the current dead streak (attempts >= 1). attempts <= 0 means
// no restart yet, so there is no backoff window (the dwell threshold gates the
// first restart instead).
func backoffFor(attempts int) time.Duration {
	if attempts <= 0 {
		return 0
	}
	i := attempts - 1
	if i >= len(BackoffSchedule) {
		i = len(BackoffSchedule) - 1
	}
	return BackoffSchedule[i]
}

// Environment knobs (read by the cmd wiring, not by the pure monitor).
const (
	// SelfHealEnvVar is the kill switch. Self-heal is DEFAULT ON for a node
	// that has the nvr module installed (the module's presence is the opt-in --
	// the owner explicitly installed a surveillance NVR and wants it to keep
	// recording unattended). Set to a falsey value to disable.
	SelfHealEnvVar = "CITADEL_NVR_SELF_HEAL"
	// StallSecondsEnvVar overrides DefaultStallThreshold (in seconds).
	StallSecondsEnvVar = "CITADEL_NVR_STALL_SECONDS"
)

// SelfHealEnabled reports whether NVR self-heal should run. Default ON: only an
// explicit falsey value ("0"/"false"/"no"/"off") turns it off.
func SelfHealEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(SelfHealEnvVar))) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

// StallThresholdFromEnv resolves the stall dwell from StallSecondsEnvVar,
// falling back to DefaultStallThreshold when unset, empty, non-numeric, or
// non-positive.
func StallThresholdFromEnv() time.Duration {
	if v := os.Getenv(StallSecondsEnvVar); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return DefaultStallThreshold
}

// ParseFrigateCameraFPS extracts per-camera camera_fps from a Frigate
// /api/stats response body. Frigate 0.17 returns {"cameras": {"<name>":
// {"camera_fps": <float>, ...}}, "detectors": {...}, "service": {...}}; older
// builds put the camera stat objects at the top level alongside
// detectors/service. This decodes the "cameras" object when present and falls
// back to a top-level scan otherwise, skipping any entry that is not a camera
// stat object (no camera_fps field). An empty result is an error, so the
// Monitor treats a cameraless / unparseable response as "could not determine"
// (fail open) rather than "all cameras stalled".
func ParseFrigateCameraFPS(body []byte) (map[string]float64, error) {
	var wrapped struct {
		Cameras map[string]json.RawMessage `json:"cameras"`
	}
	if err := json.Unmarshal(body, &wrapped); err != nil {
		return nil, fmt.Errorf("decode frigate stats: %w", err)
	}
	src := wrapped.Cameras
	if len(src) == 0 {
		// Legacy/odd shape: camera stat objects at the top level.
		var flat map[string]json.RawMessage
		if err := json.Unmarshal(body, &flat); err != nil {
			return nil, fmt.Errorf("decode frigate stats: %w", err)
		}
		src = flat
	}
	out := make(map[string]float64, len(src))
	for name, raw := range src {
		var cam struct {
			CameraFPS *float64 `json:"camera_fps"`
		}
		// Non-object entries (detectors as a map, detection_fps as a number,
		// service as an object without camera_fps) are skipped: either the
		// unmarshal fails (a number into a struct) or camera_fps is absent.
		if err := json.Unmarshal(raw, &cam); err != nil {
			continue
		}
		if cam.CameraFPS == nil {
			continue
		}
		out[name] = *cam.CameraFPS
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no cameras in frigate stats")
	}
	return out, nil
}

// ReadFrigateCameraFPS is ParseFrigateCameraFPS over an io.Reader, bounding the
// read so a wedged Frigate cannot stream an unbounded body into the monitor.
func ReadFrigateCameraFPS(r io.Reader) (map[string]float64, error) {
	body, err := io.ReadAll(io.LimitReader(r, 4<<20))
	if err != nil {
		return nil, err
	}
	return ParseFrigateCameraFPS(body)
}

// MonitorConfig configures a Monitor. Every side effect is injected so the
// state machine is testable without Docker/HTTP/a real clock.
type MonitorConfig struct {
	// Fetch returns the current per-camera camera_fps (name -> fps) from
	// Frigate's /api/stats. A non-nil error means "could not determine"
	// (Frigate unreachable or a bad response); the monitor fails open and acts
	// on nothing. REQUIRED.
	Fetch func(ctx context.Context) (map[string]float64, error)
	// BridgeRunning reports whether the wyze-bridge container is currently
	// running. The monitor NEVER restarts a bridge that is not running: a
	// `docker restart` would START a container an operator deliberately
	// stopped (e.g. `citadel module stop nvr`). REQUIRED.
	BridgeRunning func(ctx context.Context) (bool, error)
	// RestartBridge restarts the wyze-bridge container. reason is a short human
	// description for the caller's own logging. REQUIRED.
	RestartBridge func(ctx context.Context, reason string) error
	// StallThreshold is the dwell before the first restart. Zero ->
	// DefaultStallThreshold.
	StallThreshold time.Duration
	// Log, if set, receives one line per automatic restart decision (and per
	// restart failure). Optional.
	Log func(format string, args ...any)
	// Now is injectable for deterministic tests; nil -> time.Now.
	Now func() time.Time
}

type camState struct {
	fps          float64
	stalledSince time.Time // start of the current zero-fps streak (first-restart dwell); zero when recording or just reset by a restart blip
	attempts     int       // restart ATTEMPTS in the current dead streak (success OR failure); drives the backoff window
	restarted    bool      // a restart SUCCEEDED at least once in this dead streak; drives offline-vs-stalled (a failed restart must not read as "we recovered it and it's still dead")
	nextEligible time.Time // earliest next restart (backoff), used once attempts >= 1
	lastRestart  time.Time // time of the last SUCCESSFUL restart (zero if none); stamped only on success so the heartbeat never claims a restart that failed
	state        CameraState
}

// Monitor runs the stall-detection + backoff-gated recovery state machine.
// Reconcile is driven off the heartbeat's existing ~30s OnStatus tick (it adds
// no extra sweeps beyond one loopback HTTP GET + one container inspect), and
// Snapshot is the read path the heartbeat collector calls.
type Monitor struct {
	cfg MonitorConfig
	now func() time.Time

	mu            sync.Mutex
	cams          map[string]*camState
	graceUntil    time.Time
	lastRestartAt time.Time
	snap          Snapshot
	inFlight      bool // one Reconcile at a time: a restart can block ~30s, and ticks are ~30s
}

// NewMonitor builds a Monitor. Fetch/BridgeRunning/RestartBridge must be set.
func NewMonitor(cfg MonitorConfig) *Monitor {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Monitor{
		cfg:  cfg,
		now:  now,
		cams: make(map[string]*camState),
	}
}

func (m *Monitor) stallThreshold() time.Duration {
	if m.cfg.StallThreshold > 0 {
		return m.cfg.StallThreshold
	}
	return DefaultStallThreshold
}

func (m *Monitor) logf(format string, args ...any) {
	if m.cfg.Log != nil {
		m.cfg.Log(format, args...)
	}
}

// Reconcile runs one detection+recovery pass. It is safe to call concurrently
// (a second call while one is in flight is a no-op), which is what lets the
// caller dispatch it with `go` off the heartbeat tick without the slow
// fetch/restart ever stalling the publisher goroutine.
func (m *Monitor) Reconcile(ctx context.Context) {
	m.mu.Lock()
	if m.inFlight {
		m.mu.Unlock()
		return
	}
	m.inFlight = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.inFlight = false
		m.mu.Unlock()
	}()

	now := m.now()

	// 1. Never act while the bridge is not running: that is either the whole
	//    module deliberately stopped, or a state we cannot attribute to a
	//    camera. Report unknown and do nothing (fail open).
	running, err := m.cfg.BridgeRunning(ctx)
	if err != nil || !running {
		m.publishUnknown(now, false, errString(err))
		return
	}

	// 2. Fetch per-camera fps. A failure means we cannot determine state (e.g.
	//    Frigate itself is down) -- crucially we must NOT restart the bridge in
	//    that case, since the bridge is not the problem.
	fps, err := m.cfg.Fetch(ctx)
	if err != nil {
		m.publishUnknown(now, true, err.Error())
		return
	}

	inGrace := now.Before(m.graceUntil)
	var candidates []string

	for name, f := range fps {
		cs := m.cams[name]
		if cs == nil {
			cs = &camState{}
			m.cams[name] = cs
		}
		cs.fps = f

		if f > 0 {
			// Recording: reset the whole dead-streak state, including any
			// backoff -- a recovered camera starts fresh. Checked before the
			// grace branch so a camera that genuinely came back during grace is
			// recorded as recording, not held at "recovering".
			cs.state = CameraRecording
			cs.attempts = 0
			cs.restarted = false
			cs.stalledSince = time.Time{}
			cs.nextEligible = time.Time{}
			continue
		}

		// f <= 0: no frames.
		if inGrace {
			// Inside the post-restart grace window every zero reading is a
			// stream still reconnecting; do not count it toward the dwell and
			// do not let it drive another restart.
			cs.state = CameraRecovering
			cs.stalledSince = time.Time{}
			continue
		}

		if cs.stalledSince.IsZero() {
			cs.stalledSince = now
		}
		// offline only once a restart has actually SUCCEEDED and the camera is
		// still dead (probably unplugged); a camera whose restart keeps FAILING
		// stays "stalled" (the restart command itself is the problem, surfaced
		// via the snapshot's LastError), never a false "offline, never
		// restarted".
		if cs.restarted {
			cs.state = CameraOffline
		} else {
			cs.state = CameraStalled
		}

		// Eligibility: the FIRST restart is gated by the dwell threshold;
		// subsequent restarts (attempts >= 1) are gated purely by the backoff
		// window, so a persistently-dead camera restarts on the 5/10/20/40/60
		// schedule rather than re-waiting the full dwell each time.
		var eligible bool
		if cs.attempts == 0 {
			eligible = now.Sub(cs.stalledSince) >= m.stallThreshold()
		} else {
			eligible = !now.Before(cs.nextEligible)
		}
		if eligible {
			candidates = append(candidates, name)
		}
	}

	// Drop cameras Frigate no longer reports (removed from config.yml).
	for name := range m.cams {
		if _, ok := fps[name]; !ok {
			delete(m.cams, name)
		}
	}

	var restartErr string
	if !inGrace && len(candidates) > 0 {
		restartErr = m.restart(ctx, now, candidates)
	}

	m.publish(now, true, restartErr)
}

// restart issues ONE whole-bridge restart on behalf of every eligible stalled
// camera (a single restart recovers every camera the bridge serves, so this
// coalesces: two dead cameras cause one restart, not two), advances each
// candidate's backoff, and -- on success -- opens the post-restart grace
// window for ALL cameras so healthy siblings blipped by the restart do not
// cascade into further restarts. It returns the restart error text (empty on
// success) so Reconcile can surface a failed restart on the snapshot.
func (m *Monitor) restart(ctx context.Context, now time.Time, candidates []string) string {
	sort.Strings(candidates)
	for _, name := range candidates {
		cs := m.cams[name]
		m.logf("nvr self-heal: camera %q produced no frames for %s, restarting wyze-bridge (attempt %d)",
			name, now.Sub(cs.stalledSince).Round(time.Second), cs.attempts+1)
	}
	reason := fmt.Sprintf("camera(s) %s stalled (no frames)", strings.Join(candidates, ","))

	if err := m.cfg.RestartBridge(ctx, reason); err != nil {
		// A failed restart still consumed this window: advance each
		// candidate's backoff (attempts++) so a persistently-failing `docker
		// restart` does not storm one attempt per tick. But do NOT mark them
		// restarted / stamp lastRestart / open the grace window -- nothing
		// actually restarted, so they stay "stalled" (not a false "offline")
		// and the heartbeat never claims a restart that failed.
		m.logf("nvr self-heal: wyze-bridge restart failed: %v", err)
		for _, name := range candidates {
			cs := m.cams[name]
			cs.attempts++
			cs.nextEligible = now.Add(backoffFor(cs.attempts))
		}
		return err.Error()
	}

	m.lastRestartAt = now
	for _, name := range candidates {
		cs := m.cams[name]
		cs.attempts++
		cs.restarted = true
		cs.lastRestart = now
		cs.nextEligible = now.Add(backoffFor(cs.attempts))
	}
	// The restart blips every camera to 0 fps for ~30s; open the grace window
	// and reset every camera's dwell so a healthy sibling does not look stalled
	// on the next tick. Resetting stalledSince for the triggering camera too is
	// intentional and safe: it now has attempts >= 1, so its NEXT restart is
	// gated by nextEligible (backoff), not by re-accumulating the dwell.
	m.graceUntil = now.Add(PostRestartGrace)
	for _, cs := range m.cams {
		cs.stalledSince = time.Time{}
		if cs.fps <= 0 {
			cs.state = CameraRecovering
		}
	}
	return ""
}

// publishUnknown builds a snapshot marking every tracked camera unknown while
// FREEZING their internal timers (we did not observe a state, so we neither
// advance nor reset the dwell/backoff). bridgeRunning/lastErr describe why.
func (m *Monitor) publishUnknown(now time.Time, bridgeRunning bool, lastErr string) {
	cams := make([]CameraHealth, 0, len(m.cams))
	for name, cs := range m.cams {
		cams = append(cams, CameraHealth{
			Name:          name,
			State:         CameraUnknown,
			FPS:           cs.fps,
			RestartCount:  cs.attempts,
			StalledSince:  cs.stalledSince,
			LastRestartAt: cs.lastRestart,
		})
	}
	sort.Slice(cams, func(i, j int) bool { return cams[i].Name < cams[j].Name })
	m.mu.Lock()
	m.snap = Snapshot{
		Cameras:       cams,
		BridgeRunning: bridgeRunning,
		LastRestartAt: m.lastRestartAt,
		LastError:     lastErr,
		UpdatedAt:     now,
	}
	m.mu.Unlock()
}

func (m *Monitor) publish(now time.Time, bridgeRunning bool, lastErr string) {
	cams := make([]CameraHealth, 0, len(m.cams))
	for name, cs := range m.cams {
		cams = append(cams, CameraHealth{
			Name:          name,
			State:         cs.state,
			FPS:           cs.fps,
			RestartCount:  cs.attempts,
			StalledSince:  cs.stalledSince,
			LastRestartAt: cs.lastRestart,
		})
	}
	sort.Slice(cams, func(i, j int) bool { return cams[i].Name < cams[j].Name })
	m.mu.Lock()
	m.snap = Snapshot{
		Cameras:       cams,
		BridgeRunning: bridgeRunning,
		LastRestartAt: m.lastRestartAt,
		LastError:     lastErr,
		UpdatedAt:     now,
	}
	m.mu.Unlock()
}

// Snapshot returns the most recent per-camera view. It never probes; it reads
// the last snapshot Reconcile published, so the heartbeat lags one tick (fine
// for a ~30s heartbeat against an N-minute stall threshold).
func (m *Monitor) Snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snap
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
