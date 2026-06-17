// cmd/status.go
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
	"text/tabwriter"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/capabilities"
	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/aceteam-ai/citadel-cli/internal/tui"
	"github.com/aceteam-ai/citadel-cli/internal/tui/dashboard"
	"github.com/fatih/color"
	"github.com/redis/go-redis/v9"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/mem"
	"github.com/spf13/cobra"
)

var (
	headerColor   = color.New(color.FgCyan, color.Bold)
	goodColor     = color.New(color.FgGreen)
	warnColor     = color.New(color.FgYellow)
	badColor      = color.New(color.FgRed)
	labelColor    = color.New(color.Bold)
	interactiveUI bool // Flag to enable interactive TUI dashboard
	statusJSON    bool // Flag to output JSON format
)

var statusCmd = &cobra.Command{
	Use:     "status",
	Aliases: []string{"st", "info"},
	Short:   "Shows a comprehensive status of the Citadel node",
	Long: `Provides a full health check and resource overview of the Citadel node.
It checks network connectivity, system vitals (CPU, RAM, Disk), GPU status,
and the state of all managed services.`,
	Example: `  # View full node status with colors
  citadel status

  # View status without colors (for scripts/logging)
  citadel status --no-color

  # Interactive dashboard with live updates
  citadel status -i`,
	Run: func(cmd *cobra.Command, args []string) {
		// JSON output mode
		if statusJSON {
			runJSONStatus()
			return
		}

		// Check if interactive mode requested and available
		if interactiveUI && tui.ShouldUseInteractive(true, color.NoColor) {
			runInteractiveDashboard()
			return
		}

		// Fall back to standard tabwriter output
		runStandardStatus()
	},
}

// runJSONStatus outputs status data as JSON
func runJSONStatus() {
	data, err := gatherStatusData()
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Failed to gather status: %v\n", err)
		os.Exit(1)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(data); err != nil {
		fmt.Fprintf(os.Stderr, "❌ Failed to encode JSON: %v\n", err)
		os.Exit(1)
	}
}

// runStandardStatus displays status using the traditional tabwriter format
func runStandardStatus() {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer w.Flush()

	// Load manifest once for use in multiple sections
	manifest, _, _ := findAndReadManifest()

	headerColor.Fprintf(w, "--- 📊 Citadel Node Status (%s) ---\n", Version)

	headerColor.Fprintln(w, "\n💻 SYSTEM VITALS")
	printMemInfo(w)
	printCPUInfo(w)
	printDiskInfo(w)

	headerColor.Fprintln(w, "\n🗂️ CACHE USAGE (~/citadel-cache)")
	printCacheInfo(w)

	headerColor.Fprintln(w, "\n💎 GPU STATUS")
	printGPUInfo(w)

	headerColor.Fprintln(w, "\n🔧 CAPABILITIES")
	printCapabilities(w, manifest)

	headerColor.Fprintln(w, "\n🌐 NETWORK STATUS")
	printNetworkInfo(w, manifest)
	printPeerInfo(w)

	// Only show Job Queue section if configured
	if os.Getenv("REDIS_URL") != "" {
		headerColor.Fprintln(w, "\n📋 JOB QUEUE")
		printJobQueueInfo(w)
	}

	headerColor.Fprintln(w, "\n🚀 MANAGED SERVICES")
	printServiceInfo(w)
}

// runInteractiveDashboard runs the interactive TUI dashboard
func runInteractiveDashboard() {
	// Gather initial data
	data, _ := gatherStatusData()

	// Run the tview dashboard with refresh callback
	if err := dashboard.RunTviewDashboard(data, gatherStatusData); err != nil {
		fmt.Fprintf(os.Stderr, "Dashboard error: %v\n", err)
		os.Exit(1)
	}
}

