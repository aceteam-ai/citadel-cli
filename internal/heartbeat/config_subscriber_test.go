package heartbeat

import (
	"encoding/json"
	"testing"
	"time"
)

func TestNewConfigSubscriber(t *testing.T) {
	tests := []struct {
		name    string
		config  ConfigSubscriberConfig
		wantErr bool
	}{
		{
			name: "valid config",
			config: ConfigSubscriberConfig{
				RedisURL: "redis://localhost:6379",
				NodeID:   "test-node",
			},
			wantErr: false,
		},
		{
			name: "with password",
			config: ConfigSubscriberConfig{
				RedisURL:      "redis://localhost:6379",
				RedisPassword: "secret",
				NodeID:        "test-node",
			},
			wantErr: false,
		},
		{
			name: "with custom config dir",
			config: ConfigSubscriberConfig{
				RedisURL:  "redis://localhost:6379",
				NodeID:    "test-node",
				ConfigDir: "/custom/path",
			},
			wantErr: false,
		},
		{
			name: "missing redis URL",
			config: ConfigSubscriberConfig{
				NodeID: "test-node",
			},
			wantErr: true,
		},
		{
			name: "missing node ID",
			config: ConfigSubscriberConfig{
				RedisURL: "redis://localhost:6379",
			},
			wantErr: true,
		},
		{
			name: "invalid redis URL",
			config: ConfigSubscriberConfig{
				RedisURL: "not-a-valid-url",
				NodeID:   "test-node",
			},
			wantErr: true,
		},
		{
			name: "invalid node ID - too long",
			config: ConfigSubscriberConfig{
				RedisURL: "redis://localhost:6379",
				NodeID:   "this-node-id-is-way-too-long-and-should-fail-validation-because-it-exceeds-64-characters",
			},
			wantErr: true,
		},
		{
			name: "invalid node ID - special characters",
			config: ConfigSubscriberConfig{
				RedisURL: "redis://localhost:6379",
				NodeID:   "node;DROP TABLE",
			},
			wantErr: true,
		},
		{
			name: "valid node ID with dots",
			config: ConfigSubscriberConfig{
				RedisURL: "redis://localhost:6379",
				NodeID:   "my.node.name",
			},
			wantErr: false,
		},
		{
			name: "valid node ID with hyphens and underscores",
			config: ConfigSubscriberConfig{
				RedisURL: "redis://localhost:6379",
				NodeID:   "my-node_name",
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			subscriber, err := NewConfigSubscriber(tt.config)

			if tt.wantErr {
				if err == nil {
					t.Error("NewConfigSubscriber() should return error")
				}
				return
			}

			if err != nil {
				t.Errorf("NewConfigSubscriber() error = %v, want nil", err)
				return
			}

			if subscriber == nil {
				t.Fatal("NewConfigSubscriber() returned nil")
			}

			// Verify channel name
			expectedChannel := "config:node:" + tt.config.NodeID
			if subscriber.Channel() != expectedChannel {
				t.Errorf("Channel() = %v, want %v", subscriber.Channel(), expectedChannel)
			}

			// Verify node ID
			if subscriber.NodeID() != tt.config.NodeID {
				t.Errorf("NodeID() = %v, want %v", subscriber.NodeID(), tt.config.NodeID)
			}

			// Clean up
			subscriber.Close()
		})
	}
}

func TestConfigSubscriberChannel(t *testing.T) {
	subscriber, err := NewConfigSubscriber(ConfigSubscriberConfig{
		RedisURL: "redis://localhost:6379",
		NodeID:   "my-gpu-node",
	})
	if err != nil {
		t.Fatalf("Failed to create subscriber: %v", err)
	}
	defer subscriber.Close()

	expectedChannel := "config:node:my-gpu-node"
	if subscriber.Channel() != expectedChannel {
		t.Errorf("Channel() = %v, want %v", subscriber.Channel(), expectedChannel)
	}
}

