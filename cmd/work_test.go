package cmd

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/cacheindex"
	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/reconcile"
	"github.com/aceteam-ai/citadel-cli/internal/status"
	"github.com/aceteam-ai/citadel-cli/internal/update"
	"github.com/aceteam-ai/citadel-cli/internal/worker"
	"github.com/aceteam-ai/citadel-cli/services"
)

// TestResolveAutoUpdateEnabled verifies the precedence that lets the web UI /
// `citadel update enable/disable` toggle a node: explicit --auto-update flag >
// CITADEL_AUTO_UPDATE env (on/off) > persisted state > default-off.
func TestResolveAutoUpdateEnabled(t *testing.T) {
	writeState := func(t *testing.T, auto bool) {
		t.Helper()
		if err := update.SaveState(&update.State{AutoUpdate: auto, Channel: "stable"}); err != nil {
			t.Fatalf("SaveState: %v", err)
		}
	}

	cases := []struct {
		name     string
		flag     bool
		env      string // "" means unset
		state    *bool  // nil means no state file
		version  string // "" means a real release tag (v2.45.0)
		noAuto   bool   // --no-auto-update flag
		noAutoEn string // CITADEL_NO_AUTO_UPDATE env
		expected bool
	}{
		{name: "default off when nothing set", expected: false},
		{name: "persisted state enables", state: boolPtr(true), expected: true},
		{name: "persisted state disables", state: boolPtr(false), expected: false},
		{name: "env true overrides disabled state", env: "true", state: boolPtr(false), expected: true},
		{name: "env off overrides enabled state", env: "off", state: boolPtr(true), expected: false},
		{name: "flag wins over env and state", flag: true, env: "off", state: boolPtr(false), expected: true},
		// #473: a dev binary never auto-installs, even with an enable signal.
		{name: "dev binary vetoes auto-update flag", flag: true, version: "dev", expected: false},
		{name: "dev binary vetoes enabled state", state: boolPtr(true), version: "dev", expected: false},
		// #473: opt-out (flag / env) wins over every enable signal.
		{name: "no-auto-update flag vetoes auto-update flag", flag: true, noAuto: true, expected: false},
		{name: "CITADEL_NO_AUTO_UPDATE env vetoes enabled state", state: boolPtr(true), noAutoEn: "1", expected: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir()) // isolate the on-disk update state
			t.Setenv("CITADEL_AUTO_UPDATE", tc.env)
			t.Setenv("CITADEL_NO_AUTO_UPDATE", tc.noAutoEn)
			ver := tc.version
			if ver == "" {
				ver = "v2.45.0"
			}
			origVer, origFlag, origNoAuto := Version, workAutoUpdate, noAutoUpdate
			Version, workAutoUpdate, noAutoUpdate = ver, tc.flag, tc.noAuto
			t.Cleanup(func() { Version, workAutoUpdate, noAutoUpdate = origVer, origFlag, origNoAuto })
			if tc.state != nil {
				writeState(t, *tc.state)
			}
			if got := resolveAutoUpdateEnabled(); got != tc.expected {
				t.Errorf("resolveAutoUpdateEnabled() = %v, want %v", got, tc.expected)
			}
		})
	}
}

func boolPtr(v bool) *bool { return &v }

func TestShellQueueName(t *testing.T) {
	tests := []struct {
		orgID string
		want  string
	}{
		{
			orgID: "550e8400-e29b-41d4-a716-446655440000",
			want:  "jobs:v1:shell:org_550e8400-e29b-41d4-a716-446655440000",
		},
		{
			orgID: "test-org-id",
			want:  "jobs:v1:shell:org_test-org-id",
		},
		{
			orgID: "",
			want:  "jobs:v1:shell:org_",
		},
	}

	for _, tt := range tests {
		got := shellQueueName(tt.orgID)
		if got != tt.want {
			t.Errorf("shellQueueName(%q) = %q, want %q", tt.orgID, got, tt.want)
		}
	}
}

