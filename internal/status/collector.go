package status

import (
	"context"
	"encoding/json"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/apps"
	"github.com/aceteam-ai/citadel-cli/internal/compose"
	"github.com/aceteam-ai/citadel-cli/internal/desktop"
	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/aceteam-ai/citadel-cli/services"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/host"
	"github.com/shirou/gopsutil/v3/mem"
)

// Collector gathers status metrics from the system, GPU, and services.
type Collector struct {
	nodeName       string
	configDir      string
	services       []ServiceConfig
	startTime      time.Time
	modelDiscovery *ModelDiscovery
	capabilities   *NodeCapabilities      // cached capabilities (set once at startup)
	workerLiveness func() *WorkerLiveness // live worker consume-loop liveness (issue #548), optional
	idleTracker    *IdleTracker           // metrics-based per-service idle detection (aceteam#4472 / citadel #416)
	fpIdleTracker  *FootprintIdleTracker  // footprint-derived idle for engines #416 can't scrape (citadel #421)
	netIdleTracker *IdleTracker           // network-activity idle for non-vLLM services (citadel #433)
}

// ServiceConfig holds the configuration for a service from the manifest.
type ServiceConfig struct {
	Name        string
	Type        string // "llm", "database", "other"
	ComposeFile string
	Port        int
}

// CollectorConfig holds configuration for the status collector.
type CollectorConfig struct {
	NodeName     string
	ConfigDir    string
	Services     []ServiceConfig
	Capabilities *NodeCapabilities // pre-detected capabilities (optional)
	// WorkerLiveness, when set, returns the live consume-loop liveness attached
	// to each heartbeat so the platform can flag "green but wedged" nodes
	// (issue #548). Optional: nil on nodes with no worker loop.
	WorkerLiveness func() *WorkerLiveness
}

// NewCollector creates a new status collector.
func NewCollector(cfg CollectorConfig) *Collector {
	return &Collector{
		nodeName:       cfg.NodeName,
		configDir:      cfg.ConfigDir,
		services:       cfg.Services,
		startTime:      time.Now(),
		modelDiscovery: NewModelDiscovery(),
		capabilities:   cfg.Capabilities,
		workerLiveness: cfg.WorkerLiveness,
		idleTracker:    NewIdleTracker(IdleThresholdSeconds()),
		fpIdleTracker:  NewFootprintIdleTracker(),
		netIdleTracker: NewIdleTracker(IdleThresholdSeconds()),
	}
}

// SetCapabilities sets the cached capabilities for heartbeat publishing.
func (c *Collector) SetCapabilities(caps *NodeCapabilities) {
	c.capabilities = caps
}

