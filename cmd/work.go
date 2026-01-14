// cmd/work.go
package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/aceboss/citadel-cli/internal/heartbeat"
	"github.com/aceboss/citadel-cli/internal/status"
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

	// Status server and heartbeat flags
	workStatusPort   int
	workHeartbeat    bool
	workHeartbeatURL string
	workAPIKey       string
	workNodeName     string
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

  # Run with status server and heartbeat
  citadel work --mode=nexus --status-port=8080 --heartbeat

  # Run with custom queue and consumer group
  citadel work --mode=redis --queue=jobs:v1:gpu-general --group=my-workers`,
	Run: runWork,
}

func runWork(cmd *cobra.Command, args []string) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Setup signal handling
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		fmt.Println("\n   - Received shutdown signal...")
		cancel()
	}()

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

	// Get node name
	nodeName := workNodeName
	if nodeName == "" {
		nodeName = os.Getenv("CITADEL_NODE_NAME")
	}
	if nodeName == "" {
		hostname, _ := os.Hostname()
		nodeName = hostname
	}

	// Create status collector (used by both status server and heartbeat)
	var collector *status.Collector
	if workStatusPort > 0 || workHeartbeat {
		collector = status.NewCollector(status.CollectorConfig{
			NodeName:  nodeName,
			ConfigDir: "", // TODO: get from manifest
			Services:  nil,
		})
	}

	// Start status server if enabled
	if workStatusPort > 0 {
		statusServer := status.NewServer(status.ServerConfig{
			Port:    workStatusPort,
			Version: Version,
		}, collector)

		go func() {
			fmt.Printf("   - Status server: http://localhost:%d\n", workStatusPort)
			if err := statusServer.Start(ctx); err != nil && err != context.Canceled {
				fmt.Fprintf(os.Stderr, "   - ⚠️ Status server error: %v\n", err)
			}
		}()
	}

	// Start heartbeat if enabled
	if workHeartbeat {
		heartbeatURL := workHeartbeatURL
		if heartbeatURL == "" {
			heartbeatURL = os.Getenv("HEARTBEAT_URL")
		}
		if heartbeatURL == "" {
			heartbeatURL = "https://aceteam.ai"
		}

		apiKey := workAPIKey
		if apiKey == "" {
			apiKey = os.Getenv("CITADEL_API_KEY")
		}

		hbClient := heartbeat.NewClient(heartbeat.ClientConfig{
			BaseURL: heartbeatURL,
			NodeID:  nodeName,
			APIKey:  apiKey,
		}, collector)

		go func() {
			fmt.Printf("   - Heartbeat: %s (every 30s)\n", hbClient.Endpoint())
			if err := hbClient.Start(ctx); err != nil && err != context.Canceled {
				fmt.Fprintf(os.Stderr, "   - ⚠️ Heartbeat error: %v\n", err)
			}
		}()
	}

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
		if err != context.Canceled {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
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

	// Status and heartbeat flags
	workCmd.Flags().IntVar(&workStatusPort, "status-port", 0, "Enable status HTTP server on port (0 = disabled)")
	workCmd.Flags().BoolVar(&workHeartbeat, "heartbeat", false, "Enable heartbeat reporting to AceTeam API")
	workCmd.Flags().StringVar(&workHeartbeatURL, "heartbeat-url", "", "Heartbeat endpoint base URL (default: https://aceteam.ai)")
	workCmd.Flags().StringVar(&workAPIKey, "api-key", "", "API key for heartbeat authentication (or set CITADEL_API_KEY env)")
	workCmd.Flags().StringVar(&workNodeName, "node-name", "", "Node name for status reporting (default: hostname)")
}
