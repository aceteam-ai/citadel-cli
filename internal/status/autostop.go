package status

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// AutoStopEnvVar gates the optional auto-stop-when-idle behavior. It is OFF
// unless explicitly set to a truthy value ("1", "true", "yes", "on"). This is
// the safety switch required by citadel #416: a node NEVER auto-evicts a
// managed service unless the operator has opted in.
const AutoStopEnvVar = "SERVICE_AUTO_STOP_WHEN_IDLE"

// AutoStopThresholdEnvVar overrides how many seconds a service must be
// confirmed idle before auto-stop acts. When unset/invalid it falls back to the
// shared idle threshold (SERVICE_IDLE_THRESHOLD_SECONDS / DefaultIdleThresholdSeconds),
// so "enabled but threshold unset" still uses a positive default (never 0).
const AutoStopThresholdEnvVar = "SERVICE_AUTO_STOP_IDLE_SECONDS"

// AutoStopEnabled reports whether the operator has opted into auto-stop. Default
// is false: any value other than a recognized truthy token leaves it off.
func AutoStopEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(AutoStopEnvVar))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// AutoStopThresholdSeconds returns the configured auto-stop idle threshold in
// seconds. It reads AutoStopThresholdEnvVar and falls back to the shared idle
// threshold when unset, empty, non-numeric, or non-positive.
func AutoStopThresholdSeconds() int {
	if v := os.Getenv(AutoStopThresholdEnvVar); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			return n
		}
	}
	return IdleThresholdSeconds()
}

// EntityKind distinguishes the two managed-service lifecycles auto-stop must
// drive: manifest/embedded services (stopped via a compose "down") and catalog
// apps (stopped via docker stop). The two have different stop paths, so the
// reconciler reports which kind an idle candidate is and the caller routes the
// stop accordingly.
type EntityKind string

const (
	// EntityService is a manifest-declared or embedded managed service.
	EntityService EntityKind = "service"
	// EntityApp is an installed catalog app (e.g. a vLLM catalog app). This is
	// the entity kind behind the #421 idle-diffusers incident, so it must be
	// covered, not silently skipped.
	EntityApp EntityKind = "app"
)

// IdleCandidate names a single running managed entity that is confirmed idle
// past the auto-stop threshold. Kind selects the stop path; Name is the logical
// service/app name that stop path expects.
type IdleCandidate struct {
	Kind EntityKind
	Name string
}

// StopFunc stops one idle candidate. It is injected so the reconciler carries
// no dependency on the jobs/apps packages (avoiding an import cycle: internal/status
// is imported by them) and stays unit-testable without Docker. It returns an
// error describing why a stop failed; the reconciler logs it and moves on.
type StopFunc func(c IdleCandidate) error

// LogFunc is an optional structured logger for auto-stop actions. level is one
// of "info"/"warning". A nil logger discards output.
type LogFunc func(level, format string, args ...any)

// AutoStopReconciler evicts confirmed-idle managed services when the operator
// has opted in. It is deliberately conservative: it acts ONLY on a service that
// reports a concrete idle signal (IdleState present, Idle==true) whose
// IdleSeconds meets or exceeds the configured threshold. A service with an
// ABSENT idle signal (the collector omits it when it cannot scrape/derive one)
// is never touched -- "we don't know" is never treated as "idle".
//
// It holds no telemetry state of its own: it consumes a NodeStatus that the
// heartbeat path already collected, so enabling auto-stop adds no extra
// docker/nvidia-smi execs on the (often overloaded) node.
type AutoStopReconciler struct {
	enabled          bool
	thresholdSeconds int64
	stop             StopFunc
	log              LogFunc
}

// NewAutoStopReconciler builds a reconciler. enabled/thresholdSeconds are
// typically sourced from AutoStopEnabled()/AutoStopThresholdSeconds(). A
// non-positive threshold is clamped to the shared idle default so an eviction
// can never fire on a zero threshold. stop is required; log may be nil.
func NewAutoStopReconciler(enabled bool, thresholdSeconds int, stop StopFunc, log LogFunc) *AutoStopReconciler {
	if thresholdSeconds <= 0 {
		thresholdSeconds = IdleThresholdSeconds()
	}
	return &AutoStopReconciler{
		enabled:          enabled,
		thresholdSeconds: int64(thresholdSeconds),
		stop:             stop,
		log:              log,
	}
}

// Enabled reports whether the reconciler will act.
func (r *AutoStopReconciler) Enabled() bool {
	return r != nil && r.enabled && r.stop != nil
}

// Candidates returns the confirmed-idle eviction candidates in a status,
// without stopping anything. Exposed for the operator surface (so "citadel
// services" can show what WOULD be stopped) and for tests. Only running
// entities with a present idle signal past the threshold qualify.
func (r *AutoStopReconciler) Candidates(st *NodeStatus) []IdleCandidate {
	if st == nil {
		return nil
	}
	var out []IdleCandidate
	for i := range st.Services {
		s := &st.Services[i]
		if s.Status == ServiceStatusRunning && r.qualifies(s.IdleState) {
			out = append(out, IdleCandidate{Kind: EntityService, Name: s.Name})
		}
	}
	for i := range st.Apps {
		a := &st.Apps[i]
		if a.Status == ServiceStatusRunning && r.qualifies(a.IdleState) {
			out = append(out, IdleCandidate{Kind: EntityApp, Name: a.Name})
		}
	}
	return out
}

// qualifies reports whether an idle signal is a confirmed idle-past-threshold
// state. A nil signal (unknown) never qualifies -- this is the core safety
// guard.
func (r *AutoStopReconciler) qualifies(idle *IdleState) bool {
	if idle == nil || !idle.Idle {
		return false
	}
	return idle.IdleSeconds >= r.thresholdSeconds
}

// Reconcile evicts every confirmed-idle candidate in the status when auto-stop
// is enabled, logging each action. It is a no-op (returns 0) when disabled, so
// the default-OFF contract holds even if it is wired unconditionally. Returns
// the number of stop calls that succeeded.
func (r *AutoStopReconciler) Reconcile(st *NodeStatus) int {
	if !r.Enabled() {
		return 0
	}
	stopped := 0
	for _, c := range r.Candidates(st) {
		r.logf("info", "   - auto-stop: %s %q idle past %ds threshold, stopping to reclaim resources",
			c.Kind, c.Name, r.thresholdSeconds)
		if err := r.stop(c); err != nil {
			r.logf("warning", "   - auto-stop: failed to stop %s %q: %v", c.Kind, c.Name, err)
			continue
		}
		r.logf("info", "   - auto-stop: stopped %s %q", c.Kind, c.Name)
		stopped++
	}
	return stopped
}

func (r *AutoStopReconciler) logf(level, format string, args ...any) {
	if r.log != nil {
		r.log(level, format, args...)
	}
}

// idleAge is a tiny helper for callers/tests that want the idle age as a
// Duration from an IdleState (which stores seconds on the wire).
func idleAge(idle *IdleState) time.Duration {
	if idle == nil {
		return 0
	}
	return time.Duration(idle.IdleSeconds) * time.Second
}
