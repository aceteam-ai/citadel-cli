package nodestate

import (
	"context"
	"errors"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
	"github.com/aceteam-ai/citadel-cli/internal/protocol"
	fabricpb "github.com/aceteam-ai/fabric-protocol/gen/go/aceteam/fabric/v1"
	"google.golang.org/protobuf/proto"
)

// fakeInspector returns a per-module observation or error, keyed by module name.
type fakeInspector struct {
	obs  map[string]Observation
	errs map[string]error
}

func (f fakeInspector) Inspect(_ context.Context, name string) (Observation, error) {
	if f.errs != nil {
		if err, ok := f.errs[name]; ok {
			return Observation{}, err
		}
	}
	return f.obs[name], nil
}

// stubLockfile injects a fixture lockfile via the package-level loader seam,
// restoring the production loader when the test ends.
func stubLockfile(t *testing.T, entries []catalog.LockEntry) {
	t.Helper()
	orig := loadLockfile
	loadLockfile = func() (*catalog.Lockfile, error) {
		return &catalog.Lockfile{Version: 1, Modules: entries}, nil
	}
	t.Cleanup(func() { loadLockfile = orig })
}

func TestBuildActualState_Envelope(t *testing.T) {
	stubLockfile(t, nil)

	state := BuildActualState(context.Background(), nil, "node-hostname", "v9.9.9")

	if got := state.GetProtocolVersion(); got != uint32(protocol.FabricProtocolVersion) {
		t.Errorf("protocol_version = %d, want %d", got, protocol.FabricProtocolVersion)
	}
	if state.GetNodeId() != "node-hostname" {
		t.Errorf("node_id = %q, want node-hostname", state.GetNodeId())
	}
	if state.GetAgentVersion() != "v9.9.9" {
		t.Errorf("agent_version = %q, want v9.9.9", state.GetAgentVersion())
	}
	if state.GetAppliedRevision() != "" {
		t.Errorf("applied_revision = %q, want empty (no desired-state in v1)", state.GetAppliedRevision())
	}
	if state.GetReportedAt() == nil {
		t.Error("reported_at must be set")
	}
	if len(state.GetModules()) != 0 {
		t.Errorf("expected 0 modules for empty lockfile, got %d", len(state.GetModules()))
	}
}

func TestBuildActualState_ModuleMapping(t *testing.T) {
	stubLockfile(t, []catalog.LockEntry{
		{
			Name:        "embedding",
			Source:      "owner/repo@^1.2",
			Ref:         "^1.2",
			ResolvedRef: "v1.4.0",
			Commit:      "abc123",
			Images:      []catalog.LockImage{{Ref: "img:1", Digest: "sha256:deadbeef"}},
		},
	})

	insp := fakeInspector{obs: map[string]Observation{
		"embedding": {
			Status: fabricpb.ModuleStatus_MODULE_STATUS_RUNNING,
			Health: fabricpb.ModuleHealth_MODULE_HEALTH_HEALTHY,
		},
	}}

	state := BuildActualState(context.Background(), insp, "node", "v1")
	if len(state.GetModules()) != 1 {
		t.Fatalf("expected 1 module, got %d", len(state.GetModules()))
	}
	m := state.GetModules()[0]
	if m.GetSource() != "owner/repo@^1.2" {
		t.Errorf("source = %q", m.GetSource())
	}
	// installed_version prefers ResolvedRef.
	if m.GetInstalledVersion() != "v1.4.0" {
		t.Errorf("installed_version = %q, want v1.4.0", m.GetInstalledVersion())
	}
	if m.GetImageDigest() != "sha256:deadbeef" {
		t.Errorf("image_digest = %q", m.GetImageDigest())
	}
	if m.GetConfigRef() != "" {
		t.Errorf("config_ref = %q, want empty in v1", m.GetConfigRef())
	}
	if m.GetStatus() != fabricpb.ModuleStatus_MODULE_STATUS_RUNNING {
		t.Errorf("status = %v", m.GetStatus())
	}
	if m.GetHealth() != fabricpb.ModuleHealth_MODULE_HEALTH_HEALTHY {
		t.Errorf("health = %v", m.GetHealth())
	}
	if m.GetError() != "" {
		t.Errorf("error = %q, want empty", m.GetError())
	}
}