// Collect gathers all status metrics and returns a NodeStatus.
func (c *Collector) Collect() (*NodeStatus, error) {
	status := &NodeStatus{
		Version:   StatusVersion,
		Timestamp: time.Now().UTC(),
	}

	// Collect node info
	status.Node = c.collectNodeInfo()

	// Collect system metrics
	status.System = c.collectSystemMetrics()

	// Collect GPU metrics
	status.GPU = c.collectGPUMetrics()

	// Collect service status
	status.Services = c.collectServiceStatus()

	// Collect status for managed serving engines that are running but not
	// present in the manifest-driven services list (the common case: c.services
	// is nil in the heartbeat path). This is what surfaces the per-service idle
	// signal for a running vLLM in the heartbeat (citadel #416). Skip any engine
	// already reported by the manifest-driven path above to avoid a duplicate
	// entry when both paths are active.
	reported := make(map[string]struct{}, len(status.Services))
	for _, svc := range status.Services {
		reported[svc.Name] = struct{}{}
	}
	for _, eng := range c.collectManagedEngineStatus() {
		if _, dup := reported[eng.Name]; dup {
			continue
		}
		status.Services = append(status.Services, eng)
	}

	// Collect installed app status
	status.Apps = c.collectAppStatus()

	// Attach live resource footprints (CPU/RAM/VRAM/GPU) to running managed
	// services and apps in one batched pass (citadel #421). This is what makes
	// the heartbeat and TUI resource-aware: a running-but-idle vLLM/diffusers
	// holding GPU/RAM is now visible instead of a bare "running" with no
	// footprint. One stats call + one nvidia-smi pair for the whole set.
	c.attachFootprints(status)

	// Include cached capabilities if available. Copy the cached struct rather
	// than aliasing it, so populating the per-heartbeat capability flags below
	// does not mutate the shared cached value (Collect may run concurrently for
	// the status server and the heartbeat publisher).
	if c.capabilities != nil {
		capsCopy := *c.capabilities
		status.Capabilities = &capsCopy
	}

	// Collect desktop capabilities
	status.Desktop = desktop.DetectCapabilities()

	// Advertise a flat desktop capability map in the handshake so the server can
	// gate desktop affordances per node (headless nodes report desktop=false).
	if status.Desktop != nil && status.Desktop.Session != nil {
		status.DesktopCapabilities = status.Desktop.Session.CapabilityMap()
	}

	// Detect VNC server status for top-level vnc_port field.
	// Check embedded VNC server first (TUI-managed), then external (TightVNC etc).
	if port := platform.EmbeddedVNCPort(); port > 0 {
		status.VNCPort = port
	} else {
		vncMgr := platform.GetVNCManager()
		if vncMgr.IsRunning() {
			status.VNCPort = vncMgr.Port()
		}
	}

	// Report the four real node-capability flags (console/desktop/files/gpu) so
	// the AceTeam Fabric UI shows true availability (citadel-cli#324). Ensure a
	// capabilities block exists even when no pre-detected NodeCapabilities was
	// supplied, then populate the flags. Desktop is derived from the VNCPort
	// already computed above to avoid a redundant probe.
	if status.Capabilities == nil {
		status.Capabilities = &NodeCapabilities{}
	}
	populateCapabilityFlags(status.Capabilities, status.VNCPort)

	// Advertise the serving services this build can deploy (embedded ServiceMap
	// keys) so the fabric can schedule engine-specific deploys only to capable
	// nodes (aceteam#4483).
	populateServices(status.Capabilities)

	// Attach live worker consume-loop liveness so the platform can distinguish a
	// heartbeating-but-wedged node from one actually draining jobs (issue #548).
	if c.workerLiveness != nil {
		status.Worker = c.workerLiveness()
	}

	return status, nil
}

// populateServices sets AvailableServices to the sorted list of serving
// services this build knows how to deploy (the embedded services.ServiceMap
// keys). It advertises what the binary CAN run, not what is currently
// configured/running, matching the backend's tolerant matching (aceteam#4483).
func populateServices(caps *NodeCapabilities) {
	caps.AvailableServices = services.GetAvailableServices()
}

// CollectCompact returns a minimal status suitable for heartbeats.
func (c *Collector) CollectCompact() (*NodeStatus, error) {
	return c.Collect() // For now, same as full collection
}

// collectNodeInfo gathers basic node identification.
func (c *Collector) collectNodeInfo() NodeInfo {
	info := NodeInfo{
		Name:          c.nodeName,
		UptimeSeconds: int64(time.Since(c.startTime).Seconds()),
	}

	// Get Network IP - populate both fields for backwards compatibility
	networkIP := c.getNetworkIP()
	info.NetworkIP = networkIP
	info.TailscaleIP = networkIP // Kept for backwards compatibility

	return info
}

// getNetworkIP retrieves the node's AceTeam Network IP address.
func (c *Collector) getNetworkIP() string {
	ip, err := network.GetGlobalIPv4()
	if err != nil {
		return ""
	}
	return ip
}

// collectSystemMetrics gathers CPU, memory, and disk utilization.
func (c *Collector) collectSystemMetrics() SystemMetrics {
	var metrics SystemMetrics

	// Memory
	if v, err := mem.VirtualMemory(); err == nil {
		metrics.MemoryUsedGB = float64(v.Used) / (1024 * 1024 * 1024)
		metrics.MemoryTotalGB = float64(v.Total) / (1024 * 1024 * 1024)
		metrics.MemoryPercent = v.UsedPercent
	}

	// CPU
	if percentages, err := cpu.Percent(100*time.Millisecond, false); err == nil && len(percentages) > 0 {
		metrics.CPUPercent = percentages[0]
	}

	// Disk
	if d, err := disk.Usage("/"); err == nil {
		metrics.DiskUsedGB = float64(d.Used) / (1024 * 1024 * 1024)
		metrics.DiskTotalGB = float64(d.Total) / (1024 * 1024 * 1024)
		metrics.DiskPercent = d.UsedPercent
	}

	return metrics
}

