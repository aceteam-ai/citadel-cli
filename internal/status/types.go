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

import (
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/desktop"
)

// NodeStatus represents the complete status of a Citadel node.
// This is the payload sent in heartbeats and returned from /status endpoint.
type NodeStatus struct {
	Version      string                `json:"version"`
	Timestamp    time.Time             `json:"timestamp"`
	Node         NodeInfo              `json:"node"`
	System       SystemMetrics         `json:"system"`
	GPU          []GPUMetrics          `json:"gpu,omitempty"`
	Services     []ServiceInfo         `json:"services,omitempty"`
	Apps         []AppInfo             `json:"apps,omitempty"`
	Capabilities *NodeCapabilities     `json:"capabilities,omitempty"`
	Desktop      *desktop.Capabilities `json:"desktop,omitempty"`
	// DesktopCapabilities is a flat capability map advertised to the control
	// plane so the server can persist it and the frontend can gate desktop
	// affordances (VNC/screenshot/input/terminal buttons) on a per-node basis.
	// Keys: desktop, vnc, screenshot, input_injection, terminal. Additive and
	// backward-compatible: legacy nodes omit it and are treated as "unknown".
	DesktopCapabilities map[string]bool `json:"desktop_capabilities,omitempty"`
	VNCPort             int             `json:"vnc_port"`
	// Worker carries live job-consumption liveness so the platform can tell a
	// process that is alive & heartbeating from one that is actually draining
	// jobs (issue #548). Additive and back-compatible: omitted on nodes that run
	// no worker loop (pure status/desktop nodes) and on legacy builds.
	Worker *WorkerLiveness `json:"worker,omitempty"`
}

// WorkerLiveness is the heartbeat-facing view of the job consume loop. It is the
// signal the platform uses to flag a "green but wedged" node -- one whose
// heartbeat keeps flowing from a separate goroutine while the consume loop is
// blocked and draining nothing (issue #548).
//
// Interpreting the fields together (the platform must qualify, not read one in
// isolation):
//   - Consuming==false && InFlight==0  -> WEDGED: the loop stopped polling and
//     nothing is running. Alert.
//   - Consuming==false && InFlight>0   -> possibly a legitimate long job holding
//     the sequential loop; not necessarily wedged.
//   - Consuming==true                  -> healthy; polling recently.
//
// LastJobConsumedAt alone is intentionally ambiguous (naturally stale on an idle
// node with no work), so it is context, not the alarm.
type WorkerLiveness struct {
	// Consuming is true when a poll cycle completed recently (freshness bound).
	Consuming bool `json:"consuming"`
	// LastJobConsumedAt is when a job was most recently pulled off the queue.
	LastJobConsumedAt *time.Time `json:"last_job_consumed_at,omitempty"`
	// LastPollAt is when the consume loop last completed a poll (job or empty).
	LastPollAt *time.Time `json:"last_poll_at,omitempty"`
	// InFlight is the number of jobs currently executing in a handler.
	InFlight int64 `json:"in_flight"`
	// Processed / Failed are cumulative since worker start (diagnostic context).
	Processed int64 `json:"processed,omitempty"`
	Failed    int64 `json:"failed,omitempty"`
}

// AppInfo contains information about an installed catalog app.
type AppInfo struct {
	Name   string `json:"name"`
	Status string `json:"status"` // "running", "stopped", "not_found"
	Port   int    `json:"port,omitempty"`

	// Idle usage signal for LLM-serving apps (e.g. a vLLM catalog app holding
	// GPU memory). Populated only for running inference apps whose metrics
	// endpoint could be scraped; omitted otherwise. See IdleState.
	*IdleState

	// Footprint is the live resource footprint (CPU/RAM/VRAM/GPU) of the app's
	// container, populated for running managed apps (citadel #421). Omitted for
	// stopped apps or when stats could not be read. Rides the heartbeat so the
	// platform can spot idle GPU hogs.
	Footprint *ServiceFootprint `json:"footprint,omitempty"`
}

