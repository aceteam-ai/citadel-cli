package cmd

import (
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/status"
	"github.com/aceteam-ai/citadel-cli/internal/update"
)

// TestAutoUpdateShapeParity guards the hand-maintained mirror between
// update.ReportSnapshot (source of truth) and status.AutoUpdateReport (the
// heartbeat-facing projection autoUpdateReportFrom builds), mirroring
// TestSwapShapeParity. Without it, a field added to one struct would silently
// never reach the heartbeat (or dangle unmapped).
func TestAutoUpdateShapeParity(t *testing.T) {
	source := jsonFieldNames(reflect.TypeOf(update.ReportSnapshot{}))
	mirror := jsonFieldNames(reflect.TypeOf(status.AutoUpdateReport{}))
	for name := range source {
		if _, ok := mirror[name]; !ok {
			t.Errorf("update.ReportSnapshot has JSON field %q with no counterpart in status.AutoUpdateReport (update autoUpdateReportFrom)", name)
		}
	}
	for name := range mirror {
		if _, ok := source[name]; !ok {
			t.Errorf("status.AutoUpdateReport has JSON field %q with no counterpart in update.ReportSnapshot (the mirror drifted ahead of its source)", name)
		}
	}
}

func TestAutoUpdateReportFrom(t *testing.T) {
	if autoUpdateReportFrom(nil) != nil {
		t.Fatal("nil snapshot must project to nil (heartbeat omits auto_update)")
	}
	cfg, eff := true, false
	ts := time.Date(2026, 10, 9, 4, 0, 0, 0, time.UTC)
	snap := &update.ReportSnapshot{
		SchemaVersion:     1,
		ConfiguredEnabled: &cfg,
		EffectiveEnabled:  &eff,
		PolicySource:      "env",
		Mode:              "periodic",
		IntervalSeconds:   3600,
		RunningVersion:    "v2.0.0",
		PendingVersion:    "v2.1.0",
		RestartRequired:   true,
		LastCheckAt:       &ts,
		LatestVersion:     "v2.1.0",
		LastResultAt:      &ts,
		LastResult:        update.ResultUpToDate,
	}
	got := autoUpdateReportFrom(snap)
	want := &status.AutoUpdateReport{
		SchemaVersion:     1,
		ConfiguredEnabled: &cfg,
		EffectiveEnabled:  &eff,
		PolicySource:      "env",
		Mode:              "periodic",
		IntervalSeconds:   3600,
		RunningVersion:    "v2.0.0",
		PendingVersion:    "v2.1.0",
		RestartRequired:   true,
		LastCheckAt:       &ts,
		LatestVersion:     "v2.1.0",
		LastResultAt:      &ts,
		LastResult:        update.ResultUpToDate,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("autoUpdateReportFrom mismatch:\n got=%+v\nwant=%+v", got, want)
	}
}

// withVersion sets the compiled Version and resets the auto-update flags for a
// test, restoring them after. HOME is sandboxed to a temp dir so update state
// is isolated (never the operator's real ~/citadel-node/update/state.json).
func withAutoUpdateEnv(t *testing.T, version string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CITADEL_AUTO_UPDATE", "")
	t.Setenv("CITADEL_NO_AUTO_UPDATE", "")
	t.Setenv("CITADEL_AUTO_UPDATE_INTERVAL", "")
	orig, origFlag, origNoAuto := Version, workAutoUpdate, noAutoUpdate
	Version, workAutoUpdate, noAutoUpdate = version, false, false
	t.Cleanup(func() { Version, workAutoUpdate, noAutoUpdate = orig, origFlag, origNoAuto })
}

func boolVal(t *testing.T, p *bool, want bool, field string) {
	t.Helper()
	if p == nil {
		t.Fatalf("%s is nil, want %v", field, want)
	}
	if *p != want {
		t.Errorf("%s = %v, want %v", field, *p, want)
	}
}