func TestConfigMessageParsing(t *testing.T) {
	tests := []struct {
		name        string
		payload     string
		wantType    string
		wantNodeID  string
		wantDevice  string
		wantErr     bool
	}{
		{
			name: "valid config_updated message",
			payload: `{
				"type": "config_updated",
				"nodeId": "test-node",
				"config": {
					"deviceName": "GPU Server 1",
					"services": ["vllm", "ollama"],
					"autoStartServices": true,
					"sshEnabled": false,
					"customTags": ["gpu", "production"]
				},
				"updatedAt": "2024-01-15T10:30:00Z"
			}`,
			wantType:   "config_updated",
			wantNodeID: "test-node",
			wantDevice: "GPU Server 1",
			wantErr:    false,
		},
		{
			name: "message with all fields",
			payload: `{
				"type": "config_updated",
				"nodeId": "full-node",
				"config": {
					"deviceName": "Full Config Node",
					"services": ["vllm"],
					"autoStartServices": true,
					"sshEnabled": true,
					"sshAllowedUsers": ["admin", "user1"],
					"sshDeviceKeys": ["ssh-rsa ABC..."],
					"shareInferenceWithOrg": true,
					"visibleToTeam": true,
					"customTags": ["tag1", "tag2"],
					"healthMonitoringEnabled": true,
					"alertOnOffline": true,
					"alertOnHighTemp": false,
					"status": "applied",
					"updatedAt": "2024-01-15T10:30:00Z"
				},
				"updatedAt": "2024-01-15T10:30:00Z"
			}`,
			wantType:   "config_updated",
			wantNodeID: "full-node",
			wantDevice: "Full Config Node",
			wantErr:    false,
		},
		{
			name:    "invalid JSON",
			payload: `{not valid json}`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var msg ConfigMessage
			err := json.Unmarshal([]byte(tt.payload), &msg)

			if tt.wantErr {
				if err == nil {
					t.Error("json.Unmarshal should return error")
				}
				return
			}

			if err != nil {
				t.Errorf("json.Unmarshal error = %v, want nil", err)
				return
			}

			if msg.Type != tt.wantType {
				t.Errorf("Type = %v, want %v", msg.Type, tt.wantType)
			}
			if msg.NodeID != tt.wantNodeID {
				t.Errorf("NodeID = %v, want %v", msg.NodeID, tt.wantNodeID)
			}
			if msg.Config.DeviceName != tt.wantDevice {
				t.Errorf("Config.DeviceName = %v, want %v", msg.Config.DeviceName, tt.wantDevice)
			}
		})
	}
}

func TestConfigUpdateFields(t *testing.T) {
	payload := `{
		"type": "config_updated",
		"nodeId": "test-node",
		"config": {
			"deviceName": "Test Device",
			"services": ["vllm", "ollama", "llamacpp"],
			"autoStartServices": true,
			"sshEnabled": true,
			"sshAllowedUsers": ["admin"],
			"sshDeviceKeys": ["ssh-rsa ABC123"],
			"shareInferenceWithOrg": true,
			"visibleToTeam": false,
			"customTags": ["gpu", "production", "us-west"],
			"healthMonitoringEnabled": true,
			"alertOnOffline": true,
			"alertOnHighTemp": false,
			"status": "applied",
			"updatedAt": "2024-01-15T10:30:00Z"
		},
		"updatedAt": "2024-01-15T10:30:00Z"
	}`

	var msg ConfigMessage
	if err := json.Unmarshal([]byte(payload), &msg); err != nil {
		t.Fatalf("Failed to parse message: %v", err)
	}

	// Verify all fields are parsed correctly
	config := msg.Config

	if config.DeviceName != "Test Device" {
		t.Errorf("DeviceName = %v, want 'Test Device'", config.DeviceName)
	}

	if len(config.Services) != 3 {
		t.Errorf("len(Services) = %v, want 3", len(config.Services))
	}

	if !config.AutoStartServices {
		t.Error("AutoStartServices should be true")
	}

	if !config.SSHEnabled {
		t.Error("SSHEnabled should be true")
	}

	if len(config.SSHAllowedUsers) != 1 || config.SSHAllowedUsers[0] != "admin" {
		t.Errorf("SSHAllowedUsers = %v, want ['admin']", config.SSHAllowedUsers)
	}

	if len(config.SSHDeviceKeys) != 1 {
		t.Errorf("len(SSHDeviceKeys) = %v, want 1", len(config.SSHDeviceKeys))
	}

	if !config.ShareInferenceWithOrg {
		t.Error("ShareInferenceWithOrg should be true")
	}

	if config.VisibleToTeam {
		t.Error("VisibleToTeam should be false")
	}

	if len(config.CustomTags) != 3 {
		t.Errorf("len(CustomTags) = %v, want 3", len(config.CustomTags))
	}

	if !config.HealthMonitoringEnabled {
		t.Error("HealthMonitoringEnabled should be true")
	}

	if !config.AlertOnOffline {
		t.Error("AlertOnOffline should be true")
	}

	if config.AlertOnHighTemp {
		t.Error("AlertOnHighTemp should be false")
	}

	if config.Status != "applied" {
		t.Errorf("Status = %v, want 'applied'", config.Status)
	}
}

