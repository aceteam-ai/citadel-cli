package worker

import (
	"context"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/platformhttp"
)

// LivenessMonitor is the self-heal backstop for a consumption-wedged worker
// (issue #548). The per-job watchdog (executeWithDeadline) is the primary fix --
// it bounds every handler dispatch so one hung handler can no longer stall the
// consume loop. This monitor covers the residual cases the per-job watchdog
// cannot: a wedge OUTSIDE a handler (a stuck source.Next / dead loop), or a
// deployment that has disabled the per-job watchdog. It watches the worker's
// live introspection state and, when the loop has clearly stopped making
// forward progress, restarts the process so the service manager (systemd
// Restart=on-failure) brings it back clean.
//
// It is deliberately conservative -- it must never restart a healthy node that
// is merely busy with a legitimate long job:
//   - STALL: no poll cycle has completed for stallTimeout WHILE nothing is in
//     flight. A healthy idle node records a poll every few seconds (the source
//     block timeout), so a long no-poll gap with in_flight==0 means the loop
//     itself is dead, not that a job is legitimately running.
//   - STUCK: a job has been in flight longer than stuckTimeout. This only fires
//     when the per-job watchdog is off or configured beyond stuckTimeout; with
//     the watchdog on, in_flight naturally clears when a job is abandoned.
//
// Draining (auto-update) is skipped: the loop intentionally stops polling then.
type LivenessMonitor struct {
	state *WorkerState

	stallTimeout  time.Duration // max no-poll gap while in_flight==0
	stuckTimeout  time.Duration // max single-job in-flight duration (0 = disabled)
	checkInterval time.Duration
	graceStart    time.Duration // don't act until the monitor has run this long
	// startedAt is stamped when Run begins (NOT WorkerState.startedAt, which is
	// created early in startup). Grace is measured from here so the slow startup
	// BEFORE the consume loop (network verify, identity, queue setup) can never
	// be misread as "never polled" and trigger a restart loop (issue #548).
	startedAt time.Time

	isDraining      func() bool
	onWedge         func(reason string) // default: log + os.Exit(1)
	log             func(level, msg string)
	platformWindow  time.Duration
	platformBackoff time.Duration
	redialPlatform  func()
	idleRestart     func(func()) bool
	platformRestart bool
	now             func() time.Time
}

// Self-heal tuning. Following the SERVICE_* env convention already used in the
// repo. WORKER_SELF_HEAL=false disables the monitor entirely.
const (
	selfHealEnabledEnvVar          = "WORKER_SELF_HEAL"
	selfHealStallEnvVar            = "WORKER_SELF_HEAL_STALL_SECONDS"
	selfHealStuckEnvVar            = "WORKER_SELF_HEAL_STUCK_SECONDS"
	selfHealPlatformEnvVar         = "WORKER_SELF_HEAL_PLATFORM_FAIL_SECONDS"
	defaultSelfHealPlatformSeconds = 900

	// defaultSelfHealStallSeconds: 10min. Far above the source block timeout and
	// the max consume backoff, so a healthy loop never trips it.
	defaultSelfHealStallSeconds = 600
	// defaultSelfHealStuckSeconds: 5h, above the 4h long-session job cap, so it
	// is a pure backstop for a job that outlives even the generous per-job
	// watchdog.
	defaultSelfHealStuckSeconds = 18000

	defaultSelfHealCheckInterval = 30 * time.Second
	defaultSelfHealGrace         = 2 * time.Minute
)

// selfHealEnabled reports whether the self-heal monitor should run. Default ON;
// set WORKER_SELF_HEAL to a falsey value (0/false/no/off) to disable.
func selfHealEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(selfHealEnabledEnvVar))) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

// NewLivenessMonitor builds a monitor with env-tuned thresholds and sensible
// defaults. Returns nil if self-heal is disabled or state is nil (a nil monitor
// is safe to Run -- it's a no-op).
func NewLivenessMonitor(state *WorkerState, isDraining func() bool, log func(level, msg string)) *LivenessMonitor {
	if state == nil || !selfHealEnabled() {
		return nil
	}
	if isDraining == nil {
		isDraining = func() bool { return false }
	}
	if log == nil {
		log = func(string, string) {}
	}
	return &LivenessMonitor{
		state:           state,
		startedAt:       time.Now(), // re-stamped when Run begins
		stallTimeout:    envSecondsOrDefault(selfHealStallEnvVar, defaultSelfHealStallSeconds),
		stuckTimeout:    envSecondsOrDefault(selfHealStuckEnvVar, defaultSelfHealStuckSeconds),
		checkInterval:   defaultSelfHealCheckInterval,
		graceStart:      defaultSelfHealGrace,
		isDraining:      isDraining,
		log:             log,
		platformWindow:  envSecondsOrDefault(selfHealPlatformEnvVar, defaultSelfHealPlatformSeconds),
		platformBackoff: time.Minute + time.Duration(rand.Int64N(int64(4*time.Minute)+1)),
		now:             time.Now,
		onWedge: func(reason string) {
			// Exit non-zero so systemd (Restart=on-failure) restarts the
			// process clean -- clearing any leaked/wedged goroutines that an
			// in-process goroutine restart could not.
			os.Exit(1)
		},
	}
}