func TestResolveConsumerGroup(t *testing.T) {
	tests := []struct {
		name            string
		explicit        string
		headscaleNodeID string
		hostname        string
		want            string
	}{
		{
			name:            "explicit flag takes priority",
			explicit:        "my-custom-group",
			headscaleNodeID: "42",
			hostname:        "gpu-rig",
			want:            "my-custom-group",
		},
		{
			name:            "headscale node ID used when no explicit flag",
			headscaleNodeID: "42",
			hostname:        "gpu-rig",
			want:            "citadel-node-42",
		},
		{
			name:     "hostname fallback when no headscale ID",
			hostname: "gpu-rig",
			want:     "citadel-gpu-rig",
		},
		{
			name: "default fallback when nothing available",
			want: "citadel-workers",
		},
		{
			name:            "empty strings are not set",
			explicit:        "",
			headscaleNodeID: "",
			hostname:        "",
			want:            "citadel-workers",
		},
		{
			name:     "explicit citadel-workers is respected",
			explicit: "citadel-workers",
			want:     "citadel-workers",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveConsumerGroup(tt.explicit, tt.headscaleNodeID, tt.hostname)
			if got != tt.want {
				t.Errorf("resolveConsumerGroup(%q, %q, %q) = %q, want %q",
					tt.explicit, tt.headscaleNodeID, tt.hostname, got, tt.want)
			}
		})
	}
}

// TestMeetingQueueName guards the org-scoped meeting-notetaker tag queue naming
// convention. This string MUST match the Python dispatch helper byte-for-byte,
// otherwise MEETING_JOIN auto-join dispatch (aceteam-ai/aceteam#5098) never
// reaches the node. The bare jobs:v1:tag:meeting queue is rejected server-side.
func TestMeetingQueueName(t *testing.T) {
	tests := []struct {
		orgID string
		want  string
	}{
		{
			orgID: "550e8400-e29b-41d4-a716-446655440000",
			want:  "jobs:v1:tag:meeting:org_550e8400-e29b-41d4-a716-446655440000",
		},
		{
			orgID: "test-org-id",
			want:  "jobs:v1:tag:meeting:org_test-org-id",
		},
	}

	for _, tt := range tests {
		got := meetingQueueName(tt.orgID)
		if got != tt.want {
			t.Errorf("meetingQueueName(%q) = %q, want %q", tt.orgID, got, tt.want)
		}
	}
}

// TestHasCapabilityTag verifies the gate that decides whether a node subscribes
// to the meeting queue: only nodes advertising the "meeting" capability tag do,
// so a node without the audio/Chromium/Xvfb stack never claims a job it cannot
// run.
func TestHasCapabilityTag(t *testing.T) {
	tests := []struct {
		name string
		tags []string
		tag  string
		want bool
	}{
		{"present", []string{"cpu:general", "meeting", "os:linux"}, "meeting", true},
		{"absent", []string{"cpu:general", "os:linux"}, "meeting", false},
		{"nil tags", nil, "meeting", false},
		{"empty tags", []string{}, "meeting", false},
		{"no partial match", []string{"meeting:room"}, "meeting", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasCapabilityTag(tt.tags, tt.tag); got != tt.want {
				t.Errorf("hasCapabilityTag(%v, %q) = %v, want %v", tt.tags, tt.tag, got, tt.want)
			}
		})
	}
}

// TestNodeQueueName guards the per-node shell stream naming convention. This
// string MUST match the Python build_node_queue helper byte-for-byte, otherwise
// node-targeted jobs (issue #3914) silently fall back to the shared stream and
// per-node routing never engages.
func TestNodeQueueName(t *testing.T) {
	tests := []struct {
		orgID  string
		nodeID string
		want   string
	}{
		{
			orgID:  "550e8400-e29b-41d4-a716-446655440000",
			nodeID: "1008",
			want:   "jobs:v1:shell:org_550e8400-e29b-41d4-a716-446655440000:node:1008",
		},
		{
			orgID:  "test-org-id",
			nodeID: "969",
			want:   "jobs:v1:shell:org_test-org-id:node:969",
		},
	}

	for _, tt := range tests {
		got := nodeQueueName(tt.orgID, tt.nodeID)
		if got != tt.want {
			t.Errorf("nodeQueueName(%q, %q) = %q, want %q", tt.orgID, tt.nodeID, got, tt.want)
		}
	}
}