// gatherStatusData collects all status data for the dashboard
func gatherStatusData() (dashboard.StatusData, error) {
	data := dashboard.StatusData{
		Version: Version,
	}

	// Load manifest
	manifest, _, _ := findAndReadManifest()
	if manifest != nil {
		data.NodeName = manifest.Node.Name
		data.OrgID = manifest.Node.OrgID
		data.Tags = manifest.Node.Tags
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
	if percentages, err := cpu.Percent(500*time.Millisecond, false); err == nil && len(percentages) > 0 {
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
		if gpus, err := detector.GetGPUInfo(); err == nil {
			for _, gpu := range gpus {
				gpuInfo := dashboard.GPUInfo{
					Name:        gpu.Name,
					Memory:      gpu.Memory,
					Temperature: gpu.Temperature,
					Driver:      gpu.Driver,
				}
				if gpu.Utilization != "" {
					utilStr := strings.TrimSuffix(gpu.Utilization, "%")
					if util, err := strconv.ParseFloat(utilStr, 64); err == nil {
						gpuInfo.Utilization = util
					}
				}
				data.GPUs = append(data.GPUs, gpuInfo)
			}
		} else {
			// nvidia-smi failed but hardware is present — show lspci info
			hwName := platform.DetectNvidiaHardware()
			if hwName != "" {
				errMsg := platform.NvidiaSMIErrorMessage(err)
				data.GPUs = append(data.GPUs, dashboard.GPUInfo{
					Name:   hwName + " (drivers not loaded)",
					Driver: errMsg,
				})
			}
		}
	}

	// Network status
	if network.HasState() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		connected, _ := network.VerifyOrReconnect(ctx)
		data.Connected = connected

		if status, err := network.GetGlobalStatus(ctx); err == nil {
			data.NodeIP = status.IPv4
			if data.NodeName == "" {
				data.NodeName = status.Hostname
			}
		}

		// Get peers
		myIP, _ := network.GetGlobalIPv4()
		if peers, err := network.GetGlobalPeers(ctx); err == nil {
			for _, peer := range peers {
				if peer.IP != myIP {
					peerInfo := dashboard.PeerInfo{
						Hostname: peer.Hostname,
						IP:       peer.IP,
						Online:   peer.Online,
					}

					// Ping online peers
					if peer.Online {
						pingCtx, pingCancel := context.WithTimeout(context.Background(), 1*time.Second)
						if latency, connType, _, err := network.PingPeer(pingCtx, peer.IP); err == nil {
							peerInfo.Latency = fmt.Sprintf("%.0fms", latency)
							peerInfo.ConnType = connType
						}
						pingCancel()
					}

					data.Peers = append(data.Peers, peerInfo)
				}
			}
		}
	}

	// Services
	if manifest != nil {
		configDir := ""
		if m, cd, err := findAndReadManifest(); err == nil && m != nil {
			configDir = cd
		}

		for _, service := range manifest.Services {
			svcStatus := dashboard.ServiceStatus{
				Name:   service.Name,
				Status: "stopped",
			}

			if configDir != "" {
				fullComposePath := filepath.Join(configDir, service.ComposeFile)
				if _, err := os.Stat(fullComposePath); err == nil {
					psCmd := exec.Command("docker", "compose", "-f", fullComposePath, "ps", "--format", "json")
					if output, err := psCmd.Output(); err == nil {
						var containers []struct {
							State string `json:"State"`
						}
						decoder := json.NewDecoder(strings.NewReader(string(output)))
						for decoder.More() {
							var c struct {
								State string `json:"State"`
							}
							if err := decoder.Decode(&c); err == nil {
								containers = append(containers, c)
							}
						}
						if len(containers) > 0 {
							state := strings.ToLower(containers[0].State)
							if strings.Contains(state, "running") || strings.Contains(state, "up") {
								svcStatus.Status = "running"
							} else if strings.Contains(state, "exited") || strings.Contains(state, "dead") {
								svcStatus.Status = "stopped"
							} else {
								svcStatus.Status = state
							}
						}
					}
				}
			}

			data.Services = append(data.Services, svcStatus)
		}
	}

	return data, nil
}

