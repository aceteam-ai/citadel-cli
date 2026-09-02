package nodestate

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/whatsapp"
	fabricpb "github.com/aceteam-ai/fabric-protocol/gen/go/aceteam/fabric/v1"
	"google.golang.org/protobuf/proto"
)

// capturePoster records every posted body.
type capturePoster struct {
	mu     sync.Mutex
	bodies [][]byte
	err    error
}

func (p *capturePoster) PostNodeState(_ context.Context, body []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	cp := make([]byte, len(body))
	copy(cp, body)
	p.bodies = append(p.bodies, cp)
	return p.err
}

func (p *capturePoster) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.bodies)
}

func TestNew_NilWhenUnconfigured(t *testing.T) {
	if New(Config{NodeID: "n"}) != nil {
		t.Error("New with nil Poster should return nil")
	}
	if New(Config{Poster: &capturePoster{}}) != nil {
		t.Error("New with empty NodeID should return nil")
	}
}

// TestReportOnce_PostsRoundTrippableProto asserts the emitter serializes the
// report and posts a body that survives proto.Unmarshal with the expected
// envelope — the same shape the control plane will decode off the wire.
func TestReportOnce_PostsRoundTrippableProto(t *testing.T) {
	stubLockfile(t, []catalog.LockEntry{{Name: "m1", Source: "s1"}})
	p := &capturePoster{}
	e := New(Config{
		Poster:    p,
		Inspector: fakeInspector{obs: map[string]Observation{"m1": {Status: fabricpb.ModuleStatus_MODULE_STATUS_RUNNING, Health: fabricpb.ModuleHealth_MODULE_HEALTH_HEALTHY}}},
		ConfigDir: t.TempDir(), // no telemetry.yaml => default enabled
		NodeID:    "host-1",
		Version:   "v7",
	})

	e.reportOnce(context.Background())

	if p.count() != 1 {
		t.Fatalf("expected 1 post, got %d", p.count())
	}
	var got fabricpb.ActualState
	if err := proto.Unmarshal(p.bodies[0], &got); err != nil {
		t.Fatalf("posted body does not unmarshal: %v", err)
	}
	if got.GetNodeId() != "host-1" || got.GetAgentVersion() != "v7" {
		t.Errorf("envelope mismatch: node=%q version=%q", got.GetNodeId(), got.GetAgentVersion())
	}
	if len(got.GetModules()) != 1 || got.GetModules()[0].GetSource() != "s1" {
		t.Errorf("modules mismatch: %v", got.GetModules())
	}
}

// TestReportOnce_RespectsOptOut asserts that the shared anon_telemetry_enabled
// opt-out flag gates node-state reporting too: when disabled, nothing is posted.
func TestReportOnce_RespectsOptOut(t *testing.T) {
	stubLockfile(t, []catalog.LockEntry{{Name: "m1", Source: "s1"}})
	dir := t.TempDir()
	if err := config.SaveTelemetry(dir, &config.Telemetry{AnonTelemetryEnabled: false}); err != nil {
		t.Fatalf("save telemetry: %v", err)
	}
	p := &capturePoster{}
	e := New(Config{Poster: p, ConfigDir: dir, NodeID: "host-1"})

	e.reportOnce(context.Background())

	if p.count() != 0 {
		t.Errorf("opt-out: expected 0 posts, got %d", p.count())
	}
}

// TestRun_NilReceiverNoop guards the unconditional-wiring contract.
func TestRun_NilReceiverNoop(t *testing.T) {
	var e *Emitter
	e.Run(context.Background()) // must not panic
}

// fakeBridgeProvider is a settable BridgeEndpointsProvider for tests.
type fakeBridgeProvider struct {
	module *fabricpb.ActualModule
	calls  int
}

func (f *fakeBridgeProvider) BridgeModule(context.Context) *fabricpb.ActualModule {
	f.calls++
	return f.module
}

func bridgeModuleFixture(secretFingerprint string) *fabricpb.ActualModule {
	return &fabricpb.ActualModule{
		Source: "whatsapp-bridge",
		Status: fabricpb.ModuleStatus_MODULE_STATUS_RUNNING,
		Health: fabricpb.ModuleHealth_MODULE_HEALTH_HEALTHY,
		Endpoints: []*fabricpb.ModuleEndpoint{
			{
				Name:                "bridge",
				Kind:                "rest",
				Scheme:              "https",
				Port:                8443,
				Path:                "/modules/whatsapp",
				Health:              fabricpb.ModuleHealth_MODULE_HEALTH_HEALTHY,
				HealthPath:          "/health",
				AdminKeyFingerprint: secretFingerprint,
			},
		},
	}
}