// TestSwapStatsFrom pins the hand-maintained field-by-field projection from
// worker.SwapStats onto the heartbeat-facing status.SwapActivity (citadel-cli
// #717), mirroring the existing coverage intent for workerLivenessFrom: a
// mapping between two independently-evolving structs is exactly the kind of
// thing that silently loses a field, so it gets its own test.
func TestSwapStatsFrom(t *testing.T) {
	startedAt := time.Now().Add(-90 * time.Second)
	stats := worker.SwapStats{
		SwapsPerHour:         4,
		EvictingSwapsPerHour: 2,
		MaxEvictingPerHour:   6,
		Recent: []worker.SwapRecord{
			{
				Backend:   "bonsai",
				Model:     "Bonsai-27B-Q1_0.gguf",
				Evicted:   []string{"vllm"},
				StartedAt: startedAt,
				Wait:      90 * time.Second,
				Outcome:   "ready",
			},
		},
	}

	got := swapStatsFrom(stats)
	if got == nil {
		t.Fatal("swapStatsFrom returned nil")
	}
	if got.SwapsPerHour != 4 || got.EvictingSwapsPerHour != 2 || got.MaxEvictingPerHour != 6 {
		t.Errorf("counters = %+v, want swaps=4 evicting=2 max=6", got)
	}
	if len(got.Recent) != 1 {
		t.Fatalf("Recent count = %d, want 1", len(got.Recent))
	}
	rec := got.Recent[0]
	if rec.Backend != "bonsai" || rec.Model != "Bonsai-27B-Q1_0.gguf" || rec.Outcome != "ready" {
		t.Errorf("record = %+v, want backend=bonsai model=Bonsai-27B-Q1_0.gguf outcome=ready", rec)
	}
	if len(rec.Evicted) != 1 || rec.Evicted[0] != "vllm" {
		t.Errorf("Evicted = %v, want [vllm]", rec.Evicted)
	}
	if !rec.StartedAt.Equal(startedAt) {
		t.Errorf("StartedAt = %v, want %v", rec.StartedAt, startedAt)
	}
	if rec.Wait != 90*time.Second {
		t.Errorf("Wait = %v, want 90s", rec.Wait)
	}

	// Evicted must be an independent copy, not an alias of the source slice:
	// mutating the caller's stats after conversion must not reach back into the
	// already-returned heartbeat payload.
	stats.Recent[0].Evicted[0] = "mutated"
	if got.Recent[0].Evicted[0] != "vllm" {
		t.Errorf("Evicted aliases the source slice: got %v after mutating the source, want unaffected [vllm]", got.Recent[0].Evicted)
	}

	// Empty stats (no swaps yet) must not panic and must yield a non-nil struct
	// with a nil Recent slice, matching the omitempty JSON contract.
	empty := swapStatsFrom(worker.SwapStats{MaxEvictingPerHour: 6})
	if empty == nil {
		t.Fatal("swapStatsFrom(empty) returned nil")
	}
	if empty.Recent != nil {
		t.Errorf("Recent = %v, want nil for no records", empty.Recent)
	}
}

// TestSwapShapeParity guards the hand-maintained mirror between
// worker.SwapStats/SwapRecord (the source of truth, owned by the swap
// manager) and status.SwapActivity/SwapRecord (the heartbeat-facing
// projection swapStatsFrom builds). internal/status cannot import
// internal/worker (worker already imports status), so nothing but a test
// keeps the two shapes honest: without this, #835 adding a `Pulled` field to
// worker.SwapRecord would compile fine and silently never reach the
// heartbeat, because swapStatsFrom maps fields by hand with no compiler link
// between the two structs. Mirrors the "pin it so a reader can check in one
// step" convention CLAUDE.md cites for TestSwapAccountingDefaults.
//
// This checks JSON field-name parity, not type identity: SwapStats.Recent is
// []worker.SwapRecord and SwapActivity.Recent is []status.SwapRecord, which
// is expected (the whole point of the mirror) and not itself a violation.
func TestSwapShapeParity(t *testing.T) {
	assertFieldsMatch := func(t *testing.T, source, mirror reflect.Type) {
		t.Helper()
		sourceFields := jsonFieldNames(source)
		mirrorFields := jsonFieldNames(mirror)
		for name := range sourceFields {
			if _, ok := mirrorFields[name]; !ok {
				t.Errorf("%s has JSON field %q with no counterpart in %s -- "+
					"a field was added to the swap manager's source-of-truth "+
					"type without updating the heartbeat-facing mirror (and "+
					"swapStatsFrom), so it will never reach the heartbeat",
					source, name, mirror)
			}
		}
		for name := range mirrorFields {
			if _, ok := sourceFields[name]; !ok {
				t.Errorf("%s has JSON field %q with no counterpart in %s -- "+
					"the heartbeat-facing mirror has drifted ahead of its "+
					"source of truth; verify swapStatsFrom still maps this "+
					"field from something real", mirror, name, source)
			}
		}
	}

	t.Run("SwapRecord", func(t *testing.T) {
		assertFieldsMatch(t,
			reflect.TypeOf(worker.SwapRecord{}),
			reflect.TypeOf(status.SwapRecord{}))
	})
	t.Run("SwapStats/SwapActivity", func(t *testing.T) {
		assertFieldsMatch(t,
			reflect.TypeOf(worker.SwapStats{}),
			reflect.TypeOf(status.SwapActivity{}))
	})
	t.Run("HealthState/ReconcileHealth", func(t *testing.T) {
		assertFieldsMatch(t,
			reflect.TypeOf(reconcile.HealthState{}),
			reflect.TypeOf(status.ReconcileHealth{}))
	})
	t.Run("LaneSnapshot/LaneActivity", func(t *testing.T) {
		assertFieldsMatch(t,
			reflect.TypeOf(worker.LaneSnapshot{}),
			reflect.TypeOf(status.LaneActivity{}))
	})
}

