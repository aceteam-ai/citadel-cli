package cmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
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
	home := t.TempDir()
	for _, key := range []string{"HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME"} {
		t.Setenv(key, home)
	}
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
		if snap.LastResult != update.ResultStateCorrupt {
			t.Errorf("last_result = %q, want state_corrupt", snap.LastResult)
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

func TestCorruptUpdateStateKeepsPolicyPrecedenceAndClearsHeartbeatClaims(t *testing.T) {
	withAutoUpdateEnv(t, "v2.200.0")
	if err := update.EnsureUpdateDir(); err != nil {
		t.Fatal(err)
	}
	broken := []byte("{\"auto_update\":true,\"current_version\":\"v9.0.0\"}}")
	if err := os.WriteFile(update.GetStateFilePath(), broken, 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, env, optOut, version, source string
		force, enabled                     bool
	}{
		{name: "unknown", version: "v2.200.0", source: "unknown"},
		{name: "env on", env: "on", version: "v2.200.0", source: "env", enabled: true},
		{name: "env off", env: "off", version: "v2.200.0", source: "env"},
		{name: "flag before env", env: "off", force: true, version: "v2.200.0", source: "flag", enabled: true},
		{name: "opt out before flag", env: "on", force: true, optOut: "1", version: "v2.200.0", source: "opt-out"},
		{name: "dev before flag", env: "on", force: true, version: "dev", source: "dev"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CITADEL_AUTO_UPDATE", tc.env)
			t.Setenv("CITADEL_NO_AUTO_UPDATE", tc.optOut)
			Version = tc.version
			inputs := autoUpdatePolicyInputs{force: tc.force}
			if enabled, source := resolveAutoUpdatePolicy(inputs); enabled != tc.enabled || source != tc.source {
				t.Fatalf("policy mismatch: enabled=%t source=%s", enabled, source)
			}
			snapshot := buildAutoUpdateSnapshot(inputs, "periodic")
			if snapshot.ConfiguredEnabled != nil || snapshot.EffectiveEnabled == nil || *snapshot.EffectiveEnabled != tc.enabled ||
				snapshot.LastResult != update.ResultStateCorrupt || snapshot.PolicySource != tc.source ||
				snapshot.PendingVersion != "" || snapshot.RestartRequired || snapshot.LastResultAt != nil || snapshot.LastCheckAt != nil || snapshot.LatestVersion != "" {
				t.Fatalf("corrupt heartbeat lost explicit policy or inferred file fields: %+v", snapshot)
			}
		})
	}
	Version = "v2.200.0"
	if err := update.SetAutoUpdate(true); err != nil {
		t.Fatal(err)
	}
	repaired := buildAutoUpdateSnapshot(autoUpdatePolicyInputs{}, "periodic")
	if repaired.ConfiguredEnabled == nil || !*repaired.ConfiguredEnabled ||
		repaired.EffectiveEnabled == nil || !*repaired.EffectiveEnabled || repaired.LastResult == update.ResultStateCorrupt {
		t.Fatalf("explicit repair did not clear the pause/report: %+v", repaired)
	}
}

type corruptStateReleaseChecker struct{ checks, downloads int }

func (c *corruptStateReleaseChecker) CheckForUpdate() (*update.Release, error) {
	c.checks++
	return &update.Release{TagName: "v2.201.0"}, nil
}

func (c *corruptStateReleaseChecker) DownloadAndVerify(*update.Release, string) error {
	c.downloads++
	return nil
}

// Exercise the actual worker wiring rather than only policy/report helpers.
func TestCorruptUpdateStateEnvOnStillChecksAndReportsCorruption(t *testing.T) {
	withAutoUpdateEnv(t, "v2.200.0")
	t.Setenv("CITADEL_AUTO_UPDATE", "on")
	if err := update.EnsureUpdateDir(); err != nil {
		t.Fatal(err)
	}
	broken := []byte("{broken}")
	if err := os.WriteFile(update.GetStateFilePath(), broken, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ticks := make(chan time.Time, 1)
	ticks <- time.Unix(1, 0)
	checker := &corruptStateReleaseChecker{}
	applies, restarts := 0, 0
	var last update.ReportSnapshot
	done := startWorkerAutoUpdater(ctx, true, autoUpdatePolicyInputs{}, &updaterTestWorker{}, func(string, ...any) {}, autoUpdateRuntime{
		checker: checker, ticks: ticks, afterTick: cancel,
		refresh: func() { last = buildAutoUpdateSnapshot(autoUpdatePolicyInputs{}, "periodic") },
		apply:   func(string) error { applies++; return nil },
		restart: func() error { restarts++; return nil },
	})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("fixture updater did not stop")
	}
	if checker.checks != 1 || checker.downloads != 1 || applies != 1 || restarts != 1 {
		t.Fatalf("explicit env did not complete fake update: checks=%d downloads=%d applies=%d restarts=%d", checker.checks, checker.downloads, applies, restarts)
	}
	if last.EffectiveEnabled == nil || !*last.EffectiveEnabled || last.PolicySource != "env" || last.LastResult != update.ResultStateCorrupt || last.ConfiguredEnabled != nil {
		t.Fatalf("forced update lost corrupt telemetry: %+v", last)
	}
	if got, err := os.ReadFile(update.GetStateFilePath()); err != nil || !bytes.Equal(got, broken) {
		t.Fatal("forced check overwrote corrupt evidence")
	}
}

func TestCorruptUpdateStatusNamesBothRepairCommands(t *testing.T) {
	withAutoUpdateEnv(t, "v2.200.0")
	if err := update.EnsureUpdateDir(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(update.GetStateFilePath(), []byte("null"), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if state, err := loadUpdateStateForStatus(&out); state != nil || !errors.Is(err, update.ErrStateCorrupt) {
		t.Fatalf("corrupt status loaded defaults: state=%v err=%v", state, err)
	}
	for _, command := range []string{"citadel update enable", "citadel update disable"} {
		if !strings.Contains(out.String(), command) {
			t.Fatalf("status omitted repair command %q: %s", command, &out)
		}
	}
}

func TestAutoUpdateSnapshotRejectsStalePendingVersion(t *testing.T) {
	for _, tc := range []struct {
		name, recorded string
		pending        bool
	}{
		{"old install", "v2.97.0", false},
		{"equal", "2.202.0", false},
		{"equal precedence", "v2.202.0+another", false},
		{"newer staged", "v2.203.0", true},
		{"malformed", "broken", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withAutoUpdateEnv(t, "v2.202.0")
			if err := update.SaveState(&update.State{CurrentVersion: tc.recorded, AutoUpdate: true, Channel: "stable"}); err != nil {
				t.Fatal(err)
			}
			snapshot := buildAutoUpdateSnapshot(autoUpdatePolicyInputs{}, "periodic")
			want := ""
			if tc.pending {
				want = tc.recorded
			}
			if snapshot.PendingVersion != want || snapshot.RestartRequired != tc.pending {
				t.Fatalf("pending=%q restart=%t, want %q/%t", snapshot.PendingVersion, snapshot.RestartRequired, want, tc.pending)
			}
		})
	}
}
