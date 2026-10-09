// internal/update/report.go
//
// Heartbeat auto-update telemetry (citadel-cli#1134): a bounded, versioned
// snapshot of the EFFECTIVE auto-update policy this node is running under and
// the most recent update ATTEMPT OUTCOME, so the platform can see per-node
// update health from the heartbeat alone — no shell access, no release lookup.
//
// This file owns the node-side half:
//   - the bounded result codes recorded on each attempt/terminal outcome,
//   - the atomic load-modify-save of state.json that keeps a telemetry result
//     write from clobbering a concurrent preference write (and vice versa),
//   - ReportSnapshot, the source-of-truth shape cmd projects onto the
//     heartbeat-facing status.AutoUpdateReport (via autoUpdateReportFrom), and
//   - ReportCache, the atomic-pointer cache the heartbeat serialization path
//     reads with no disk/network op.
//
// The policy resolution (configured/effective/source) and snapshot assembly
// live in the cmd layer, which owns the flag/env/dev precedence; this package
// only owns persistence, result recording, and the cache primitive.
package update

import (
	"encoding/json"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// AutoUpdateReportSchemaVersion is the contract version of the heartbeat
// auto_update block. Bump only on a breaking change to the fields a consumer
// reads; an additive field does not require a bump.
const AutoUpdateReportSchemaVersion = 1

// Bounded result codes recorded on State.LastResult and surfaced as the
// heartbeat auto_update.last_result. Bounded by design: never a raw URL, path,
// log line, or secret. Keep this set and status.AutoUpdateReport's doc in sync.
const (
	ResultNeverChecked         = "never_checked"          // no check has run yet
	ResultUpToDate             = "up_to_date"             // check succeeded, already current
	ResultCheckFailed          = "check_failed"           // the release lookup failed (offline etc.)
	ResultDownloadFailed       = "download_failed"        // download or checksum verification failed
	ResultDrainDeferred        = "drain_deferred"         // could not reach idle to swap this cycle
	ResultApplyFailed          = "apply_failed"           // the in-place binary swap failed
	ResultRestartPending       = "restart_pending"        // applied/staged; not yet proven running
	ResultRestartFailed        = "restart_failed"         // swap applied but the restart call failed
	ResultManualUpdateRequired = "manual_update_required" // homebrew/desktop-managed; can't self-swap
	ResultUnknown              = "unknown"                // state unreadable/corrupt; degrade, don't guess
)

// stateMu serializes the load-modify-save of state.json so a preference write
// (`citadel update enable/disable`) and a telemetry result write never clobber
// one another. A result write only ever touches the telemetry fields; each
// load-modify-save preserves every other field it read.
var stateMu sync.Mutex

// mutateState atomically loads state.json, applies mutate, and saves it, under
// stateMu. A missing or unreadable file degrades to a fresh default before
// mutate runs. Best-effort: the error is returned for tests, but production
// callers ignore it — telemetry must never interrupt the worker.
func mutateState(mutate func(*State)) error {
	stateMu.Lock()
	defer stateMu.Unlock()
	state, err := LoadState()
	if err != nil || state == nil {
		state = defaultState()
	}
	mutate(state)
	return SaveState(state)
}

// RecordCheckResult atomically records one auto-update attempt outcome into
// state.json, preserving the preference fields (AutoUpdate/Channel). checked
// marks whether a release check was actually performed (so last_check_at
// advances only then); latest is the release tag observed ("" leaves
// latest_version unchanged). Best-effort.
func RecordCheckResult(now time.Time, result string, checked bool, latest string) error {
	now = now.UTC()
	return mutateState(func(s *State) {
		s.LastResult = result
		s.LastResultAt = now
		if checked {
			s.LastCheckAt = now
		}
		if latest != "" {
			s.LatestVersion = latest
		}
	})
}

// RecordStagedUpdate records that newVersion's binary was applied (staged) but
// is not yet proven running, via the #923 CurrentVersion!=runningVersion
// mechanism, and marks last_result=restart_pending. "A successful apply records
// staging, not proof the new process is running." Atomic/preference-preserving.
func RecordStagedUpdate(now time.Time, newVersion string) error {
	now = now.UTC()
	return mutateState(func(s *State) {
		s.PreviousVersion = s.CurrentVersion
		s.CurrentVersion = newVersion
		s.LastUpdate = now
		s.AvailableUpdate = ""
		s.LastCheck = now
		s.LastCheckAt = now
		s.LatestVersion = newVersion
		s.LastResult = ResultRestartPending
		s.LastResultAt = now
	})
}

// ReconcilePendingOnStartup reconciles a persisted restart-pending state with
// the compiled running version at worker startup. When the staged
// CurrentVersion now equals the running binary, the restart succeeded, so a
// lingering restart_pending/restart_failed result is cleared to up_to_date (the
// staged version is now current). When they differ (restart failed, still on
// the old binary), nothing is cleared — pending_version/restart_required remain
// computed from the mismatch by the snapshot builder. Best-effort; corrupt
// state is left untouched (never repaired into a confident report).
func ReconcilePendingOnStartup(now time.Time, runningVersion string) error {
	state, corrupt := ReadStateForReport()
	if corrupt || state == nil {
		return nil
	}
	if state.CurrentVersion == "" || !VersionsEqual(state.CurrentVersion, runningVersion) {
		return nil
	}
	if state.LastResult != ResultRestartPending && state.LastResult != ResultRestartFailed {
		return nil
	}
	now = now.UTC()
	return mutateState(func(s *State) {
		// Re-check under the lock: a concurrent write may have changed things.
		if VersionsEqual(s.CurrentVersion, runningVersion) &&
			(s.LastResult == ResultRestartPending || s.LastResult == ResultRestartFailed) {
			s.LastResult = ResultUpToDate
			s.LastResultAt = now
		}
	})
}

// ReadStateForReport reads state.json for the heartbeat report, distinguishing
// a corrupt/unreadable file (corrupt=true, so the caller reports "unknown"
// rather than a confident disabled) from a cleanly-absent one (corrupt=false,
// default-off state — auto-update is opt-in, so an absent file legitimately
// means disabled). Unlike LoadState it does not swallow a corrupt file into a
// default, and it never writes or repairs the file.
func ReadStateForReport() (state *State, corrupt bool) {
	data, err := os.ReadFile(GetStateFilePath())
	if err != nil {
		if os.IsNotExist(err) {
			return defaultState(), false
		}
		return nil, true
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, true
	}
	return &st, false
}

// VersionsEqual compares two version strings ignoring a leading "v" and
// surrounding whitespace, matching agentNodeInfo's #923 pending-version check.
func VersionsEqual(a, b string) bool {
	return strings.TrimPrefix(strings.TrimSpace(a), "v") == strings.TrimPrefix(strings.TrimSpace(b), "v")
}

// ReportSnapshot is the source-of-truth shape of the heartbeat auto_update
// block. cmd assembles it (policy + persisted outcomes) off the heartbeat path
// and stores it in a ReportCache; the heartbeat provider projects it onto
// status.AutoUpdateReport via autoUpdateReportFrom. The JSON tags match that
// mirror's tags so TestAutoUpdateShapeParity can keep the two in sync.
type ReportSnapshot struct {
	SchemaVersion     int        `json:"schema_version"`
	ConfiguredEnabled *bool      `json:"configured_enabled"` // nil = unknown (corrupt/unreadable)
	EffectiveEnabled  *bool      `json:"effective_enabled"`  // nil = unknown
	PolicySource      string     `json:"policy_source"`      // persisted|env|flag|dev|opt-out|unknown
	Mode              string     `json:"mode"`               // periodic|unavailable
	IntervalSeconds   int        `json:"interval_seconds"`
	RunningVersion    string     `json:"running_version"`
	PendingVersion    string     `json:"pending_version,omitempty"` // "" = none staged
	RestartRequired   bool       `json:"restart_required"`
	LastCheckAt       *time.Time `json:"last_check_at,omitempty"`
	LatestVersion     string     `json:"latest_version,omitempty"`
	LastResultAt      *time.Time `json:"last_result_at,omitempty"`
	LastResult        string     `json:"last_result"`
}

// ReportCache holds the latest ReportSnapshot behind an atomic pointer so the
// heartbeat serialization path reads it with no disk or network operation. It
// is created before the status publishers start, so the provider closure that
// reads it on every publish races nothing (the citadel-cli#717 lesson).
type ReportCache struct {
	ptr atomic.Pointer[ReportSnapshot]
}

// Store publishes a new snapshot for subsequent heartbeats. A copy of s is
// retained so the caller may reuse its value freely.
func (c *ReportCache) Store(s ReportSnapshot) { c.ptr.Store(&s) }

// Load returns the most recently stored snapshot, or nil if none has been
// stored yet (the heartbeat then omits the auto_update block entirely).
func (c *ReportCache) Load() *ReportSnapshot { return c.ptr.Load() }
