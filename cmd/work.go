// cmd/work.go
package cmd

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/capabilities"
	"github.com/aceteam-ai/citadel-cli/internal/gateway"
	"github.com/aceteam-ai/citadel-cli/internal/heartbeat"
	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/aceteam-ai/citadel-cli/internal/redisapi"
	internalServices "github.com/aceteam-ai/citadel-cli/internal/services"
	"github.com/aceteam-ai/citadel-cli/internal/status"
	"github.com/aceteam-ai/citadel-cli/internal/terminal"
	"github.com/aceteam-ai/citadel-cli/internal/tlscert"
	"github.com/aceteam-ai/citadel-cli/internal/usage"
	"github.com/aceteam-ai/citadel-cli/internal/worker"
	"github.com/google/uuid"
	goredis "github.com/redis/go-redis/v9"
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

	// Status server flags
	workStatusPort int
	workBaseURL    string
	workAPIKey     string
	workNodeName   string

	// SSH key sync flags
	workSSHSync     bool
	workSSHSyncMins int

	// Redis status publishing flags
	workRedisStatus   bool
	workDeviceCode    string
	workStatusChannel string

	// Terminal server flags
	workTerminal      bool
	workNoTerminal    bool
	workTerminalHost  string
	workTerminalPort  int
	workTerminalDebug bool

	// Service auto-start flags
	workNoServices bool

	// Update check flag
	workNoUpdate bool

	// Capability detection flags
	workCapabilities string
	workAutoDetect   bool

	// Concurrency flag
	workMaxConcurrency int

	// Workspace directory for file-operation handlers
	workWorkspaceDir string

	// Gateway flags
	workGateway        bool
	workNoGateway      bool
	workGatewayPort    int
	workGatewayBind    string
	workGatewayVNC     int
	workGatewayNoTLS   bool
	workGatewayCertDir string
)

var workCmd = &cobra.Command{
	Use:   "work",
	Short: "Start services and run the job worker (primary node command)",
	Long: `Unified Citadel worker that starts services and processes jobs.

This is the primary command for running a Citadel node. It:
  1. Auto-starts services from manifest (use --no-services to skip)
  2. Connects to the AceTeam API and consumes jobs via secure proxy
  3. Reports status via pub/sub (enabled by default)
  4. Subscribes to real-time config updates
  5. Starts HTTPS gateway on port 8443 (use --no-gateway to skip)
  6. Starts terminal server on port 7860 (use --no-terminal to skip)

Run 'citadel init' first to authenticate and configure the node.

Examples:
  # Run after citadel init (recommended — gateway + terminal enabled)
  citadel work

  # Disable the HTTPS gateway
  citadel work --no-gateway

  # Disable terminal access
  citadel work --no-terminal

  # Worker only (no gateway, no terminal)
  citadel work --no-gateway --no-terminal

  # Disable status publishing
  citadel work --redis-status=false

  # Run without auto-starting services
  citadel work --no-services`,
	Run: runWork,
}