// TestLaneActivityFrom pins the worker.LaneSnapshot -> status.LaneActivity
// projection (citadel-cli#908), mirroring swapStatsFrom/reservationsFrom.
func TestLaneActivityFrom(t *testing.T) {
	if got := laneActivityFrom(nil); got != nil {
		t.Errorf("laneActivityFrom(nil) = %v, want nil", got)
	}
	since := time.Now().Add(-2 * time.Minute)
	in := []worker.LaneSnapshot{
		{Lane: "unbounded", Queued: 3, Executing: 1, ExecCapacity: 1, BusySince: &since},
		{Lane: "inference", Queued: 0, Executing: 2, ExecCapacity: 4},
	}
	got := laneActivityFrom(in)
	want := []status.LaneActivity{
		{Lane: "unbounded", Queued: 3, Executing: 1, ExecCapacity: 1, BusySince: &since},
		{Lane: "inference", Queued: 0, Executing: 2, ExecCapacity: 4},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("laneActivityFrom(%+v) = %+v, want %+v", in, got, want)
	}
}

// TestReconcileHealthFrom pins the hand-maintained projection from
// reconcile.HealthState onto the heartbeat-facing status.ReconcileHealth
// (citadel-cli#742), mirroring TestSwapStatsFrom's coverage intent above: a
// field-by-field mapping between two independently-evolving structs is
// exactly what silently loses a field, so it gets its own test.
//
// This also pins the deliberate asymmetry with workerLivenessFrom/
// swapStatsFrom: those are unconditionally non-nil once their subsystem is
// wired; reconcileHealthFrom returns nil whenever NOT currently refused, so
// the heartbeat field is omitted for the common (healthy) case instead of
// always present with Refused=false.
func TestReconcileHealthFrom(t *testing.T) {
	// Healthy / never-refused: nil in, nil out -- NodeStatus.Reconcile must
	// stay omitted, not become a present-but-false block.
	if got := reconcileHealthFrom(reconcile.HealthState{}); got != nil {
		t.Errorf("reconcileHealthFrom(zero value) = %+v, want nil (no change to the healthy heartbeat payload)", got)
	}

	since := time.Now().Add(-90 * time.Minute)
	refused := reconcile.HealthState{
		Refused: true,
		Reason:  "reconcile: refused full wipe: refusing empty desired state with 3 module(s) installed",
		Since:   since,
		Count:   17,
	}
	got := reconcileHealthFrom(refused)
	if got == nil {
		t.Fatal("reconcileHealthFrom(refused) returned nil, want a populated block")
	}
	if !got.Refused {
		t.Error("Refused should be true when the source state is refused")
	}
	if got.Reason != refused.Reason {
		t.Errorf("Reason = %q, want %q", got.Reason, refused.Reason)
	}
	if !got.Since.Equal(since) {
		t.Errorf("Since = %v, want %v", got.Since, since)
	}
	if got.Count != 17 {
		t.Errorf("Count = %d, want 17", got.Count)
	}
}

// jsonFieldNames returns the set of JSON field names a struct type encodes
// to, keyed by the tag name (falling back to the Go field name when there is
// no json tag, and skipping `json:"-"` fields).
func jsonFieldNames(t reflect.Type) map[string]struct{} {
	out := make(map[string]struct{}, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag, ok := f.Tag.Lookup("json")
		name := f.Name
		if ok {
			parts := strings.Split(tag, ",")
			if parts[0] == "-" {
				continue
			}
			if parts[0] != "" {
				name = parts[0]
			}
		}
		out[name] = struct{}{}
	}
	return out
}

