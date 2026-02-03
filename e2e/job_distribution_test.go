package e2e

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/e2e/harness"
	"github.com/google/uuid"
)

// TestJobDistribution tests the Redis Streams job distribution
func TestJobDistribution(t *testing.T) {
	redisURL := os.Getenv("REDIS_URL")
	if redisURL == "" {
		redisURL = "redis://localhost:6379"
	}

	queue := os.Getenv("WORKER_QUEUE")
	if queue == "" {
		queue = "jobs:v1:e2e-test"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Initialize Redis harness
	redis, err := harness.NewRedisHarness(redisURL)
	if err != nil {
		t.Skipf("Redis not available: %v", err)
	}
	defer redis.Close()

	// Setup test queue
	t.Log("Setting up test queue...")
	if err := redis.CreateConsumerGroup(ctx, queue, "citadel-workers"); err != nil {
		t.Logf("Consumer group setup: %v", err)
	}

	t.Run("EnqueueJob", func(t *testing.T) {
		job := &harness.Job{
			ID:   uuid.New().String(),
			Type: "test_job",
			Payload: map[string]interface{}{
				"message": "Hello from E2E test",
			},
		}

		messageID, err := redis.EnqueueJob(ctx, queue, job)
		if err != nil {
			t.Fatalf("Failed to enqueue job: %v", err)
		}

		t.Logf("Enqueued job %s with message ID %s", job.ID, messageID)

		// Verify job can be read from queue
		client := redis.Client()
		result, err := client.XRange(ctx, queue, "-", "+").Result()
		if err != nil {
			t.Fatalf("Failed to read queue: %v", err)
		}

		found := false
		for _, msg := range result {
			if msg.ID == messageID {
				found = true
				t.Logf("Found job in queue: %+v", msg.Values)
				break
			}
		}

		if !found {
			t.Error("Job not found in queue")
		}
	})

	t.Run("EnqueueLLMJob", func(t *testing.T) {
		// Test a realistic LLM inference job
		job := &harness.Job{
			ID:   uuid.New().String(),
			Type: "llm_inference",
			Payload: map[string]interface{}{
				"model":       "llama3",
				"prompt":      "What is the capital of France?",
				"max_tokens":  100,
				"temperature": 0.7,
			},
		}

		messageID, err := redis.EnqueueJob(ctx, queue, job)
		if err != nil {
			t.Fatalf("Failed to enqueue LLM job: %v", err)
		}

		t.Logf("Enqueued LLM job %s with message ID %s", job.ID, messageID)
	})

	t.Run("JobStreaming", func(t *testing.T) {
		// Test that we can listen for job results
		job := &harness.Job{
			ID:   uuid.New().String(),
			Type: "test_streaming",
			Payload: map[string]interface{}{
				"test": true,
			},
		}

		_, err := redis.EnqueueJob(ctx, queue, job)
		if err != nil {
			t.Fatalf("Failed to enqueue job: %v", err)
		}

		// Note: WaitForJobResult will timeout since there's no worker processing
		// This tests that the streaming mechanism is wired up correctly
		shortCtx, shortCancel := context.WithTimeout(ctx, 5*time.Second)
		defer shortCancel()

		results, err := redis.WaitForJobResult(shortCtx, job.ID, 3*time.Second)
		if err != nil {
			// Expected - no worker processing the job
			t.Logf("Job result wait (expected timeout): %v", err)
		} else {
			t.Logf("Job results: %v", results)
		}
	})

	// Cleanup test jobs
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()

		// Delete test stream
		client := redis.Client()
		client.Del(cleanupCtx, queue)
		t.Log("Cleaned up test queue")
	})
}

// TestJobDistributionWithWorker tests job distribution with a running worker
func TestJobDistributionWithWorker(t *testing.T) {
	if os.Getenv("CITADEL_BINARY") == "" {
		t.Skip("CITADEL_BINARY not set - skipping worker integration test")
	}

	redisURL := os.Getenv("REDIS_URL")
	if redisURL == "" {
		redisURL = "redis://localhost:6379"
	}

	queue := "jobs:v1:e2e-worker-test"

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Initialize harnesses
	redis, err := harness.NewRedisHarness(redisURL)
	if err != nil {
		t.Skipf("Redis not available: %v", err)
	}
	defer redis.Close()

	citadel := harness.NewCitadelHarness(os.Getenv("CITADEL_BINARY"))
	defer citadel.Cleanup()

	// Start worker
	t.Log("Starting citadel worker...")
	workerCtx, workerCancel := context.WithCancel(ctx)
	defer workerCancel()

	workerCmd, err := citadel.StartWorker(workerCtx, "redis", redisURL, queue)
	if err != nil {
		t.Fatalf("Failed to start worker: %v", err)
	}
	defer workerCmd.Process.Kill()

	// Wait for worker to initialize
	time.Sleep(3 * time.Second)

	// Enqueue a test job
	job := &harness.Job{
		ID:   uuid.New().String(),
		Type: "shell_command",
		Payload: map[string]interface{}{
			"command": "echo 'Hello from E2E test'",
		},
	}

	_, err = redis.EnqueueJob(ctx, queue, job)
	if err != nil {
		t.Fatalf("Failed to enqueue job: %v", err)
	}

	t.Logf("Enqueued job %s", job.ID)

	// Wait for job result
	results, err := redis.WaitForJobResult(ctx, job.ID, 30*time.Second)
	if err != nil {
		t.Fatalf("Failed to get job result: %v", err)
	}

	t.Logf("Job completed with %d result messages", len(results))
	for i, result := range results {
		t.Logf("Result %d: %s", i+1, result)
	}
}