// collectGPUMetrics gathers GPU utilization from NVIDIA or Metal.
func (c *Collector) collectGPUMetrics() []GPUMetrics {
	detector, err := platform.GetGPUDetector()
	if err != nil || !detector.HasGPU() {
		return nil
	}

	gpuInfos, err := detector.GetGPUInfo()
	if err != nil {
		return nil
	}

	gpuMetrics := make([]GPUMetrics, 0, len(gpuInfos))
	for i, gpu := range gpuInfos {
		metrics := GPUMetrics{
			Index:  i,
			Name:   gpu.Name,
			Driver: gpu.Driver,
		}

		// Parse total memory (e.g., "24576 MB" or "24 GB")
		if gpu.Memory != "" {
			memStr := strings.TrimSuffix(gpu.Memory, " MB")
			memStr = strings.TrimSuffix(memStr, "MB")
			if mb, err := strconv.Atoi(strings.TrimSpace(memStr)); err == nil {
				metrics.MemoryTotalMB = mb
			}
		}

		// Parse used memory (e.g., "8192 MB")
		if gpu.MemoryUsed != "" {
			memStr := strings.TrimSuffix(gpu.MemoryUsed, " MB")
			memStr = strings.TrimSuffix(memStr, "MB")
			if mb, err := strconv.Atoi(strings.TrimSpace(memStr)); err == nil {
				metrics.MemoryUsedMB = mb
			}
		}

		// Parse temperature (e.g., "72°C")
		if gpu.Temperature != "" {
			tempStr := strings.TrimSuffix(gpu.Temperature, "°C")
			if temp, err := strconv.Atoi(strings.TrimSpace(tempStr)); err == nil {
				metrics.TemperatureCelsius = temp
			}
		}

		// Parse utilization (e.g., "85%")
		if gpu.Utilization != "" {
			utilStr := strings.TrimSuffix(gpu.Utilization, "%")
			if util, err := strconv.ParseFloat(strings.TrimSpace(utilStr), 64); err == nil {
				metrics.UtilizationPercent = util
			}
		}

		gpuMetrics = append(gpuMetrics, metrics)
	}

	return gpuMetrics
}

// collectServiceStatus gathers status for all configured services.
func (c *Collector) collectServiceStatus() []ServiceInfo {
	services := make([]ServiceInfo, 0, len(c.services))
	ctx := context.Background()

	for _, svc := range c.services {
		info := ServiceInfo{
			Name:   svc.Name,
			Type:   svc.Type,
			Port:   svc.Port,
			Status: ServiceStatusStopped,
			Health: HealthStatusUnknown,
		}

		// Check if service is running using docker compose
		if svc.ComposeFile != "" {
			status := c.getDockerComposeStatus(svc.ComposeFile)
			info.Status = status
			if status == ServiceStatusRunning {
				info.Health = HealthStatusOK
			}
		}

		// For LLM services, try to discover loaded models
		if info.Status == ServiceStatusRunning && info.Type == ServiceTypeLLM && info.Port > 0 {
			// Try model discovery based on service name
			serviceType := c.detectLLMServiceType(svc.Name)
			if serviceType != "" {
				if models, err := c.modelDiscovery.DiscoverModels(ctx, serviceType, svc.Port); err == nil {
					info.Models = models
				}
				// Also do a health check
				if health, err := c.modelDiscovery.CheckServiceHealth(ctx, serviceType, svc.Port); err == nil {
					info.Health = health
				}
			}
			// Attach the per-service idle signal for engines we can scrape.
			if idle := c.observeIdle(ctx, svc.Name, svc.Name, svc.Port); idle != nil {
				info.IdleState = idle
			}
		}

		services = append(services, info)
	}

	return services
}

// detectLLMServiceType determines the LLM service type from the service name.
func (c *Collector) detectLLMServiceType(serviceName string) string {
	return EngineTypeFromName(serviceName)
}

// getDockerComposeStatus checks if a docker compose service is running.
func (c *Collector) getDockerComposeStatus(composeFile string) string {
	// Pass the install-time config env (<name>.env) explicitly: compose
	// interpolates the file even for `ps`, so a ${VAR:?...}-guarded service
	// (claudecode, livekit) would report a false ServiceStatusError on every
	// heartbeat without it.
	args := append([]string{"compose", "-f", composeFile}, compose.EnvFileArgs(composeFile)...)
	args = append(args, "ps", "--format", "json")
	cmd := exec.Command("docker", args...)
	output, err := cmd.Output()
	if err != nil {
		return ServiceStatusError
	}

	// Parse JSON output (each line is a separate JSON object)
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	for _, line := range lines {
		if line == "" {
			continue
		}
		var container struct {
			State string `json:"State"`
		}
		if err := json.Unmarshal([]byte(line), &container); err == nil {
			state := strings.ToLower(container.State)
			if strings.Contains(state, "running") || strings.Contains(state, "up") {
				return ServiceStatusRunning
			}
		}
	}

	return ServiceStatusStopped
}