func printCacheInfo(w *tabwriter.Writer) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(w, "  %s\n", badColor.Sprintf("Could not determine home directory: %v", err))
		return
	}
	cacheDir := filepath.Join(homeDir, "citadel-cache")

	if _, err := os.Stat(cacheDir); os.IsNotExist(err) {
		fmt.Fprintf(w, "  (Cache directory not found)\t\n")
		return
	}

	totalCmd := exec.Command("du", "-sh", cacheDir)
	totalOutput, err := totalCmd.Output()
	if err == nil {
		parts := strings.Fields(string(totalOutput))
		if len(parts) > 0 {
			fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("Total Size"), parts[0])
		}
	}

	fmt.Fprintf(w, "  %s:\n", labelColor.Sprint("Breakdown"))
	// Use Go's Glob to find all subdirectories/files
	entries, err := filepath.Glob(filepath.Join(cacheDir, "*"))
	if err != nil || len(entries) == 0 {
		fmt.Fprintf(w, "    (Empty)\n")
		return
	}

	// Iterate over the found entries and run `du` on each one
	for _, entry := range entries {
		cmd := exec.Command("du", "-sh", entry)
		output, err := cmd.Output()
		if err != nil {
			continue // Skip if we can't get the size of an entry
		}
		parts := strings.Fields(string(output))
		if len(parts) >= 2 {
			size := parts[0]
			// Use filepath.Base to get just the directory name
			name := filepath.Base(parts[1])
			fmt.Fprintf(w, "    - %s:\t%s\n", name, size)
		}
	}
}

func printMemInfo(w *tabwriter.Writer) {
	v, err := mem.VirtualMemory()
	if err != nil {
		fmt.Fprintf(w, "  🧠 Memory:\t%s\n", badColor.Sprintf("Error getting memory info: %v", err))
		return
	}
	percentStr := colorizePercent(v.UsedPercent)
	fmt.Fprintf(w, "  🧠 %s:\t%s (%s / %s)\n", labelColor.Sprint("Memory"), percentStr, formatBytes(v.Used), formatBytes(v.Total))
}

func printCPUInfo(w *tabwriter.Writer) {
	percentages, err := cpu.Percent(time.Second, false)
	if err != nil || len(percentages) == 0 {
		fmt.Fprintf(w, "  ⚡️ CPU Usage:\t%s\n", badColor.Sprintf("Error getting CPU info: %v", err))
		return
	}
	percentStr := colorizePercent(percentages[0])
	fmt.Fprintf(w, "  ⚡️ %s:\t%s\n", labelColor.Sprint("CPU Usage"), percentStr)
}

func printDiskInfo(w *tabwriter.Writer) {
	d, err := disk.Usage("/")
	if err != nil {
		fmt.Fprintf(w, "  💾 Disk (/):\t%s\n", badColor.Sprintf("Error getting disk info: %v", err))
		return
	}
	percentStr := colorizePercent(d.UsedPercent)
	fmt.Fprintf(w, "  💾 %s:\t%s (%s / %s)\n", labelColor.Sprint("Disk (/)"), percentStr, formatBytes(d.Used), formatBytes(d.Total))
}

func printGPUInfo(w *tabwriter.Writer) {
	detector, err := platform.GetGPUDetector()
	if err != nil {
		fmt.Fprintf(w, "  GPU:\t%s\n", badColor.Sprintf("Error: %v", err))
		return
	}

	if !detector.HasGPU() {
		fmt.Fprintln(w, "  GPU:\tNo GPU detected on this system.")
		return
	}

	gpus, err := detector.GetGPUInfo()
	if err != nil {
		// Hardware detected via lspci but nvidia-smi failed — show actionable message
		hwName := platform.DetectNvidiaHardware()
		if hwName != "" {
			fmt.Fprintf(w, "  GPU:\t%s\n", warnColor.Sprintf("NVIDIA hardware detected (drivers not working)"))
			fmt.Fprintf(w, "    %s:\t%s\n", labelColor.Sprint("Hardware"), hwName)
		} else {
			fmt.Fprintf(w, "  GPU:\t%s\n", warnColor.Sprint("Hardware detected, but could not get details"))
		}
		errMsg := platform.NvidiaSMIErrorMessage(err)
		fmt.Fprintf(w, "    %s:\t%s\n", labelColor.Sprint("Issue"), warnColor.Sprint(errMsg))
		return
	}

	for i, gpu := range gpus {
		fmt.Fprintf(w, "  %s %d:\t%s\n", labelColor.Sprint("GPU"), i, gpu.Name)

		if gpu.Memory != "" {
			fmt.Fprintf(w, "    - %s:\t%s\n", labelColor.Sprint("Memory"), gpu.Memory)
		}

		if gpu.Temperature != "" {
			// Parse temperature to colorize it
			tempStr := strings.TrimSuffix(gpu.Temperature, "°C")
			fmt.Fprintf(w, "    - %s:\t%s\n", labelColor.Sprint("Temp"), colorizeTemp(tempStr))
		}

		if gpu.Utilization != "" {
			// Parse utilization to colorize it
			utilStr := strings.TrimSuffix(gpu.Utilization, "%")
			if utilFloat, err := strconv.ParseFloat(utilStr, 64); err == nil {
				fmt.Fprintf(w, "    - %s:\t%s\n", labelColor.Sprint("Util"), colorizePercent(utilFloat))
			} else {
				fmt.Fprintf(w, "    - %s:\t%s\n", labelColor.Sprint("Util"), gpu.Utilization)
			}
		}

		if gpu.Driver != "" {
			fmt.Fprintf(w, "    - %s:\t%s\n", labelColor.Sprint("Driver"), gpu.Driver)
		}
	}
}

