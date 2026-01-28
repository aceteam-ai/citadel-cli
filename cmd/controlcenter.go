// cmd/controlcenter.go
package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/demo"
	"github.com/aceteam-ai/citadel-cli/internal/heartbeat"
	"github.com/aceteam-ai/citadel-cli/internal/terminal"
	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/aceteam-ai/citadel-cli/internal/status"
	"github.com/aceteam-ai/citadel-cli/internal/tui"
	"github.com/aceteam-ai/citadel-cli/internal/tui/controlcenter"
	"github.com/aceteam-ai/citadel-cli/internal/worker"
	"github.com/aceteam-ai/citadel-cli/services"
	"github.com/google/uuid"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/mem"
)

// Worker state for TUI management
var (
	ccWorkerMu        sync.Mutex
	ccWorkerRunning   bool
	ccWorkerCancel    context.CancelFunc
	ccWorkerQueue     string
	ccActivityFn      func(level, msg string)
	ccHeartbeatFn     func(active bool) // Callback when heartbeat publishes
)

// Demo server state
var (
	ccDemoServer  *demo.Server
	ccDemoCancel  context.CancelFunc
	ccDemoPort    = 7777
)

// Terminal server state
var (
	ccTerminalServer  *terminal.Server
	ccTerminalAuth    *terminal.CachingTokenValidator
	ccTerminalPort    = 7860
	ccTerminalRunning bool
)

// runControlCenter launches the unified control center TUI
func runControlCenter() {
	if !tui.IsTTY() {
		fmt.Fprintln(os.Stderr, "Control center requires a terminal. Use --daemon for background mode.")
		os.Exit(1)
	}

	// Suppress tsnet/tailscale logs to prevent TUI corruption
	network.SuppressLogs()

	// Start the demo server in the background
	startDemoServer()
	defer stopDemoServer()
	defer stopTerminalServer()

	cfg := controlcenter.Config{
		Version:            Version,
		AuthServiceURL:     authServiceURL,
		NexusURL:           nexusURL,
		RefreshFn:          gatherControlCenterData,
		StartServiceFn:     ccStartService,
		StopServiceFn:      ccStopService,
		RestartServiceFn:   ccRestartService,
		AddServiceFn:       ccAddService,
		GetServicesFn:      services.GetAvailableServices,
		GetConfiguredFn:    ccGetConfiguredServices,
		GetServiceDetailFn: ccGetServiceDetail,
		GetServiceLogsFn:   ccGetServiceLogs,
		DeviceAuth: controlcenter.DeviceAuthCallbacks{
			StartFlow:  ccStartDeviceAuthFlow,
			PollToken:  ccPollDeviceAuthToken,
			Connect:    ccConnectWithAuthkey,
			Disconnect: ccDisconnect,
		},
		Worker: controlcenter.WorkerCallbacks{
			Start:     ccStartWorker,
			Stop:      ccStopWorker,
			IsRunning: ccWorkerIsRunning,
		},
	}

	cc := controlcenter.New(cfg)

	// Show deferred update notification in TUI activity log (if any)
	if deferredUpdateNotification != "" {
		cc.AddActivity("info", fmt.Sprintf("Update available: %s -> %s (run 'citadel update install')", Version, deferredUpdateNotification))
	}

	// Set heartbeat callback so worker can update TUI
	ccHeartbeatFn = cc.UpdateHeartbeat

	// Auto-connect to network and start worker
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		networkConnected := false

		// Try to reconnect if we have saved state
		if network.HasState() {
			cc.AddActivity("info", "Reconnecting to network...")
			if connected, err := network.VerifyOrReconnect(ctx); err != nil {
				cc.AddActivity("warning", fmt.Sprintf("Network reconnect failed: %v", err))
			} else if connected {
				cc.AddActivity("success", "Connected to AceTeam Network")
				networkConnected = true
			}
		}

		// Gather initial data after network check
		if data, err := gatherControlCenterData(); err == nil {
			cc.UpdateData(data)
			// Update networkConnected based on actual status
			networkConnected = data.Connected
		}

		// Auto-start worker and terminal server after network is connected
		if networkConnected {
			// Start terminal server for remote SSH access
			if orgID := getOrgIDFromConfig(); orgID != "" {
				if err := startTerminalServer(orgID); err != nil {
					cc.AddActivity("warning", fmt.Sprintf("Terminal server failed: %v", err))
				} else {
					cc.AddActivity("info", fmt.Sprintf("Terminal server listening on port %d", ccTerminalPort))
				}
			}

			deviceConfig := getDeviceConfigFromFile()
			if deviceConfig != nil && (deviceConfig.DeviceAPIToken != "" || deviceConfig.RedisURL != "") {
				// Small delay to ensure network is fully ready
				time.Sleep(500 * time.Millisecond)
				cc.AddActivity("info", "Starting worker...")
				if err := ccStartWorker(cc.AddActivity); err != nil {
					cc.AddActivity("warning", fmt.Sprintf("Worker auto-start failed: %v", err))
				} else {
					// Refresh to update worker status in UI
					time.Sleep(500 * time.Millisecond)
					if data, err := gatherControlCenterData(); err == nil {
						cc.UpdateData(data)
					}
				}
			}
		} else {
			// Not connected - show login prompt after a short delay
			// to allow the TUI to fully initialize
			time.Sleep(1 * time.Second)
			cc.QueueShowLoginPrompt()
		}
	}()

	if err := cc.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Control center error: %v\n", err)
		os.Exit(1)
	}
}

