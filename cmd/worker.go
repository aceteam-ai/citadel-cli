// cmd/worker.go
package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	redisclient "github.com/aceteam-ai/citadel-cli/internal/redis"
	"github.com/spf13/cobra"
)

var (
	redisURL      string
	redisPassword string
	workerQueue   string
	consumerGroup string
)

// WorkerJobHandler is the interface for Redis-based job handlers.
type WorkerJobHandler interface {
	Execute(ctx context.Context, client *redisclient.Client, job *redisclient.Job) error
	CanHandle(jobType string) bool
}

// workerHandlers holds all registered handlers for the worker
var workerHandlers []WorkerJobHandler

var workerCmd = &cobra.Command{
	Use:   "worker",
	Short: "Run as a high-performance Redis Streams worker for AceTeam's private GPU cloud",
	Long: `High-performance job queue worker for AceTeam's private GPU infrastructure.

Written in Go for maximum concurrency and throughput, this worker:
- Consumes jobs from Redis Streams using consumer groups
- Routes inference requests to private vLLM/Ollama/llama.cpp clusters
- Streams responses back via Redis Pub/Sub
- Scales horizontally across GPU nodes

This is the Citadel Worker - designed for AceTeam's private cloud.
For external API calls (OpenAI, Anthropic), use the Python Worker instead.`,
	Run: runWorker,
}

