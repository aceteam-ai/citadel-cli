package worker

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/redisapi"
)

func TestWSSource_ConvertWSJob(t *testing.T) {
	src := &WSSource{
		config: WSSourceConfig{},
	}

	msg := redisapi.WSMessage{
		Type:  "job",
		Queue: "jobs:v1:cpu-general",
		ID:    "1718000000000-0",
		Data: map[string]string{
			"jobId":      "test-job-uuid",
			"type":       "SHELL_COMMAND",
			"payload":    `{"command":"ls -la","cwd":"/tmp"}`,
			"enqueuedAt": "2026-06-19T12:00:00Z",
			"rayId":      "ray-abc-123",
		},
	}

	job, err := src.convertWSJob(msg)
	if err != nil {
		t.Fatalf("convertWSJob error: %v", err)
	}

	if job.ID != "test-job-uuid" {
		t.Errorf("ID = %q, want %q", job.ID, "test-job-uuid")
	}
	if job.Type != "SHELL_COMMAND" {
		t.Errorf("Type = %q, want %q", job.Type, "SHELL_COMMAND")
	}
	if job.Source != "websocket" {
		t.Errorf("Source = %q, want %q", job.Source, "websocket")
	}
	if job.SourceQueue != "jobs:v1:cpu-general" {
		t.Errorf("SourceQueue = %q, want %q", job.SourceQueue, "jobs:v1:cpu-general")
	}
	if job.MessageID != "1718000000000-0" {
		t.Errorf("MessageID = %q, want %q", job.MessageID, "1718000000000-0")
	}
	if job.RayID != "ray-abc-123" {
		t.Errorf("RayID = %q, want %q", job.RayID, "ray-abc-123")
	}
	if cmd, ok := job.Payload["command"].(string); !ok || cmd != "ls -la" {
		t.Errorf("Payload[command] = %v, want %q", job.Payload["command"], "ls -la")
	}
	if cwd, ok := job.Payload["cwd"].(string); !ok || cwd != "/tmp" {
		t.Errorf("Payload[cwd] = %v, want %q", job.Payload["cwd"], "/tmp")
	}
}

func TestWSSource_ConvertWSJob_PayloadFallbackType(t *testing.T) {
	src := &WSSource{
		config: WSSourceConfig{},
	}

	msg := redisapi.WSMessage{
		Type:  "job",
		Queue: "jobs:v1:gpu",
		ID:    "1718000000001-0",
		Data: map[string]string{
			"jobId":   "job-uuid-456",
			"type":    "", // Empty top-level type
			"payload": `{"type":"llm_inference","model":"llama3"}`,
		},
	}

	job, err := src.convertWSJob(msg)
	if err != nil {
		t.Fatalf("convertWSJob error: %v", err)
	}

	if job.Type != "llm_inference" {
		t.Errorf("Type = %q, want %q (should fall back to payload type)", job.Type, "llm_inference")
	}
}

func TestWSSource_ConvertWSJob_NilData(t *testing.T) {
	src := &WSSource{
		config: WSSourceConfig{},
	}

	msg := redisapi.WSMessage{
		Type: "job",
		ID:   "1718000000002-0",
		Data: nil,
	}

	_, err := src.convertWSJob(msg)
	if err == nil {
		t.Error("convertWSJob should error on nil data")
	}
}

func TestWSSource_ConvertWSJob_InvalidPayload(t *testing.T) {
	src := &WSSource{
		config: WSSourceConfig{},
	}

	msg := redisapi.WSMessage{
		Type:  "job",
		Queue: "jobs:v1:cpu-general",
		ID:    "1718000000003-0",
		Data: map[string]string{
			"jobId":   "job-uuid-bad",
			"type":    "SHELL_COMMAND",
			"payload": "not-valid-json{{{",
		},
	}

	_, err := src.convertWSJob(msg)
	if err == nil {
		t.Error("convertWSJob should error on invalid JSON payload")
	}
}

func TestWSSource_AddQueue_Dedup(t *testing.T) {
	src := NewWSSource(WSSourceConfig{
		QueueNames: []string{"jobs:v1:cpu-general"},
		// No client -- AddQueue will skip the sendConsume call (logged as warning)
		LogFn: func(level, msg string) {}, // suppress output
	})

	// Add a new queue
	src.AddQueue("jobs:v1:gpu")
	queues := src.QueueNames()
	if len(queues) != 2 {
		t.Fatalf("QueueNames len = %d, want 2", len(queues))
	}

	// Add duplicate -- should be a no-op
	src.AddQueue("jobs:v1:gpu")
	queues = src.QueueNames()
	if len(queues) != 2 {
		t.Fatalf("QueueNames len = %d after dedup, want 2", len(queues))
	}

	// Add empty -- should be a no-op
	src.AddQueue("")
	queues = src.QueueNames()
	if len(queues) != 2 {
		t.Fatalf("QueueNames len = %d after empty add, want 2", len(queues))
	}
}