// TestReservationsFrom pins the internal/jobs.ReservationSummary ->
// internal/status.GPUReservation projection (citadel-cli#832), mirroring
// swapStatsFrom.
func TestReservationsFrom(t *testing.T) {
	t.Run("nil for empty input", func(t *testing.T) {
		if got := reservationsFrom(nil); got != nil {
			t.Errorf("reservationsFrom(nil) = %v, want nil", got)
		}
	})

	t.Run("maps fields and copies slices", func(t *testing.T) {
		in := []jobs.ReservationSummary{
			{JobID: "job-1", EvictedServices: []string{"vllm", "bonsai"}},
		}
		got := reservationsFrom(in)
		want := []status.GPUReservation{
			{JobID: "job-1", EvictedServices: []string{"vllm", "bonsai"}},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("reservationsFrom(%+v) = %+v, want %+v", in, got, want)
		}
		// The returned slice must be independent of the source (defensive
		// copy), so a caller mutating one cannot corrupt the other.
		in[0].EvictedServices[0] = "mutated"
		if got[0].EvictedServices[0] == "mutated" {
			t.Error("reservationsFrom aliased the source slice instead of copying it")
		}
	})
}

// TestReservationShapeParity mirrors TestSwapShapeParity: it pins that
// jobs.ReservationSummary (the reservation primitive's local source-of-truth
// shape) and status.GPUReservation (its heartbeat-facing projection) carry the
// same fields, by Go field name -- ReservationSummary has no json tags (it is
// never itself serialized; see its doc comment), so this compares field names
// directly rather than reusing jsonFieldNames' tag-based comparison.
func TestReservationShapeParity(t *testing.T) {
	sourceFields := goFieldNames(reflect.TypeOf(jobs.ReservationSummary{}))
	mirrorFields := goFieldNames(reflect.TypeOf(status.GPUReservation{}))
	for name := range sourceFields {
		if _, ok := mirrorFields[name]; !ok {
			t.Errorf("jobs.ReservationSummary has field %q with no counterpart in status.GPUReservation -- "+
				"a field was added to the reservation primitive's source-of-truth type "+
				"without updating the heartbeat-facing mirror (and reservationsFrom), "+
				"so it will never reach the heartbeat", name)
		}
	}
	for name := range mirrorFields {
		if _, ok := sourceFields[name]; !ok {
			t.Errorf("status.GPUReservation has field %q with no counterpart in jobs.ReservationSummary -- "+
				"the heartbeat-facing mirror has drifted ahead of its source of truth", name)
		}
	}
}

// goFieldNames returns the set of exported Go field names on a struct type.
func goFieldNames(t reflect.Type) map[string]struct{} {
	out := make(map[string]struct{}, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		out[t.Field(i).Name] = struct{}{}
	}
	return out
}

// TestCacheReportFromNil pins cacheReportFrom's defensive nil handling
// (citadel #682 P3) -- mirrors swapStatsFrom(empty)'s non-nil-but-empty
// contract check, except here nil really does mean nil: no index means no
// report at all, not an empty one.
func TestCacheReportFromNil(t *testing.T) {
	if got := cacheReportFrom(nil); got != nil {
		t.Errorf("cacheReportFrom(nil) = %+v, want nil", got)
	}
}

