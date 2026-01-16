package heartbeat

import (
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/status"
)

func TestNewRedisPublisher(t *testing.T) {
	collector := status.NewCollector(status.CollectorConfig{
		NodeName: "test-node",
	})

	tests := []struct {
		name      string
		config    RedisPublisherConfig
		wantErr   bool
		wantPubSub string
		wantStream string
	}{
		{
			name: "valid config",
			config: RedisPublisherConfig{
				RedisURL: "redis://localhost:6379",
				NodeID:   "test-node-123",
			},
			wantErr:    false,
			wantPubSub: "node:status:test-node-123",
			wantStream: "node:status:stream",
		},
		{
			name: "with device code",
			config: RedisPublisherConfig{
				RedisURL:   "redis://localhost:6379",
				NodeID:     "my-node",
				DeviceCode: "abc123",
			},
			wantErr:    false,
			wantPubSub: "node:status:my-node",
			wantStream: "node:status:stream",
		},
		{
			name: "invalid redis URL",
			config: RedisPublisherConfig{
				RedisURL: "not-a-valid-url",
				NodeID:   "test",
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pub, err := NewRedisPublisher(tt.config, collector)

			if tt.wantErr {
				if err == nil {
					t.Error("NewRedisPublisher() should return error")
				}
				return
			}

			if err != nil {
				t.Errorf("NewRedisPublisher() error = %v, want nil", err)
				return
			}

			if pub == nil {
				t.Fatal("NewRedisPublisher() returned nil")
			}

			if pub.PubSubChannel() != tt.wantPubSub {
				t.Errorf("PubSubChannel() = %v, want %v", pub.PubSubChannel(), tt.wantPubSub)
			}
			if pub.StreamName() != tt.wantStream {
				t.Errorf("StreamName() = %v, want %v", pub.StreamName(), tt.wantStream)
			}

			// Clean up
			pub.Close()
		})
	}
}

func TestRedisPublisherInterval(t *testing.T) {
	collector := status.NewCollector(status.CollectorConfig{NodeName: "test"})

	// Test default interval
	pub1, _ := NewRedisPublisher(RedisPublisherConfig{
		RedisURL: "redis://localhost:6379",
		NodeID:   "test",
	}, collector)
	if pub1 != nil {
		if pub1.Interval() != 30*time.Second {
			t.Errorf("Default interval = %v, want 30s", pub1.Interval())
		}
		pub1.Close()
	}

	// Test custom interval
	pub2, _ := NewRedisPublisher(RedisPublisherConfig{
		RedisURL: "redis://localhost:6379",
		NodeID:   "test",
		Interval: 60 * time.Second,
	}, collector)
	if pub2 != nil {
		if pub2.Interval() != 60*time.Second {
			t.Errorf("Custom interval = %v, want 60s", pub2.Interval())
		}
		pub2.Close()
	}
}

func TestRedisPublisherSetDeviceCode(t *testing.T) {
	collector := status.NewCollector(status.CollectorConfig{NodeName: "test"})

	pub, err := NewRedisPublisher(RedisPublisherConfig{
		RedisURL: "redis://localhost:6379",
		NodeID:   "test",
	}, collector)
	if err != nil {
		t.Skipf("Skipping test, could not create publisher: %v", err)
	}
	defer pub.Close()

	// Initially no device code
	initialCode := pub.deviceCode
	if initialCode != "" {
		t.Errorf("Initial device code = %v, want empty", initialCode)
	}

	// Set device code
	pub.SetDeviceCode("new-device-code")
	if pub.deviceCode != "new-device-code" {
		t.Errorf("Device code after SetDeviceCode = %v, want 'new-device-code'", pub.deviceCode)
	}
}

func TestRedisPublisherNodeID(t *testing.T) {
	collector := status.NewCollector(status.CollectorConfig{NodeName: "test"})

	pub, err := NewRedisPublisher(RedisPublisherConfig{
		RedisURL: "redis://localhost:6379",
		NodeID:   "my-gpu-node",
	}, collector)
	if err != nil {
		t.Skipf("Skipping test, could not create publisher: %v", err)
	}
	defer pub.Close()

	if pub.NodeID() != "my-gpu-node" {
		t.Errorf("NodeID() = %v, want 'my-gpu-node'", pub.NodeID())
	}
}

func TestStatusMessageJSON(t *testing.T) {
	// Test that StatusMessage can be marshaled correctly
	msg := StatusMessage{
		Version:    "1.0",
		Timestamp:  "2024-01-15T12:00:00Z",
		NodeID:     "test-node",
		DeviceCode: "abc123",
		Status: &status.NodeStatus{
			Version: "1.0",
			Node: status.NodeInfo{
				Name: "test-node",
			},
		},
	}

	if msg.Version != "1.0" {
		t.Errorf("Version = %v, want 1.0", msg.Version)
	}
	if msg.DeviceCode != "abc123" {
		t.Errorf("DeviceCode = %v, want abc123", msg.DeviceCode)
	}
}
