// cmd/work.go
package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/heartbeat"
	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	internalServices "github.com/aceteam-ai/citadel-cli/internal/services"
	"github.com/aceteam-ai/citadel-cli/internal/status"
	"github.com/aceteam-ai/citadel-cli/internal/terminal"
	"github.com/aceteam-ai/citadel-cli/internal/worker"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var (
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
	workRedisStatus   bool
	workDeviceCode    string
	workStatusChannel string

	// Terminal server flags
	workTerminal     bool
	workTerminalPort int

	// Service auto-start flags
	workNoServices bool

	// Update check flag
	workNoUpdate bool

	// API mode flag
	workForceDirectRedis bool
)

var workCmd = &cobra.Command{
	Use:   "work",
	Short: "Start services and run the job worker (primary node command)",
	Long: `Unified Citadel worker that starts services and processes jobs.

This is the primary command for running a Citadel node. It:
  1. Auto-starts services from manifest (use --no-services to skip)
  2. Connects to Redis and consumes jobs from the queue
  3. Reports status via Redis pub/sub (enabled by default)
  4. Subscribes to real-time config updates

Redis URL is obtained from local config (set by 'citadel init').
Use REDIS_URL env var or --redis-url flag to override.

Examples:
  # Run after citadel init (uses config)
  citadel work

  # Override Redis URL for development
  citadel work --redis-url=redis://localhost:6379

  # Disable status publishing
  citadel work --redis-status=false

  # Run without auto-starting services
  citadel work --no-services

  # Run with heartbeat reporting to AceTeam API
  citadel work --heartbeat`,
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

	// Resolve job source: API mode (device_api_token) vs direct Redis (legacy)
	Debug("resolving job source mode...")

	// Load device config from file
	deviceConfig := getDeviceConfigFromFile()
	if deviceConfig != nil {
		Debug("config loaded: device_api_token=%q, api_base_url=%q, redis_url=%q",
			maskToken(deviceConfig.DeviceAPIToken),
			deviceConfig.APIBaseURL,
			deviceConfig.RedisURL)
	} else {
		Debug("config file not found or empty")
	}

	// Check for API mode: device_api_token takes precedence (unless forced to direct Redis)
	var source worker.JobSource
	var streamFactory func(jobID string) worker.StreamWriter
	var useAPIMode bool
	var apiSource *worker.APISource // Keep reference for heartbeat

	if !workForceDirectRedis && deviceConfig != nil && deviceConfig.DeviceAPIToken != "" {
		// API mode: use secure HTTP API instead of direct Redis
		Debug("using API mode (device_api_token found)")
		useAPIMode = true

		apiBaseURL := deviceConfig.APIBaseURL
		if apiBaseURL == "" {
			apiBaseURL = authServiceURL // Default to auth service URL
		}
		Debug("API base URL: %s", apiBaseURL)

		if workQueue == "" {
			workQueue = os.Getenv("WORKER_QUEUE")
		}
		if workGroup == "" {
			workGroup = os.Getenv("CONSUMER_GROUP")
		}

		apiSource = worker.NewAPISource(worker.APISourceConfig{
			BaseURL:       apiBaseURL,
			Token:         deviceConfig.DeviceAPIToken,
			QueueName:     workQueue,
			ConsumerGroup: workGroup,
			BlockMs:       workPollMs,
			MaxAttempts:   workMaxRetries,
			DebugFunc:     Debug,
		})

		// Connect early so client is available for heartbeat
		if err := apiSource.Connect(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "Error: Failed to connect to Redis API: %v\n", err)
			os.Exit(1)
		}

		// Enable WebSocket for real-time pub/sub (heartbeat, streaming responses)
		if err := apiSource.Client().EnableWebSocket(ctx); err != nil {
			// WebSocket is optional - fall back to HTTP
			Debug("WebSocket not available, using HTTP for pub/sub: %v", err)
		} else {
			Debug("WebSocket enabled for real-time pub/sub")
		}

		source = apiSource
		streamFactory = worker.CreateAPIStreamWriterFactory(ctx, apiSource)

		fmt.Println("   - Mode: Redis API (secure)")
		if deviceConfig.UserEmail != "" {
			if deviceConfig.UserName != "" {
				fmt.Printf("   - Account: %s (%s)\n", deviceConfig.UserEmail, deviceConfig.UserName)
			} else {
				fmt.Printf("   - Account: %s\n", deviceConfig.UserEmail)
			}
		}
	} else {
		// Legacy mode: direct Redis connection
		Debug("using direct Redis mode")

		// Resolve Redis URL: flag > env > config
		Debug("resolving Redis URL...")
		Debug("--redis-url flag: %q", workRedisURL)
		Debug("REDIS_URL env: %q", os.Getenv("REDIS_URL"))
		if workRedisURL == "" {
			workRedisURL = os.Getenv("REDIS_URL")
		}
		if workRedisURL == "" && deviceConfig != nil {
			Debug("redis URL from config: %q", deviceConfig.RedisURL)
			workRedisURL = deviceConfig.RedisURL
		}
		if workRedisURL == "" {
			// Fallback to old config reading for backwards compatibility
			configURL := getRedisURLFromConfig()
			Debug("redis URL from legacy config: %q", configURL)
			workRedisURL = configURL
		}
		if workRedisURL == "" {
			fmt.Fprintf(os.Stderr, "Error: Redis URL not configured.\n")
			fmt.Fprintf(os.Stderr, "Run 'citadel init' to configure your node, or set REDIS_URL env var.\n")
			os.Exit(1)
		}
		Debug("final Redis URL: %s", workRedisURL)

		if workRedisPass == "" {
			workRedisPass = os.Getenv("REDIS_PASSWORD")
		}
		if workQueue == "" {
			workQueue = os.Getenv("WORKER_QUEUE")
		}
		if workGroup == "" {
			workGroup = os.Getenv("CONSUMER_GROUP")
		}

		// Create Redis job source
		redisSource := worker.NewRedisSource(worker.RedisSourceConfig{
			URL:           workRedisURL,
			Password:      workRedisPass,
			QueueName:     workQueue,
			ConsumerGroup: workGroup,
			BlockMs:       workPollMs,
			MaxAttempts:   workMaxRetries,
		})
		source = redisSource
		streamFactory = worker.CreateRedisStreamWriterFactory(ctx, redisSource)

		fmt.Println("   - Mode: Direct Redis (legacy)")
	}

	// Log mode for debugging
	_ = useAPIMode

	// Create worker ID
	workerID := fmt.Sprintf("citadel-%s", uuid.New().String()[:8])

	// Ensure network connection is established (reconnects if state exists)
	// This is needed to get the actual Headscale-assigned hostname
	Debug("verifying network connection...")
	if connected, err := network.VerifyOrReconnect(ctx); err != nil {
		Debug("network reconnect failed: %v", err)
	} else if connected {
		Debug("network connected")
	} else {
		Debug("network not configured (no saved state)")
	}

	// Get node name - prefer the actual registered name from network (Headscale-assigned)
	nodeName := workNodeName
	Debug("resolving node name...")
	Debug("workNodeName flag: %q", workNodeName)
	Debug("CITADEL_NODE_NAME env: %q", os.Getenv("CITADEL_NODE_NAME"))
	if nodeName == "" {
		nodeName = os.Getenv("CITADEL_NODE_NAME")
	}
	if nodeName == "" {
		// Try to get the actual registered hostname from network status
		// This returns the Headscale-assigned name (e.g., "ubuntu-gpu-8gluaaom") not just the requested name
		if netStatus, err := network.GetGlobalStatus(ctx); err == nil && netStatus.Connected && netStatus.Hostname != "" {
			Debug("got hostname from network status: %s", netStatus.Hostname)
			nodeName = netStatus.Hostname
		} else {
			// Fallback to local hostname
			hostname, _ := os.Hostname()
			Debug("using local hostname fallback: %s", hostname)
			nodeName = hostname
		}
	}
	Debug("final node name: %s", nodeName)

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

	// Start status publisher if enabled
	if workRedisStatus {
		// Create collector if not already created
		if collector == nil {
			collector = status.NewCollector(status.CollectorConfig{
				NodeName:  nodeName,
				ConfigDir: "",
				Services:  nil,
			})
		}

		if useAPIMode && apiSource != nil {
			// API mode: use secure API publisher
			// Get org ID from device config (saved during init)
			orgID := ""
			if deviceConfig != nil {
				orgID = deviceConfig.OrgID
			}
			// Fallback to manifest if not in device config
			if orgID == "" {
				if manifest, _, err := findAndReadManifest(); err == nil {
					orgID = manifest.Node.OrgID
				}
			}

			if orgID == "" {
				fmt.Fprintln(os.Stderr, "   - ⚠️ API status publisher requires org-id (run 'citadel init' first)")
			} else {
				apiPublisher, err := heartbeat.NewAPIPublisher(heartbeat.APIPublisherConfig{
					Client:    apiSource.Client(),
					NodeID:    nodeName,
					OrgID:     orgID,
					DebugFunc: Debug,
				}, collector)
				if err != nil {
					fmt.Fprintf(os.Stderr, "   - ⚠️ Failed to create API publisher: %v\n", err)
				} else {
					go func() {
						fmt.Printf("   - API status: %s (every 30s)\n", apiPublisher.PubSubChannel())
						if err := apiPublisher.Start(ctx); err != nil && err != context.Canceled {
							fmt.Fprintf(os.Stderr, "   - ⚠️ API status publisher error: %v\n", err)
						}
					}()
				}
			}
		} else if workRedisURL != "" {
			// Legacy mode: direct Redis publisher
			// Get device code from flag or environment
			deviceCode := workDeviceCode
			if deviceCode == "" {
				deviceCode = os.Getenv("CITADEL_DEVICE_CODE")
			}

			redisPublisher, err := heartbeat.NewRedisPublisher(heartbeat.RedisPublisherConfig{
				RedisURL:        workRedisURL,
				RedisPassword:   workRedisPass,
				NodeID:          nodeName,
				DeviceCode:      deviceCode,
				ChannelOverride: workStatusChannel,
				DebugFunc:       Debug,
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

// getRedisURLFromConfig reads Redis URL from global config file.
// Returns empty string if not configured.
func getRedisURLFromConfig() string {
	globalConfigFile := filepath.Join(platform.ConfigDir(), "config.yaml")
	data, err := os.ReadFile(globalConfigFile)
	if err != nil {
		return ""
	}

	var config struct {
		RedisURL string `yaml:"redis_url"`
	}
	if err := yaml.Unmarshal(data, &config); err != nil {
		return ""
	}
	return config.RedisURL
}

// DeviceConfig holds device authentication configuration from the global config file.
type DeviceConfig struct {
	DeviceAPIToken string `yaml:"device_api_token"`
	APIBaseURL     string `yaml:"api_base_url"`
	OrgID          string `yaml:"org_id"`
	RedisURL       string `yaml:"redis_url"`
	UserEmail      string `yaml:"user_email"`
	UserName       string `yaml:"user_name"`
}

// getDeviceConfigFromFile reads device authentication config from global config file.
func getDeviceConfigFromFile() *DeviceConfig {
	globalConfigFile := filepath.Join(platform.ConfigDir(), "config.yaml")
	data, err := os.ReadFile(globalConfigFile)
	if err != nil {
		return nil
	}

	var config DeviceConfig
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil
	}

	// Return nil if no relevant config found
	if config.DeviceAPIToken == "" && config.RedisURL == "" {
		return nil
	}

	return &config
}

func init() {
	rootCmd.AddCommand(workCmd)

	// Redis flags
	workCmd.Flags().StringVar(&workRedisURL, "redis-url", "", "Redis connection URL (or set REDIS_URL env)")
	workCmd.Flags().StringVar(&workRedisPass, "redis-password", "", "Redis password (or set REDIS_PASSWORD env)")
	workCmd.Flags().StringVar(&workQueue, "queue", "", "Queue/stream name to consume from (default: jobs:v1:gpu-general)")
	workCmd.Flags().StringVar(&workGroup, "group", "", "Consumer group name (default: citadel-workers)")
	workCmd.Flags().IntVar(&workPollMs, "poll-ms", 5000, "Block timeout in milliseconds")
	workCmd.Flags().IntVar(&workMaxRetries, "max-retries", 3, "Maximum retry attempts before DLQ")
	workCmd.Flags().BoolVar(&workForceDirectRedis, "force-direct-redis", false, "Force direct Redis mode instead of API mode")

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
	workCmd.Flags().BoolVar(&workRedisStatus, "redis-status", true, "Enable Redis status publishing for real-time updates")
	workCmd.Flags().StringVar(&workDeviceCode, "device-code", "", "Device authorization code for config lookup (or set CITADEL_DEVICE_CODE env)")
	workCmd.Flags().StringVar(&workStatusChannel, "status-channel", "", "Override Redis pub/sub channel for status (default: node:status:{node-name})")

	// Terminal server flags
	workCmd.Flags().BoolVar(&workTerminal, "terminal", false, "Enable terminal WebSocket server for remote access")
	workCmd.Flags().IntVar(&workTerminalPort, "terminal-port", 7860, "Terminal server port (default: 7860)")

	// Service auto-start flags
	workCmd.Flags().BoolVar(&workNoServices, "no-services", false, "Skip auto-starting services from manifest")

	// Update check flags
	workCmd.Flags().BoolVar(&workNoUpdate, "no-update", false, "Skip checking for updates on startup")
}