// TestCacheReportFrom pins the cacheindex.Index -> status.CacheReport
// projection (citadel #682 P3, design doc §9.4): per-dir aggregation,
// indexed-vs-measured unindexed-remainder math, size-descending model rows,
// and the legacy-cache passthrough.
func TestCacheReportFrom(t *testing.T) {
	dir := t.TempDir()
	s := cacheindex.Open(filepath.Join(dir, cacheindex.FileName), nil)
	fixedNow := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)

	if err := s.Upsert(cacheindex.Entry{
		CacheDir:  services.HFHubCacheDirName,
		Family:    services.CacheFamilyHFHub,
		Model:     "org/small",
		Engine:    "vllm",
		SizeBytes: 100,
		PulledAt:  fixedNow.Add(-48 * time.Hour),
		LastUsed:  fixedNow.Add(-1 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Upsert(cacheindex.Entry{
		CacheDir:  services.HFHubCacheDirName,
		Family:    services.CacheFamilyHFHub,
		Model:     "org/big",
		Engine:    "vllm",
		SizeBytes: 900,
		PulledAt:  fixedNow.Add(-72 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	idx := s.Snapshot()
	got := cacheReportFrom(idx)
	if got == nil {
		t.Fatalf("cacheReportFrom returned nil for a non-nil index")
	}
	if got.TotalIndexedBytes != 1000 {
		t.Errorf("TotalIndexedBytes = %d, want 1000", got.TotalIndexedBytes)
	}
	if len(got.Dirs) != 1 {
		t.Fatalf("Dirs = %+v, want exactly one dir report", got.Dirs)
	}
	dr := got.Dirs[0]
	if dr.Dir != services.HFHubCacheDirName || dr.Family != string(services.CacheFamilyHFHub) {
		t.Errorf("unexpected dir report identity: %+v", dr)
	}
	if dr.IndexedBytes != 1000 || dr.EntryCount != 2 {
		t.Errorf("IndexedBytes/EntryCount = %d/%d, want 1000/2", dr.IndexedBytes, dr.EntryCount)
	}
	// No scan metadata was ever recorded (no ReconcileScan ran), so
	// Measured/Unindexed must stay zero -- absence, not a fabricated zero.
	if dr.MeasuredBytes != 0 || dr.UnindexedBytes != 0 {
		t.Errorf("MeasuredBytes/UnindexedBytes = %d/%d, want 0/0 with no scan metadata", dr.MeasuredBytes, dr.UnindexedBytes)
	}
	if len(dr.Models) != 2 || dr.Models[0].Model != "org/big" || dr.Models[1].Model != "org/small" {
		t.Fatalf("Models = %+v, want size-descending [org/big, org/small]", dr.Models)
	}
	if got.LegacyHFCache != nil {
		t.Errorf("LegacyHFCache = %+v, want nil (none recorded)", got.LegacyHFCache)
	}
}

// TestCacheReportFromCapsModelRows pins maxCacheHeartbeatModelRows (citadel
// #682 P3, design doc §9.4): a dir with more entries than the cap still
// reports the true EntryCount, but truncates Models.
func TestCacheReportFromCapsModelRows(t *testing.T) {
	dir := t.TempDir()
	s := cacheindex.Open(filepath.Join(dir, cacheindex.FileName), nil)
	for i := 0; i < maxCacheHeartbeatModelRows+5; i++ {
		if err := s.Upsert(cacheindex.Entry{
			CacheDir:  services.LlamaCppCacheDirName,
			Family:    services.CacheFamilyGGUFDir,
			Model:     filepath.Join("model", string(rune('a'+i))+".gguf"),
			SizeBytes: int64(i + 1),
		}); err != nil {
			t.Fatal(err)
		}
	}
	got := cacheReportFrom(s.Snapshot())
	if len(got.Dirs) != 1 {
		t.Fatalf("Dirs = %+v, want exactly one dir report", got.Dirs)
	}
	dr := got.Dirs[0]
	if dr.EntryCount != maxCacheHeartbeatModelRows+5 {
		t.Errorf("EntryCount = %d, want %d (the true count, uncapped)", dr.EntryCount, maxCacheHeartbeatModelRows+5)
	}
	if len(dr.Models) != maxCacheHeartbeatModelRows {
		t.Errorf("len(Models) = %d, want the capped %d", len(dr.Models), maxCacheHeartbeatModelRows)
	}
}

// TestCacheGCReportFromNil pins cacheGCReportFrom's defensive nil handling
// (citadel #682 P5, design doc §10.4) -- a disabled/never-constructed GC
// reconciler's stats (Enabled==false, the zero value) must project to a nil
// *status.CacheGCReport, so CacheReport.Gc stays omitted exactly like a
// pre-P5 node.
func TestCacheGCReportFromNil(t *testing.T) {
	if got := cacheGCReportFrom(jobs.CacheGCStats{}); got != nil {
		t.Errorf("cacheGCReportFrom(zero value) = %+v, want nil", got)
	}
}

// TestCacheGCReportFrom pins the jobs.CacheGCStats -> status.CacheGCReport
// field-by-field projection.
func TestCacheGCReportFrom(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	stats := jobs.CacheGCStats{
		Enabled:               true,
		LastRunAt:             now,
		LastRunReclaimedBytes: 500,
		TotalReclaimedBytes:   1500,
		LastSkipReason:        "no_candidates",
	}
	got := cacheGCReportFrom(stats)
	if got == nil {
		t.Fatalf("cacheGCReportFrom returned nil for Enabled=true stats")
	}
	if !got.Enabled || !got.LastRunAt.Equal(now) || got.LastRunReclaimedBytes != 500 ||
		got.TotalReclaimedBytes != 1500 || got.LastSkipReason != "no_candidates" {
		t.Errorf("unexpected projection: %+v", got)
	}
}