func runWork(cmd *cobra.Command, args []string) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Apply --no-gateway / --no-terminal overrides (take precedence over defaults)
	if workNoGateway {
		workGateway = false
	}
	if workNoTerminal {
		workTerminal = false
	}

	// Setup signal handling
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		fmt.Println("\n   - Received shutdown signal...")
		cancel()
	}()

	// Note: Update check is now handled by root.go's PersistentPreRun

	// Auto-start services from manifest (unless --no-services is set)
	if !workNoServices {
		if err := autoStartServices(); err != nil {
			fmt.Fprintf(os.Stderr, "   - Warning: Service auto-start: %v\n", err)
		}
	}

	// Resolve node capabilities early (used for queue routing and heartbeat)
	var nodeCaps *capabilities.NodeCapabilities
	workManifest, workConfigDir, _ := findAndReadManifest()
	if workManifest != nil && workManifest.Capabilities != nil {
		nodeCaps = manifestToNodeCapabilities(workManifest.Capabilities)
		fmt.Println("   - Capabilities: loaded from manifest")
	} else {
		nodeCaps = capabilities.DetectNodeCapabilities()
		fmt.Println("   - Capabilities: auto-detected")
	}
	if nodeCaps != nil && nodeCaps.GPU != nil && len(nodeCaps.GPU.Devices) > 0 {
		fmt.Printf("   - GPUs: %d detected", nodeCaps.GPU.Count)
		fmt.Printf(" (%s", nodeCaps.GPU.Devices[0].Name)
		if nodeCaps.GPU.Devices[0].VRAMTag != "" {
			fmt.Printf(", %s", strings.ToUpper(nodeCaps.GPU.Devices[0].VRAMTag))
		}
		fmt.Printf(")\n")
	}
	if len(nodeCaps.Engines) > 0 {
		fmt.Printf("   - Engines: %s\n", strings.Join(nodeCaps.Engines, ", "))
	}
	if len(nodeCaps.Tags) > 0 {
		Debug("capability tags: %v", nodeCaps.Tags)
	}

	// Convert capabilities to status types for heartbeat
	var statusCaps *status.NodeCapabilities
	if nodeCaps != nil {
		statusCaps = &status.NodeCapabilities{
			Engines: nodeCaps.Engines,
			Tags:    nodeCaps.Tags,
		}
		if nodeCaps.GPU != nil {
			for _, dev := range nodeCaps.GPU.Devices {
				statusCaps.GPUs = append(statusCaps.GPUs, status.GPUCapability{
					Name:    dev.Name,
					VRAMMb:  dev.VRAMMb,
					Tag:     dev.Tag,
					VRAMTag: dev.VRAMTag,
				})
			}
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
	var streamFactory func(job *worker.Job) worker.StreamWriter
	var useAPIMode bool
	var apiSource *worker.APISource               // Keep reference for heartbeat
	var setNodeMeta func(nodeID, nodeName string) // Set after node identity is resolved

	if workRedisURL == "" && deviceConfig != nil && deviceConfig.DeviceAPIToken != "" {
		// API mode: use secure HTTP API instead of direct Redis.
		Debug("using API mode (device_api_token found)")
		useAPIMode = true

		apiBaseURL := deviceConfig.APIBaseURL
		if apiBaseURL == "" {
			apiBaseURL = authServiceURL // Default to auth service URL
		}
		Debug("API base URL: %s", apiBaseURL)

		// Fetch worker config from API (queue, consumer group, org).
		// This replaces the need for WORKER_QUEUE / CONSUMER_GROUP env vars.
		if workQueue == "" || workGroup == "" {
			tempClient := redisapi.NewClient(redisapi.ClientConfig{
				BaseURL:   apiBaseURL,
				Token:     deviceConfig.DeviceAPIToken,
				DebugFunc: Debug,
			})
			workerCfg, err := tempClient.FetchWorkerConfig(ctx)
			if err != nil {
				Debug("worker-config fetch failed: %v (using defaults)", err)
			} else if workerCfg != nil {
				Debug("worker-config: queue=%s, group=%s, org=%s",
					workerCfg.Queue, workerCfg.ConsumerGroup, workerCfg.OrgID)
				if workQueue == "" && workerCfg.Queue != "" {
					workQueue = workerCfg.Queue
				}
				if workGroup == "" && workerCfg.ConsumerGroup != "" {
					workGroup = workerCfg.ConsumerGroup
				}
				// Store org_id from API if not already in config
				if deviceConfig.OrgID == "" && workerCfg.OrgID != "" {
					deviceConfig.OrgID = workerCfg.OrgID
				}
			} else {
				Debug("worker-config: endpoint not available, using defaults")
			}
			_ = tempClient.Close()
		}

		// Build queue list: primary queue + per-org shell queue.
		// Ensure a base queue is always present so that appending
		// the shell queue does not suppress the NewAPISource default.
		var apiQueueNames []string
		if workQueue != "" {
			apiQueueNames = append(apiQueueNames, workQueue)
		}
		orgID := deviceConfig.OrgID
		if orgID != "" {
			shellQueue := shellQueueName(orgID)
			if len(apiQueueNames) == 0 {
				apiQueueNames = []string{"jobs:v1:cpu-general"}
			}
			apiQueueNames = append(apiQueueNames, shellQueue)
			Debug("shell queue: %s", shellQueue)
		}

		apiSource = worker.NewAPISource(worker.APISourceConfig{
			BaseURL:       apiBaseURL,
			Token:         deviceConfig.DeviceAPIToken,
			QueueNames:    apiQueueNames,
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
		setNodeMeta = func(nodeID, nodeName string) {
			apiSource.Client().SetNodeMeta(nodeID, nodeName)
		}

		fmt.Println("   - Mode: Redis API (secure)")
		if deviceConfig.UserEmail != "" {
			if deviceConfig.UserName != "" {
				fmt.Printf("   - Account: %s (%s)\n", deviceConfig.UserEmail, deviceConfig.UserName)
			} else {
				fmt.Printf("   - Account: %s\n", deviceConfig.UserEmail)
			}
		}
	} else if workRedisURL != "" || (deviceConfig != nil && deviceConfig.RedisURL != "") {
		// Direct Redis mode: either --debug-redis-url flag or legacy redis_url from config
		if workRedisURL == "" {
			workRedisURL = deviceConfig.RedisURL
			fmt.Fprintln(os.Stderr, "WARNING: Using legacy Redis URL from config. Run 'citadel init' again to upgrade to API mode.")
		} else {
			fmt.Fprintln(os.Stderr, "WARNING: Direct Redis mode is for debugging only. Run 'citadel init' for production use.")
		}
		Debug("using direct Redis mode")
		Debug("Redis URL: %s", workRedisURL)

		// Resolve queue names: explicit --queue takes priority, otherwise use capabilities
		var queueNames []string
		if workQueue != "" {
			// Explicit queue specified -- use it directly (backwards compat)
			queueNames = []string{workQueue}
		} else {
			// Use resolved node capabilities for queue routing
			var allCaps []capabilities.Capability

			// Add capabilities from auto-detected nodeCaps (already resolved above)
			if nodeCaps != nil && len(nodeCaps.Tags) > 0 {
				for _, tag := range nodeCaps.Tags {
					category := tag
					if idx := strings.Index(tag, ":"); idx > 0 {
						category = tag[:idx]
					}
					allCaps = append(allCaps, capabilities.Capability{Tag: tag, Category: category})
				}
			}

			// Also honor --capabilities flag for manual overrides
			if workCapabilities != "" {
				manual := capabilities.ParseTags(workCapabilities)
				allCaps = append(allCaps, manual...)
				for _, c := range manual {
					fmt.Printf("   - Manual tag: %s\n", c.Tag)
				}
			}

			if len(allCaps) > 0 {
				baseQueue := "jobs:v1:gpu-general"
				queueNames = capabilities.ResolveQueues(allCaps, baseQueue)
			}
		}

		// Add per-org shell queue if org_id is known.
		// Ensure the default base queue is present when no explicit queue
		// or capabilities were resolved (otherwise shell-only list would
		// suppress the default fallback inside NewRedisSource).
		orgID := ""
		if deviceConfig != nil {
			orgID = deviceConfig.OrgID
		}
		if orgID != "" {
			shellQ := shellQueueName(orgID)
			if len(queueNames) == 0 {
				queueNames = []string{"jobs:v1:gpu-general"}
			}
			queueNames = append(queueNames, shellQ)
			Debug("shell queue: %s", shellQ)
		}

		// Create Redis job source
		redisSource := worker.NewRedisSource(worker.RedisSourceConfig{
			URL:           workRedisURL,
			Password:      workRedisPass,
			QueueName:     workQueue,
			QueueNames:    queueNames,
			ConsumerGroup: workGroup,
			BlockMs:       workPollMs,
			MaxAttempts:   workMaxRetries,
		})
		source = redisSource
		streamFactory = worker.CreateRedisStreamWriterFactory(ctx, redisSource)
		setNodeMeta = func(nodeID, nodeName string) {
			if c := redisSource.Client(); c != nil {
				c.SetNodeMeta(nodeID, nodeName)
			}
		}

		fmt.Println("   - Mode: Direct Redis (debug)")
	} else {
		// Neither API token nor debug Redis URL available
		fmt.Fprintf(os.Stderr, "Error: Node not initialized. Run 'citadel init' first.\n")
		fmt.Fprintf(os.Stderr, "\n")
		fmt.Fprintf(os.Stderr, "  citadel init    Authenticate and connect to the AceTeam Network\n")
		fmt.Fprintf(os.Stderr, "\n")
		fmt.Fprintf(os.Stderr, "For development/debugging, use --debug-redis-url to connect directly.\n")
		os.Exit(1)
	}

	// Log mode for debugging
	_ = useAPIMode

	// Create worker ID
	workerID := fmt.Sprintf("citadel-%s", uuid.New().String()[:8])

	// Ensure network connection is established (reconnects if state exists)
	// This is needed to get the actual Headscale-assigned hostname.
	//
	// When the VPN state is stale (expired/revoked Headscale key), the
	// reconnect attempt times out and returns ErrStaleState. If the node
	// has a device API token, we attempt automatic re-authentication:
	//   1. Try reconnecting with existing state + fresh authkey (preserves IP)
	//   2. If that fails, clear state and reconnect from scratch (new IP)
	//   3. If no token is available, log a helpful error message
	Debug("verifying network connection...")
	connected, err := network.VerifyOrReconnect(ctx)
	if err != nil && errors.Is(err, network.ErrStaleState) {
		Debug("network state is stale, attempting auto-recovery...")
		fmt.Println("   - VPN state is stale, attempting auto-recovery...")

		connected = false // Reset for the recovery flow
		recovered := false

		// Only attempt auto-recovery if we have a device API token
		if deviceConfig != nil && deviceConfig.DeviceAPIToken != "" {
			apiBaseURL := deviceConfig.APIBaseURL
			if apiBaseURL == "" {
				apiBaseURL = authServiceURL
			}

			// Fetch a fresh authkey from the platform
			Debug("requesting fresh authkey from %s", apiBaseURL)
			freshKey, fetchErr := network.FetchFreshAuthkey(ctx, apiBaseURL, deviceConfig.DeviceAPIToken)
			if fetchErr != nil {
				Debug("failed to fetch fresh authkey: %v", fetchErr)
				fmt.Fprintf(os.Stderr, "   - Warning: Could not fetch fresh authkey: %v\n", fetchErr)
			} else {
				Debug("got fresh authkey, attempting reconnect with existing state...")

				// Attempt 1: reconnect with existing state + fresh key (preserves IP)
				if ok, reconnErr := network.ReconnectWithAuthKey(ctx, freshKey); reconnErr == nil && ok {
					Debug("reconnected with existing state (IP preserved)")
					fmt.Println("   - VPN reconnected (IP preserved)")
					connected = true
					recovered = true
				} else {
					Debug("reconnect with existing state failed: %v, clearing state...", reconnErr)

					// Attempt 2: clear state and connect from scratch (new IP/hostname)
					if clearErr := network.ClearState(); clearErr != nil {
						Debug("failed to clear network state: %v", clearErr)
					}
					freshCtx, freshCancel := context.WithTimeout(ctx, 30*time.Second)
					config := network.ServerConfig{
						Hostname:   getWorkHostname(),
						ControlURL: network.DefaultControlURL,
						StateDir:   network.GetStateDir(),
						AuthKey:    freshKey,
					}
					srv, connectErr := network.Connect(freshCtx, config)
					freshCancel()
					if connectErr == nil {
						_ = srv
						Debug("reconnected with fresh state (new IP)")
						fmt.Println("   - VPN reconnected (fresh state)")
						connected = true
						recovered = true
					} else {
						Debug("fresh connect also failed: %v", connectErr)
						fmt.Fprintf(os.Stderr, "   - Warning: VPN auto-recovery failed: %v\n", connectErr)
					}
				}
			}
		}

		if !recovered {
			fmt.Fprintln(os.Stderr, "   - Warning: VPN connection could not be restored automatically.")
			fmt.Fprintln(os.Stderr, "     Run 'citadel login --authkey <key>' to re-authenticate manually.")
		}
	} else if err != nil {
		Debug("network reconnect failed: %v", err)
	} else if connected {
		Debug("network connected")
	} else {
		Debug("network not configured (no saved state)")
	}

	// Get node name and Headscale node ID from network status
	nodeName := workNodeName
	var headscaleNodeID string // Headscale numeric node ID (e.g., "758")
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
			if netStatus.NodeID != "" {
				headscaleNodeID = netStatus.NodeID
				Debug("got Headscale node ID from network status: %s", headscaleNodeID)
			}
		} else {
			// Fallback to local hostname
			hostname, _ := os.Hostname()
			Debug("using local hostname fallback: %s", hostname)
			nodeName = hostname
		}
	} else {
		// Even with an explicit node name, resolve the Headscale node ID
		headscaleNodeID = network.GetGlobalNodeID(ctx)
		if headscaleNodeID != "" {
			Debug("got Headscale node ID: %s", headscaleNodeID)
		}
	}
	Debug("final node name: %s", nodeName)
	if headscaleNodeID != "" {
		Debug("final Headscale node ID: %s", headscaleNodeID)
	}

	// Set node identity on the stream event publisher for operator attribution.
	// Note: We keep using nodeName (hostname) here rather than the Headscale
	// numeric ID, because the usage/earnings pipeline may key on hostname.
	// The Headscale numeric ID is passed in the heartbeat payload via
	// headscaleNodeId for the Python NodeStatusWorker to use directly.
	if setNodeMeta != nil {
		setNodeMeta(nodeName, nodeName)
		Debug("node meta set: node_id=%s, node_name=%s", nodeName, nodeName)
	}

	// Open usage store for per-job compute tracking
	var usageStore *usage.Store
	if nodeDir, err := platform.DefaultNodeDir(""); err != nil {
		fmt.Fprintf(os.Stderr, "   - Warning: Usage tracking disabled (no node dir): %v\n", err)
	} else {
		usageDBPath := filepath.Join(nodeDir, "usage.db")
		store, err := usage.OpenStore(usageDBPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "   - Warning: Usage tracking disabled: %v\n", err)
		} else {
			usageStore = store
			defer usageStore.Close()
			Debug("usage store: %s", usageDBPath)
		}
	}

	// Resolve API key and base URL (used by status server, SSH sync, and terminal)
	apiKey := workAPIKey
	if apiKey == "" {
		apiKey = os.Getenv("CITADEL_API_KEY")
	}

	baseURL := workBaseURL
	if baseURL == "" {
		baseURL = os.Getenv("ACETEAM_URL")
	}
	if baseURL == "" {
		baseURL = os.Getenv("HEARTBEAT_URL") // backward compat
	}
	if baseURL == "" && deviceConfig != nil && deviceConfig.APIBaseURL != "" {
		baseURL = deviceConfig.APIBaseURL
	}
	if baseURL == "" {
		baseURL = "https://aceteam.ai"
	}

	// When --gateway is set, auto-enable the status server on port 8080 so the
	// gateway has a working upstream. This must happen before the status server
	// block below so that the full status server (with token cache, VPN listener,
	// desktop API, etc.) is started correctly.
	if workGateway && workStatusPort == 0 {
		workStatusPort = 8080
	}

	// Create status collector (used by status server and Redis status publisher)
	var collector *status.Collector
	if workStatusPort > 0 {
		collector = status.NewCollector(status.CollectorConfig{
			NodeName:     nodeName,
			ConfigDir:    "",
			Services:     nil,
			Capabilities: statusCaps,
		})
	}

	// Start status server if enabled
	if workStatusPort > 0 {
		serverCfg := status.ServerConfig{
			Port:    workStatusPort,
			Version: Version,
		}

		// Wire up desktop API auth if org ID is available
		statusOrgID := ""
		if deviceConfig != nil {
			statusOrgID = deviceConfig.OrgID
		}
		if statusOrgID == "" {
			if manifest, _, err := findAndReadManifest(); err == nil {
				statusOrgID = manifest.Node.OrgID
			}
		}
		if statusOrgID != "" && baseURL != "" {
			statusAPIToken := ""
			if deviceConfig != nil {
				statusAPIToken = deviceConfig.DeviceAPIToken
			}
			serverCfg.TokenValidator = terminal.NewCachingTokenValidator(baseURL, statusOrgID, statusAPIToken, 30*time.Second)
			serverCfg.OrgID = statusOrgID
		}

		// Auto-register desktop endpoints when VNC is running and auth is available
		statusVNC := platform.GetVNCManager()
		if statusVNC.IsRunning() {
			if serverCfg.TokenValidator != nil {
				serverCfg.EnableDesktop = true
				Debug("VNC detected (port %d), desktop API enabled", statusVNC.Port())
			} else {
				fmt.Fprintln(os.Stderr, "   - ⚠️ VNC is running but desktop API disabled (no org ID for auth)")
			}
		}

		statusServer := status.NewServer(serverCfg, collector)

		// Add VPN listener so the status server is reachable over the tsnet VPN
		if network.IsGlobalConnected() {
			vpnAddr := fmt.Sprintf(":%d", workStatusPort)
			if vpnLn, err := network.Listen("tcp", vpnAddr); err != nil {
				Debug("status server VPN listener failed (LAN-only): %v", err)
			} else {
				statusServer.AddListener(vpnLn)
				fmt.Printf("   - Status server VPN: http://<vpn-ip>:%d\n", workStatusPort)
			}
		}

		go func() {
			if serverCfg.TokenValidator != nil {
				if cachingVal, ok := serverCfg.TokenValidator.(*terminal.CachingTokenValidator); ok {
					if err := cachingVal.Start(); err != nil {
						fmt.Fprintf(os.Stderr, "   - ⚠️ Desktop API token cache error: %v\n", err)
					}
				}
				if serverCfg.EnableDesktop {
					fmt.Printf("   - Status server: http://localhost:%d (desktop API enabled, VNC port %d)\n", workStatusPort, statusVNC.Port())
				} else {
					fmt.Printf("   - Status server: http://localhost:%d (auth enabled)\n", workStatusPort)
				}
			} else {
				fmt.Printf("   - Status server: http://localhost:%d\n", workStatusPort)
			}
			if err := statusServer.Start(ctx); err != nil && err != context.Canceled {
				fmt.Fprintf(os.Stderr, "   - ⚠️ Status server error: %v\n", err)
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
				NodeName:     nodeName,
				ConfigDir:    "",
				Services:     nil,
				Capabilities: statusCaps,
			})
		}

		// Log VNC status for heartbeat visibility
		heartbeatVNC := platform.GetVNCManager()
		if heartbeatVNC.IsRunning() {
			fmt.Printf("   - VNC detected: port %d (included in heartbeats)\n", heartbeatVNC.Port())
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
					Client:          apiSource.Client(),
					NodeID:          nodeName,
					HeadscaleNodeID: headscaleNodeID,
					OrgID:           orgID,
					DebugFunc:       Debug,
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
				HeadscaleNodeID: headscaleNodeID,
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
			// Get org ID from device config (saved during init), fall back to manifest
			orgID := ""
			if deviceConfig != nil {
				orgID = deviceConfig.OrgID
			}
			if orgID == "" && workManifest != nil {
				orgID = workManifest.Node.OrgID
			}
			if orgID == "" {
				if manifest, _, err := findAndReadManifest(); err == nil {
					orgID = manifest.Node.OrgID
				}
			}

			if orgID == "" {
				fmt.Fprintln(os.Stderr, "   - ⚠️ Terminal server requires org-id (run 'citadel init' first)")
			} else {
				termConfig := terminal.DefaultConfig()
				termConfig.OrgID = orgID
				termConfig.AuthServiceURL = baseURL
				termConfig.Version = Version
				if workTerminalHost != "" {
					termConfig.Host = workTerminalHost
				}
				if workTerminalPort > 0 {
					termConfig.Port = workTerminalPort
				}
				if workTerminalDebug {
					termConfig.Debug = true
				}

				// Use CachingTokenValidator for production
				termAPIToken := ""
				if deviceConfig != nil {
					termAPIToken = deviceConfig.DeviceAPIToken
				}
				cachingAuth := terminal.NewCachingTokenValidator(
					baseURL,
					orgID,
					termAPIToken,
					termConfig.TokenRefreshInterval,
				)

				termServer := terminal.NewServer(termConfig, cachingAuth)

				// Add VPN listener so the terminal server is reachable over the tsnet VPN
				if network.IsGlobalConnected() {
					vpnAddr := fmt.Sprintf(":%d", termConfig.Port)
					if vpnLn, err := network.Listen("tcp", vpnAddr); err != nil {
						Debug("terminal server VPN listener failed (LAN-only): %v", err)
					} else {
						termServer.AddListener(vpnLn)
						fmt.Printf("   - Terminal server VPN: ws://<vpn-ip>:%d/terminal\n", termConfig.Port)
					}
				}

				go func() {
					// Start the token cache
					if err := cachingAuth.Start(); err != nil {
						fmt.Fprintf(os.Stderr, "   - ⚠️ Token cache error: %v\n", err)
					}

					fmt.Printf("   - Terminal server: ws://%s:%d/terminal\n", termConfig.Host, termConfig.Port)
					if err := termServer.Start(); err != nil {
						fmt.Fprintf(os.Stderr, "   - ⚠️ Terminal server error: %v\n", err)
					}
				}()
			}
		}
	}

	// Start in-process HTTPS gateway if enabled
	if workGateway {
		// Validate that gateway port does not collide with upstream ports
		upstreamPorts := map[int]string{
			workStatusPort:   "status-port",
			workTerminalPort: "terminal-port",
			workGatewayVNC:   "gateway-vnc-port",
		}
		if name, collision := upstreamPorts[workGatewayPort]; collision {
			fmt.Fprintf(os.Stderr, "Error: gateway port %d collides with --%s; choose a different --gateway-port\n", workGatewayPort, name)
			os.Exit(1)
		}

		// Fetch VPN IPs for TLS certificate SANs
		var vpnIPs []net.IP
		if network.IsGlobalConnected() {
			if netStatus, err := network.GetGlobalStatus(ctx); err == nil && netStatus.Connected {
				if netStatus.IPv4 != "" {
					if ip := net.ParseIP(netStatus.IPv4); ip != nil {
						vpnIPs = append(vpnIPs, ip)
					}
				}
				if netStatus.IPv6 != "" {
					if ip := net.ParseIP(netStatus.IPv6); ip != nil {
						vpnIPs = append(vpnIPs, ip)
					}
				}
			}
		}

		// Set up TLS (unless --gateway-no-tls)
		var gwTLSConfig *tls.Config
		if !workGatewayNoTLS {
			cert, err := tlscert.EnsureCert(tlscert.Config{
				Hostname:    nodeName,
				IPAddresses: vpnIPs,
				CertDir:     workGatewayCertDir,
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: TLS certificate error: %v\n", err)
				os.Exit(1)
			}
			gwTLSConfig = &tls.Config{
				Certificates: []tls.Certificate{cert},
				MinVersion:   tls.VersionTLS12,
			}
			fmt.Printf("   - Gateway TLS: self-signed (cert: %s)\n", tlscert.CertPath(workGatewayCertDir))
		}

		// Build upstream addresses
		statusAddr := fmt.Sprintf("127.0.0.1:%d", workStatusPort)
		termAddr := fmt.Sprintf("127.0.0.1:%d", workTerminalPort)
		vncAddr := fmt.Sprintf("127.0.0.1:%d", workGatewayVNC)

		// Create gateway server
		gw := gateway.NewServer(gateway.Config{
			Port:          workGatewayPort,
			ListenAddress: fmt.Sprintf("%s:%d", workGatewayBind, workGatewayPort),
			TLSConfig:     gwTLSConfig,
			NodeName:      nodeName,
		})

		// Register upstreams (same routes as cmd/serve.go)
		gw.AddUpstream("/health", &gateway.Upstream{Address: statusAddr})
		gw.AddUpstream("/status", &gateway.Upstream{Address: statusAddr})
		gw.AddUpstream("/ping", &gateway.Upstream{Address: statusAddr})
		gw.AddUpstream("/services", &gateway.Upstream{Address: statusAddr})
		gw.AddUpstream("/api/screenshot", &gateway.Upstream{Address: statusAddr})
		gw.AddUpstream("/api/actions", &gateway.Upstream{Address: statusAddr})
		gw.AddUpstream("/ssh/authorized-keys", &gateway.Upstream{Address: statusAddr})

		gw.AddUpstream("/vnc", &gateway.Upstream{
			Address:     vncAddr,
			StripPrefix: true,
			WebSocket:   true,
		})

		gw.AddUpstream("/terminal", &gateway.Upstream{
			Address:     termAddr,
			StripPrefix: false,
			WebSocket:   true,
		})

		// Add VPN listener (TLS-wrapped) so the gateway is reachable over tsnet
		if network.IsGlobalConnected() {
			vpnAddr := fmt.Sprintf(":%d", workGatewayPort)
			rawLn, err := network.Listen("tcp", vpnAddr)
			if err != nil {
				Debug("gateway VPN listener failed (LAN-only): %v", err)
			} else {
				var vpnGwLn net.Listener
				if gwTLSConfig != nil {
					vpnGwLn = tls.NewListener(rawLn, gwTLSConfig)
				} else {
					vpnGwLn = rawLn
				}
				gw.AddListener(vpnGwLn)
			}
		}

		// Print route table
		scheme := "https"
		if workGatewayNoTLS {
			scheme = "http"
		}
		listenAddr := fmt.Sprintf("%s:%d", workGatewayBind, workGatewayPort)
		fmt.Printf("   - Gateway: %s://%s\n", scheme, listenAddr)
		fmt.Println("   - Routes:")
		fmt.Printf("     /health, /status, /ping  -> %s (status server)\n", statusAddr)
		fmt.Printf("     /api/screenshot, /api/actions -> %s\n", statusAddr)
		fmt.Printf("     /ssh/authorized-keys     -> %s (SSH key deploy)\n", statusAddr)
		fmt.Printf("     /vnc/...                 -> %s (websockify)\n", vncAddr)
		fmt.Printf("     /terminal/...            -> %s (terminal)\n", termAddr)

		if len(vpnIPs) > 0 {
			fmt.Printf("   - Gateway VPN: %s://%s:%d\n", scheme, vpnIPs[0], workGatewayPort)
		}

		go func() {
			if err := gw.Start(ctx); err != nil && err != context.Canceled {
				fmt.Fprintf(os.Stderr, "   - Warning: Gateway error: %v\n", err)
			}
		}()
	}

	// Start usage syncer if store is available
	if usageStore != nil {
		publishFn := createUsagePublishFn(useAPIMode, apiSource, workRedisURL, workRedisPass)
		if publishFn != nil {
			syncer := usage.NewSyncer(usage.SyncerConfig{
				Store:     usageStore,
				PublishFn: publishFn,
			})
			go func() {
				Debug("usage syncer started (60s interval)")
				if err := syncer.Start(ctx); err != nil && err != context.Canceled {
					fmt.Fprintf(os.Stderr, "   - Warning: Usage syncer error: %v\n", err)
				}
			}()
		} else {
			Debug("usage syncer: no publish target available (local-only tracking)")
		}
	}

	// Create handlers with optional workspace for file-operation jobs.
	wsDir := resolveWorkspaceDir()
	handlers := worker.CreateLegacyHandlersWithOpts(worker.LegacyHandlerOpts{
		WorkspaceDir: wsDir,
		ConfigDir:    workConfigDir,
	})

	// Build job record function for usage tracking
	var jobRecordFn func(record usage.UsageRecord)
	if usageStore != nil {
		jobRecordFn = func(record usage.UsageRecord) {
			record.NodeID = nodeName
			if err := usageStore.Insert(record); err != nil {
				Debug("usage insert error: %v", err)
			}
		}
	}

	// Resolve concurrency from flag or auto-detect from GPU count
	maxConcurrency := workMaxConcurrency
	var gpuTracker *worker.GPUTracker
	gpuCount := platform.GetGPUCountSimple()
	if gpuCount > 0 {
		gpuTracker = worker.NewGPUTracker(gpuCount)
		if maxConcurrency == 0 {
			maxConcurrency = gpuCount
		}
	}
	if maxConcurrency == 0 {
		maxConcurrency = 1 // Default: sequential
	}

	// Create runner
	runner := worker.NewRunner(source, handlers, worker.RunnerConfig{
		WorkerID:       workerID,
		Verbose:        true,
		JobRecordFn:    jobRecordFn,
		MaxConcurrency: maxConcurrency,
		GPUTracker:     gpuTracker,
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

// shellQueueName returns the per-org shell command queue name.
// Jobs dispatched by platform MCP tools (terminal_exec, code_read, etc.)
// are enqueued to this queue using the pattern jobs:v1:shell:org_{org_id}.
func shellQueueName(orgID string) string {
	return fmt.Sprintf("jobs:v1:shell:org_%s", orgID)
}

// getWorkHostname returns the hostname to use for VPN reconnection.
// Prefers the --node-name flag, then CITADEL_NODE_NAME env, then OS hostname.
func getWorkHostname() string {
	if workNodeName != "" {
		return workNodeName
	}
	if envName := os.Getenv("CITADEL_NODE_NAME"); envName != "" {
		return envName
	}
	hostname, _ := os.Hostname()
	if hostname == "" {
		return "citadel-node"
	}
	return hostname
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

// resolveWorkspaceDir returns the sandbox directory for file-operation handlers.
// Priority: --workspace flag > CITADEL_WORKSPACE env > ~/citadel-node/workspace default.
// Returns empty string if the directory cannot be resolved or created.
func resolveWorkspaceDir() string {
	dir := workWorkspaceDir
	if dir == "" {
		dir = os.Getenv("CITADEL_WORKSPACE")
	}
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, "citadel-node", "workspace")
	}
	// Ensure the directory exists.
	if err := os.MkdirAll(dir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "   - Warning: cannot create workspace dir %s: %v\n", dir, err)
		return ""
	}
	// Resolve to absolute path (handles relative flag values).
	abs, err := filepath.Abs(dir)
	if err != nil {
		return dir
	}
	return abs
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

// createUsagePublishFn returns a function that publishes usage records to Redis.
// Returns nil if no publish target is available (records will be stored locally only).
func createUsagePublishFn(useAPI bool, apiSource *worker.APISource, redisURL, redisPass string) usage.PublishFunc {
	const streamName = "node:usage:stream"
	const maxLen int64 = 50000

	if useAPI && apiSource != nil {
		// API mode: use the secure API client
		return func(ctx context.Context, records []usage.UsageRecord) error {
			client := apiSource.Client()
			for _, r := range records {
				payload, err := json.Marshal(usageStreamEntry{
					Version: "1.0",
					NodeID:  r.NodeID,
					Record:  usageRecordPayload(r),
				})
				if err != nil {
					return fmt.Errorf("marshal usage record: %w", err)
				}
				if err := client.StreamAdd(ctx, streamName, map[string]string{
					"data": string(payload),
				}, maxLen); err != nil {
					return fmt.Errorf("stream add: %w", err)
				}
			}
			return nil
		}
	}

	if redisURL != "" {
		// Direct Redis mode: create client once, reuse across sync cycles
		opts, err := goredis.ParseURL(redisURL)
		if err != nil {
			return nil
		}
		if redisPass != "" {
			opts.Password = redisPass
		}
		rdb := goredis.NewClient(opts)

		return func(ctx context.Context, records []usage.UsageRecord) error {
			for _, r := range records {
				payload, err := json.Marshal(usageStreamEntry{
					Version: "1.0",
					NodeID:  r.NodeID,
					Record:  usageRecordPayload(r),
				})
				if err != nil {
					return fmt.Errorf("marshal usage record: %w", err)
				}
				if err := rdb.XAdd(ctx, &goredis.XAddArgs{
					Stream: streamName,
					MaxLen: maxLen,
					Approx: true,
					Values: map[string]string{
						"data": string(payload),
					},
				}).Err(); err != nil {
					return fmt.Errorf("xadd: %w", err)
				}
			}
			return nil
		}
	}

	return nil
}

type usageStreamEntry struct {
	Version string          `json:"version"`
	NodeID  string          `json:"nodeId"`
	Record  usageRecordJSON `json:"record"`
}

// usageRecordJSON is the JSON representation published to Redis.
// ErrorMessage is intentionally excluded to avoid leaking internal error details.
type usageRecordJSON struct {
	JobID            string `json:"jobId"`
	JobType          string `json:"jobType"`
	Backend          string `json:"backend,omitempty"`
	Model            string `json:"model,omitempty"`
	Status           string `json:"status"`
	StartedAt        string `json:"startedAt"`
	CompletedAt      string `json:"completedAt"`
	DurationMs       int64  `json:"durationMs"`
	PromptTokens     int64  `json:"promptTokens,omitempty"`
	CompletionTokens int64  `json:"completionTokens,omitempty"`
	TotalTokens      int64  `json:"totalTokens,omitempty"`
	RequestBytes     int64  `json:"requestBytes,omitempty"`
	ResponseBytes    int64  `json:"responseBytes,omitempty"`
}

func usageRecordPayload(r usage.UsageRecord) usageRecordJSON {
	return usageRecordJSON{
		JobID:            r.JobID,
		JobType:          r.JobType,
		Backend:          r.Backend,
		Model:            r.Model,
		Status:           r.Status,
		StartedAt:        r.StartedAt.UTC().Format(time.RFC3339),
		CompletedAt:      r.CompletedAt.UTC().Format(time.RFC3339),
		DurationMs:       r.DurationMs,
		PromptTokens:     r.PromptTokens,
		CompletionTokens: r.CompletionTokens,
		TotalTokens:      r.TotalTokens,
		RequestBytes:     r.RequestBytes,
		ResponseBytes:    r.ResponseBytes,
	}
}

func init() {
	rootCmd.AddCommand(workCmd)

	// Queue flags
	workCmd.Flags().StringVar(&workQueue, "queue", "", "Queue/stream name to consume from (default: jobs:v1:cpu-general)")
	workCmd.Flags().StringVar(&workGroup, "group", "", "Consumer group name (default: citadel-workers)")
	workCmd.Flags().IntVar(&workPollMs, "poll-ms", 5000, "Block timeout in milliseconds")
	workCmd.Flags().IntVar(&workMaxRetries, "max-retries", 3, "Maximum retry attempts before DLQ")

	// Debug flags (hidden) - direct Redis for development/debugging only
	workCmd.Flags().StringVar(&workRedisURL, "debug-redis-url", "", "Direct Redis URL for debugging (bypasses API mode)")
	workCmd.Flags().StringVar(&workRedisPass, "debug-redis-password", "", "Redis password for debugging")
	workCmd.Flags().MarkHidden("debug-redis-url")
	workCmd.Flags().MarkHidden("debug-redis-password")

	// Status flags
	workCmd.Flags().IntVar(&workStatusPort, "status-port", 0, "Enable status HTTP server on port (0 = disabled)")
	workCmd.Flags().StringVar(&workBaseURL, "base-url", "", "AceTeam API base URL (default: https://aceteam.ai, or set ACETEAM_URL env)")
	workCmd.Flags().StringVar(&workAPIKey, "api-key", "", "API key for authentication (or set CITADEL_API_KEY env)")
	workCmd.Flags().StringVar(&workNodeName, "node-name", "", "Node name for status reporting (default: hostname)")

	// SSH key sync flags
	workCmd.Flags().BoolVar(&workSSHSync, "ssh-sync", false, "Enable SSH authorized_keys synchronization from AceTeam")
	workCmd.Flags().IntVar(&workSSHSyncMins, "ssh-sync-interval", 5, "SSH sync interval in minutes")

	// Redis status publishing flags
	workCmd.Flags().BoolVar(&workRedisStatus, "redis-status", true, "Enable Redis status publishing for real-time updates")
	workCmd.Flags().StringVar(&workDeviceCode, "device-code", "", "Device authorization code for config lookup (or set CITADEL_DEVICE_CODE env)")
	workCmd.Flags().StringVar(&workStatusChannel, "status-channel", "", "Override Redis pub/sub channel for status (default: node:status:{node-name})")

	// Terminal server flags
	workCmd.Flags().BoolVar(&workTerminal, "terminal", true, "Enable terminal WebSocket server (enabled by default; use --no-terminal to disable)")
	workCmd.Flags().BoolVar(&workNoTerminal, "no-terminal", false, "Disable the terminal WebSocket server")
	workCmd.Flags().StringVar(&workTerminalHost, "terminal-host", "", "Terminal server bind address (default: 0.0.0.0)")
	workCmd.Flags().IntVar(&workTerminalPort, "terminal-port", 7860, "Terminal server port (default: 7860)")
	workCmd.Flags().BoolVar(&workTerminalDebug, "terminal-debug", false, "Enable verbose debug logging for terminal server")

	// Service auto-start flags
	workCmd.Flags().BoolVar(&workNoServices, "no-services", false, "Skip auto-starting services from manifest")

	// Update check flags (deprecated - update check now runs on all commands via root.go)
	workCmd.Flags().BoolVar(&workNoUpdate, "no-update", false, "(Deprecated) No longer has any effect - use 'citadel update disable' instead")
	workCmd.Flags().MarkDeprecated("no-update", "use 'citadel update disable' to disable auto-update checks")

	// Capability detection flags
	workCmd.Flags().StringVar(&workCapabilities, "capabilities", "", "Additional comma-separated capability tags (e.g., gpu:rtx4090,llm:llama3)")
	workCmd.Flags().BoolVar(&workAutoDetect, "auto-detect", false, "(Deprecated) Capabilities are now always auto-detected unless declared in citadel.yaml")

	// Concurrency flags
	workCmd.Flags().IntVar(&workMaxConcurrency, "max-concurrency", 0, "Maximum concurrent jobs (0 = auto-detect from GPU count)")

	// Workspace flags
	workCmd.Flags().StringVar(&workWorkspaceDir, "workspace", "", "Workspace directory for file-operation jobs (or set CITADEL_WORKSPACE env)")

	// Gateway flags
	workCmd.Flags().BoolVar(&workGateway, "gateway", true, "Start the HTTPS gateway in-process (enabled by default; use --no-gateway to disable)")
	workCmd.Flags().BoolVar(&workNoGateway, "no-gateway", false, "Disable the HTTPS gateway")
	workCmd.Flags().IntVar(&workGatewayPort, "gateway-port", 8443, "HTTPS gateway port (default: 8443)")
	workCmd.Flags().StringVar(&workGatewayBind, "gateway-bind", "0.0.0.0", "Gateway bind address")
	workCmd.Flags().IntVar(&workGatewayVNC, "gateway-vnc-port", 6080, "VNC websockify port for gateway proxy")
	workCmd.Flags().BoolVar(&workGatewayNoTLS, "gateway-no-tls", false, "Disable TLS on the gateway (for testing only)")
	workCmd.Flags().StringVar(&workGatewayCertDir, "gateway-cert-dir", "", "Custom directory for gateway TLS certificates")
	workCmd.Flags().MarkHidden("gateway-no-tls")
}
