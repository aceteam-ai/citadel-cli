package redis

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"
)

// setupMiniredis starts a miniredis instance and returns a connected Client.
func setupMiniredis(t *testing.T) (*miniredis.Miniredis, *Client, *goredis.Client) {
	t.Helper()

	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("failed to start miniredis: %v", err)
	}
	t.Cleanup(func() { mr.Close() })

	client := NewClient(ClientConfig{
		QueueName:     "jobs:v1:integration-test",
		ConsumerGroup: "test-workers",
		BlockMs:       100,
		MaxAttempts:   3,
	})

	ctx := context.Background()
	if err := client.Connect(ctx, "redis://"+mr.Addr(), ""); err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	// Also create a raw go-redis client for assertions
	raw := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { raw.Close() })

	return mr, client, raw
}

func TestPublishStreamEventIncludesRayID(t *testing.T) {
	_, client, raw := setupMiniredis(t)
	ctx := context.Background()

	jobID := "job-ray-test-001"
	rayID := "ray-abc-001"
	channel := "stream:v1:" + jobID

	// Subscribe BEFORE publishing (Pub/Sub has no replay)
	pubsub := raw.Subscribe(ctx, channel)
	defer pubsub.Close()
	if _, err := pubsub.Receive(ctx); err != nil {
		t.Fatalf("failed to subscribe: %v", err)
	}

	// Publish
	if err := client.PublishStart(ctx, jobID, rayID, "starting"); err != nil {
		t.Fatalf("PublishStart failed: %v", err)
	}

	// Receive and verify
	msg, err := pubsub.ReceiveMessage(ctx)
	if err != nil {
		t.Fatalf("failed to receive message: %v", err)
	}

	var event StreamEvent
	if err := json.Unmarshal([]byte(msg.Payload), &event); err != nil {
		t.Fatalf("failed to unmarshal event: %v", err)
	}

	if event.RayID != rayID {
		t.Errorf("event.RayID = %q, want %q", event.RayID, rayID)
	}
	if event.Type != "start" {
		t.Errorf("event.Type = %q, want %q", event.Type, "start")
	}
	if event.JobID != jobID {
		t.Errorf("event.JobID = %q, want %q", event.JobID, jobID)
	}
	if event.Version != "1.0" {
		t.Errorf("event.Version = %q, want %q", event.Version, "1.0")
	}
}

func TestPublishStreamEventOmitsEmptyRayID(t *testing.T) {
	_, client, raw := setupMiniredis(t)
	ctx := context.Background()

	jobID := "job-no-ray-002"
	channel := "stream:v1:" + jobID

	pubsub := raw.Subscribe(ctx, channel)
	defer pubsub.Close()
	if _, err := pubsub.Receive(ctx); err != nil {
		t.Fatalf("failed to subscribe: %v", err)
	}

	// Publish with empty rayID
	if err := client.PublishStart(ctx, jobID, "", "starting"); err != nil {
		t.Fatalf("PublishStart failed: %v", err)
	}

	msg, err := pubsub.ReceiveMessage(ctx)
	if err != nil {
		t.Fatalf("failed to receive message: %v", err)
	}

	// Raw JSON should NOT contain "rayId" key (omitempty)
	if strings.Contains(msg.Payload, `"rayId"`) {
		t.Errorf("empty rayId should be omitted from JSON, got: %s", msg.Payload)
	}
}

func TestPublishCancelledEvent(t *testing.T) {
	_, client, raw := setupMiniredis(t)
	ctx := context.Background()

	jobID := "job-cancel-003"
	rayID := "ray-cancel-003"
	channel := "stream:v1:" + jobID

	pubsub := raw.Subscribe(ctx, channel)
	defer pubsub.Close()
	if _, err := pubsub.Receive(ctx); err != nil {
		t.Fatalf("failed to subscribe: %v", err)
	}

	// Publish cancelled event
	reason := "User requested cancellation"
	if err := client.PublishCancelled(ctx, jobID, rayID, reason); err != nil {
		t.Fatalf("PublishCancelled failed: %v", err)
	}

	msg, err := pubsub.ReceiveMessage(ctx)
	if err != nil {
		t.Fatalf("failed to receive message: %v", err)
	}

	var event StreamEvent
	if err := json.Unmarshal([]byte(msg.Payload), &event); err != nil {
		t.Fatalf("failed to unmarshal event: %v", err)
	}

	if event.Type != "cancelled" {
		t.Errorf("event.Type = %q, want %q", event.Type, "cancelled")
	}
	if event.RayID != rayID {
		t.Errorf("event.RayID = %q, want %q", event.RayID, rayID)
	}
	if event.Data["reason"] != reason {
		t.Errorf("event.Data[reason] = %q, want %q", event.Data["reason"], reason)
	}
}

func TestIsJobCancelledTrue(t *testing.T) {
	mr, client, _ := setupMiniredis(t)
	ctx := context.Background()

	jobID := "job-cancelled-004"
	mr.Set("job:cancelled:"+jobID, "1")

	cancelled, err := client.IsJobCancelled(ctx, jobID)
	if err != nil {
		t.Fatalf("IsJobCancelled error: %v", err)
	}
	if !cancelled {
		t.Error("expected IsJobCancelled to return true")
	}
}