func TestBackoffValues(t *testing.T) {
	subscriber, err := NewConfigSubscriber(ConfigSubscriberConfig{
		RedisURL: "redis://localhost:6379",
		NodeID:   "test-node",
	})
	if err != nil {
		t.Fatalf("Failed to create subscriber: %v", err)
	}
	defer subscriber.Close()

	// Verify initial backoff values
	if subscriber.minBackoff != 1*time.Second {
		t.Errorf("minBackoff = %v, want 1s", subscriber.minBackoff)
	}

	if subscriber.maxBackoff != 5*time.Minute {
		t.Errorf("maxBackoff = %v, want 5m", subscriber.maxBackoff)
	}

	if subscriber.backoff != 1*time.Second {
		t.Errorf("initial backoff = %v, want 1s", subscriber.backoff)
	}
}

func TestConfigSubscriberClose(t *testing.T) {
	subscriber, err := NewConfigSubscriber(ConfigSubscriberConfig{
		RedisURL: "redis://localhost:6379",
		NodeID:   "test-node",
	})
	if err != nil {
		t.Fatalf("Failed to create subscriber: %v", err)
	}

	// Close should not error
	err = subscriber.Close()
	if err != nil {
		t.Errorf("Close() error = %v, want nil", err)
	}
}

func TestValidateConfigLimits(t *testing.T) {
	subscriber, err := NewConfigSubscriber(ConfigSubscriberConfig{
		RedisURL: "redis://localhost:6379",
		NodeID:   "test-node",
	})
	if err != nil {
		t.Fatalf("Failed to create subscriber: %v", err)
	}
	defer subscriber.Close()

	tests := []struct {
		name    string
		config  ConfigUpdate
		wantErr bool
	}{
		{
			name: "valid config",
			config: ConfigUpdate{
				DeviceName: "Test Device",
				Services:   []string{"vllm", "ollama"},
				CustomTags: []string{"gpu"},
			},
			wantErr: false,
		},
		{
			name: "device name too long",
			config: ConfigUpdate{
				DeviceName: string(make([]byte, 300)), // 300 chars > 256 limit
			},
			wantErr: true,
		},
		{
			name: "too many services",
			config: ConfigUpdate{
				Services: make([]string, 60), // 60 > 50 limit
			},
			wantErr: true,
		},
		{
			name: "too many tags",
			config: ConfigUpdate{
				CustomTags: make([]string, 110), // 110 > 100 limit
			},
			wantErr: true,
		},
		{
			name: "too many SSH users",
			config: ConfigUpdate{
				SSHAllowedUsers: make([]string, 60), // 60 > 50 limit
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := subscriber.validateConfigLimits(&tt.config)
			if tt.wantErr && err == nil {
				t.Error("validateConfigLimits() should return error")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("validateConfigLimits() error = %v, want nil", err)
			}
		})
	}
}