func printCapabilities(w *tabwriter.Writer, manifest *CitadelManifest) {
	// If manifest declares capabilities, show those; otherwise auto-detect
	nodeCaps := resolveCapabilities(manifest)

	if nodeCaps.GPU != nil && len(nodeCaps.GPU.Devices) > 0 {
		if nodeCaps.GPU.DriverStatus == "not_loaded" || nodeCaps.GPU.DriverStatus == "error" {
			// Hardware present but drivers not working — show status without routing tags
			fmt.Fprintf(w, "  %s:\t%d\n", labelColor.Sprint("GPU Count"), nodeCaps.GPU.Count)
			for i, dev := range nodeCaps.GPU.Devices {
				fmt.Fprintf(w, "  %s %d:\t%s %s\n", labelColor.Sprint("GPU"), i, dev.Name,
					warnColor.Sprint("(drivers not loaded)"))
			}
			if nodeCaps.GPU.DriverError != "" {
				fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("Issue"), warnColor.Sprint(nodeCaps.GPU.DriverError))
			}
		} else {
			fmt.Fprintf(w, "  %s:\t%d\n", labelColor.Sprint("GPU Count"), nodeCaps.GPU.Count)
			for i, dev := range nodeCaps.GPU.Devices {
				vramStr := ""
				if dev.VRAMTag != "" {
					vramStr = fmt.Sprintf(" (%s)", strings.ToUpper(dev.VRAMTag))
				}
				fmt.Fprintf(w, "  %s %d:\t%s%s\n", labelColor.Sprint("GPU"), i, dev.Name, vramStr)
			}
		}
	} else {
		fmt.Fprintln(w, "  GPU:\tNone detected")
	}

	if len(nodeCaps.Engines) > 0 {
		fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("Engines"), strings.Join(nodeCaps.Engines, ", "))
	}

	if len(nodeCaps.Tags) > 0 {
		fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("Tags"), strings.Join(nodeCaps.Tags, ", "))
	}
}

// resolveCapabilities returns node capabilities from manifest if declared, or auto-detects.
func resolveCapabilities(manifest *CitadelManifest) *capabilities.NodeCapabilities {
	if manifest != nil && manifest.Capabilities != nil {
		return manifestToNodeCapabilities(manifest.Capabilities)
	}
	return capabilities.DetectNodeCapabilities()
}

