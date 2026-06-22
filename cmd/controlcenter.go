// cmd/controlcenter.go
package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/demo"
	"github.com/aceteam-ai/citadel-cli/internal/desktop"
	"github.com/aceteam-ai/citadel-cli/internal/heartbeat"
	"github.com/aceteam-ai/citadel-cli/internal/instance"
	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	pmx "github.com/aceteam-ai/citadel-cli/internal/proxmox"
	"github.com/aceteam-ai/citadel-cli/internal/status"
	"github.com/aceteam-ai/citadel-cli/internal/terminal"
	"github.com/aceteam-ai/citadel-cli/internal/tui"
	"github.com/aceteam-ai/citadel-cli/internal/tui/controlcenter"
	"github.com/aceteam-ai/citadel-cli/internal/tui/whimsy"
	"github.com/aceteam-ai/citadel-cli/internal/update"
	"github.com/aceteam-ai/citadel-cli/internal/usage"
	"github.com/aceteam-ai/citadel-cli/internal/worker"
	"github.com/aceteam-ai/citadel-cli/services"
	"github.com/google/uuid"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/mem"
)

// Worker state for TUI management
var (
	ccWorkerMu      sync.Mutex
	ccWorkerRunning bool
	ccWorkerCancel  context.CancelFunc
	ccWorkerQueue   string
	ccActivityFn    func(level, msg string)
	ccHeartbeatFn   func(active bool) // Callback when heartbeat publishes
)

// Demo server state
var (
	ccDemoServer *demo.Server
	ccDemoCancel context.CancelFunc
	ccDemoPort   = 7777
)

// Terminal server state
var (
	ccTerminalServer   *terminal.Server
	ccTerminalAuth     *terminal.CachingTokenValidator
	ccTerminalPort     = 7860
	ccTerminalRunning  bool
	ccTerminalOrgID    string // orgID the terminal server was started with
	ccTerminalAPIToken string // device API token the terminal server was started with
)

// VNC server state
var (
	ccVNCServer  *desktop.VNCServer
	ccVNCPort    = 5900
	ccVNCRunning bool
)