func TestIsJobCancelledFalse(t *testing.T) {
	_, client, _ := setupMiniredis(t)
	ctx := context.Background()

	jobID := "job-not-cancelled-005"

	cancelled, err := client.IsJobCancelled(ctx, jobID)
	if err != nil {
		t.Fatalf("IsJobCancelled error: %v", err)
	}
	if cancelled {
		t.Error("expected IsJobCancelled to return false")
	}
}

func TestMoveToDLQPreservesEnqueuedAt(t *testing.T) {
	_, client, raw := setupMiniredis(t)
	ctx := context.Background()

	enqueuedAt := "2025-01-15T12:00:00Z"
	job := &Job{
		MessageID:  "msg-006",
		JobID:      "job-dlq-006",
		Type:       "test_job",
		Payload:    map[string]interface{}{"key": "value"},
		RawData:    map[string]interface{}{},
		EnqueuedAt: enqueuedAt,
	}

	if err := client.MoveToDLQ(ctx, job, "test failure"); err != nil {
		t.Fatalf("MoveToDLQ failed: %v", err)
	}

	// Read DLQ stream directly
	dlqName := "dlq:v1:integration-test"
	msgs, err := raw.XRange(ctx, dlqName, "-", "+").Result()
	if err != nil {
		t.Fatalf("XRange failed: %v", err)
	}

	if len(msgs) != 1 {
		t.Fatalf("expected 1 DLQ entry, got %d", len(msgs))
	}

	entry := msgs[0].Values
	if entry["enqueuedAt"] != enqueuedAt {
		t.Errorf("DLQ enqueuedAt = %q, want %q", entry["enqueuedAt"], enqueuedAt)
	}
	if entry["jobId"] != job.JobID {
		t.Errorf("DLQ jobId = %q, want %q", entry["jobId"], job.JobID)
	}
	if entry["reason"] != "test failure" {
		t.Errorf("DLQ reason = %q, want %q", entry["reason"], "test failure")
	}
}

func TestMoveToDLQFallsBackToRawData(t *testing.T) {
	_, client, raw := setupMiniredis(t)
	ctx := context.Background()

	enqueuedAt := "2025-02-20T08:30:00Z"
	job := &Job{
		MessageID:  "msg-007",
		JobID:      "job-dlq-007",
		Type:       "test_job",
		Payload:    map[string]interface{}{"key": "value"},
		RawData:    map[string]interface{}{"enqueuedAt": enqueuedAt},
		EnqueuedAt: "", // Empty — should fall back to RawData
	}

	if err := client.MoveToDLQ(ctx, job, "fallback test"); err != nil {
		t.Fatalf("MoveToDLQ failed: %v", err)
	}

	dlqName := "dlq:v1:integration-test"
	msgs, err := raw.XRange(ctx, dlqName, "-", "+").Result()
	if err != nil {
		t.Fatalf("XRange failed: %v", err)
	}

	if len(msgs) == 0 {
		t.Fatal("expected at least 1 DLQ entry")
	}

	// Find our entry (there may be entries from other tests)
	var found bool
	for _, msg := range msgs {
		if msg.Values["jobId"] == "job-dlq-007" {
			if msg.Values["enqueuedAt"] != enqueuedAt {
				t.Errorf("DLQ enqueuedAt = %q, want %q", msg.Values["enqueuedAt"], enqueuedAt)
			}
			found = true
			break
		}
	}
	if !found {
		t.Error("DLQ entry for job-dlq-007 not found")
	}
}

func TestMoveToDLQFromQueuePreservesEnqueuedAt(t *testing.T) {
	_, client, raw := setupMiniredis(t)
	ctx := context.Background()

	enqueuedAt := time.Now().UTC().Format(time.RFC3339)
	sourceQueue := "jobs:v1:tag:gpu:rtx4090"
	job := &Job{
		MessageID:  "msg-008",
		JobID:      "job-dlq-008",
		Type:       "test_job",
		Payload:    map[string]interface{}{"model": "llama3"},
		RawData:    map[string]interface{}{},
		EnqueuedAt: enqueuedAt,
	}

	if err := client.MoveToDLQFromQueue(ctx, sourceQueue, job, "tag queue failure"); err != nil {
		t.Fatalf("MoveToDLQFromQueue failed: %v", err)
	}

	// DLQ name should preserve tag context: dlq:v1:tag:gpu:rtx4090
	expectedDLQ := "dlq:v1:tag:gpu:rtx4090"
	msgs, err := raw.XRange(ctx, expectedDLQ, "-", "+").Result()
	if err != nil {
		t.Fatalf("XRange failed: %v", err)
	}

	if len(msgs) != 1 {
		t.Fatalf("expected 1 DLQ entry in %s, got %d", expectedDLQ, len(msgs))
	}

	entry := msgs[0].Values
	if entry["enqueuedAt"] != enqueuedAt {
		t.Errorf("DLQ enqueuedAt = %q, want %q", entry["enqueuedAt"], enqueuedAt)
	}
	if entry["original_queue"] != sourceQueue {
		t.Errorf("DLQ original_queue = %q, want %q", entry["original_queue"], sourceQueue)
	}
	if entry["jobId"] != job.JobID {
		t.Errorf("DLQ jobId = %q, want %q", entry["jobId"], job.JobID)
	}
}