// TestReportOnce_AppendsBridgeModule: with telemetry ON, the bridge provider's
// row is appended alongside the lockfile-driven modules.
func TestReportOnce_AppendsBridgeModule(t *testing.T) {
	stubLockfile(t, []catalog.LockEntry{{Name: "m1", Source: "s1"}})
	p := &capturePoster{}
	bridge := &fakeBridgeProvider{module: bridgeModuleFixture("sha256:abcd")}
	e := New(Config{
		Poster:          p,
		ConfigDir:       t.TempDir(),
		NodeID:          "host-1",
		Version:         "v7",
		BridgeEndpoints: bridge,
	})

	e.reportOnce(context.Background())

	if bridge.calls != 1 {
		t.Fatalf("bridge provider called %d times, want 1", bridge.calls)
	}
	var got fabricpb.ActualState
	if err := proto.Unmarshal(p.bodies[0], &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.GetModules()) != 2 {
		t.Fatalf("want 2 modules (lockfile + bridge), got %d", len(got.GetModules()))
	}
	var sawBridge bool
	for _, m := range got.GetModules() {
		if m.GetSource() == "whatsapp-bridge" {
			sawBridge = true
			if len(m.GetEndpoints()) != 1 {
				t.Errorf("bridge module endpoints = %d, want 1", len(m.GetEndpoints()))
			}
		}
	}
	if !sawBridge {
		t.Fatalf("bridge module row missing from report")
	}
}

// TestReportOnce_BridgeExemptFromTelemetryOptOut is the required "telemetry
// gate coupling" fix (citadel#624 design review, point 2): with
// anon_telemetry_enabled=false, the lockfile-driven modules must NOT be
// reported, but the bridge's operational endpoint facts still ARE.
func TestReportOnce_BridgeExemptFromTelemetryOptOut(t *testing.T) {
	stubLockfile(t, []catalog.LockEntry{{Name: "m1", Source: "s1"}})
	dir := t.TempDir()
	if err := config.SaveTelemetry(dir, &config.Telemetry{AnonTelemetryEnabled: false}); err != nil {
		t.Fatalf("save telemetry: %v", err)
	}
	p := &capturePoster{}
	bridge := &fakeBridgeProvider{module: bridgeModuleFixture("sha256:abcd")}
	e := New(Config{Poster: p, ConfigDir: dir, NodeID: "host-1", BridgeEndpoints: bridge})

	e.reportOnce(context.Background())

	if p.count() != 1 {
		t.Fatalf("telemetry off but bridge deployed: want 1 post (bridge is operational, exempt), got %d", p.count())
	}
	var got fabricpb.ActualState
	if err := proto.Unmarshal(p.bodies[0], &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.GetModules()) != 1 || got.GetModules()[0].GetSource() != "whatsapp-bridge" {
		t.Fatalf("want exactly the bridge module (lockfile modules must stay opted out), got %v", got.GetModules())
	}
}

// TestReportOnce_OptOutNoOpWhenNoBridge: the ORIGINAL opt-out behavior (no
// report at all) is preserved when there is no bridge to exempt-report either.
func TestReportOnce_OptOutNoOpWhenNoBridge(t *testing.T) {
	stubLockfile(t, []catalog.LockEntry{{Name: "m1", Source: "s1"}})
	dir := t.TempDir()
	if err := config.SaveTelemetry(dir, &config.Telemetry{AnonTelemetryEnabled: false}); err != nil {
		t.Fatalf("save telemetry: %v", err)
	}
	p := &capturePoster{}
	e := New(Config{Poster: p, ConfigDir: dir, NodeID: "host-1"}) // no BridgeEndpoints

	e.reportOnce(context.Background())

	if p.count() != 0 {
		t.Errorf("opt-out with no bridge: expected 0 posts, got %d", p.count())
	}
}

// TestReportOnce_MarshaledStateNeverContainsSecretBytes is the required
// structural-safety pin at the ActualState level (citadel#624 design review):
// a marshaled report for a module whose env holds a secret never contains
// those secret bytes -- only the one-way fingerprint whatsapp.AdminKeyFingerprint
// derives from it. The fixture computes the fingerprint via the REAL
// production function (not a hand-picked stand-in), so this fails if that
// function is ever changed to embed more of the key than its digest.
func TestReportOnce_MarshaledStateNeverContainsSecretBytes(t *testing.T) {
	stubLockfile(t, nil)
	const secretAdminKey = "wab_admin_TOTALLY-SECRET-DO-NOT-LEAK-9f8e7d6c5b4a"
	fp := whatsapp.AdminKeyFingerprint(secretAdminKey)
	p := &capturePoster{}
	bridge := &fakeBridgeProvider{module: bridgeModuleFixture(fp)}
	e := New(Config{Poster: p, ConfigDir: t.TempDir(), NodeID: "host-1", BridgeEndpoints: bridge})

	e.reportOnce(context.Background())

	if p.count() != 1 {
		t.Fatalf("want 1 post, got %d", p.count())
	}
	if containsBytes(p.bodies[0], secretAdminKey) {
		t.Fatalf("marshaled ActualState contains raw secret bytes")
	}
	if !containsBytes(p.bodies[0], strings.TrimPrefix(fp, "sha256:")) {
		t.Fatalf("expected the derived fingerprint to be present on the wire")
	}
}

func containsBytes(haystack []byte, needle string) bool {
	return len(needle) > 0 && strings.Contains(string(haystack), needle)
}