// manifestToNodeCapabilities converts declared manifest capabilities to the common struct.
func manifestToNodeCapabilities(mc *ManifestCapabilities) *capabilities.NodeCapabilities {
	caps := &capabilities.NodeCapabilities{
		Engines: mc.Engines,
	}

	if len(mc.GPUs) > 0 {
		gpuCaps := &capabilities.GPUCapabilities{}
		totalCount := 0
		for _, mg := range mc.GPUs {
			count := mg.Count
			if count <= 0 {
				count = 1
			}
			totalCount += count
			tag := capabilities.NormalizeGPUName(mg.Name)
			vramTag := ""
			if mg.VRAMMb > 0 {
				vramTag = capabilities.NormalizeVRAM(fmt.Sprintf("%d", mg.VRAMMb))
				if vramTag != "" {
					vramTag += "gb"
				}
			}
			for i := 0; i < count; i++ {
				gpuCaps.Devices = append(gpuCaps.Devices, capabilities.GPUDevice{
					Name:    mg.Name,
					VRAMMb:  mg.VRAMMb,
					Tag:     tag,
					VRAMTag: vramTag,
				})
			}
		}
		gpuCaps.Count = totalCount
		caps.GPU = gpuCaps

		// Build tags from GPU info
		seen := make(map[string]bool)
		for i, dev := range gpuCaps.Devices {
			if dev.Tag != "" {
				tag := "gpu:" + dev.Tag
				if !seen[tag] {
					seen[tag] = true
					caps.Tags = append(caps.Tags, tag)
				}
			}
			if dev.VRAMTag != "" {
				tag := "vram:" + dev.VRAMTag
				if !seen[tag] {
					seen[tag] = true
					caps.Tags = append(caps.Tags, tag)
				}
			}
			if dev.Tag != "" && dev.VRAMTag != "" {
				indexedTag := fmt.Sprintf("gpu:%d:%s:%s", i, dev.Tag, dev.VRAMTag)
				if capabilities.ValidateTag(indexedTag) {
					caps.Tags = append(caps.Tags, indexedTag)
				}
			}
		}
	}

	// Add engine tags
	for _, engine := range mc.Engines {
		tag := "engine:" + engine
		if capabilities.ValidateTag(tag) {
			caps.Tags = append(caps.Tags, tag)
		}
	}

	caps.Tags = append(caps.Tags, "cpu:general")
	return caps
}

func printNetworkInfo(w *tabwriter.Writer, manifest *CitadelManifest) {
	// Check if we have network state
	if !network.HasState() {
		fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("Connection"), badColor.Sprint("🔴 OFFLINE (Not logged in)"))
		fmt.Fprintf(w, "  %s\n", "   Run 'citadel login' to connect to the AceTeam Network")
		return
	}

	// Try to reconnect if state exists but not connected
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	connected, reconnectErr := network.VerifyOrReconnect(ctx)
	if !connected {
		if reconnectErr != nil {
			fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("Connection"), warnColor.Sprintf("🟡 DISCONNECTED (%v)", reconnectErr))
		} else {
			fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("Connection"), badColor.Sprint("🔴 OFFLINE (Could not reconnect)"))
		}
		fmt.Fprintf(w, "  %s\n", "   Run 'citadel login' to re-authenticate")
		return
	}

	// Get detailed status
	status, err := network.GetGlobalStatus(ctx)
	if err != nil {
		fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("Connection"), warnColor.Sprint("⚠️  WARNING (Could not get network status)"))
		return
	}

	if status.Connected {
		fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("Connection"), goodColor.Sprint("🟢 ONLINE to AceTeam Network"))
		if status.Hostname != "" {
			fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("Hostname"), status.Hostname)
		}
		if status.IPv4 != "" {
			fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("IP Address"), status.IPv4)
		}
		// Display organization info from manifest
		if manifest != nil && manifest.Node.OrgID != "" {
			fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("Organization"), manifest.Node.OrgID)
		}
		// Display node tags from manifest
		if manifest != nil && len(manifest.Node.Tags) > 0 {
			tagsStr := strings.Join(manifest.Node.Tags, ", ")
			fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("Tags"), tagsStr)
		}
	} else {
		fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("Connection"), badColor.Sprint("🔴 OFFLINE (Not connected to AceTeam Network)"))
	}
}