func TestBuildAutoUpdateSnapshot(t *testing.T) {
	t.Run("persisted enabled, periodic", func(t *testing.T) {
		withAutoUpdateEnv(t, "v2.45.0")
		if err := update.SaveState(&update.State{AutoUpdate: true, Channel: "stable"}); err != nil {
			t.Fatalf("SaveState: %v", err)
		}
		snap := buildAutoUpdateSnapshot(autoUpdatePolicyInputs{}, "periodic")
		if snap.SchemaVersion != update.AutoUpdateReportSchemaVersion {
			t.Errorf("schema_version = %d", snap.SchemaVersion)
		}
		boolVal(t, snap.ConfiguredEnabled, true, "configured_enabled")
		boolVal(t, snap.EffectiveEnabled, true, "effective_enabled")
		if snap.PolicySource != "persisted" {
			t.Errorf("policy_source = %q, want persisted", snap.PolicySource)
		}
		if snap.Mode != "periodic" {
			t.Errorf("mode = %q, want periodic", snap.Mode)
		}
		if snap.RunningVersion != "v2.45.0" {
			t.Errorf("running_version = %q", snap.RunningVersion)
		}
		if snap.IntervalSeconds <= 0 {
			t.Errorf("interval_seconds = %d, want > 0", snap.IntervalSeconds)
		}
		if snap.LastResult != update.ResultNeverChecked {
			t.Errorf("last_result = %q, want never_checked", snap.LastResult)
		}
	})

	t.Run("env off overrides enabled state", func(t *testing.T) {
		withAutoUpdateEnv(t, "v2.45.0")
		t.Setenv("CITADEL_AUTO_UPDATE", "off")
		if err := update.SaveState(&update.State{AutoUpdate: true, Channel: "stable"}); err != nil {
			t.Fatalf("SaveState: %v", err)
		}
		snap := buildAutoUpdateSnapshot(autoUpdatePolicyInputs{}, "periodic")
		boolVal(t, snap.ConfiguredEnabled, true, "configured_enabled") // persisted pref unchanged
		boolVal(t, snap.EffectiveEnabled, false, "effective_enabled")  // env vetoes it
		if snap.PolicySource != "env" {
			t.Errorf("policy_source = %q, want env", snap.PolicySource)
		}
	})

	t.Run("dev build vetoes, configured still reported", func(t *testing.T) {
		withAutoUpdateEnv(t, "dev")
		if err := update.SaveState(&update.State{AutoUpdate: true, Channel: "stable"}); err != nil {
			t.Fatalf("SaveState: %v", err)
		}
		snap := buildAutoUpdateSnapshot(autoUpdatePolicyInputs{}, "periodic")
		boolVal(t, snap.ConfiguredEnabled, true, "configured_enabled")
		boolVal(t, snap.EffectiveEnabled, false, "effective_enabled")
		if snap.PolicySource != "dev" {
			t.Errorf("policy_source = %q, want dev", snap.PolicySource)
		}
	})

	t.Run("corrupt state degrades to unknown, never a confident disabled", func(t *testing.T) {
		withAutoUpdateEnv(t, "v2.45.0")
		if err := update.EnsureUpdateDir(); err != nil {
			t.Fatalf("EnsureUpdateDir: %v", err)
		}
		if err := os.WriteFile(update.GetStateFilePath(), []byte("{broken"), 0644); err != nil {
			t.Fatalf("write corrupt: %v", err)
		}
		snap := buildAutoUpdateSnapshot(autoUpdatePolicyInputs{}, "periodic")
		if snap.ConfiguredEnabled != nil {
			t.Errorf("configured_enabled = %v, want nil (unknown) on corrupt state", *snap.ConfiguredEnabled)
		}
		if snap.PolicySource != "unknown" {
			t.Errorf("policy_source = %q, want unknown", snap.PolicySource)
		}
		if snap.LastResult != update.ResultUnknown {
			t.Errorf("last_result = %q, want unknown", snap.LastResult)
		}
	})

	t.Run("pending version computed from staged vs running", func(t *testing.T) {
		withAutoUpdateEnv(t, "v2.0.0")
		if err := update.SaveState(&update.State{CurrentVersion: "v9.9.9", AutoUpdate: true, Channel: "stable"}); err != nil {
			t.Fatalf("SaveState: %v", err)
		}
		snap := buildAutoUpdateSnapshot(autoUpdatePolicyInputs{}, "periodic")
		if snap.PendingVersion != "v9.9.9" || !snap.RestartRequired {
			t.Errorf("pending=%q restart_required=%v, want v9.9.9/true", snap.PendingVersion, snap.RestartRequired)
		}
	})

	t.Run("outcome fields surfaced", func(t *testing.T) {
		withAutoUpdateEnv(t, "v2.0.0")
		ts := time.Date(2026, 10, 9, 4, 0, 0, 0, time.UTC)
		if err := update.SaveState(&update.State{
			AutoUpdate: true, Channel: "stable", CurrentVersion: "v2.0.0",
			LastCheckAt: ts, LatestVersion: "v2.0.0", LastResultAt: ts, LastResult: update.ResultUpToDate,
		}); err != nil {
			t.Fatalf("SaveState: %v", err)
		}
		snap := buildAutoUpdateSnapshot(autoUpdatePolicyInputs{}, "periodic")
		if snap.LastResult != update.ResultUpToDate {
			t.Errorf("last_result = %q", snap.LastResult)
		}
		if snap.LastCheckAt == nil || !snap.LastCheckAt.Equal(ts) {
			t.Errorf("last_check_at = %v, want %v", snap.LastCheckAt, ts)
		}
		if snap.LatestVersion != "v2.0.0" {
			t.Errorf("latest_version = %q", snap.LatestVersion)
		}
		if snap.PendingVersion != "" || snap.RestartRequired {
			t.Errorf("no pending expected when staged==running, got pending=%q restart=%v", snap.PendingVersion, snap.RestartRequired)
		}
	})
}

