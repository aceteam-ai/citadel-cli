package heartbeat

import (
	"encoding/json"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/pulse"
	"github.com/aceteam-ai/citadel-cli/internal/status"
)

// TestStatusMessageWithoutStats proves the heartbeat wire format is unchanged
// when no stats provider is wired (legacy nodes) or the collector has nothing
// (provider returned nil): the "stats" key must be entirely absent, not null.
func TestStatusMessageWithoutStats(t *testing.T) {
	msg := StatusMessage{
		Version:   "1.0",
		Timestamp: "2026-07-22T00:00:00Z",
		NodeID:    "test-node",
		Status:    &status.NodeStatus{Version: "1.0"},
	}

	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := decoded["stats"]; ok {
		t.Errorf("heartbeat without a stats block must not carry a stats key: %s", data)
	}
}

// TestStatusMessageWithStats proves the stats block rides the heartbeat under
// the exact "stats" key with the contract's inner shape.
func TestStatusMessageWithStats(t *testing.T) {
	msg := StatusMessage{
		Version:   "1.0",
		Timestamp: "2026-07-22T00:00:00Z",
		NodeID:    "test-node",
		Status:    &status.NodeStatus{Version: "1.0"},
		Stats: &pulse.StatsBlock{
			V:    pulse.StatsVersion,
			TS:   1753142400,
			GPUs: []pulse.GPUStat{{Index: 0}},
		},
	}

	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	statsRaw, ok := decoded["stats"]
	if !ok {
		t.Fatalf("stats key missing: %s", data)
	}
	if want := `{"v":1,"ts":1753142400,"gpus":[{"i":0}]}`; string(statsRaw) != want {
		t.Errorf("stats shape: got %s, want %s", statsRaw, want)
	}
}

// TestPublisherStatsProviderNeverBlocksNilProvider exercises the publisher
// seam: an unset or nil-returning provider leaves msg.Stats nil, and a
// provider is a pure cache read (pulse.Collector.Latest) so the heartbeat
// path never triggers collection.
func TestPublisherStatsProvider(t *testing.T) {
	p, err := NewRedisPublisher(RedisPublisherConfig{
		RedisURL: "redis://localhost:6379",
		NodeID:   "test-node",
	}, nil)
	if err != nil {
		t.Fatalf("NewRedisPublisher: %v", err)
	}

	if p.statsFn != nil {
		t.Error("statsFn must default to nil (no stats field on heartbeats)")
	}

	c := pulse.NewCollector(pulse.CollectorConfig{Targets: []pulse.EngineTarget{}})
	p.SetStatsProvider(c.Latest)
	if got := p.statsFn(); got != nil {
		t.Errorf("Latest() before any collection must be nil, got %+v", got)
	}
}
