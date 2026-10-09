package cmd

import (
	"os"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/status"
	"github.com/aceteam-ai/citadel-cli/internal/update"
)

// resolveAutoUpdatePolicy decides, for the heartbeat auto_update report
// (citadel-cli#1134), BOTH whether the periodic updater will install
// (effective) AND which signal decided it (source). It mirrors
// resolveAutoUpdateEnabled's precedence exactly — opt-out/dev veto > work flag >
// CITADEL_AUTO_UPDATE env > persisted preference > default-off — so the two can
// never disagree on the effective bool (resolveAutoUpdateEnabled delegates
// here). The source is the extra telemetry: persisted|env|flag|dev|opt-out|
// unknown. A corrupt/unreadable persisted preference resolves to
// (false, "unknown") rather than a confident disabled.
func resolveAutoUpdatePolicy(inputs autoUpdatePolicyInputs) (effective bool, source string) {
	if autoUpdateOptedOut() {
		return false, "opt-out"
	}
	if !update.IsReleaseVersion(Version) {
		return false, "dev"
	}
	if inputs.force {
		return true, "flag"
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CITADEL_AUTO_UPDATE"))) {
	case "1", "true", "yes", "on":
		return true, "env"
	case "0", "false", "no", "off":
		return false, "env"
	}
	state, corrupt := update.ReadStateForReport()
	if corrupt || state == nil {
		return false, "unknown"
	}
	return state.AutoUpdate, "persisted"
}

// autoUpdateIntervalSeconds reports the interval the periodic loop will actually
// use (clamped by the same ClampInterval NewAutoUpdater applies), in seconds.
// A misconfigured interval (ParseInterval error — the loop won't start) reports
// 0 rather than a misleading default.
func autoUpdateIntervalSeconds(inputs autoUpdatePolicyInputs) int {
	d, err := update.ParseInterval(resolveAutoUpdateInterval(inputs))
	if err != nil {
		return 0
	}
	return int(update.ClampInterval(d).Seconds())
}

// buildAutoUpdateSnapshot assembles the cached heartbeat auto_update snapshot
// (citadel-cli#1134) OFF the serialization path: it resolves the effective
// policy, reads the persisted outcome fields, and computes pending_version/
// restart_required via the #923 CurrentVersion!=running mechanism agentNodeInfo
// uses. mode is "periodic" when a periodic updater loop is running in this
// process, else "unavailable". It performs NO release lookup — only a local
// state read.
//
// Degradation rules (do not turn a corrupt state into a confident report):
//   - unreadable/corrupt state -> configured_enabled=null, last_result=unknown,
//     pending not computed;
//   - absent state -> default-off (auto-update is opt-in), last_result=
//     never_checked;
//   - older state missing the telemetry fields -> never_checked / zero times,
//     which read as absent in the projection.
func buildAutoUpdateSnapshot(inputs autoUpdatePolicyInputs, mode string) update.ReportSnapshot {
	effective, source := resolveAutoUpdatePolicy(inputs)
	snap := update.ReportSnapshot{
		SchemaVersion:    update.AutoUpdateReportSchemaVersion,
		EffectiveEnabled: &effective,
		PolicySource:     source,
		Mode:             mode,
		IntervalSeconds:  autoUpdateIntervalSeconds(inputs),
		RunningVersion:   Version,
		LastResult:       update.ResultNeverChecked,
	}

	state, corrupt := update.ReadStateForReport()
	if corrupt || state == nil {
		// Unknown preference, unknown result; never a confident disabled.
		snap.ConfiguredEnabled = nil
		snap.LastResult = update.ResultUnknown
		return snap
	}

	configured := state.AutoUpdate
	snap.ConfiguredEnabled = &configured

	// pending_version/restart_required: the staged CurrentVersion differs from
	// the running binary (apply succeeded, restart not yet proven). Reuses
	// agentNodeInfo's #923 rule.
	if state.CurrentVersion != "" && !update.VersionsEqual(state.CurrentVersion, Version) {
		snap.PendingVersion = state.CurrentVersion
		snap.RestartRequired = true
	}

	if !state.LastCheckAt.IsZero() {
		t := state.LastCheckAt.UTC()
		snap.LastCheckAt = &t
	}
	snap.LatestVersion = state.LatestVersion
	if !state.LastResultAt.IsZero() {
		t := state.LastResultAt.UTC()
		snap.LastResultAt = &t
	}
	if state.LastResult != "" {
		snap.LastResult = state.LastResult
	}
	return snap
}

// autoUpdateReportFrom projects the update-owned ReportSnapshot onto the
// heartbeat-facing status.AutoUpdateReport (citadel-cli#1134), the same hand-
// mapped split as swapStatsFrom/laneActivityFrom. nil in -> nil out (the
// heartbeat omits auto_update entirely). TestAutoUpdateShapeParity keeps the
// two struct shapes from drifting.
func autoUpdateReportFrom(s *update.ReportSnapshot) *status.AutoUpdateReport {
	if s == nil {
		return nil
	}
	return &status.AutoUpdateReport{
		SchemaVersion:     s.SchemaVersion,
		ConfiguredEnabled: s.ConfiguredEnabled,
		EffectiveEnabled:  s.EffectiveEnabled,
		PolicySource:      s.PolicySource,
		Mode:              s.Mode,
		IntervalSeconds:   s.IntervalSeconds,
		RunningVersion:    s.RunningVersion,
		PendingVersion:    s.PendingVersion,
		RestartRequired:   s.RestartRequired,
		LastCheckAt:       s.LastCheckAt,
		LatestVersion:     s.LatestVersion,
		LastResultAt:      s.LastResultAt,
		LastResult:        s.LastResult,
	}
}
