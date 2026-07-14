package heartbeat

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/redisapi"
	"github.com/aceteam-ai/citadel-cli/internal/status"
)

// TestAPIPublisherWiresAgentVersion verifies the citadel-cli binary version
// configured on APIPublisherConfig is carried through to the publisher, and
// thus into every heartbeat StatusMessage. This is the wiring that was missing
// (aceteam#5819): the heartbeat path never carried the agent version, so a
// current node showed "unknown" in the fleet version view.
func TestAPIPublisherWiresAgentVersion(t *testing.T) {
	collector := status.NewCollector(status.CollectorConfig{NodeName: "test"})

	pub, err := NewAPIPublisher(APIPublisherConfig{
		Client:       &redisapi.Client{},
		NodeID:       "ubuntu-gpu",
		OrgID:        "org-123",
		AgentVersion: "v2.75.0",
	}, collector)
	if err != nil {
		t.Fatalf("NewAPIPublisher failed: %v", err)
	}

	if pub.agentVersion != "v2.75.0" {
		t.Errorf("agentVersion = %q, want %q", pub.agentVersion, "v2.75.0")
	}
}

// TestStatusMessageAgentVersionIncludedWhenSet verifies agentVersion is emitted
// in the heartbeat JSON when set, so the backend can persist it.
func TestStatusMessageAgentVersionIncludedWhenSet(t *testing.T) {
	msg := StatusMessage{
		Version:      "1.0",
		Timestamp:    "2024-01-15T12:00:00Z",
		NodeID:       "ubuntu-gpu",
		AgentVersion: "v2.75.0",
		Status:       &status.NodeStatus{Version: "1.0"},
	}

	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("Failed to marshal StatusMessage: %v", err)
	}

	if !strings.Contains(string(data), `"agentVersion":"v2.75.0"`) {
		t.Errorf("JSON should include agentVersion when set, got: %s", string(data))
	}
}

// TestStatusMessageAgentVersionOmittedWhenEmpty verifies agentVersion is
// omitted from the heartbeat JSON when empty (omitempty). An older citadel that
// does not wire the version must NOT send an empty string, so the backend can
// leave a previously-recorded version untouched ("unknown never clobbers
// known-good").
func TestStatusMessageAgentVersionOmittedWhenEmpty(t *testing.T) {
	msg := StatusMessage{
		Version:   "1.0",
		Timestamp: "2024-01-15T12:00:00Z",
		NodeID:    "ubuntu-gpu",
		Status:    &status.NodeStatus{Version: "1.0"},
	}

	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("Failed to marshal StatusMessage: %v", err)
	}

	if strings.Contains(string(data), "agentVersion") {
		t.Errorf("JSON should omit agentVersion when empty, got: %s", string(data))
	}
}