// startDemoServer starts the demo HTTP server in the background
func startDemoServer() {
	// Create info callback for demo server
	getInfo := func() demo.NodeInfo {
		hostname, _ := os.Hostname()
		info := demo.NodeInfo{
			Hostname: hostname,
			Version:  Version,
		}

		// Check network connection
		if network.IsGlobalConnected() {
			info.Connected = true
			if ip, err := network.GetGlobalIPv4(); err == nil {
				info.NetworkIP = ip
			}
		}

		// Get services from manifest
		if manifest, _, err := findAndReadManifest(); err == nil {
			for _, svc := range manifest.Services {
				info.Services = append(info.Services, svc.Name)
			}
		}

		return info
	}

	ccDemoServer = demo.NewServer(ccDemoPort, Version, getInfo)

	// Start in background with cancellable context
	var ctx context.Context
	ctx, ccDemoCancel = context.WithCancel(context.Background())
	go func() {
		// Ignore error - server runs until cancelled
		_ = ccDemoServer.Start(ctx)
	}()
}

// stopDemoServer stops the demo HTTP server
func stopDemoServer() {
	if ccDemoCancel != nil {
		ccDemoCancel()
	}
	if ccDemoServer != nil {
		_ = ccDemoServer.Stop()
	}
}

// startTerminalServer starts the terminal server for remote SSH access
func startTerminalServer(orgID string) error {
	if ccTerminalRunning {
		return nil // Already running
	}

	// Build configuration
	config := terminal.DefaultConfig()
	config.OrgID = orgID
	config.AuthServiceURL = authServiceURL
	config.Port = ccTerminalPort

	// Create the caching token validator
	ccTerminalAuth = terminal.NewCachingTokenValidator(
		config.AuthServiceURL,
		config.OrgID,
		config.TokenRefreshInterval,
	)

	// Set log callback to suppress stdout output in TUI mode
	// Warnings will be silently ignored to prevent TUI corruption
	ccTerminalAuth.SetLogFn(func(level, msg string) {
		// In TUI mode, terminal server warnings are suppressed
		// since they would corrupt the display
	})

	// Start the validator's background refresh
	if err := ccTerminalAuth.Start(); err != nil {
		return fmt.Errorf("failed to start token cache: %w", err)
	}

	// Create and start the server
	ccTerminalServer = terminal.NewServer(config, ccTerminalAuth)
	if err := ccTerminalServer.Start(); err != nil {
		ccTerminalAuth.Stop()
		return fmt.Errorf("failed to start terminal server: %w", err)
	}

	ccTerminalRunning = true
	return nil
}