// NodeCapabilities describes the GPU and inference engine capabilities of a node.
//
// The four boolean flags (Console/Desktop/Files/GPU) report what the node
// ACTUALLY supports right now, so the AceTeam Fabric UI can show true
// availability instead of guessing (citadel-cli#324). They are ingested by the
// backend exactly as the keys "console"/"desktop"/"files"/"gpu" inside this
// "capabilities" block (aceteam#4223, PR #4231 — CitadelStatus.capabilityFlags).
//
// They are *bool (pointers) so the field is omitted entirely when never set:
// the backend treats an absent flag as "unknown" (tri-state) rather than false,
// keeping legacy nodes that report no flags backward-compatible. The status
// collector always populates all four on every heartbeat, so live nodes always
// emit concrete true/false values.
type NodeCapabilities struct {
	GPUs       []GPUCapability `json:"gpus,omitempty"`
	Engines    []string        `json:"engines,omitempty"`
	Tags       []string        `json:"tags,omitempty"`
	Hypervisor *HypervisorInfo `json:"hypervisor,omitempty"`

	// AvailableServices lists the serving services/engines this build knows how
	// to deploy: the keys of the embedded services.ServiceMap (vllm, ollama,
	// whisper, diffusers, ...). The fabric scheduler uses it to route
	// engine-specific deploys only to capable nodes (aceteam#4483).
	//
	// This is distinct from Engines above:
	//   - AvailableServices = what this binary version CAN run (static per build,
	//     from the embedded compose registry). Sorted for deterministic output.
	//   - Engines = engines currently detected/running on the node.
	// Both can overlap in value (e.g. "vllm"); they answer different questions.
	// Emitted under the "available_services" key so it never collides with the
	// top-level NodeStatus.Services (running service instances). The backend does
	// tolerant matching against these keys (aceteam#4483), so advertising the full
	// runnable set (rather than only configured/enabled ones) is the correct first
	// cut. Omitted on legacy builds, which the backend treats as "unknown".
	AvailableServices []string `json:"available_services,omitempty"`

	// Real node capability flags (citadel-cli#324). Console = shell/SSH
	// available, Desktop = VNC reachable, Files = node-files filesystem access,
	// GPU = GPU present / inference-capable.
	Console *bool `json:"console,omitempty"`
	Desktop *bool `json:"desktop,omitempty"`
	Files   *bool `json:"files,omitempty"`
	GPU     *bool `json:"gpu,omitempty"`

	// H264 reports whether the node can serve an H.264 desktop video stream over
	// the mesh (citadel-cli#338): ffmpeg + an H.264 encoder + an X display are
	// available. Clients use it to choose H.264 streaming and fall back to noVNC
	// when absent. Additive to the four flags above (aceteam#4250).
	H264 *bool `json:"h264,omitempty"`
}

// HypervisorInfo describes a detected hypervisor on the node.
type HypervisorInfo struct {
	Type      string `json:"type"`                 // e.g. "proxmox"
	Version   string `json:"version,omitempty"`    // e.g. "pve-manager/8.2.4/..."
	NodeName  string `json:"node_name,omitempty"`  // this hypervisor node's name
	NodeCount int    `json:"node_count,omitempty"` // total nodes in cluster
	VMCount   int    `json:"vm_count,omitempty"`   // VMs on this node
	CTCount   int    `json:"ct_count,omitempty"`   // containers on this node
}

// GPUCapability describes a single GPU's identity for capability reporting.
type GPUCapability struct {
	Name    string `json:"name"`
	VRAMMb  int    `json:"vram_mb"`
	Tag     string `json:"tag"`
	VRAMTag string `json:"vram_tag"`
}

// NodeInfo contains basic node identification.
type NodeInfo struct {
	Name          string `json:"name"`
	NetworkIP     string `json:"network_ip,omitempty"`   // Preferred: AceTeam Network IP
	TailscaleIP   string `json:"tailscale_ip,omitempty"` // Kept for backwards compatibility
	UptimeSeconds int64  `json:"uptime_seconds"`
}

// SystemMetrics contains system resource utilization.
type SystemMetrics struct {
	CPUPercent    float64 `json:"cpu_percent"`
	MemoryUsedGB  float64 `json:"memory_used_gb"`
	MemoryTotalGB float64 `json:"memory_total_gb"`
	MemoryPercent float64 `json:"memory_percent"`
	DiskUsedGB    float64 `json:"disk_used_gb"`
	DiskTotalGB   float64 `json:"disk_total_gb"`
	DiskPercent   float64 `json:"disk_percent"`
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
	Type   string   `json:"type"`   // "llm", "database", "other"
	Status string   `json:"status"` // "running", "stopped", "error"
	Port   int      `json:"port,omitempty"`
	Health string   `json:"health,omitempty"` // "healthy", "unhealthy", "unknown"
	Models []string `json:"models,omitempty"` // For LLM services

	// Idle usage signal for running LLM services. Populated only when the
	// service is a running inference engine whose metrics endpoint could be
	// scraped; omitted otherwise. Promotes idle/idle_seconds/last_request_at
	// to the top level of the JSON object. See IdleState.
	*IdleState

	// Footprint is the live resource footprint (CPU/RAM/VRAM/GPU) of the
	// service's container, populated for running managed services (citadel
	// #421). Omitted when stats could not be read. Rides the heartbeat so the
	// platform can spot a heavy-and-idle eviction candidate.
	Footprint *ServiceFootprint `json:"footprint,omitempty"`
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