// SetPlatformRecovery enables the HTTP backstop only for an API-mode worker.
// Configure before Run. The guard fences job admission before process exit.
func (m *LivenessMonitor) SetPlatformRecovery(redial func(), idleRestart func(func()) bool) {
	if m == nil {
		return
	}
	m.redialPlatform, m.idleRestart = redial, idleRestart
}

// envSecondsOrDefault reads a seconds-valued env var. A value <= 0 disables that
// check (returns 0 duration). Garbage falls back to def.
func envSecondsOrDefault(envVar string, def int) time.Duration {
	secs := def
	if v := strings.TrimSpace(os.Getenv(envVar)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			secs = n
		}
	}
	if secs <= 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}

// Run polls the worker state until ctx is cancelled, restarting the process via
// onWedge when a wedge is detected. Safe to call on a nil *LivenessMonitor.
func (m *LivenessMonitor) Run(ctx context.Context) {
	if m == nil {
		return
	}
	// Stamp grace from the moment the monitor (and thus the consume loop) starts,
	// so pre-loop startup time is never counted against the "never polled" check.
	m.startedAt = time.Now()
	m.log("info", "Self-heal monitor active (stall="+m.stallTimeout.String()+")")
	ticker := time.NewTicker(m.checkInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now()
			if reason, wedged := m.check(now); wedged {
				if m.handleWedge(reason, now) {
					return
				}
			}
		}
	}
}

func (m *LivenessMonitor) handleWedge(reason string, now time.Time) bool {
	exit := func() {
		m.log("error", "Self-heal: "+reason+"; restarting worker process")
		m.onWedge(reason)
	}
	if m.platformRestart {
		committed := false
		currentTime := func() time.Time {
			if m.now != nil {
				return m.now()
			}
			return now
		}
		if m.idleRestart == nil || !m.idleRestart(func() { committed = m.state.platform.CommitRestart(currentTime(), exit) }) || !committed {
			m.state.platform.Suspend(currentTime())
			return false
		}
		return true
	}
	exit()
	return true
}

// check evaluates the current state at time now and returns a non-empty reason
// with wedged=true when the worker should be restarted. Split out from Run so it
// is unit-testable without a real ticker or process exit.
func (m *LivenessMonitor) check(now time.Time) (reason string, wedged bool) {
	m.platformRestart = false
	if m.isDraining() {
		m.state.platform.Suspend(now)
		return "", false // intentional pause (auto-update): not a wedge
	}
	snap := m.state.Snapshot()

	// Respect a startup grace period, measured from when the MONITOR started (see
	// startedAt), so the slow work before the consume loop begins is never
	// mistaken for a wedge and cannot trigger a restart loop.
	if now.Sub(m.startedAt) < m.graceStart {
		m.state.platform.Suspend(now)
		return "", false
	}
	if m.redialPlatform != nil && m.platformWindow > 0 {
		if snap.InFlight > 0 || snap.Executing > 0 {
			m.state.platform.Suspend(now)
		} else {
			switch m.state.platform.NextAction(now, m.platformWindow, m.platformBackoff) {
			case platformhttp.RedialPlatform:
				m.log("warning", "Self-heal: sustained platform wrong answers; re-dialing before restart backoff")
				m.redialPlatform()
			case platformhttp.RestartWorker:
				m.platformRestart = true
				return "sustained platform wrong answers after two fresh-failure windows and bounded backoff", true
			}
		}
	}

	// STALL: loop not polling while nothing is running.
	if snap.InFlight == 0 && m.stallTimeout > 0 {
		if snap.LastPollAt == nil {
			// Up past grace, nothing in flight, and never a single poll: the
			// loop never started consuming.
			return "consume loop never polled since startup", true
		}
		if gap := now.Sub(*snap.LastPollAt); gap > m.stallTimeout {
			return "consume loop stalled: no poll for " + gap.Truncate(time.Second).String() + " with no jobs in flight", true
		}
	}

	// STUCK: a handler has occupied an execution slot far past any legitimate
	// budget.
	//
	// Reads OldestExecutingAt and gates on Executing>0, NOT InFlight /
	// OldestInFlightAt (citadel-cli#908, extending the #489-review reasoning).
	// Two independent reasons this must measure EXECUTING, not merely in-flight:
	//   1. Like #489's LastJobAt problem, OldestInFlightAt would be reset by a
	//      short job only if it were the sole in-flight job -- but it is not
	//      perturbed by concurrent jobs, so that half is already handled.
	//   2. With claim/execute decoupling, a job can be legitimately QUEUED for
	//      hours on the general lane (exec-concurrency 1) behind a large model
	//      pull. Such a job is in_flight the whole time but is NOT executing --
	//      nothing is wedged, it is waiting its turn. Measuring in-flight would
	//      restart the node for a perfectly healthy backlog. OldestExecutingAt
	//      marks when the current EXECUTING streak began and is nil while every
	//      in-flight job is still queued, so only a handler that has genuinely
	//      run past the ceiling trips this.
	if snap.Executing > 0 && m.stuckTimeout > 0 && snap.OldestExecutingAt != nil {
		if held := now.Sub(*snap.OldestExecutingAt); held > m.stuckTimeout {
			return "job stuck in handler for " + held.Truncate(time.Second).String() + " (exceeds self-heal ceiling)", true
		}
	}

	return "", false
}