func printPeerInfo(w *tabwriter.Writer) {
	// Only show peers if we're connected
	if !network.HasState() {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Get our own IP to filter ourselves out of peer list
	myIP, _ := network.GetGlobalIPv4()

	peers, err := network.GetGlobalPeers(ctx)
	if err != nil {
		// Silently skip if we can't get peers (e.g., not connected)
		return
	}

	// Filter out ourselves from the peer list
	var otherPeers []network.PeerInfo
	for _, peer := range peers {
		if peer.IP != myIP {
			otherPeers = append(otherPeers, peer)
		}
	}

	if len(otherPeers) == 0 {
		fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("Peers"), "(no other nodes on network)")
		return
	}

	fmt.Fprintf(w, "  %s:\n", labelColor.Sprint("Peers"))
	for _, peer := range otherPeers {
		statusStr := badColor.Sprint("⚫")
		extraInfo := ""

		if peer.Online {
			statusStr = goodColor.Sprint("🟢")

			// Ping online peers (with short timeout)
			pingCtx, pingCancel := context.WithTimeout(context.Background(), 2*time.Second)
			latency, connType, relay, err := network.PingPeer(pingCtx, peer.IP)
			pingCancel()

			if err == nil {
				extraInfo = fmt.Sprintf(" %.0fms", latency)
				if connType == "relay" && relay != "" {
					extraInfo += fmt.Sprintf(" [relay:%s]", relay)
				} else if connType == "direct" {
					extraInfo += " [direct]"
				}
			}

			// Add OS if available
			if peer.OS != "" {
				extraInfo += fmt.Sprintf(" (%s)", peer.OS)
			}
		}

		// Show hostname and IP
		peerDisplay := peer.Hostname
		if peer.IP != "" {
			peerDisplay = fmt.Sprintf("%s %s", peer.Hostname, peer.IP)
		}

		fmt.Fprintf(w, "    %s %s%s\n", statusStr, peerDisplay, extraInfo)
	}
}

func printJobQueueInfo(w *tabwriter.Writer) {
	// Get Redis URL from environment (same as work command)
	redisURL := os.Getenv("REDIS_URL")
	if redisURL == "" {
		return
	}

	// Get queue name from environment or use default
	queueName := os.Getenv("WORKER_QUEUE")
	if queueName == "" {
		queueName = "jobs:v1:gpu-general"
	}

	consumerGroup := os.Getenv("CONSUMER_GROUP")
	if consumerGroup == "" {
		consumerGroup = "citadel-workers"
	}

	// Extract just the channel/tag part for display (e.g., "gpu-general" from "jobs:v1:gpu-general")
	channel := extractChannelName(queueName)

	// Connect to Redis
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("Status"), warnColor.Sprintf("Invalid URL: %v", err))
		return
	}

	if password := os.Getenv("REDIS_PASSWORD"); password != "" {
		opts.Password = password
	}

	client := redis.NewClient(opts)
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Check connection
	if err := client.Ping(ctx).Err(); err != nil {
		fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("Status"), badColor.Sprintf("❌ Connection failed: %v", err))
		return
	}

	fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("Status"), goodColor.Sprint("🟢 Listening"))
	fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("Channel"), channel)

	// Get queue length (XLEN) - displayed as "Pending Jobs"
	queueLen, err := client.XLen(ctx, queueName).Result()
	if err == nil {
		fmt.Fprintf(w, "  %s:\t%d\n", labelColor.Sprint("Pending Jobs"), queueLen)
	}

	// Get in-progress count (XPENDING)
	pending, err := client.XPending(ctx, queueName, consumerGroup).Result()
	if err == nil && pending != nil {
		fmt.Fprintf(w, "  %s:\t%d\n", labelColor.Sprint("In Progress"), pending.Count)
	}

	// Get DLQ count - displayed as "Failed Jobs"
	dlqName := getDLQName(queueName)
	dlqLen, err := client.XLen(ctx, dlqName).Result()
	if err == nil {
		colorFn := goodColor
		if dlqLen > 0 {
			colorFn = warnColor
		}
		fmt.Fprintf(w, "  %s:\t%s\n", labelColor.Sprint("Failed Jobs"), colorFn.Sprintf("%d", dlqLen))
	}
}

func extractChannelName(queueName string) string {
	// "jobs:v1:gpu-general" → "gpu-general"
	parts := strings.Split(queueName, ":")
	if len(parts) >= 3 {
		return parts[len(parts)-1]
	}
	return queueName
}

func getDLQName(queueName string) string {
	parts := strings.Split(queueName, ":")
	suffix := parts[len(parts)-1]
	return fmt.Sprintf("dlq:v1:%s", suffix)
}