// TestAutoUpdateSnapshotNextProcessClearsPending is the full lifecycle the
// issue calls out: a restart error retains the old running version + a pending
// new version; the simulated next-process startup clears pending only when the
// running binary now matches the staged version.
func TestAutoUpdateSnapshotNextProcessClearsPending(t *testing.T) {
	withAutoUpdateEnv(t, "v1.0.0") // still on the old binary (restart failed)
	if err := update.SaveState(&update.State{
		CurrentVersion: "v2.0.0", PreviousVersion: "v1.0.0",
		LastResult: update.ResultRestartFailed, AutoUpdate: true, Channel: "stable",
	}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	before := buildAutoUpdateSnapshot(autoUpdatePolicyInputs{}, "periodic")
	if before.PendingVersion != "v2.0.0" || !before.RestartRequired {
		t.Fatalf("restart error must retain pending new version: pending=%q restart=%v", before.PendingVersion, before.RestartRequired)
	}
	if before.LastResult != update.ResultRestartFailed {
		t.Errorf("last_result = %q, want restart_failed", before.LastResult)
	}

	// Simulate the next process starting ON the staged binary.
	Version = "v2.0.0"
	if err := update.ReconcilePendingOnStartup(time.Now(), Version); err != nil {
		t.Fatalf("ReconcilePendingOnStartup: %v", err)
	}
	after := buildAutoUpdateSnapshot(autoUpdatePolicyInputs{}, "periodic")
	if after.PendingVersion != "" || after.RestartRequired {
		t.Errorf("matching startup must clear pending: pending=%q restart=%v", after.PendingVersion, after.RestartRequired)
	}
	if after.LastResult != update.ResultUpToDate {
		t.Errorf("matching startup must clear restart_failed to up_to_date, got %q", after.LastResult)
	}
}