// stopTerminalServer stops the terminal server
func stopTerminalServer() {
	if !ccTerminalRunning {
		return
	}

	if ccTerminalAuth != nil {
		ccTerminalAuth.Stop()
	}

	if ccTerminalServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = ccTerminalServer.Stop(ctx)
	}

	ccTerminalRunning = false
}

// getOrgIDFromConfig gets the organization ID from manifest or device config
func getOrgIDFromConfig() string {
	// Try manifest first
	if manifest, _, err := findAndReadManifest(); err == nil && manifest.Node.OrgID != "" {
		return manifest.Node.OrgID
	}
	// Try device config
	if deviceConfig := getDeviceConfigFromFile(); deviceConfig != nil && deviceConfig.OrgID != "" {
		return deviceConfig.OrgID
	}
	return ""
}

// gatherControlCenterData collects all data for the control center
func gatherControlCenterData() (controlcenter.StatusData, error) {
	data := controlcenter.StatusData{
		Version: Version,
	}

	// Load manifest
	manifest, configDir, _ := findAndReadManifest()
	if manifest != nil {
		data.NodeName = manifest.Node.Name
		data.OrgID = manifest.Node.OrgID
	}

	// Load user info from device config
	if deviceConfig := getDeviceConfigFromFile(); deviceConfig != nil {
		data.UserEmail = deviceConfig.UserEmail
		data.UserName = deviceConfig.UserName
		if data.OrgID == "" && deviceConfig.OrgID != "" {
			data.OrgID = deviceConfig.OrgID
		}
	}

	// Get hostname if not in manifest
	if data.NodeName == "" {
		data.NodeName, _ = os.Hostname()
	}

	// System vitals - Memory
	if v, err := mem.VirtualMemory(); err == nil {
		data.MemoryPercent = v.UsedPercent
		data.MemoryUsed = formatBytes(v.Used)
		data.MemoryTotal = formatBytes(v.Total)
	}

	// System vitals - CPU
	if percentages, err := cpu.Percent(200*time.Millisecond, false); err == nil && len(percentages) > 0 {
		data.CPUPercent = percentages[0]
	}

	// System vitals - Disk
	if d, err := disk.Usage("/"); err == nil {
		data.DiskPercent = d.UsedPercent
		data.DiskUsed = formatBytes(d.Used)
		data.DiskTotal = formatBytes(d.Total)
	}

	// GPU info
	if detector, err := platform.GetGPUDetector(); err == nil && detector.HasGPU() {
		if gpus, err := detector.GetGPUInfo(); err == nil && len(gpus) > 0 {
			gpu := gpus[0]
			data.GPUName = gpu.Name
			data.GPUMemory = gpu.Memory
			data.GPUTemp = gpu.Temperature
			if gpu.Utilization != "" {
				utilStr := strings.TrimSuffix(gpu.Utilization, "%")
				if util, err := strconv.ParseFloat(utilStr, 64); err == nil {
					data.GPUUtilization = util
				}
			}
		}
	}

	// Network status - check connection state without blocking
	if network.HasState() {
		// Check if already connected (don't try to reconnect, that blocks)
		if network.IsGlobalConnected() {
			data.Connected = true

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			if status, err := network.GetGlobalStatus(ctx); err == nil {
				data.NodeIP = status.IPv4
				if data.NodeName == "" {
					data.NodeName = status.Hostname
				}
			}
			cancel()

			// Get peers (with short timeout)
			ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
			myIP, _ := network.GetGlobalIPv4()
			if peers, err := network.GetGlobalPeers(ctx2); err == nil {
				for _, peer := range peers {
					if peer.IP != myIP {
						peerInfo := controlcenter.PeerInfo{
							Hostname: peer.Hostname,
							IP:       peer.IP,
							Online:   peer.Online,
						}

						// Skip ping for now to avoid blocking
						data.Peers = append(data.Peers, peerInfo)
					}
				}
			}
			cancel2()
		}
	}

	// Services
	if manifest != nil && configDir != "" {
		for _, service := range manifest.Services {
			svcInfo := controlcenter.ServiceInfo{
				Name:   service.Name,
				Status: "stopped",
			}

			fullComposePath := filepath.Join(configDir, service.ComposeFile)
			if _, err := os.Stat(fullComposePath); err == nil {
				psCmd := exec.Command("docker", "compose", "-f", fullComposePath, "ps", "--format", "json")
				if output, err := psCmd.Output(); err == nil {
					var containers []struct {
						State  string `json:"State"`
						Status string `json:"Status"`
					}
					decoder := json.NewDecoder(strings.NewReader(string(output)))
					for decoder.More() {
						var c struct {
							State  string `json:"State"`
							Status string `json:"Status"`
						}
						if err := decoder.Decode(&c); err == nil {
							containers = append(containers, c)
						}
					}
					if len(containers) > 0 {
						state := strings.ToLower(containers[0].State)
						if strings.Contains(state, "running") || strings.Contains(state, "up") {
							svcInfo.Status = "running"
							// Try to extract uptime from Status field
							if containers[0].Status != "" {
								svcInfo.Uptime = extractUptime(containers[0].Status)
							}
						} else if strings.Contains(state, "exited") || strings.Contains(state, "dead") {
							svcInfo.Status = "stopped"
						} else {
							svcInfo.Status = state
						}
					}
				}
			}

			data.Services = append(data.Services, svcInfo)
		}
	}

	// Detect system tailscale (dual connection)
	running, ip, name, sameNetwork := controlcenter.DetectSystemTailscale(nexusURL)
	data.SystemTailscaleRunning = running
	data.SystemTailscaleIP = ip
	data.SystemTailscaleName = name
	data.DualConnection = running && sameNetwork && data.Connected

	// Worker status
	ccWorkerMu.Lock()
	data.WorkerRunning = ccWorkerRunning
	data.WorkerQueue = ccWorkerQueue
	ccWorkerMu.Unlock()

	// Demo server URL (always running when control center is active)
	data.DemoServerURL = fmt.Sprintf("http://localhost:%d", ccDemoPort)

	// Terminal server URL (only shown when running and connected)
	if ccTerminalRunning {
		data.TerminalServerURL = fmt.Sprintf("ws://localhost:%d/terminal", ccTerminalPort)
	}

	return data, nil
}

