// cmd/work.go
package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/aceboss/citadel-cli/internal/worker"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

var (
	workMode       string
	workNexusURL   string
	workRedisURL   string
	workRedisPass  string
	workQueue      string
	workGroup      string
	workPollMs     int
	workMaxRetries int
)

var workCmd = &cobra.Command{
	Use:   "work",
	Short: "Run as a job worker for Nexus or Redis",
	Long: `Unified job worker that can consume from either Nexus (HTTP polling) or
Redis Streams (consumer groups).

Modes:
  nexus   Poll Nexus API for jobs (for on-premise nodes)
  redis   Consume from Redis Streams (for AceTeam private GPU cloud)

Examples:
  # Run as Nexus worker (polls HTTP endpoint)
  citadel work --mode=nexus --nexus-url=https://nexus.aceteam.ai

  # Run as Redis worker (consumes from stream)
  citadel work --mode=redis --redis-url=redis://localhost:6379

  # Run with custom queue and consumer group
  citadel work --mode=redis --queue=jobs:v1:gpu-general --group=my-workers`,
	Run: runWork,
}

func runWork(cmd *cobra.Command, args []string) {
	ctx := context.Background()

	// Validate mode
	if workMode != "nexus" && workMode != "redis" {
		fmt.Fprintf(os.Stderr, "Error: --mode must be 'nexus' or 'redis'\n")
		os.Exit(1)
	}

	// Create the appropriate job source
	var source worker.JobSource
	var streamFactory func(jobID string) worker.StreamWriter

	switch workMode {
	case "nexus":
		if workNexusURL == "" {
			workNexusURL = os.Getenv("NEXUS_URL")
		}
		if workNexusURL == "" {
			workNexusURL = nexusURL // Use global nexusURL from root.go
		}
		if workNexusURL == "" {
			fmt.Fprintf(os.Stderr, "Error: Nexus URL is required. Set --nexus-url or NEXUS_URL env var.\n")
			os.Exit(1)
		}

		source = worker.NewNexusSource(worker.NexusSourceConfig{
			NexusURL: workNexusURL,
		})

	case "redis":
		if workRedisURL == "" {
			workRedisURL = os.Getenv("REDIS_URL")
		}
		if workRedisURL == "" {
			fmt.Fprintf(os.Stderr, "Error: Redis URL is required. Set --redis-url or REDIS_URL env var.\n")
			os.Exit(1)
		}

		if workRedisPass == "" {
			workRedisPass = os.Getenv("REDIS_PASSWORD")
		}
		if workQueue == "" {
			workQueue = os.Getenv("WORKER_QUEUE")
		}
		if workGroup == "" {
			workGroup = os.Getenv("CONSUMER_GROUP")
		}

		redisSource := worker.NewRedisSource(worker.RedisSourceConfig{
			URL:           workRedisURL,
			Password:      workRedisPass,
			QueueName:     workQueue,
			ConsumerGroup: workGroup,
			BlockMs:       workPollMs,
			MaxAttempts:   workMaxRetries,
		})
		source = redisSource

		// Enable streaming for Redis mode
		streamFactory = worker.CreateRedisStreamWriterFactory(ctx, redisSource)
	}

	// Create worker ID
	workerID := fmt.Sprintf("citadel-%s", uuid.New().String()[:8])

	// Create handlers (use legacy adapters for now)
	handlers := worker.CreateLegacyHandlers()

	// Create runner
	runner := worker.NewRunner(source, handlers, worker.RunnerConfig{
		WorkerID: workerID,
		Verbose:  true,
	})

	// Add stream writer factory if available
	if streamFactory != nil {
		runner.WithStreamWriterFactory(streamFactory)
	}

	// Run the worker
	if err := runner.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func init() {
	rootCmd.AddCommand(workCmd)

	// Mode selection
	workCmd.Flags().StringVar(&workMode, "mode", "nexus", "Worker mode: 'nexus' (HTTP polling) or 'redis' (Redis Streams)")

	// Nexus flags
	workCmd.Flags().StringVar(&workNexusURL, "nexus-url", "", "Nexus server URL (or set NEXUS_URL env)")

	// Redis flags
	workCmd.Flags().StringVar(&workRedisURL, "redis-url", "", "Redis connection URL (or set REDIS_URL env)")
	workCmd.Flags().StringVar(&workRedisPass, "redis-password", "", "Redis password (or set REDIS_PASSWORD env)")
	workCmd.Flags().StringVar(&workQueue, "queue", "", "Queue/stream name to consume from (default: jobs:v1:gpu-general)")
	workCmd.Flags().StringVar(&workGroup, "group", "", "Consumer group name (default: citadel-workers)")
	workCmd.Flags().IntVar(&workPollMs, "poll-ms", 5000, "Poll/block timeout in milliseconds")
	workCmd.Flags().IntVar(&workMaxRetries, "max-retries", 3, "Maximum retry attempts before DLQ")
}
