package chat

import (
	"encoding/json"
	"testing"
	"time"
)

func TestChannelName(t *testing.T) {
	tests := []struct {
		orgID   string
		channel string
		want    string
	}{
		{"abc-123", "general", "chat:v1:org_abc-123:general"},
		{"org-uuid", "random", "chat:v1:org_org-uuid:random"},
	}
	for _, tt := range tests {
		got := ChannelName(tt.orgID, tt.channel)
		if got != tt.want {
			t.Errorf("ChannelName(%q, %q) = %q, want %q", tt.orgID, tt.channel, got, tt.want)
		}
	}
}

func TestPresenceChannel(t *testing.T) {
	got := PresenceChannel("org-uuid")
	want := "chat:v1:org_org-uuid:presence"
	if got != want {
		t.Errorf("PresenceChannel = %q, want %q", got, want)
	}
}

func TestStreamName(t *testing.T) {
	got := StreamName("org-uuid", "general")
	want := "chat:v1:org_org-uuid:general:stream"
	if got != want {
		t.Errorf("StreamName = %q, want %q", got, want)
	}
}

func TestMessageMarshalRoundTrip(t *testing.T) {
	ts := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	original := Message{
		FromNodeID:   "node-abc",
		FromNodeName: "gpu-workstation",
		Channel:      "general",
		Body:         "hello world",
		Timestamp:    ts,
	}

	data, err := MarshalMessage(original)
	if err != nil {
		t.Fatalf("MarshalMessage: %v", err)
	}

	got, err := UnmarshalMessage(data)
	if err != nil {
		t.Fatalf("UnmarshalMessage: %v", err)
	}

	if got.FromNodeID != original.FromNodeID {
		t.Errorf("FromNodeID = %q, want %q", got.FromNodeID, original.FromNodeID)
	}
	if got.FromNodeName != original.FromNodeName {
		t.Errorf("FromNodeName = %q, want %q", got.FromNodeName, original.FromNodeName)
	}
	if got.Channel != original.Channel {
		t.Errorf("Channel = %q, want %q", got.Channel, original.Channel)
	}
	if got.Body != original.Body {
		t.Errorf("Body = %q, want %q", got.Body, original.Body)
	}
	if !got.Timestamp.Equal(original.Timestamp) {
		t.Errorf("Timestamp = %v, want %v", got.Timestamp, original.Timestamp)
	}
}

func TestUnmarshalMessageFromMap(t *testing.T) {
	m := map[string]any{
		"from_node_id":   "node-abc",
		"from_node_name": "gpu-workstation",
		"channel":        "general",
		"body":           "test message",
		"ts":             "2026-06-20T12:00:00Z",
	}

	msg, err := UnmarshalMessageFromMap(m)
	if err != nil {
		t.Fatalf("UnmarshalMessageFromMap: %v", err)
	}

	if msg.FromNodeID != "node-abc" {
		t.Errorf("FromNodeID = %q, want %q", msg.FromNodeID, "node-abc")
	}
	if msg.Body != "test message" {
		t.Errorf("Body = %q, want %q", msg.Body, "test message")
	}
}

func TestUnmarshalPresenceFromMap(t *testing.T) {
	m := map[string]any{
		"node_id":   "node-abc",
		"node_name": "gpu-workstation",
		"last_seen": "2026-06-20T12:00:00Z",
	}

	p, err := UnmarshalPresenceFromMap(m)
	if err != nil {
		t.Fatalf("UnmarshalPresenceFromMap: %v", err)
	}

	if p.NodeID != "node-abc" {
		t.Errorf("NodeID = %q, want %q", p.NodeID, "node-abc")
	}
	if p.NodeName != "gpu-workstation" {
		t.Errorf("NodeName = %q, want %q", p.NodeName, "gpu-workstation")
	}
}

func TestPresenceInfo_IsOnline(t *testing.T) {
	recent := PresenceInfo{LastSeen: time.Now().Add(-10 * time.Second)}
	if !recent.IsOnline(60 * time.Second) {
		t.Error("recent presence should be online")
	}

	stale := PresenceInfo{LastSeen: time.Now().Add(-5 * time.Minute)}
	if stale.IsOnline(60 * time.Second) {
		t.Error("stale presence should be offline")
	}
}

func TestMessageJSONFieldNames(t *testing.T) {
	msg := Message{
		FromNodeID:   "id",
		FromNodeName: "name",
		Channel:      "ch",
		Body:         "hi",
		Timestamp:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}

	data, _ := json.Marshal(msg)
	var raw map[string]any
	_ = json.Unmarshal(data, &raw)

	// Verify JSON field names match protocol spec
	expectedKeys := []string{"from_node_id", "from_node_name", "channel", "body", "ts"}
	for _, key := range expectedKeys {
		if _, ok := raw[key]; !ok {
			t.Errorf("expected JSON key %q not found in serialized message", key)
		}
	}
}