// GetSystemUptime returns the host system uptime in seconds.
func GetSystemUptime() (int64, error) {
	info, err := host.Info()
	if err != nil {
		return 0, err
	}
	return int64(info.Uptime), nil
}

// InferServiceType attempts to determine service type from name.
func InferServiceType(serviceName string) string {
	name := strings.ToLower(serviceName)

	// LLM services
	llmKeywords := []string{"vllm", "ollama", "llamacpp", "llama.cpp", "lmstudio", "llm", "inference"}
	for _, keyword := range llmKeywords {
		if strings.Contains(name, keyword) {
			return ServiceTypeLLM
		}
	}

	// Database services
	dbKeywords := []string{"postgres", "mysql", "redis", "mongo", "elasticsearch", "database", "db"}
	for _, keyword := range dbKeywords {
		if strings.Contains(name, keyword) {
			return ServiceTypeDatabase
		}
	}

	return ServiceTypeOther
}

// collectAppStatus queries installed catalog apps for their Docker container status.
func (c *Collector) collectAppStatus() []AppInfo {
	state, err := apps.LoadState()
	if err != nil {
		return nil
	}

	if len(state.Apps) == 0 {
		return nil
	}

	ctx := context.Background()
	runner := apps.ExecRunner{}
	result := make([]AppInfo, 0, len(state.Apps))

	for _, installed := range state.Apps {
		dockerStatus := apps.ContainerStatus(ctx, runner, installed.Name)
		info := AppInfo{
			Name:   installed.Name,
			Status: dockerStatus,
			Port:   installed.HostPort,
		}
		// Attach the per-service idle signal for running inference apps (e.g. a
		// vLLM catalog app holding GPU memory). This is the live heartbeat path:
		// catalog apps are always collected, so the idle signal reaches every
		// heartbeat without any manifest wiring. The engine is discriminated on
		// both the app name and its image (e.g. "vllm/vllm-openai"), since a
		// catalog slug like "llm-server" would not match on name alone.
		if dockerStatus == "running" && installed.HostPort > 0 {
			if idle := c.observeIdle(ctx, installed.Name, installed.Name+" "+installed.Image, installed.HostPort); idle != nil {
				info.IdleState = idle
			}
		}
		result = append(result, info)
	}

	return result
}

// observeIdle scrapes the idle signal for a serving engine on the given port.
// key is the stable per-service identity used to accumulate idle_seconds across
// calls; hint is a free-text discriminator (name and/or image) used to select
// the metrics dialect. Returns nil when the hint does not map to a scrapeable
// engine or the metrics endpoint could not be read, so callers omit the idle
// fields rather than report a misleading value.
func (c *Collector) observeIdle(ctx context.Context, key, hint string, port int) *IdleState {
	engine := idleEngineType(hint)
	if engine == "" || c.idleTracker == nil {
		return nil
	}
	state, ok := c.idleTracker.Observe(ctx, key, engine, port)
	if !ok {
		return nil
	}
	return &state
}

// idleEngineType maps a service/app name-or-image hint to the metrics dialect
// used for idle detection, or "" when the engine has no scrapeable request
// signal. Only vLLM exposes the Prometheus request counters + running/waiting
// gauges this feature relies on; other engines (ollama, llama.cpp) are skipped
// for now.
func idleEngineType(hint string) string {
	if strings.Contains(strings.ToLower(hint), "vllm") {
		return "vllm"
	}
	return ""
}

// InferServicePort attempts to determine port from service name.
func InferServicePort(serviceName string) int {
	name := strings.ToLower(serviceName)

	// Common default ports. The citadel-owned host ports (services/ports.go)
	// are used for the engines whose host publish citadel controls so status
	// probes hit the actual published port.
	portMap := map[string]int{
		"vllm":          services.VLLMHostPort,
		"ollama":        11434,
		"llamacpp":      services.LlamacppHostPort,
		"lmstudio":      1234,
		"postgres":      5432,
		"mysql":         3306,
		"redis":         6379,
		"mongo":         27017,
		"elasticsearch": 9200,
	}

	for keyword, port := range portMap {
		if strings.Contains(name, keyword) {
			return port
		}
	}

	return 0
}
