package nodestate

import (
	"context"
	"sync"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
	"github.com/aceteam-ai/citadel-cli/internal/config"
	fabricpb "github.com/aceteam-ai/citadel-cli/internal/fabricpb"
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
