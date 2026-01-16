package heartbeat

import (
	"testing"

	"github.com/redis/go-redis/v9"
)

func TestNewConfigConsumer(t *testing.T) {
	tests := []struct {
		name    string
		config  ConfigConsumerConfig
		wantErr bool
	}{
		{
			name: "valid config",
			config: ConfigConsumerConfig{
				RedisURL: "redis://localhost:6379",
			},
			wantErr: false,
		},
		{
			name: "with password",
			config: ConfigConsumerConfig{
				RedisURL:      "redis://localhost:6379",
				RedisPassword: "secret",
			},
			wantErr: false,
		},
		{
			name: "with custom config dir",
			config: ConfigConsumerConfig{
				RedisURL:  "redis://localhost:6379",
				ConfigDir: "/custom/path",
			},
			wantErr: false,
		},
		{
			name: "invalid redis URL",
			config: ConfigConsumerConfig{
				RedisURL: "not-a-valid-url",
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			consumer, err := NewConfigConsumer(tt.config)

			if tt.wantErr {
				if err == nil {
					t.Error("NewConfigConsumer() should return error")
				}
				return
			}

			if err != nil {
				t.Errorf("NewConfigConsumer() error = %v, want nil", err)
				return
			}

			if consumer == nil {
				t.Fatal("NewConfigConsumer() returned nil")
			}

			// Verify queue name
			if consumer.QueueName() != ConfigQueueName {
				t.Errorf("QueueName() = %v, want %v", consumer.QueueName(), ConfigQueueName)
			}

			// Verify consumer group
			if consumer.consumerGroup != ConfigConsumerGroup {
				t.Errorf("consumerGroup = %v, want %v", consumer.consumerGroup, ConfigConsumerGroup)
			}

			// Verify worker ID has correct prefix
			if len(consumer.workerID) == 0 {
				t.Error("workerID should not be empty")
			}

			// Clean up
			consumer.Close()
		})
	}
}

func TestConfigConsumerParseMessage(t *testing.T) {
	consumer, err := NewConfigConsumer(ConfigConsumerConfig{
		RedisURL: "redis://localhost:6379",
	})
	if err != nil {
		t.Fatalf("Failed to create consumer: %v", err)
	}
	defer consumer.Close()

	tests := []struct {
		name        string
		msg         redis.XMessage
		wantJobID   string
		wantJobType string
		wantConfig  bool
	}{
		{
			name: "job with payload containing config",
			msg: redis.XMessage{
				ID: "1234-0",
				Values: map[string]interface{}{
					"jobId":   "job-123",
					"type":    "APPLY_DEVICE_CONFIG",
					"payload": `{"config":{"deviceName":"test-node","services":["ollama"]}}`,
				},
			},
			wantJobID:   "job-123",
			wantJobType: "APPLY_DEVICE_CONFIG",
			wantConfig:  true,
		},
		{
			name: "job with top-level config",
			msg: redis.XMessage{
				ID: "1235-0",
				Values: map[string]interface{}{
					"jobId":  "job-456",
					"type":   "APPLY_DEVICE_CONFIG",
					"config": `{"deviceName":"another-node","services":["vllm"]}`,
				},
			},
			wantJobID:   "job-456",
			wantJobType: "APPLY_DEVICE_CONFIG",
			wantConfig:  true,
		},
		{
			name: "job with raw payload",
			msg: redis.XMessage{
				ID: "1236-0",
				Values: map[string]interface{}{
					"jobId":   "job-789",
					"type":    "APPLY_DEVICE_CONFIG",
					"payload": `{"deviceName":"raw-node","services":["llamacpp"]}`,
				},
			},
			wantJobID:   "job-789",
			wantJobType: "APPLY_DEVICE_CONFIG",
			wantConfig:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job, err := consumer.parseMessage(tt.msg)
			if err != nil {
				t.Fatalf("parseMessage() error = %v", err)
			}

			if job.ID != tt.wantJobID {
				t.Errorf("job.ID = %v, want %v", job.ID, tt.wantJobID)
			}
			if job.Type != tt.wantJobType {
				t.Errorf("job.Type = %v, want %v", job.Type, tt.wantJobType)
			}
			if tt.wantConfig {
				if _, ok := job.Payload["config"]; !ok {
					t.Error("job.Payload should contain 'config' field")
				}
			}
		})
	}
}

func TestConfigConsumerConstants(t *testing.T) {
	// Verify constants are set correctly
	if ConfigQueueName != "jobs:v1:config" {
		t.Errorf("ConfigQueueName = %v, want 'jobs:v1:config'", ConfigQueueName)
	}

	if ConfigConsumerGroup != "citadel-config-consumers" {
		t.Errorf("ConfigConsumerGroup = %v, want 'citadel-config-consumers'", ConfigConsumerGroup)
	}
}

func TestConfigConsumerClose(t *testing.T) {
	consumer, err := NewConfigConsumer(ConfigConsumerConfig{
		RedisURL: "redis://localhost:6379",
	})
	if err != nil {
		t.Fatalf("Failed to create consumer: %v", err)
	}

	// Close should not error
	err = consumer.Close()
	if err != nil {
		t.Errorf("Close() error = %v, want nil", err)
	}
}
