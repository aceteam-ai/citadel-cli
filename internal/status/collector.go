package status

import (
	"context"
	"encoding/json"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
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
	NodeName  string
	ConfigDir string
	Services  []ServiceConfig
}

// NewCollector creates a new status collector.
func NewCollector(cfg CollectorConfig) *Collector {
	return &Collector{
		nodeName:       cfg.NodeName,
		configDir:      cfg.ConfigDir,
		services:       cfg.Services,
		startTime:      time.Now(),
		modelDiscovery: NewModelDiscovery(),
	}
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

	return status, nil
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

	// Get Network IP
	info.TailscaleIP = c.getNetworkIP()

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

		// Parse memory (e.g., "24576 MB" or "24 GB")
		if gpu.Memory != "" {
			memStr := strings.TrimSuffix(gpu.Memory, " MB")
			memStr = strings.TrimSuffix(memStr, "MB")
			if mb, err := strconv.Atoi(strings.TrimSpace(memStr)); err == nil {
				metrics.MemoryTotalMB = mb
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
		}

		services = append(services, info)
	}

	return services
}

// detectLLMServiceType determines the LLM service type from the service name.
func (c *Collector) detectLLMServiceType(serviceName string) string {
	name := strings.ToLower(serviceName)
	if strings.Contains(name, "vllm") {
		return "vllm"
	}
	if strings.Contains(name, "ollama") {
		return "ollama"
	}
	return ""
}

// getDockerComposeStatus checks if a docker compose service is running.
func (c *Collector) getDockerComposeStatus(composeFile string) string {
	cmd := exec.Command("docker", "compose", "-f", composeFile, "ps", "--format", "json")
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

// InferServicePort attempts to determine port from service name.
func InferServicePort(serviceName string) int {
	name := strings.ToLower(serviceName)

	// Common default ports
	portMap := map[string]int{
		"vllm":          8000,
		"ollama":        11434,
		"llamacpp":      8080,
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