// runControlCenter launches the unified control center TUI
func runControlCenter() {
	if !tui.IsTTY() {
		fmt.Fprintln(os.Stderr, "Control center requires a terminal. Use --daemon for background mode.")
		os.Exit(1)
	}

	// Single-instance detection: if another TUI is running, attach to it
	configDir := platform.ConfigDir()
	if instance.IsRunning(configDir) {
		if err := instance.Attach(configDir); err != nil {
			fmt.Fprintf(os.Stderr, "Attach failed: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// Auto-update on startup
	if updated := ccAutoUpdate(); updated {
		// Binary was updated, restart
		return
	}

	// Suppress tsnet/tailscale logs to prevent TUI corruption
	network.SuppressLogs()

	// Start the instance server for attach support
	instanceServer, _ := instance.Listen(configDir)
	if instanceServer != nil {
		_ = instance.WritePID(configDir)
		defer func() {
			instanceServer.Close()
			instance.RemovePID(configDir)
		}()
	}

	// Start the demo server in the background
	startDemoServer()
	defer stopDemoServer()
	defer stopTerminalServer()
	defer stopVNCServer()

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
		Permissions: controlcenter.PermissionsCallbacks{
			Load: func() *config.Permissions {
				return config.LoadPermissions(platform.ConfigDir())
			},
			Save: func(p *config.Permissions) error {
				return config.SavePermissions(platform.ConfigDir(), p)
			},
		},
		OnConnect: ccOnNetworkConnect,
		Chat:      buildChatConfig(),
		Proxmox:   buildProxmoxConfig(),
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
			connected, err := network.VerifyOrReconnect(ctx)
			if err != nil && errors.Is(err, network.ErrStaleState) {
				// Stale VPN keys — try auto-recovery with device API token
				deviceConfig := getDeviceConfigFromFile()
				if deviceConfig != nil && deviceConfig.DeviceAPIToken != "" {
					cc.AddActivity("info", "VPN keys expired, attempting auto-recovery...")
					apiBaseURL := deviceConfig.APIBaseURL
					if apiBaseURL == "" {
						apiBaseURL = authServiceURL
					}
					if freshKey, fetchErr := network.FetchFreshAuthkey(ctx, apiBaseURL, deviceConfig.DeviceAPIToken); fetchErr != nil {
						cc.AddActivity("warning", fmt.Sprintf("Auto-recovery failed: %v", fetchErr))
					} else {
						// Try reconnect with existing state (preserves IP)
						if ok, reconnErr := network.ReconnectWithAuthKey(ctx, freshKey); reconnErr == nil && ok {
							cc.AddActivity("success", "VPN reconnected (IP preserved)")
							networkConnected = true
						} else {
							// Clear state and connect fresh (new IP)
							_ = network.ClearState()
							hostname, _ := os.Hostname()
							freshCtx, freshCancel := context.WithTimeout(ctx, 15*time.Second)
							config := network.ServerConfig{
								Hostname:   hostname,
								ControlURL: nexusURL,
								AuthKey:    freshKey,
							}
							if _, connectErr := network.Connect(freshCtx, config); connectErr == nil {
								cc.AddActivity("success", "VPN reconnected (fresh state)")
								networkConnected = true
							} else {
								cc.AddActivity("warning", fmt.Sprintf("VPN recovery failed: %v", connectErr))
							}
							freshCancel()
						}
					}
				} else {
					cc.AddActivity("warning", "VPN keys expired, login required")
				}
			} else if err != nil {
				cc.AddActivity("warning", fmt.Sprintf("Network reconnect failed: %v", err))
			} else if connected {
				cc.AddActivity("success", "Connected to AceTeam Network")
				networkConnected = true
			}
		}

		// Gather initial data after network check
		if data, err := gatherControlCenterData(); err == nil {
			cc.UpdateData(data)
			networkConnected = data.Connected
		}

		// Set device URL for "V" key shortcut if connected
		if networkConnected {
			deviceURL := authServiceURL + "/fabric"
			if d, err := gatherControlCenterData(); err == nil && d.HeadscaleNodeID != "" {
				deviceURL = authServiceURL + "/fabric/machines/" + d.HeadscaleNodeID
			}
			cc.SetDeviceURL(deviceURL)
		}

		// Auto-start servers and worker after network is connected
		if networkConnected {
			ccOnNetworkConnect(cc.AddActivity)
			cc.ShowChat()
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

// ccOnNetworkConnect starts terminal server, VNC server, and worker after VPN connects.
// Called both on auto-reconnect at startup and after interactive login.
func ccOnNetworkConnect(activityFn func(level, msg string)) {
	if orgID := getOrgIDFromConfig(); orgID != "" {
		// Detect if credentials changed (e.g., after re-pairing). If the terminal
		// server is already running with stale credentials, restart it so the
		// CachingTokenValidator fetches fresh token hashes from the platform API.
		currentAPIToken := ""
		if cfg := getDeviceConfigFromFile(); cfg != nil {
			currentAPIToken = cfg.DeviceAPIToken
		}
		if ccTerminalRunning && (orgID != ccTerminalOrgID || currentAPIToken != ccTerminalAPIToken) {
			activityFn("info", "Credentials changed, restarting terminal server...")
			stopTerminalServer()
		}

		if err := startTerminalServer(orgID, activityFn); err != nil {
			activityFn("warning", fmt.Sprintf("Terminal server failed: %v", err))
		} else {
			activityFn("info", fmt.Sprintf("Terminal server listening on port %d", ccTerminalPort))
		}
	}

	perms := config.LoadPermissions(platform.ConfigDir())
	if perms.Desktop {
		if err := startVNCServer(); err != nil {
			activityFn("warning", fmt.Sprintf("VNC server failed: %v", err))
		} else {
			activityFn("info", fmt.Sprintf("VNC server listening on port %d", ccVNCPort))
		}
	}

	deviceConfig := getDeviceConfigFromFile()
	if deviceConfig != nil && deviceConfig.DeviceAPIToken != "" {
		time.Sleep(500 * time.Millisecond)
		activityFn("info", "Starting worker...")
		if err := ccStartWorker(activityFn); err != nil {
			activityFn("warning", fmt.Sprintf("Worker auto-start failed: %v", err))
		}
	}
}

// startTerminalServer starts the terminal server for remote SSH access.
// activityFn routes log messages through the TUI activity panel. It must
// not be nil — callers always pass one from ccOnNetworkConnect.
func startTerminalServer(orgID string, activityFn func(level, msg string)) error {
	if ccTerminalRunning {
		return nil // Already running
	}

	// Build configuration
	config := terminal.DefaultConfig()
	config.OrgID = orgID
	config.AuthServiceURL = authServiceURL
	config.Port = ccTerminalPort

	// Create the caching token validator
	apiToken := ""
	if cfg := getDeviceConfigFromFile(); cfg != nil {
		apiToken = cfg.DeviceAPIToken
	}
	ccTerminalAuth = terminal.NewCachingTokenValidator(
		config.AuthServiceURL,
		config.OrgID,
		apiToken,
		config.TokenRefreshInterval,
	)

	// Route validator log messages through the TUI activity log so token
	// fetch failures are visible (previously swallowed silently, making
	// 401 errors after re-pairing impossible to diagnose).
	ccTerminalAuth.SetLogFn(func(level, msg string) {
		activityFn(level, fmt.Sprintf("[terminal auth] %s", msg))
	})

	// Start the validator's background refresh
	if err := ccTerminalAuth.Start(); err != nil {
		return fmt.Errorf("failed to start token cache: %w", err)
	}

	// Create and start the server
	ccTerminalServer = terminal.NewServer(config, ccTerminalAuth)

	// Add VPN listener so the terminal server is reachable over the tsnet VPN.
	// The TCP bind is localhost-only for security; external access comes through tsnet.
	// Bind to the explicit assigned VPN IP (not ":port") so inbound connections from
	// the platform relay are matched by tsnet. See network.ListenVPN and issue #286.
	if network.IsGlobalConnected() {
		vpnPort := fmt.Sprintf("%d", config.Port)
		if vpnLn, vpnIP, err := network.ListenVPN("tcp", vpnPort); err != nil {
			Log("terminal server VPN listener failed (localhost-only): %v", err)
		} else {
			ccTerminalServer.AddListener(vpnLn)
			Log("terminal server VPN listener on %s:%s", vpnIP, vpnPort)
		}
	}

	// Suppress terminal server logging in TUI mode to prevent display corruption
	ccTerminalServer.SetSilent()

	if err := ccTerminalServer.Start(); err != nil {
		ccTerminalAuth.Stop()
		return fmt.Errorf("failed to start terminal server: %w", err)
	}

	// Record which credentials the server was started with, so we can
	// detect when they change (e.g., after re-pairing) and restart.
	ccTerminalOrgID = orgID
	ccTerminalAPIToken = apiToken
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
	ccTerminalOrgID = ""
	ccTerminalAPIToken = ""
}

// startVNCServer starts the embedded VNC server for remote desktop access.
// It retries transient failures (e.g. screen capture init racing with display
// server startup) up to 3 times with exponential backoff.
func startVNCServer() error {
	if ccVNCRunning {
		return nil
	}

	ccVNCServer = desktop.NewVNCServer(desktop.VNCServerConfig{
		Host: "127.0.0.1",
		Port: ccVNCPort,
		FPS:  10,
	})

	// Add VPN listener so the VNC server is reachable over the tsnet VPN.
	// Bind to the explicit assigned VPN IP (not ":port"); see network.ListenVPN
	// and issue #286.
	if network.IsGlobalConnected() {
		vpnPort := fmt.Sprintf("%d", ccVNCPort)
		if vpnLn, vpnIP, err := network.ListenVPN("tcp", vpnPort); err != nil {
			Log("VNC server VPN listener failed (localhost-only): %v", err)
		} else {
			ccVNCServer.AddListener(vpnLn)
			Log("VNC server VPN listener on %s:%s", vpnIP, vpnPort)
		}
	}

	ccVNCServer.SetSilent()

	// Retry with backoff for transient failures (display not ready yet).
	// Permanent failures (unsupported platform) bail immediately.
	const maxRetries = 3
	interval := 500 * time.Millisecond
	var lastErr error

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if err := ccVNCServer.Start(); err != nil {
			lastErr = err
			// Permanent failure: platform doesn't support screen capture
			if strings.Contains(err.Error(), "not supported") {
				return fmt.Errorf("failed to start VNC server: %w", err)
			}
			if attempt < maxRetries {
				Log("VNC server start attempt %d/%d failed (retrying in %s): %v", attempt+1, maxRetries+1, interval, err)
				time.Sleep(interval)
				interval *= 2
				// Re-create server for retry since Start() sets running=false on failure
				ccVNCServer = desktop.NewVNCServer(desktop.VNCServerConfig{
					Host: "127.0.0.1",
					Port: ccVNCPort,
					FPS:  10,
				})
				ccVNCServer.SetSilent()
				continue
			}
			return fmt.Errorf("failed to start VNC server after %d attempts: %w", maxRetries+1, lastErr)
		}
		break
	}

	ccVNCRunning = true
	platform.SetEmbeddedVNCPort(ccVNCPort)
	return nil
}

// stopVNCServer stops the embedded VNC server
func stopVNCServer() {
	if !ccVNCRunning {
		return
	}
	if ccVNCServer != nil {
		ccVNCServer.Stop()
	}
	ccVNCRunning = false
	platform.ClearEmbeddedVNCPort()
}

// buildChatConfig constructs ChatPageConfig from device config and manifest.
// Returns a zero-value config if credentials are not yet available — the
// ChatPage handles this gracefully by showing an error on activation.
func buildChatConfig() controlcenter.ChatPageConfig {
	deviceConfig := getDeviceConfigFromFile()
	if deviceConfig == nil || deviceConfig.DeviceAPIToken == "" {
		return controlcenter.ChatPageConfig{}
	}

	apiBaseURL := deviceConfig.APIBaseURL
	if apiBaseURL == "" {
		apiBaseURL = authServiceURL
	}

	orgID := deviceConfig.OrgID
	if orgID == "" {
		if manifest, _, err := findAndReadManifest(); err == nil {
			orgID = manifest.Node.OrgID
		}
	}

	nodeName, _ := os.Hostname()
	nodeID := nodeName
	if netStatus, err := network.GetGlobalStatus(context.Background()); err == nil && netStatus.Connected {
		if netStatus.Hostname != "" {
			nodeName = netStatus.Hostname
		}
		if netStatus.NodeID != "" {
			nodeID = netStatus.NodeID
		}
	}

	return controlcenter.ChatPageConfig{
		APIBaseURL: apiBaseURL,
		APIToken:   deviceConfig.DeviceAPIToken,
		OrgID:      orgID,
		NodeID:     nodeID,
		NodeName:   nodeName,
	}
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
		if data.OrgName == "" && deviceConfig.OrgName != "" {
			data.OrgName = deviceConfig.OrgName
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
				data.HeadscaleNodeID = status.NodeID
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
	var streamFactory func(job *worker.Job) worker.StreamWriter

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
	} else {
		// No device_api_token configured - worker can't start
		// Note: Direct Redis mode is only available via 'citadel work' command
		return fmt.Errorf("no API token configured (complete device authorization first)")
	}

	// Get node name and Headscale node ID
	nodeName := ""
	headscaleNodeID := ""
	if netStatus, err := network.GetGlobalStatus(ctx); err == nil && netStatus.Connected && netStatus.Hostname != "" {
		nodeName = netStatus.Hostname
		headscaleNodeID = netStatus.NodeID
	} else {
		nodeName, _ = os.Hostname()
	}

	// Subscribe to this node's per-node shell stream so node-targeted MCP jobs
	// (terminal_exec, code_*, file reads, node attachments) reach ONLY this node
	// instead of being claimed by a greedy peer on the shared shell stream
	// (issue #3914). The canonical `citadel` (TUI) path must do this too, not
	// just `citadel work` -- otherwise node-targeted jobs are never consumed in
	// the normal run mode. Mirrors the subscription in runWork(); purely additive
	// (the shared shell stream is still consumed for untargeted work).
	if headscaleNodeID == "" {
		if id := network.GetGlobalNodeID(ctx); id != "" {
			headscaleNodeID = id
		}
	}
	if headscaleNodeID != "" {
		perNodeOrgID := ""
		if deviceConfig != nil {
			perNodeOrgID = deviceConfig.OrgID
		}
		if perNodeOrgID == "" {
			if manifest, _, mErr := findAndReadManifest(); mErr == nil && manifest != nil {
				perNodeOrgID = manifest.Node.OrgID
			}
		}
		if perNodeOrgID != "" {
			perNodeQueue := nodeQueueName(perNodeOrgID, headscaleNodeID)
			if apiSrc, ok := source.(*worker.APISource); ok {
				apiSrc.AddQueue(perNodeQueue)
			}
			activity("info", fmt.Sprintf("Per-node shell stream: %s", perNodeQueue))
		} else {
			activity("warning", "Per-node shell stream skipped (org id unknown); node-targeted jobs fall back to the shared stream")
		}
	} else {
		activity("warning", "Per-node shell stream skipped (node ID unavailable); node-targeted jobs fall back to the shared stream")
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
					Client:          apiSource.Client(),
					NodeID:          nodeName,
					HeadscaleNodeID: headscaleNodeID,
					OrgID:           orgID,
					DebugFunc:       nil,
					LogFn:           activity, // Route logs through TUI
				}, collector)
				if err == nil {
					// Include current permissions in heartbeat
					apiPublisher.SetPermissions(permissionsToHeartbeat(config.LoadPermissions(platform.ConfigDir())))
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

	// Create handlers with activity callback to route job output through TUI.
	wsDir := resolveWorkspaceDir()
	handlers := worker.CreateLegacyHandlersWithOpts(worker.LegacyHandlerOpts{
		LogFn:        activity,
		WorkspaceDir: wsDir,
	})

	// Create runner with TUI callbacks
	runner := worker.NewRunner(source, handlers, worker.RunnerConfig{
		WorkerID:   workerID,
		Verbose:    false,
		ActivityFn: activity, // Route logs through TUI
		JobRecordFn: func(record usage.UsageRecord) {
			// Job recording callback - could be extended to pass to TUI
			// For now, the activity log covers job status
			_ = record
		},
	})

	if streamFactory != nil {
		runner.WithStreamWriterFactory(streamFactory)
	}

	activity("success", "Worker started, listening for jobs...")

	// Run the worker (blocks until context is cancelled)
	return runner.Run(ctx)
}

// ccAutoUpdate checks for updates and auto-updates if available.
// Returns true if the binary was updated (caller should restart).
func ccAutoUpdate() bool {
	// Check for updates
	spinner := whimsy.NewSimpleSpinner([]string{"Checking for updates..."})
	spinner.Start()

	client := update.NewClient(Version)
	release, err := client.CheckForUpdate()
	if err != nil {
		spinner.StopWithWarning(fmt.Sprintf("Update check failed: %v", err))
		return false
	}

	if release == nil {
		spinner.StopWithSuccess(fmt.Sprintf("Running latest version (%s)", Version))
		return false
	}

	spinner.StopWithSuccess(fmt.Sprintf("Update available: %s → %s", Version, release.TagName))

	// Download update
	dlSpinner := whimsy.NewSimpleSpinner([]string{"Downloading update..."})
	dlSpinner.Start()

	pendingPath := update.GetPendingBinaryPath()
	if err := client.DownloadAndVerify(release, pendingPath); err != nil {
		dlSpinner.StopWithError(fmt.Sprintf("Download failed: %v", err))
		return false
	}

	dlSpinner.StopWithSuccess("Downloaded and verified")

	// Install update
	installSpinner := whimsy.NewSimpleSpinner([]string{"Installing update..."})
	installSpinner.Start()

	if err := update.ApplyUpdate(pendingPath); err != nil {
		installSpinner.StopWithError(fmt.Sprintf("Install failed: %v", err))
		return false
	}

	// Update state
	state, _ := update.LoadState()
	update.RecordUpdate(state, Version, release.TagName)
	update.UpdateLastCheck(state)
	_ = update.SaveState(state)

	installSpinner.StopWithSuccess(fmt.Sprintf("Updated to %s, restarting...", release.TagName))

	// Small delay to show the message
	time.Sleep(500 * time.Millisecond)

	// Restart the binary
	execPath, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to get executable path: %v\n", err)
		fmt.Println("Please restart citadel manually.")
		return true
	}

	if runtime.GOOS == "windows" {
		// Windows doesn't support syscall.Exec; start a new process and exit
		cmd := exec.Command(execPath, os.Args[1:]...)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		_ = cmd.Start()
		os.Exit(0)
	}

	// Unix: replace the current process in-place
	if err := syscall.Exec(execPath, os.Args, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to restart: %v\n", err)
		fmt.Println("Please restart citadel manually.")
	}

	return true
}

// buildProxmoxConfig checks for saved Proxmox configuration or auto-detects
// a local Proxmox installation and returns a ProxmoxConfig for the TUI.
func buildProxmoxConfig() controlcenter.ProxmoxConfig {
	configDir := platform.ConfigDir()

	// Try saved configuration first
	if pmxCfg, err := pmx.LoadConfig(configDir); err == nil && pmxCfg != nil && pmxCfg.BaseURL != "" {
		return controlcenter.ProxmoxConfig{
			Enabled:  true,
			BaseURL:  pmxCfg.BaseURL,
			TokenID:  pmxCfg.TokenID,
			Secret:   pmxCfg.TokenSecret,
			NodeName: pmxCfg.NodeName,
		}
	}

	// Try auto-detection (checks /etc/pve and pvesh)
	if pveInfo, err := platform.DetectProxmox(); err == nil && pveInfo.IsInstalled {
		return controlcenter.ProxmoxConfig{
			Enabled:  true,
			BaseURL:  "https://localhost:8006",
			NodeName: pveInfo.NodeName,
		}
	}

	return controlcenter.ProxmoxConfig{}
}
