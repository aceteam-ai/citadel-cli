package worker

import (
	"testing"
)

func TestNewAPISource_Defaults(t *testing.T) {
	source := NewAPISource(APISourceConfig{
		BaseURL: "https://aceteam.ai",
		Token:   "test-token",
	})

	if source == nil {
		t.Fatal("NewAPISource returned nil")
	}
	if len(source.queueNames) != 1 || source.queueNames[0] != "jobs:v1:cpu-general" {
		t.Errorf("queueNames = %v, want [jobs:v1:cpu-general]", source.queueNames)
	}
	if source.config.ConsumerGroup != "citadel-workers" {
		t.Errorf("ConsumerGroup = %v, want citadel-workers", source.config.ConsumerGroup)
	}
	if source.config.BlockMs != 5000 {
		t.Errorf("BlockMs = %v, want 5000", source.config.BlockMs)
	}
	if source.config.MaxAttempts != 3 {
		t.Errorf("MaxAttempts = %v, want 3", source.config.MaxAttempts)
	}
}

func TestNewAPISource_SingleQueue(t *testing.T) {
	source := NewAPISource(APISourceConfig{
		BaseURL:   "https://aceteam.ai",
		Token:     "test-token",
		QueueName: "jobs:v1:gpu-a100",
	})

	if len(source.queueNames) != 1 || source.queueNames[0] != "jobs:v1:gpu-a100" {
		t.Errorf("queueNames = %v, want [jobs:v1:gpu-a100]", source.queueNames)
	}
}

func TestNewAPISource_MultiQueue(t *testing.T) {
	queues := []string{
		"jobs:v1:cpu-general",
		"jobs:v1:shell:org_550e8400-e29b-41d4-a716-446655440000",
	}
	source := NewAPISource(APISourceConfig{
		BaseURL:    "https://aceteam.ai",
		Token:      "test-token",
		QueueNames: queues,
	})

	if len(source.queueNames) != 2 {
		t.Fatalf("queueNames length = %d, want 2", len(source.queueNames))
	}
	if source.queueNames[0] != queues[0] {
		t.Errorf("queueNames[0] = %v, want %v", source.queueNames[0], queues[0])
	}
	if source.queueNames[1] != queues[1] {
		t.Errorf("queueNames[1] = %v, want %v", source.queueNames[1], queues[1])
	}
}

func TestNewAPISource_QueueNamesPrecedence(t *testing.T) {
	// QueueNames should take precedence over QueueName
	source := NewAPISource(APISourceConfig{
		BaseURL:    "https://aceteam.ai",
		Token:      "test-token",
		QueueName:  "should-be-ignored",
		QueueNames: []string{"queue-a", "queue-b"},
	})

	if len(source.queueNames) != 2 {
		t.Fatalf("queueNames length = %d, want 2", len(source.queueNames))
	}
	if source.queueNames[0] != "queue-a" {
		t.Errorf("queueNames[0] = %v, want queue-a", source.queueNames[0])
	}
}

func TestAPISourceName(t *testing.T) {
	source := NewAPISource(APISourceConfig{
		BaseURL: "https://aceteam.ai",
		Token:   "test-token",
	})

	if source.Name() != "redis-api" {
		t.Errorf("Name() = %v, want redis-api", source.Name())
	}
}

func TestAPISourceClose(t *testing.T) {
	source := NewAPISource(APISourceConfig{
		BaseURL: "https://aceteam.ai",
		Token:   "test-token",
	})

	// Close without connecting should not error
	if err := source.Close(); err != nil {
		t.Errorf("Close() error = %v, want nil", err)
	}
}

func TestAPISourceImplementsJobSource(t *testing.T) {
	var _ JobSource = (*APISource)(nil)
}

func TestAPISourceQueueNames(t *testing.T) {
	queues := []string{"queue-a", "queue-b", "queue-c"}
	source := NewAPISource(APISourceConfig{
		BaseURL:    "https://aceteam.ai",
		Token:      "test-token",
		QueueNames: queues,
	})

	got := source.QueueNames()
	if len(got) != len(queues) {
		t.Fatalf("QueueNames() length = %d, want %d", len(got), len(queues))
	}
	for i, q := range queues {
		if got[i] != q {
			t.Errorf("QueueNames()[%d] = %v, want %v", i, got[i], q)
		}
	}
}
