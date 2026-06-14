package redisapi

import (
	"encoding/json"
	"testing"
)

func TestParseStreamMessage_TopLevelType(t *testing.T) {
	// Simulate a stream message from the Python backend's xadd:
	//   xadd(queue, { "jobId": ..., "type": "SHELL_COMMAND", "payload": json.dumps({"command": "ls"}) })
	msg := StreamMessage{
		ID: "1234567890-0",
		Data: StreamMessageData{
			JobID:   "job-uuid-123",
			Type:    "SHELL_COMMAND",
			Payload: `{"command":"ls -la"}`,
		},
	}

	job, err := parseStreamMessage(msg)
	if err != nil {
		t.Fatalf("parseStreamMessage error: %v", err)
	}
	if job.Type != "SHELL_COMMAND" {
		t.Errorf("Type = %q, want %q", job.Type, "SHELL_COMMAND")
	}
	if job.JobID != "job-uuid-123" {
		t.Errorf("JobID = %q, want %q", job.JobID, "job-uuid-123")
	}
	if cmd, ok := job.Payload["command"].(string); !ok || cmd != "ls -la" {
		t.Errorf("Payload[command] = %v, want %q", job.Payload["command"], "ls -la")
	}
}

func TestParseStreamMessage_PayloadFallbackType(t *testing.T) {
	// When type is only in the payload (legacy format), it should still be extracted.
	payload := map[string]any{"type": "llm_inference", "model": "llama3"}
	payloadJSON, _ := json.Marshal(payload)

	msg := StreamMessage{
		ID: "1234567890-1",
		Data: StreamMessageData{
			JobID:   "job-uuid-456",
			Type:    "", // Empty top-level type
			Payload: string(payloadJSON),
		},
	}

	job, err := parseStreamMessage(msg)
	if err != nil {
		t.Fatalf("parseStreamMessage error: %v", err)
	}
	if job.Type != "llm_inference" {
		t.Errorf("Type = %q, want %q", job.Type, "llm_inference")
	}
}

func TestParseStreamMessage_TopLevelTypeWins(t *testing.T) {
	// Top-level type should take precedence over payload type.
	payload := map[string]any{"type": "wrong_type", "data": "test"}
	payloadJSON, _ := json.Marshal(payload)

	msg := StreamMessage{
		ID: "1234567890-2",
		Data: StreamMessageData{
			JobID:   "job-uuid-789",
			Type:    "FILE_READ",
			Payload: string(payloadJSON),
		},
	}

	job, err := parseStreamMessage(msg)
	if err != nil {
		t.Fatalf("parseStreamMessage error: %v", err)
	}
	if job.Type != "FILE_READ" {
		t.Errorf("Type = %q, want %q (top-level should win)", job.Type, "FILE_READ")
	}
}

func TestParseStreamMessage_RayID(t *testing.T) {
	msg := StreamMessage{
		ID: "1234567890-3",
		Data: StreamMessageData{
			JobID:   "job-uuid-aaa",
			Type:    "SHELL_COMMAND",
			Payload: `{"command":"echo hello"}`,
			RayID:   "ray-test-123",
		},
	}

	job, err := parseStreamMessage(msg)
	if err != nil {
		t.Fatalf("parseStreamMessage error: %v", err)
	}
	if job.RawData["rayId"] != "ray-test-123" {
		t.Errorf("RawData[rayId] = %v, want %q", job.RawData["rayId"], "ray-test-123")
	}
}

func TestParseStreamMessage_InvalidPayload(t *testing.T) {
	msg := StreamMessage{
		ID: "1234567890-4",
		Data: StreamMessageData{
			JobID:   "job-uuid-bbb",
			Type:    "SHELL_COMMAND",
			Payload: "not-valid-json{{{",
		},
	}

	_, err := parseStreamMessage(msg)
	if err == nil {
		t.Error("parseStreamMessage should error on invalid JSON payload")
	}
}

func TestParseStreamMessage_EmptyPayload(t *testing.T) {
	msg := StreamMessage{
		ID: "1234567890-5",
		Data: StreamMessageData{
			JobID: "job-uuid-ccc",
			Type:  "SHELL_COMMAND",
		},
	}

	job, err := parseStreamMessage(msg)
	if err != nil {
		t.Fatalf("parseStreamMessage error: %v", err)
	}
	if job.Type != "SHELL_COMMAND" {
		t.Errorf("Type = %q, want %q", job.Type, "SHELL_COMMAND")
	}
	if job.Payload != nil {
		t.Errorf("Payload should be nil for empty payload string, got %v", job.Payload)
	}
}
