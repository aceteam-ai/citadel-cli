// Package status provides telemetry collection and HTTP server for node status reporting.
//
// This package implements the node-side of the distributed telemetry system that enables
// real-time visibility into Citadel nodes on the AceTeam Fabric page.
//
// Architecture:
//   - StatusCollector gathers metrics from system, GPU, and services
//   - StatusServer exposes an HTTP endpoint for on-demand queries
//   - Both are used by the heartbeat client for periodic reporting
package status

import "time"

// NodeStatus represents the complete status of a Citadel node.
// This is the payload sent in heartbeats and returned from /status endpoint.
type NodeStatus struct {
	Version   string        `json:"version"`
	Timestamp time.Time     `json:"timestamp"`
	Node      NodeInfo      `json:"node"`
	System    SystemMetrics `json:"system"`
	GPU       []GPUMetrics  `json:"gpu,omitempty"`
	Services  []ServiceInfo `json:"services,omitempty"`
}

// NodeInfo contains basic node identification.
type NodeInfo struct {
	Name          string `json:"name"`
	NetworkIP     string `json:"network_ip,omitempty"`     // Preferred: AceTeam Network IP
	TailscaleIP   string `json:"tailscale_ip,omitempty"`   // Kept for backwards compatibility
	UptimeSeconds int64  `json:"uptime_seconds"`
}

// SystemMetrics contains system resource utilization.
type SystemMetrics struct {
	CPUPercent      float64 `json:"cpu_percent"`
	MemoryUsedGB    float64 `json:"memory_used_gb"`
	MemoryTotalGB   float64 `json:"memory_total_gb"`
	MemoryPercent   float64 `json:"memory_percent"`
	DiskUsedGB      float64 `json:"disk_used_gb"`
	DiskTotalGB     float64 `json:"disk_total_gb"`
	DiskPercent     float64 `json:"disk_percent"`
}

// GPUMetrics contains GPU utilization information.
type GPUMetrics struct {
	Index              int     `json:"index"`
	Name               string  `json:"name"`
	MemoryUsedMB       int     `json:"memory_used_mb,omitempty"`
	MemoryTotalMB      int     `json:"memory_total_mb,omitempty"`
	UtilizationPercent float64 `json:"utilization_percent,omitempty"`
	TemperatureCelsius int     `json:"temperature_celsius,omitempty"`
	Driver             string  `json:"driver,omitempty"`
}

// ServiceInfo contains information about a running service.
type ServiceInfo struct {
	Name   string   `json:"name"`
	Type   string   `json:"type"` // "llm", "database", "other"
	Status string   `json:"status"` // "running", "stopped", "error"
	Port   int      `json:"port,omitempty"`
	Health string   `json:"health,omitempty"` // "healthy", "unhealthy", "unknown"
	Models []string `json:"models,omitempty"` // For LLM services
}

// HealthResponse is the response for /health endpoint.
type HealthResponse struct {
	Status  string `json:"status"` // "ok", "degraded", "unhealthy"
	Version string `json:"version"`
}

// ServiceType constants for service classification.
const (
	ServiceTypeLLM      = "llm"
	ServiceTypeDatabase = "database"
	ServiceTypeOther    = "other"
)

// ServiceStatus constants for service state.
const (
	ServiceStatusRunning = "running"
	ServiceStatusStopped = "stopped"
	ServiceStatusError   = "error"
)

// HealthStatus constants for health checks.
const (
	HealthStatusOK        = "ok"
	HealthStatusDegraded  = "degraded"
	HealthStatusUnhealthy = "unhealthy"
	HealthStatusUnknown   = "unknown"
)

// StatusVersion is the current version of the status payload format.
const StatusVersion = "1.0"
