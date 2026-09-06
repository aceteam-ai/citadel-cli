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

// TestXReadGroupNeverRedeliversAlreadyDeliveredMessage pins the underlying
// Redis-protocol behavior issue #871 turned on: XREADGROUP's ">" ID hands out
// each message exactly once to the consumer group, never again -- not to a
// different consumer, and not even back to the SAME consumer that originally
// read it. Only re-reading with ID "0" (a consumer's own pending entries) or
// XCLAIM/XAUTOCLAIM (stealing another consumer's pending entries) can ever
// surface it again. This is the fact that made RedisSource.Nack's "don't
// ACK, let it retry" comment aspirational rather than true before this fix,
// and the reason ReclaimStalePendingOnQueue (tested below) exists at all.
func TestXReadGroupNeverRedeliversAlreadyDeliveredMessage(t *testing.T) {
	_, _, raw := setupMiniredis(t)
	ctx := context.Background()

	stream := "test-xreadgroup-semantics"
	group := "test-group"
	if err := raw.XGroupCreateMkStream(ctx, stream, group, "0").Err(); err != nil {
		t.Fatalf("XGroupCreateMkStream failed: %v", err)
	}
	if _, err := raw.XAdd(ctx, &goredis.XAddArgs{Stream: stream, Values: map[string]interface{}{"foo": "bar"}}).Result(); err != nil {
		t.Fatalf("XAdd failed: %v", err)
	}

	// First delivery, to consumerA.
	res1, err := raw.XReadGroup(ctx, &goredis.XReadGroupArgs{
		Group: group, Consumer: "consumerA", Streams: []string{stream, ">"}, Count: 1,
	}).Result()
	if err != nil {
		t.Fatalf("first XReadGroup failed: %v", err)
	}
	if len(res1) != 1 || len(res1[0].Messages) != 1 {
		t.Fatalf("expected exactly 1 message on first delivery, got %+v", res1)
	}

	// Re-reading with ">" as the SAME consumer must return nothing.
	res2, err := raw.XReadGroup(ctx, &goredis.XReadGroupArgs{
		Group: group, Consumer: "consumerA", Streams: []string{stream, ">"}, Count: 1, Block: 50 * time.Millisecond,
	}).Result()
	if err != nil && err != goredis.Nil {
		t.Fatalf("second (same consumer) XReadGroup failed: %v", err)
	}
	if len(res2) != 0 {
		t.Errorf("expected no redelivery to the same consumer via '>', got %+v", res2)
	}

	// Re-reading with ">" as a DIFFERENT consumer must also return nothing --
	// the pool of "new" messages is per-group, not per-consumer, and this
	// message already left it.
	res3, err := raw.XReadGroup(ctx, &goredis.XReadGroupArgs{
		Group: group, Consumer: "consumerB", Streams: []string{stream, ">"}, Count: 1, Block: 50 * time.Millisecond,
	}).Result()
	if err != nil && err != goredis.Nil {
		t.Fatalf("third (different consumer) XReadGroup failed: %v", err)
	}
	if len(res3) != 0 {
		t.Errorf("expected no redelivery to a different consumer via '>', got %+v", res3)
	}

	// Only reading the SAME consumer's own pending entries with "0" surfaces
	// it again -- and doing so increments the delivery count.
	res4, err := raw.XReadGroup(ctx, &goredis.XReadGroupArgs{
		Group: group, Consumer: "consumerA", Streams: []string{stream, "0"}, Count: 1, Block: 50 * time.Millisecond,
	}).Result()
	if err != nil {
		t.Fatalf("fourth (self-pending, '0') XReadGroup failed: %v", err)
	}
	if len(res4) != 1 || len(res4[0].Messages) != 1 {
		t.Fatalf("expected the pending message back via '0', got %+v", res4)
	}

	pending, err := raw.XPendingExt(ctx, &goredis.XPendingExtArgs{Stream: stream, Group: group, Start: "-", End: "+", Count: 10}).Result()
	if err != nil {
		t.Fatalf("XPendingExt failed: %v", err)
	}
	if len(pending) != 1 || pending[0].RetryCount != 2 {
		t.Errorf("expected 1 pending entry with RetryCount 2 after the self-pending re-read, got %+v", pending)
	}
}

// TestReclaimStalePendingOnQueue pins ReclaimStalePendingOnQueue's contract:
// nothing eligible before the idle threshold, and a real reclaim (with an
// incremented delivery count) once it elapses.
func TestReclaimStalePendingOnQueue(t *testing.T) {
	orig := StalePendingReclaimMinIdle
	StalePendingReclaimMinIdle = 1 * time.Second
	t.Cleanup(func() { StalePendingReclaimMinIdle = orig })

	mr, client, raw := setupMiniredis(t)
	ctx := context.Background()

	queue := "jobs:v1:integration-test"
	if err := client.EnsureConsumerGroup(ctx); err != nil {
		t.Fatalf("EnsureConsumerGroup failed: %v", err)
	}
	msgID, err := raw.XAdd(ctx, &goredis.XAddArgs{
		Stream: queue,
		Values: map[string]interface{}{
			"jobId":   "job-reclaim-001",
			"type":    "test_job",
			"payload": `{"data":"x"}`,
		},
	}).Result()
	if err != nil {
		t.Fatalf("XAdd failed: %v", err)
	}

	// Deliver it once via the client's own read path.
	job, err := client.ReadJobBlock(ctx, 100)
	if err != nil {
		t.Fatalf("ReadJobBlock failed: %v", err)
	}
	if job == nil || job.MessageID != msgID {
		t.Fatalf("expected to read message %s, got %+v", msgID, job)
	}

	// Not idle long enough yet -- must not be reclaimable.
	reclaimed, err := client.ReclaimStalePendingOnQueue(ctx, queue)
	if err != nil {
		t.Fatalf("ReclaimStalePendingOnQueue (too soon) failed: %v", err)
	}
	if reclaimed != nil {
		t.Fatalf("expected no reclaim before the idle threshold, got %+v", reclaimed)
	}

	// Advance the fake clock past the (shrunk) threshold.
	mr.SetTime(time.Now().Add(2 * time.Second))

	reclaimed, err = client.ReclaimStalePendingOnQueue(ctx, queue)
	if err != nil {
		t.Fatalf("ReclaimStalePendingOnQueue (idle enough) failed: %v", err)
	}
	if reclaimed == nil {
		t.Fatal("expected the stale message to be reclaimed")
	}
	if reclaimed.MessageID != msgID {
		t.Errorf("reclaimed MessageID = %q, want %q", reclaimed.MessageID, msgID)
	}
	if reclaimed.JobID != "job-reclaim-001" {
		t.Errorf("reclaimed JobID = %q, want %q", reclaimed.JobID, "job-reclaim-001")
	}

	deliveryCount, err := client.GetDeliveryCount(ctx, msgID)
	if err != nil {
		t.Fatalf("GetDeliveryCount failed: %v", err)
	}
	if deliveryCount != 2 {
		t.Errorf("delivery count after reclaim = %d, want 2", deliveryCount)
	}

	// A second reclaim attempt immediately after must find nothing new
	// (it was just re-claimed by this same consumer, so it is fresh again).
	reclaimed2, err := client.ReclaimStalePendingOnQueue(ctx, queue)
	if err != nil {
		t.Fatalf("ReclaimStalePendingOnQueue (immediately again) failed: %v", err)
	}
	if reclaimed2 != nil {
		t.Errorf("expected no immediate re-reclaim right after a reclaim, got %+v", reclaimed2)
	}
}