// extractUptime tries to extract uptime from docker status string like "Up 2 hours"
func extractUptime(status string) string {
	status = strings.ToLower(status)
	if uptime, found := strings.CutPrefix(status, "up "); found {
		return uptime
	}
	return ""
}

// ccStartService starts a service by name
func ccStartService(name string) error {
	manifest, configDir, err := findAndReadManifest()
	if err != nil {
		return err
	}

	for _, service := range manifest.Services {
		if service.Name == name {
			fullComposePath := filepath.Join(configDir, service.ComposeFile)
			cmd := exec.Command("docker", "compose", "-f", fullComposePath, "-p", "citadel-"+name, "up", "-d")
			return cmd.Run()
		}
	}

	return fmt.Errorf("service not found: %s", name)
}

// ccStopService stops a service by name
func ccStopService(name string) error {
	manifest, configDir, err := findAndReadManifest()
	if err != nil {
		return err
	}

	for _, service := range manifest.Services {
		if service.Name == name {
			fullComposePath := filepath.Join(configDir, service.ComposeFile)
			cmd := exec.Command("docker", "compose", "-f", fullComposePath, "-p", "citadel-"+name, "down")
			return cmd.Run()
		}
	}

	return fmt.Errorf("service not found: %s", name)
}

// ccRestartService restarts a service by name
func ccRestartService(name string) error {
	manifest, configDir, err := findAndReadManifest()
	if err != nil {
		return err
	}

	for _, service := range manifest.Services {
		if service.Name == name {
			fullComposePath := filepath.Join(configDir, service.ComposeFile)
			cmd := exec.Command("docker", "compose", "-f", fullComposePath, "-p", "citadel-"+name, "restart")
			return cmd.Run()
		}
	}

	return fmt.Errorf("service not found: %s", name)
}