func TestWSSource_AddQueue_ThreadSafe(t *testing.T) {
	src := NewWSSource(WSSourceConfig{
		QueueNames: []string{"jobs:v1:cpu-general"},
		LogFn:      func(level, msg string) {},
	})

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			src.AddQueue("jobs:v1:q" + string(rune('A'+n%26)))
		}(i)
	}
	wg.Wait()

	queues := src.QueueNames()
	// Should have 1 original + up to 26 unique queues (A-Z)
	if len(queues) < 2 || len(queues) > 27 {
		t.Errorf("QueueNames len = %d, expected between 2 and 27", len(queues))
	}

	// Verify no duplicates
	seen := make(map[string]bool)
	for _, q := range queues {
		if seen[q] {
			t.Errorf("Duplicate queue found: %s", q)
		}
		seen[q] = true
	}
}

func TestWSSource_NextReturnsFromChannel(t *testing.T) {
	src := NewWSSource(WSSourceConfig{
		QueueNames: []string{"jobs:v1:cpu-general"},
		LogFn:      func(level, msg string) {},
	})

	// Simulate a job arriving on the channel
	testJob := &Job{
		ID:          "test-123",
		Type:        "SHELL_COMMAND",
		Source:      "websocket",
		SourceQueue: "jobs:v1:cpu-general",
	}

	go func() {
		time.Sleep(10 * time.Millisecond)
		src.jobs <- testJob
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	job, err := src.Next(ctx)
	if err != nil {
		t.Fatalf("Next error: %v", err)
	}
	if job == nil {
		t.Fatal("Next returned nil job")
	}
	if job.ID != "test-123" {
		t.Errorf("job.ID = %q, want %q", job.ID, "test-123")
	}
}

func TestWSSource_NextCancelledContext(t *testing.T) {
	src := NewWSSource(WSSourceConfig{
		QueueNames: []string{"jobs:v1:cpu-general"},
		LogFn:      func(level, msg string) {},
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	_, err := src.Next(ctx)
	if err == nil {
		t.Fatal("Next should return error on cancelled context")
	}
}

func TestWSSource_NextClosedSource(t *testing.T) {
	src := NewWSSource(WSSourceConfig{
		QueueNames: []string{"jobs:v1:cpu-general"},
		LogFn:      func(level, msg string) {},
	})

	// Close the source
	src.doneOnce.Do(func() {
		close(src.done)
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_, err := src.Next(ctx)
	if err == nil {
		t.Fatal("Next should return error when source is closed")
	}
}

func TestWSSource_Name(t *testing.T) {
	src := NewWSSource(WSSourceConfig{})
	if src.Name() != "websocket" {
		t.Errorf("Name() = %q, want %q", src.Name(), "websocket")
	}
}

func TestWSSource_DefaultConfig(t *testing.T) {
	src := NewWSSource(WSSourceConfig{})

	if src.config.ConsumerGroup != "citadel-workers" {
		t.Errorf("ConsumerGroup = %q, want %q", src.config.ConsumerGroup, "citadel-workers")
	}
	if src.config.BlockMs != 5000 {
		t.Errorf("BlockMs = %d, want %d", src.config.BlockMs, 5000)
	}
	queues := src.QueueNames()
	if len(queues) != 1 || queues[0] != "jobs:v1:cpu-general" {
		t.Errorf("default queue = %v, want [jobs:v1:cpu-general]", queues)
	}
}

func TestWSSource_SingleQueueFallback(t *testing.T) {
	src := NewWSSource(WSSourceConfig{
		QueueName: "jobs:v1:my-queue",
	})

	queues := src.QueueNames()
	if len(queues) != 1 || queues[0] != "jobs:v1:my-queue" {
		t.Errorf("queue = %v, want [jobs:v1:my-queue]", queues)
	}
}

func TestWSSource_MultiQueue(t *testing.T) {
	src := NewWSSource(WSSourceConfig{
		QueueName:  "ignored",
		QueueNames: []string{"q1", "q2", "q3"},
	})

	queues := src.QueueNames()
	if len(queues) != 3 {
		t.Errorf("QueueNames len = %d, want 3", len(queues))
	}
}
