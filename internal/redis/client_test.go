package redis

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestNewClient(t *testing.T) {
	tests := []struct {
		name          string
		config        ClientConfig
		wantGroup     string
		wantBlockMs   int
		wantMaxRetry  int
	}{
		{
			name:          "with defaults",
			config:        ClientConfig{},
			wantGroup:     "citadel-workers",
			wantBlockMs:   5000,
			wantMaxRetry:  3,
		},
		{
			name: "with custom values",
			config: ClientConfig{
				ConsumerGroup: "custom-group",
				BlockMs:       10000,
				MaxAttempts:   5,
			},
			wantGroup:    "custom-group",
			wantBlockMs:  10000,
			wantMaxRetry: 5,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := NewClient(tt.config)

			if client == nil {
				t.Fatal("NewClient returned nil")
			}
			if client.consumerGroup != tt.wantGroup {
				t.Errorf("consumerGroup = %v, want %v", client.consumerGroup, tt.wantGroup)
			}
			if client.blockMs != tt.wantBlockMs {
				t.Errorf("blockMs = %v, want %v", client.blockMs, tt.wantBlockMs)
			}
			if client.maxAttempts != tt.wantMaxRetry {
				t.Errorf("maxAttempts = %v, want %v", client.maxAttempts, tt.wantMaxRetry)
			}
		})
	}
}

func TestClientWorkerID(t *testing.T) {
	client := NewClient(ClientConfig{})

	workerID := client.WorkerID()

	if workerID == "" {
		t.Error("WorkerID() returned empty string")
	}
	if len(workerID) < 8 {
		t.Errorf("WorkerID length = %d, want >= 8", len(workerID))
	}
	// Should start with "citadel-"
	if workerID[:8] != "citadel-" {
		t.Errorf("WorkerID prefix = %s, want 'citadel-'", workerID[:8])
	}
}

func TestClientQueueName(t *testing.T) {
	client := NewClient(ClientConfig{
		QueueName: "test-queue",
	})

	if client.QueueName() != "test-queue" {
		t.Errorf("QueueName() = %v, want test-queue", client.QueueName())
	}
}

func TestClientMaxAttempts(t *testing.T) {
	client := NewClient(ClientConfig{
		MaxAttempts: 7,
	})

	if client.MaxAttempts() != 7 {
		t.Errorf("MaxAttempts() = %v, want 7", client.MaxAttempts())
	}
}

func TestClientClose(t *testing.T) {
	client := NewClient(ClientConfig{})

	// Close without connecting should not error
	err := client.Close()
	if err != nil {
		t.Errorf("Close() error = %v, want nil", err)
	}
}

func TestGetDLQName(t *testing.T) {
	tests := []struct {
		queueName string
		wantDLQ   string
	}{
		{"jobs:v1:gpu-general", "dlq:v1:gpu-general"},
		{"jobs:v1:cpu-only", "dlq:v1:cpu-only"},
		{"simple-queue", "dlq:v1:simple-queue"},
	}

	for _, tt := range tests {
		t.Run(tt.queueName, func(t *testing.T) {
			client := NewClient(ClientConfig{
				QueueName: tt.queueName,
			})

			got := client.getDLQName()
			if got != tt.wantDLQ {
				t.Errorf("getDLQName() = %v, want %v", got, tt.wantDLQ)
			}
		})
	}
}

func TestStreamEvent(t *testing.T) {
	event := StreamEvent{
		Version:   "1.0",
		Type:      "chunk",
		JobID:     "job-123",
		RayID:     "ray-abc",
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Data: map[string]interface{}{
			"content": "test chunk",
			"index":   0,
		},
	}

	if event.Version != "1.0" {
		t.Errorf("Version = %v, want 1.0", event.Version)
	}
	if event.Type != "chunk" {
		t.Errorf("Type = %v, want chunk", event.Type)
	}
	if event.JobID != "job-123" {
		t.Errorf("JobID = %v, want job-123", event.JobID)
	}
	if event.RayID != "ray-abc" {
		t.Errorf("RayID = %v, want ray-abc", event.RayID)
	}
	if event.Data["content"] != "test chunk" {
		t.Errorf("Data[content] = %v, want 'test chunk'", event.Data["content"])
	}
}

func TestStreamEventOmitsEmptyRayID(t *testing.T) {
	event := StreamEvent{
		Version: "1.0",
		Type:    "start",
		JobID:   "job-456",
	}

	data, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}

	// RayID should be omitted when empty (omitempty tag)
	jsonStr := string(data)
	if strings.Contains(jsonStr, "rayId") {
		t.Errorf("Empty RayID should be omitted from JSON, got: %s", jsonStr)
	}
}

func TestStreamEventIncludesRayID(t *testing.T) {
	event := StreamEvent{
		Version: "1.0",
		Type:    "start",
		JobID:   "job-456",
		RayID:   "ray-xyz",
	}

	data, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}

	jsonStr := string(data)
	if !strings.Contains(jsonStr, `"rayId":"ray-xyz"`) {
		t.Errorf("RayID should be included in JSON, got: %s", jsonStr)
	}
}

func TestJob(t *testing.T) {
	job := &Job{
		MessageID: "msg-123",
		JobID:     "job-456",
		Type:      "llm_inference",
		Payload: map[string]interface{}{
			"prompt": "Hello",
			"model":  "gpt-4",
		},
		RawData: map[string]interface{}{
			"jobId":   "job-456",
			"type":    "llm_inference",
			"payload": `{"prompt":"Hello","model":"gpt-4"}`,
		},
	}

	if job.MessageID != "msg-123" {
		t.Errorf("MessageID = %v, want msg-123", job.MessageID)
	}
	if job.JobID != "job-456" {
		t.Errorf("JobID = %v, want job-456", job.JobID)
	}
	if job.Type != "llm_inference" {
		t.Errorf("Type = %v, want llm_inference", job.Type)
	}
	if job.Payload["prompt"] != "Hello" {
		t.Errorf("Payload[prompt] = %v, want Hello", job.Payload["prompt"])
	}
}