// ccGetServiceDetail returns detailed information about a service
func ccGetServiceDetail(name string) *controlcenter.ServiceDetailInfo {
	manifest, configDir, err := findAndReadManifest()
	if err != nil {
		return nil
	}

	for _, service := range manifest.Services {
		if service.Name == name {
			fullComposePath := filepath.Join(configDir, service.ComposeFile)
			detail := &controlcenter.ServiceDetailInfo{
				ComposePath: service.ComposeFile,
			}

			// Get container info via docker compose ps
			psCmd := exec.Command("docker", "compose", "-f", fullComposePath, "-p", "citadel-"+name, "ps", "--format", "json")
			if output, err := psCmd.Output(); err == nil {
				var container struct {
					ID      string `json:"ID"`
					Image   string `json:"Image"`
					Service string `json:"Service"`
					State   string `json:"State"`
					Ports   string `json:"Ports"`
				}
				decoder := json.NewDecoder(strings.NewReader(string(output)))
				if err := decoder.Decode(&container); err == nil {
					if len(container.ID) > 12 {
						detail.ContainerID = container.ID[:12]
					} else {
						detail.ContainerID = container.ID
					}
					detail.Image = container.Image
					if container.Ports != "" {
						// Parse ports string like "0.0.0.0:8000->8000/tcp"
						detail.Ports = strings.Split(container.Ports, ", ")
					}
				}
			}

			return detail
		}
	}

	return nil
}

// ccGetServiceLogs returns recent log lines for a service
func ccGetServiceLogs(name string) ([]string, error) {
	manifest, configDir, err := findAndReadManifest()
	if err != nil {
		return nil, err
	}

	for _, service := range manifest.Services {
		if service.Name == name {
			fullComposePath := filepath.Join(configDir, service.ComposeFile)
			cmd := exec.Command("docker", "compose", "-f", fullComposePath, "-p", "citadel-"+name, "logs", "--tail", "50", "--no-color")
			output, err := cmd.Output()
			if err != nil {
				return nil, err
			}
			lines := strings.Split(string(output), "\n")
			// Filter out empty lines
			var result []string
			for _, line := range lines {
				if strings.TrimSpace(line) != "" {
					result = append(result, line)
				}
			}
			return result, nil
		}
	}

	return nil, fmt.Errorf("service not found: %s", name)
}

// ccAddService adds a new service to the manifest and extracts its compose file
func ccAddService(name string) error {
	// Find or create config directory
	_, configDir, err := findOrCreateManifest()
	if err != nil {
		return fmt.Errorf("failed to get config directory: %w", err)
	}

	// Ensure compose file exists
	if err := ensureComposeFile(configDir, name); err != nil {
		return err
	}

	// Add to manifest
	if err := addServiceToManifest(configDir, name); err != nil {
		return err
	}

	return nil
}

// ccGetConfiguredServices returns the list of services already configured in the manifest
func ccGetConfiguredServices() []string {
	manifest, _, err := findAndReadManifest()
	if err != nil {
		return nil
	}

	var configured []string
	for _, svc := range manifest.Services {
		configured = append(configured, svc.Name)
	}
	return configured
}

// ccStartDeviceAuthFlow starts the device authorization flow and returns the config
func ccStartDeviceAuthFlow() (*controlcenter.DeviceAuthConfig, error) {
	client := nexus.NewDeviceAuthClient(authServiceURL)

	resp, err := client.StartFlow(nil)
	if err != nil {
		return nil, err
	}

	return &controlcenter.DeviceAuthConfig{
		UserCode:        resp.UserCode,
		VerificationURI: resp.VerificationURI,
		DeviceCode:      resp.DeviceCode,
		ExpiresIn:       resp.ExpiresIn,
		Interval:        resp.Interval,
	}, nil
}