func TestInstalledVersion_Fallbacks(t *testing.T) {
	cases := []struct {
		name string
		e    catalog.LockEntry
		want string
	}{
		{"resolved-ref wins", catalog.LockEntry{ResolvedRef: "v2", Ref: "^2", Commit: "c"}, "v2"},
		{"ref when no resolved", catalog.LockEntry{Ref: "^2", Commit: "c"}, "^2"},
		{"commit when no ref", catalog.LockEntry{Commit: "c"}, "c"},
		{"empty when nothing", catalog.LockEntry{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := installedVersion(tc.e); got != tc.want {
				t.Errorf("installedVersion = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFirstDigest(t *testing.T) {
	if got := firstDigest(nil); got != "" {
		t.Errorf("nil images: got %q", got)
	}
	if got := firstDigest([]catalog.LockImage{{Digest: ""}, {Digest: "sha256:x"}}); got != "sha256:x" {
		t.Errorf("skip-empty: got %q, want sha256:x", got)
	}
}

// TestBuildActualState_PerModuleErrorIsolation is the core isolation test: one
// module whose inspection fails must report MODULE_HEALTH_ERROR with the error,
// while every other module is reported normally — the bad module never aborts
// the report.
func TestBuildActualState_PerModuleErrorIsolation(t *testing.T) {
	stubLockfile(t, []catalog.LockEntry{
		{Name: "good-a", Source: "a"},
		{Name: "bad", Source: "b"},
		{Name: "good-c", Source: "c"},
	})

	insp := fakeInspector{
		obs: map[string]Observation{
			"good-a": {Status: fabricpb.ModuleStatus_MODULE_STATUS_RUNNING, Health: fabricpb.ModuleHealth_MODULE_HEALTH_HEALTHY},
			"good-c": {Status: fabricpb.ModuleStatus_MODULE_STATUS_STOPPED, Health: fabricpb.ModuleHealth_MODULE_HEALTH_UNHEALTHY},
		},
		errs: map[string]error{"bad": errors.New("docker daemon unreachable")},
	}

	state := BuildActualState(context.Background(), insp, "node", "v1")
	if len(state.GetModules()) != 3 {
		t.Fatalf("expected 3 modules (none dropped), got %d", len(state.GetModules()))
	}

	byName := map[string]*fabricpb.ActualModule{}
	for _, m := range state.GetModules() {
		byName[m.GetSource()] = m
	}

	if byName["a"].GetHealth() != fabricpb.ModuleHealth_MODULE_HEALTH_HEALTHY {
		t.Errorf("good-a health = %v, want HEALTHY", byName["a"].GetHealth())
	}
	if byName["c"].GetHealth() != fabricpb.ModuleHealth_MODULE_HEALTH_UNHEALTHY {
		t.Errorf("good-c health = %v, want UNHEALTHY", byName["c"].GetHealth())
	}

	bad := byName["b"]
	if bad.GetHealth() != fabricpb.ModuleHealth_MODULE_HEALTH_ERROR {
		t.Errorf("bad health = %v, want ERROR", bad.GetHealth())
	}
	if bad.GetStatus() != fabricpb.ModuleStatus_MODULE_STATUS_UNSPECIFIED {
		t.Errorf("bad status = %v, want UNSPECIFIED", bad.GetStatus())
	}
	if bad.GetError() != "docker daemon unreachable" {
		t.Errorf("bad error = %q", bad.GetError())
	}
}

// TestBuildActualState_RoundTrip proves the report serializes and survives
// proto.Unmarshal back to an equal message — the wire contract holds.
func TestBuildActualState_RoundTrip(t *testing.T) {
	stubLockfile(t, []catalog.LockEntry{
		{Name: "m1", Source: "s1", ResolvedRef: "v1", Images: []catalog.LockImage{{Digest: "sha256:aa"}}},
		{Name: "m2", Source: "s2", Ref: "^2"},
	})
	insp := fakeInspector{obs: map[string]Observation{
		"m1": {Status: fabricpb.ModuleStatus_MODULE_STATUS_RUNNING, Health: fabricpb.ModuleHealth_MODULE_HEALTH_HEALTHY},
		"m2": {Status: fabricpb.ModuleStatus_MODULE_STATUS_STOPPED, Health: fabricpb.ModuleHealth_MODULE_HEALTH_UNHEALTHY},
	}}

	orig := BuildActualState(context.Background(), insp, "node-x", "v3")
	wire, err := proto.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got fabricpb.ActualState
	if err := proto.Unmarshal(wire, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !proto.Equal(orig, &got) {
		t.Errorf("round-trip mismatch:\norig = %v\ngot  = %v", orig, &got)
	}
}