func runWorker(cmd *cobra.Command, args []string) {
	fmt.Println("--- 🚀 Starting Citadel Worker ---")

	// Validate configuration
	if redisURL == "" {
		redisURL = os.Getenv("REDIS_URL")
	}
	if redisURL == "" {
		fmt.Fprintln(os.Stderr, "Error: Redis URL is required. Set --redis-url or REDIS_URL env var.")
		os.Exit(1)
	}

	if workerQueue == "" {
		workerQueue = os.Getenv("WORKER_QUEUE")
	}
	if workerQueue == "" {
		workerQueue = "jobs:v1:gpu-general" // Default queue
	}

	if redisPassword == "" {
		redisPassword = os.Getenv("REDIS_PASSWORD")
	}

	if consumerGroup == "" {
		consumerGroup = os.Getenv("CONSUMER_GROUP")
	}

	// Create Redis client
	client := redisclient.NewClient(redisclient.ClientConfig{
		URL:           redisURL,
		Password:      redisPassword,
		QueueName:     workerQueue,
		ConsumerGroup: consumerGroup,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Connect to Redis
	fmt.Printf("   - Connecting to Redis...\n")
	if err := client.Connect(ctx, redisURL, redisPassword); err != nil {
		fmt.Fprintf(os.Stderr, "   - ❌ Failed to connect to Redis: %v\n", err)
		os.Exit(1)
	}
	defer client.Close()

	fmt.Printf("   - ✅ Connected to Redis\n")
	fmt.Printf("   - Worker ID: %s\n", client.WorkerID())
	fmt.Printf("   - Queue: %s\n", client.QueueName())

	// Ensure consumer group exists
	if err := client.EnsureConsumerGroup(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "   - ❌ Failed to create consumer group: %v\n", err)
		os.Exit(1)
	}

	// Setup signal handling for graceful shutdown
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	fmt.Println("   - ✅ Worker started. Listening for jobs...")

	// Main worker loop
workerLoop:
	for {
		select {
		case sig := <-sigs:
			fmt.Printf("\n   - Received signal %v, initiating graceful shutdown...\n", sig)
			cancel()
			break workerLoop
		default:
			// Read next job from queue
			job, err := client.ReadJob(ctx)
			if err != nil {
				if ctx.Err() != nil {
					break workerLoop // Context cancelled
				}
				fmt.Fprintf(os.Stderr, "   - ⚠️ Error reading job: %v\n", err)
				continue
			}

			if job == nil {
				continue // No job available, loop again
			}

			// Process the job
			processWorkerJob(ctx, client, job)
		}
	}

	fmt.Println("--- 🛑 Worker shutdown complete ---")
}

func processWorkerJob(ctx context.Context, client *redisclient.Client, job *redisclient.Job) {
	fmt.Printf("   - 📥 Received job %s (type: %s)\n", job.JobID, job.Type)

	// Check delivery count for DLQ handling
	deliveryCount, _ := client.GetDeliveryCount(ctx, job.MessageID)
	if int(deliveryCount) >= client.MaxAttempts() {
		fmt.Printf("   - ⚠️ Job %s exceeded max attempts (%d), moving to DLQ\n", job.JobID, client.MaxAttempts())
		if err := client.MoveToDLQ(ctx, job, "Exceeded max retry attempts"); err != nil {
			fmt.Fprintf(os.Stderr, "   - ❌ Failed to move job to DLQ: %v\n", err)
		}
		client.AckJob(ctx, job.MessageID)
		return
	}

	// Update status to processing
	client.SetJobStatus(ctx, job.JobID, "processing", nil)

	// Publish start event
	client.PublishStart(ctx, job.JobID, "Job processing started")

	// Find handler for this job type
	var handler WorkerJobHandler
	for _, h := range workerHandlers {
		if h.CanHandle(job.Type) {
			handler = h
			break
		}
	}

	if handler == nil {
		errMsg := fmt.Sprintf("No handler for job type: %s", job.Type)
		fmt.Printf("   - ❌ %s\n", errMsg)
		client.PublishError(ctx, job.JobID, errMsg, false)
		client.SetJobStatus(ctx, job.JobID, "failed", map[string]interface{}{"error": errMsg})
		// Don't ACK - let it retry or go to DLQ
		return
	}

	// Execute the handler
	err := handler.Execute(ctx, client, job)

	if err != nil {
		fmt.Printf("   - ❌ Job %s failed: %v\n", job.JobID, err)
		client.PublishError(ctx, job.JobID, err.Error(), false)
		client.SetJobStatus(ctx, job.JobID, "failed", map[string]interface{}{"error": err.Error()})
		// Don't ACK - let it retry or go to DLQ
		return
	}

	// Success - ACK the message
	fmt.Printf("   - ✅ Job %s completed successfully\n", job.JobID)
	client.SetJobStatus(ctx, job.JobID, "completed", nil)
	if err := client.AckJob(ctx, job.MessageID); err != nil {
		fmt.Fprintf(os.Stderr, "   - ⚠️ Failed to ACK job: %v\n", err)
	}
}

// RegisterWorkerHandler adds a handler to the worker's handler list.
func RegisterWorkerHandler(handler WorkerJobHandler) {
	workerHandlers = append(workerHandlers, handler)
}

// LLMInferenceWorkerHandler wraps jobs.LLMInferenceHandler for the worker interface.
type LLMInferenceWorkerHandler struct {
	handler jobs.LLMInferenceHandler
}

// CanHandle returns true if this handler can process the given job type.
func (h *LLMInferenceWorkerHandler) CanHandle(jobType string) bool {
	return h.handler.CanHandle(jobType)
}

// Execute processes an llm_inference job.
func (h *LLMInferenceWorkerHandler) Execute(ctx context.Context, client *redisclient.Client, job *redisclient.Job) error {
	return h.handler.Execute(ctx, client, job)
}

func init() {
	rootCmd.AddCommand(workerCmd)

	workerCmd.Flags().StringVar(&redisURL, "redis-url", "", "Redis connection URL (or set REDIS_URL env)")
	workerCmd.Flags().StringVar(&redisPassword, "redis-password", "", "Redis password (or set REDIS_PASSWORD env)")
	workerCmd.Flags().StringVar(&workerQueue, "queue", "", "Queue name to consume from (default: jobs:v1:gpu-general)")
	workerCmd.Flags().StringVar(&consumerGroup, "consumer-group", "", "Consumer group name (default: citadel-workers)")

	// Register job handlers
	RegisterWorkerHandler(&LLMInferenceWorkerHandler{})
}