func printServiceInfo(w *tabwriter.Writer) {
	manifest, configDir, err := findAndReadManifest()
	if err != nil {
		// The error from findAndReadManifest is already user-friendly
		fmt.Fprintf(w, "  %s\n", badColor.Sprint(err.Error()))
		return
	}

	if len(manifest.Services) == 0 {
		fmt.Fprintln(w, "  No managed services are configured.")
		return
	}

	// If services are listed in the manifest, the 'services' directory must exist.
	servicesDir := filepath.Join(configDir, "services")
	if _, statErr := os.Stat(servicesDir); os.IsNotExist(statErr) {
		fmt.Fprintf(w, "  %s\n", warnColor.Sprint("⚠️  Configuration Error"))
		fmt.Fprintf(w, "    The configuration file lists services, but the 'services' directory is missing.\n")
		fmt.Fprintf(w, "    Expected at: %s\n", servicesDir)
		return
	}

	for _, service := range manifest.Services {
		fullComposePath := filepath.Join(configDir, service.ComposeFile)

		// Proactively check if the compose file exists to provide a better error message.
		if _, statErr := os.Stat(fullComposePath); os.IsNotExist(statErr) {
			fmt.Fprintf(w, "  - %s:\t%s\n", service.Name, warnColor.Sprint("⚠️  Configuration Error"))
			fmt.Fprintf(w, "    Compose file not found: %s\n", service.ComposeFile)
			continue
		}

		psCmd := exec.Command("docker", "compose", "-f", fullComposePath, "ps", "--format", "json")
		output, err := psCmd.CombinedOutput() // Use CombinedOutput to get stderr
		if err != nil {
			errMsg := string(output)
			if strings.Contains(errMsg, "permission denied") && strings.Contains(errMsg, "docker.sock") {
				fmt.Fprintf(w, "  - %s:\t%s\n", service.Name, badColor.Sprint("❌ PERMISSION DENIED"))
				fmt.Fprintf(w, "    %s\n", "Could not connect to the Docker daemon.")
				fmt.Fprintf(w, "    %s\n", "Hint: Add your user to the 'docker' group (`sudo usermod -aG docker $USER`)")
				fmt.Fprintf(w, "    %s\n", "      then log out and log back in for the change to take effect.")
			} else {
				fmt.Fprintf(w, "  - %s:\t%s\n", service.Name, warnColor.Sprint("⚠️  Could not get status"))
				fmt.Fprintf(w, "    %s\n", strings.TrimSpace(errMsg))
			}
			continue
		}

		var containers []struct {
			State string `json:"State"`
		}
		decoder := json.NewDecoder(strings.NewReader(string(output)))
		for decoder.More() {
			var c struct {
				State string `json:"State"`
			}
			if err := decoder.Decode(&c); err == nil {
				containers = append(containers, c)
			}
		}

		if len(containers) == 0 {
			fmt.Fprintf(w, "  - %s:\t%s\n", service.Name, labelColor.Sprint("⚫ STOPPED"))
		} else {
			state := strings.ToUpper(containers[0].State)
			var statusStr string
			if strings.Contains(state, "RUNNING") || strings.Contains(state, "UP") {
				statusStr = goodColor.Sprintf("🟢 %s", state)
			} else if strings.Contains(state, "EXITED") || strings.Contains(state, "DEAD") {
				statusStr = badColor.Sprintf("🔴 %s", state)
			} else {
				statusStr = warnColor.Sprintf("🟡 %s", state)
			}
			fmt.Fprintf(w, "  - %s:\t%s\n", service.Name, statusStr)
		}
	}
}

func colorizePercent(p float64) string {
	s := fmt.Sprintf("%.1f%%", p)
	if p > 90.0 {
		return badColor.Sprint(s)
	}
	if p > 75.0 {
		return warnColor.Sprint(s)
	}
	return goodColor.Sprint(s)
}

func colorizeTemp(t string) string {
	temp, err := strconv.ParseFloat(t, 64)
	if err != nil {
		return t
	}
	s := fmt.Sprintf("%s°C", t)
	if temp > 85.0 {
		return badColor.Sprint(s)
	}
	if temp > 70.0 {
		return warnColor.Sprint(s)
	}
	return goodColor.Sprint(s)
}

func formatBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

func init() {
	rootCmd.AddCommand(statusCmd)
	statusCmd.Flags().BoolVarP(&interactiveUI, "interactive", "i", false, "Launch interactive dashboard with live updates")
	statusCmd.Flags().BoolVar(&statusJSON, "json", false, "Output in JSON format")
}