// ccPollDeviceAuthToken polls for device authorization token (single check)
func ccPollDeviceAuthToken(deviceCode string, interval int) (string, error) {
	client := nexus.NewDeviceAuthClient(authServiceURL)

	// Use checkToken for single poll (not blocking PollForToken)
	token, err := client.CheckToken(deviceCode)
	if err != nil {
		return "", err
	}
	if token == nil || token.Authkey == "" {
		return "", fmt.Errorf("authorization_pending")
	}

	// Save the device config including user info when auth succeeds
	if token.DeviceAPIToken != "" || token.UserEmail != "" {
		if err := saveDeviceConfigToFile(token); err != nil {
			// Log but don't fail - we have the authkey
			Debug("failed to save device config: %v", err)
		}
	}

	return token.Authkey, nil
}

// ccConnectWithAuthkey connects to the network using an authkey
func ccConnectWithAuthkey(authkey string) error {
	// Get node name from manifest or hostname
	nodeName := ""
	if manifest, _, err := findAndReadManifest(); err == nil && manifest != nil {
		nodeName = manifest.Node.Name
	}
	if nodeName == "" {
		nodeName, _ = os.Hostname()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	config := network.ServerConfig{
		Hostname:   nodeName,
		ControlURL: nexusURL,
		AuthKey:    authkey,
	}

	_, err := network.Connect(ctx, config)
	return err
}

// ccDisconnect disconnects from the network
func ccDisconnect() error {
	return network.Logout()
}

// ccWorkerIsRunning returns true if the worker is running
func ccWorkerIsRunning() bool {
	ccWorkerMu.Lock()
	defer ccWorkerMu.Unlock()
	return ccWorkerRunning
}

// ccStartWorker starts the worker in the background
func ccStartWorker(activityFn func(level, msg string)) error {
	ccWorkerMu.Lock()
	if ccWorkerRunning {
		ccWorkerMu.Unlock()
		return fmt.Errorf("worker is already running")
	}
	ccActivityFn = activityFn
	ccWorkerMu.Unlock()

	// Create a cancellable context for the worker
	ctx, cancel := context.WithCancel(context.Background())

	ccWorkerMu.Lock()
	ccWorkerCancel = cancel
	ccWorkerMu.Unlock()

	// Start worker in goroutine
	go func() {
		defer func() {
			ccWorkerMu.Lock()
			ccWorkerRunning = false
			ccWorkerQueue = ""
			ccWorkerMu.Unlock()
		}()

		if err := runTUIWorker(ctx, activityFn); err != nil {
			if err != context.Canceled && activityFn != nil {
				activityFn("error", fmt.Sprintf("Worker error: %v", err))
			}
		}
	}()

	ccWorkerMu.Lock()
	ccWorkerRunning = true
	ccWorkerMu.Unlock()

	return nil
}

// ccStopWorker stops the running worker
func ccStopWorker() error {
	ccWorkerMu.Lock()
	defer ccWorkerMu.Unlock()

	if !ccWorkerRunning {
		return fmt.Errorf("worker is not running")
	}

	if ccWorkerCancel != nil {
		ccWorkerCancel()
		ccWorkerCancel = nil
	}

	return nil
}

// runTUIWorker runs the worker for the TUI (simplified version of runWork)
func runTUIWorker(ctx context.Context, activityFn func(level, msg string)) error {
	activity := func(level, msg string) {
		if activityFn != nil {
			activityFn(level, msg)
		}
	}

	// Load device config from file
	deviceConfig := getDeviceConfigFromFile()

	// Determine job source mode
	var source worker.JobSource
	var streamFactory func(jobID string) worker.StreamWriter

	if deviceConfig != nil && deviceConfig.DeviceAPIToken != "" {
		// API mode
		apiBaseURL := deviceConfig.APIBaseURL
		if apiBaseURL == "" {
			apiBaseURL = authServiceURL
		}

		apiSource := worker.NewAPISource(worker.APISourceConfig{
			BaseURL:       apiBaseURL,
			Token:         deviceConfig.DeviceAPIToken,
			QueueName:     "",
			ConsumerGroup: "",
			BlockMs:       5000,
			MaxAttempts:   3,
			LogFn:         activity, // Route logs through TUI
		})

		if err := apiSource.Connect(ctx); err != nil {
			return fmt.Errorf("failed to connect to Redis API: %w", err)
		}

		// Enable WebSocket for real-time pub/sub
		if err := apiSource.Client().EnableWebSocket(ctx); err != nil {
			activity("warning", "WebSocket not available, using HTTP")
		}

		source = apiSource
		streamFactory = worker.CreateAPIStreamWriterFactory(ctx, apiSource)

		activity("info", "Worker mode: API (secure)")

		ccWorkerMu.Lock()
		ccWorkerQueue = "api"
		ccWorkerMu.Unlock()
	} else if deviceConfig != nil && deviceConfig.RedisURL != "" {
		// Direct Redis mode
		redisSource := worker.NewRedisSource(worker.RedisSourceConfig{
			URL:           deviceConfig.RedisURL,
			Password:      "",
			QueueName:     "",
			ConsumerGroup: "",
			BlockMs:       5000,
			MaxAttempts:   3,
			LogFn:         activity, // Route logs through TUI
		})
		source = redisSource
		streamFactory = worker.CreateRedisStreamWriterFactory(ctx, redisSource)

		activity("info", "Worker mode: Direct Redis")

		ccWorkerMu.Lock()
		ccWorkerQueue = "redis"
		ccWorkerMu.Unlock()
	} else {
		return fmt.Errorf("no job source configured (run 'citadel init' first)")
	}

	// Get node name
	nodeName := ""
	if netStatus, err := network.GetGlobalStatus(ctx); err == nil && netStatus.Connected && netStatus.Hostname != "" {
		nodeName = netStatus.Hostname
	} else {
		nodeName, _ = os.Hostname()
	}

	// Create status collector for heartbeat
	collector := status.NewCollector(status.CollectorConfig{
		NodeName:  nodeName,
		ConfigDir: "",
		Services:  nil,
	})

	// Start heartbeat publisher if we have API mode
	if deviceConfig != nil && deviceConfig.DeviceAPIToken != "" {
		orgID := ""
		if deviceConfig.OrgID != "" {
			orgID = deviceConfig.OrgID
		} else if manifest, _, err := findAndReadManifest(); err == nil {
			orgID = manifest.Node.OrgID
		}

		if orgID != "" {
			if apiSource, ok := source.(*worker.APISource); ok {
				apiPublisher, err := heartbeat.NewAPIPublisher(heartbeat.APIPublisherConfig{
					Client:    apiSource.Client(),
					NodeID:    nodeName,
					OrgID:     orgID,
					DebugFunc: nil,
					LogFn:     activity, // Route logs through TUI
				}, collector)
				if err == nil {
					go func() {
						activity("info", "Heartbeat publishing started")

						// Update TUI heartbeat indicator periodically
						heartbeatTicker := time.NewTicker(30 * time.Second)
						defer heartbeatTicker.Stop()

						go func() {
							// Initial heartbeat indicator
							if ccHeartbeatFn != nil {
								ccHeartbeatFn(true)
							}
							for {
								select {
								case <-ctx.Done():
									return
								case <-heartbeatTicker.C:
									if ccHeartbeatFn != nil {
										ccHeartbeatFn(true)
									}
								}
							}
						}()

						if err := apiPublisher.Start(ctx); err != nil && err != context.Canceled {
							activity("warning", fmt.Sprintf("Heartbeat error: %v", err))
						}
					}()
				}
			}
		}
	}

	// Create worker ID
	workerID := fmt.Sprintf("citadel-tui-%s", uuid.New().String()[:8])

	// Create handlers
	handlers := worker.CreateLegacyHandlers()

	// Create runner with TUI callbacks
	runner := worker.NewRunner(source, handlers, worker.RunnerConfig{
		WorkerID:   workerID,
		Verbose:    false,
		ActivityFn: activity, // Route logs through TUI
		JobRecordFn: func(id, jobType, status string, started, completed time.Time, err error) {
			// Job recording callback - could be extended to pass to TUI
			// For now, the activity log covers job status
		},
	})

	if streamFactory != nil {
		runner.WithStreamWriterFactory(streamFactory)
	}

	activity("success", "Worker started, listening for jobs...")

	// Run the worker (blocks until context is cancelled)
	return runner.Run(ctx)
}

