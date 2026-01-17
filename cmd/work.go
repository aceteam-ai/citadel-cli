// cmd/work.go
package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/heartbeat"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	internalServices "github.com/aceteam-ai/citadel-cli/internal/services"
	"github.com/aceteam-ai/citadel-cli/internal/status"
	"github.com/aceteam-ai/citadel-cli/internal/terminal"
	"github.com/aceteam-ai/citadel-cli/internal/worker"
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

	// SSH key sync flags
	workSSHSync     bool
	workSSHSyncMins int

	// Redis status publishing flags
	workRedisStatus bool
	workDeviceCode  string

	// Terminal server flags
	workTerminal     bool
	workTerminalPort int

	// Service auto-start flags
	workNoServices bool

	// Update check flag
	workNoUpdate bool
)

var workCmd = &cobra.Command{
	Use:   "work",
	Short: "Start services and run the job worker (primary node command)",
	Long: `Unified Citadel worker that starts services and processes jobs.

This is the primary command for running a Citadel node. It:
  1. Auto-starts services from manifest (use --no-services to skip)
  2. Runs the job worker (Nexus or Redis mode)
  3. Reports status via heartbeat
  4. Optionally runs terminal server for remote access

Modes:
  nexus   Poll Nexus API for jobs (for on-premise nodes)
  redis   Consume from Redis Streams (for AceTeam private GPU cloud)

Examples:
  # Run node (starts services + worker)
  citadel work --mode=nexus

  # Run without auto-starting services
  citadel work --mode=nexus --no-services

  # Run with status server and heartbeat
  citadel work --mode=nexus --status-port=8080 --heartbeat

  # Run as Redis worker (for private GPU cloud)
  citadel work --mode=redis --redis-url=redis://localhost:6379`,
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

	// Check for updates in background (unless --no-update is set)
	if !workNoUpdate {
		go CheckForUpdateInBackground()
	}

	// Auto-start services from manifest (unless --no-services is set)
	if !workNoServices {
		if err := autoStartServices(); err != nil {
			fmt.Fprintf(os.Stderr, "   - Warning: Service auto-start: %v\n", err)
		}
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

	// Resolve API key and base URL (used by heartbeat and SSH sync)
	apiKey := workAPIKey
	if apiKey == "" {
		apiKey = os.Getenv("CITADEL_API_KEY")
	}

	baseURL := workHeartbeatURL
	if baseURL == "" {
		baseURL = os.Getenv("HEARTBEAT_URL")
	}
	if baseURL == "" {
		baseURL = "https://aceteam.ai"
	}

	// Start heartbeat if enabled
	if workHeartbeat {
		hbClient := heartbeat.NewClient(heartbeat.ClientConfig{
			BaseURL: baseURL,
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

	// Start SSH key sync if enabled
	if workSSHSync && apiKey != "" {
		syncInterval := time.Duration(workSSHSyncMins) * time.Minute
		sshConfig := nexus.SSHSyncConfig{
			APIToken: apiKey,
			NodeID:   nodeName,
			BaseURL:  baseURL,
		}

		go func() {
			fmt.Printf("   - SSH key sync: enabled (every %dm)\n", workSSHSyncMins)

			// Initial sync
			if err := nexus.SyncAuthorizedKeys(sshConfig); err != nil {
				fmt.Fprintf(os.Stderr, "   - ⚠️ Initial SSH sync failed: %v\n", err)
			} else {
				fmt.Println("   - ✅ SSH keys synchronized")
			}

			// Periodic sync
			ticker := time.NewTicker(syncInterval)
			defer ticker.Stop()

			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if err := nexus.SyncAuthorizedKeys(sshConfig); err != nil {
						fmt.Fprintf(os.Stderr, "   - ⚠️ SSH sync failed: %v\n", err)
					}
				}
			}
		}()
	} else if workSSHSync && apiKey == "" {
		fmt.Fprintln(os.Stderr, "   - ⚠️ SSH sync enabled but no API key configured")
	}

	// Start Redis status publisher if enabled (for Redis mode)
	if workRedisStatus && workRedisURL != "" {
		// Get device code from flag or environment
		deviceCode := workDeviceCode
		if deviceCode == "" {
			deviceCode = os.Getenv("CITADEL_DEVICE_CODE")
		}

		// Create collector if not already created
		if collector == nil {
			collector = status.NewCollector(status.CollectorConfig{
				NodeName:  nodeName,
				ConfigDir: "",
				Services:  nil,
			})
		}

		redisPublisher, err := heartbeat.NewRedisPublisher(heartbeat.RedisPublisherConfig{
			RedisURL:      workRedisURL,
			RedisPassword: workRedisPass,
			NodeID:        nodeName,
			DeviceCode:    deviceCode,
		}, collector)
		if err != nil {
			fmt.Fprintf(os.Stderr, "   - ⚠️ Failed to create Redis publisher: %v\n", err)
		} else {
			go func() {
				fmt.Printf("   - Redis status: %s (every 30s)\n", redisPublisher.PubSubChannel())
				if deviceCode != "" {
					fmt.Printf("   - Device code: %s (for config lookup)\n", deviceCode[:8]+"...")
				}
				if err := redisPublisher.Start(ctx); err != nil && err != context.Canceled {
					fmt.Fprintf(os.Stderr, "   - ⚠️ Redis status publisher error: %v\n", err)
				}
			}()
		}

		// Start config queue consumer for device configuration jobs
		configConsumer, err := heartbeat.NewConfigConsumer(heartbeat.ConfigConsumerConfig{
			RedisURL:      workRedisURL,
			RedisPassword: workRedisPass,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "   - ⚠️ Failed to create config consumer: %v\n", err)
		} else {
			go func() {
				fmt.Printf("   - Config queue: %s (listening for device config)\n", configConsumer.QueueName())
				if err := configConsumer.Start(ctx); err != nil && err != context.Canceled {
					fmt.Fprintf(os.Stderr, "   - ⚠️ Config consumer error: %v\n", err)
				}
			}()
		}

		// Start config Pub/Sub subscriber for real-time config updates
		configSubscriber, err := heartbeat.NewConfigSubscriber(heartbeat.ConfigSubscriberConfig{
			RedisURL:      workRedisURL,
			RedisPassword: workRedisPass,
			NodeID:        nodeName,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "   - ⚠️ Failed to create config subscriber: %v\n", err)
		} else {
			go func() {
				defer configSubscriber.Close() // Ensure Redis connection is cleaned up
				fmt.Printf("   - Config subscriber: %s (real-time config updates)\n", configSubscriber.Channel())
				if err := configSubscriber.Start(ctx); err != nil && err != context.Canceled {
					fmt.Fprintf(os.Stderr, "   - ⚠️ Config subscriber error: %v\n", err)
				}
			}()
		}
	}

	// Start terminal server if enabled
	if workTerminal {
		// Check platform support
		if runtime.GOOS == "windows" {
			fmt.Fprintln(os.Stderr, "   - ⚠️ Terminal server is not supported on Windows")
		} else {
			// Get org ID from manifest
			orgID := ""
			if manifest, _, err := findAndReadManifest(); err == nil {
				orgID = manifest.Node.OrgID
			}

			if orgID == "" {
				fmt.Fprintln(os.Stderr, "   - ⚠️ Terminal server requires org-id (run 'citadel init' first)")
			} else {
				termConfig := terminal.DefaultConfig()
				termConfig.OrgID = orgID
				termConfig.AuthServiceURL = baseURL
				if workTerminalPort > 0 {
					termConfig.Port = workTerminalPort
				}

				// Use CachingTokenValidator for production
				cachingAuth := terminal.NewCachingTokenValidator(
					baseURL,
					orgID,
					termConfig.TokenRefreshInterval,
				)

				termServer := terminal.NewServer(termConfig, cachingAuth)

				go func() {
					// Start the token cache
					if err := cachingAuth.Start(); err != nil {
						fmt.Fprintf(os.Stderr, "   - ⚠️ Token cache error: %v\n", err)
					}

					fmt.Printf("   - Terminal server: ws://localhost:%d/terminal\n", termConfig.Port)
					if err := termServer.Start(); err != nil {
						fmt.Fprintf(os.Stderr, "   - ⚠️ Terminal server error: %v\n", err)
					}
				}()
			}
		}
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

// autoStartServices starts all services defined in the manifest.
// This is called automatically by the work command unless --no-services is set.
func autoStartServices() error {
	manifest, configDir, err := findAndReadManifest()
	if err != nil {
		// No manifest found - this is fine, just skip service startup
		return nil
	}

	if len(manifest.Services) == 0 {
		return nil
	}

	fmt.Printf("--- Starting %d service(s) ---\n", len(manifest.Services))

	for _, service := range manifest.Services {
		serviceType := determineServiceType(service)

		if serviceType == internalServices.ServiceTypeNative {
			fmt.Printf("   - Starting %s (native)...\n", service.Name)
			if err := startNativeService(service.Name, configDir); err != nil {
				fmt.Fprintf(os.Stderr, "     Warning: %s: %v\n", service.Name, err)
				continue
			}
		} else {
			// Validate that compose file path stays within config directory (prevent path traversal)
			fullComposePath, err := platform.ValidatePathWithinDir(configDir, service.ComposeFile)
			if err != nil {
				fmt.Fprintf(os.Stderr, "     Warning: %s: invalid compose file path: %v\n", service.Name, err)
				continue
			}
			fmt.Printf("   - Starting %s...\n", service.Name)
			if err := startService(service.Name, fullComposePath); err != nil {
				fmt.Fprintf(os.Stderr, "     Warning: %s: %v\n", service.Name, err)
				continue
			}
		}
		fmt.Printf("   - %s is running\n", service.Name)
	}

	fmt.Println("--- Services started ---")
	return nil
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

	// SSH key sync flags
	workCmd.Flags().BoolVar(&workSSHSync, "ssh-sync", false, "Enable SSH authorized_keys synchronization from AceTeam")
	workCmd.Flags().IntVar(&workSSHSyncMins, "ssh-sync-interval", 5, "SSH sync interval in minutes")

	// Redis status publishing flags
	workCmd.Flags().BoolVar(&workRedisStatus, "redis-status", false, "Enable Redis status publishing for real-time updates")
	workCmd.Flags().StringVar(&workDeviceCode, "device-code", "", "Device authorization code for config lookup (or set CITADEL_DEVICE_CODE env)")

	// Terminal server flags
	workCmd.Flags().BoolVar(&workTerminal, "terminal", false, "Enable terminal WebSocket server for remote access")
	workCmd.Flags().IntVar(&workTerminalPort, "terminal-port", 7860, "Terminal server port (default: 7860)")

	// Service auto-start flags
	workCmd.Flags().BoolVar(&workNoServices, "no-services", false, "Skip auto-starting services from manifest")

	// Update check flags
	workCmd.Flags().BoolVar(&workNoUpdate, "no-update", false, "Skip checking for updates on startup")
}
