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
	// Swap carries model-hotswap activity (citadel-cli#687, #717) so "is this
	// node thrashing?" is answerable from the heartbeat instead of shell access.
	// Additive and back-compatible: omitted whenever no swap manager is wired in
	// (model hotswap disabled via CITADEL_MODEL_HOTSWAP, no config dir, or a
	// legacy build).
	Swap *SwapActivity `json:"swap,omitempty"`
	// Reconcile carries the queryable full-wipe-guard refusal state
	// (citadel-cli#742): a node whose desired-state reconcile pass is being
	// refused by the full-wipe guard (internal/reconcile.Reconciler.
	// RefuseFullWipe) looks green on every other signal -- it heartbeats fine,
	// it just silently stopped converging module state -- so this field is the
	// alarm. Unlike Worker/Swap above (present whenever their subsystem is
	// wired, healthy or not), this is present ONLY while a refusal is
	// currently active; omitted the rest of the time, including on a node that
	// has never hit the guard, so this addition changes no existing heartbeat
	// payload until a node actually enters the refused state. See
	// internal/reconcile.HealthTracker for the throttled-logging half of the
	// same fix, and reconcileHealthFrom (cmd/work.go) for the conversion.
	Reconcile *ReconcileHealth `json:"reconcile,omitempty"`
	// GPUReservations lists active job-scoped GPU VRAM reservations (citadel-cli
	// #832) not yet released — durable evicted_by_job tags in citadel.yaml,
	// extending #577's plain preemption with an auto-restore leg. Surfaced so a
	// scheduler consuming the heartbeat (a future issue, out of scope here) can
	// see which nodes currently hold exclusive reservations and what they are
	// blocking, without shelling into the node. Additive/omitempty: nil on a
	// node with none active, no reservation provider wired, or a legacy build.
	GPUReservations []GPUReservation `json:"gpu_reservations,omitempty"`
	// Lanes reports the live activity of the node's bounded execution lanes
	// (citadel-cli#908): how many jobs are queued vs executing per lane, and
	// since when each lane has been fully saturated. It lets a dispatcher see a
	// busy-but-healthy node (one whose lane is at capacity) rather than only
	// reacting after a fast-fail or a queued response. Additive/omitempty: nil on
	// a node with no lanes wired (no worker loop) or a legacy build; a heartbeat
	// consumer that does not know this field is unaffected. Projected from
	// worker.LaneSnapshot by cmd/work.go's laneActivityFrom (internal/status
	// cannot import internal/worker); TestLaneShapeParity pins the two shapes.
	Lanes []LaneActivity `json:"lanes,omitempty"`
	// PairingDisplay advertises which surfaces this node can currently render
	// a platform-pushed node:exec pairing code on (citadel-cli#659 P0). Nil
	// means no capability -- the backend's `_node_screen_delivery` falls
	// through to the operator's linked device, exactly as it does on any
	// pre-#659 node. Additive/omitempty; a legacy build, a node with no VT
	// subsystem (container, headless without a console), or a node whose
	// active console is a graphical session all report this as absent. See
	// internal/pairingdisplay.DetectSurfaces for what the probe actually
	// checks (deliberately NOT internal/desktop.DetectCapabilities -- see
	// docs/design-pairing-display.md §2.2/§9.1 for why that signal is the
	// wrong gate for a root systemd worker).
	PairingDisplay *PairingDisplayCapability `json:"pairing_display,omitempty"`
}

// PairingDisplayCapability is the heartbeat-facing pairing-display
// capability (citadel-cli#659 P0). Surfaces is non-empty only when the
// node can render right now; P0 only ever reports ["console"] (the Linux
// VT-text-console mechanism). Later phases may add "pull"/"tui"/"gui".
type PairingDisplayCapability struct {
	Surfaces []string `json:"surfaces"`
}

// LaneActivity is the heartbeat-facing view of one bounded execution lane
// (citadel-cli#908) -- the "how loaded is this node right now" signal (§4). It
// is a hand-maintained mirror of worker.LaneSnapshot (internal/status cannot
// import internal/worker), same split as SwapActivity/WorkerLiveness; the
// conversion is laneActivityFrom in cmd/work.go.
type LaneActivity struct {
	// Lane names the lane: "unbounded" (manifest/lockfile writers, exec
	// concurrency 1) or "inference" (GPU-bound, exec concurrency = GPU count).
	Lane string `json:"lane"`
	// Queued is the number of jobs claimed and admitted onto this lane but still
	// waiting for a free execution slot.
	Queued int `json:"queued"`
	// Executing is the number of jobs currently inside a handler on this lane.
	Executing int `json:"executing"`
	// ExecCapacity is the lane's execution concurrency (the ceiling on
	// Executing).
	ExecCapacity int `json:"exec_capacity"`
	// BusySince is set the moment Executing first reaches ExecCapacity (the lane
	// is fully saturated) and cleared the moment it drops below -- the literal
	// "busy since T" primitive. Omitted while the lane has spare capacity.
	BusySince *time.Time `json:"busy_since,omitempty"`
}

// GPUReservation describes one active job-scoped GPU VRAM reservation
// (citadel-cli#832): a durable evicted_by_job tag in citadel.yaml that has not
// yet been released. A reservation restores automatically when the reserving
// job releases it, or — if the reserving process crashed instead — at the next
// worker startup. See internal/jobs.ServiceHandler.Reserve / .Release /
// .ReconcileOrphanedReservations for the primitive this projects.
type GPUReservation struct {
	// JobID is the reservation's key, matching the id a future caller (e.g.
	// `model run --exclusive`) passed to Reserve.
	JobID string `json:"job_id"`
	// EvictedServices lists the non-pinned services this reservation durably
	// stopped to free VRAM, in eviction order.
	EvictedServices []string `json:"evicted_services,omitempty"`
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

	// IdentityUnresolved reports that this worker never resolved its Headscale
	// node ID. Such a node is NOT wedged -- it polls, it heartbeats, it serves
	// untargeted work, so every field above reads healthy -- but it declines
	// every target_node-addressed job (citadel-cli#654), which surfaces to the
	// caller as an unexplained dispatch timeout far from the cause. This is the
	// field that explains it, and it is the only one here describing a node that
	// is degraded rather than stuck.
	//
	// Stated positively on purpose: the node's ID is not on this struct at all,
	// and on the /agent snapshot it is `omitempty`, so its absence cannot
	// distinguish a degraded node from an older payload.
	IdentityUnresolved bool `json:"identity_unresolved,omitempty"`

	// PubSubTransport is the transport the node's pub/sub publishes currently
	// use: "websocket" (real-time, healthy) or "http" (the fallback). Empty on
	// nodes with no API-mode client and on legacy builds.
	//
	// It is here because "http" is a FAILURE, not a mode: the HTTP publish
	// fallback misreads the route's ack and always reports failure
	// (citadel-cli#721), so a node on it publishes no durable heartbeat and the
	// platform reads it as offline while every other field above looks healthy.
	// Nothing else distinguishes that node from a working one (issue #723).
	PubSubTransport string `json:"pubsub_transport,omitempty"`
}

// SwapActivity is the heartbeat-facing view of model-hotswap activity
// (citadel-cli#632, #687, #717) — the operator-facing worker.SwapStats
// projected onto the heartbeat. internal/status cannot import internal/worker
// (worker already imports status), so this is a hand-maintained mirror; the
// conversion lives in cmd/work.go (swapStatsFrom), same split as
// WorkerLiveness/workerLivenessFrom above.
//
// Present only when a swap manager is wired on this node (model hotswap on,
// which is the default, AND a config dir resolved); absent entirely otherwise,
// so a hotswap-off heartbeat is unchanged by this field's existence.
type SwapActivity struct {
	// SwapsPerHour counts every swap attempt in the trailing window.
	SwapsPerHour int `json:"swaps_per_hour"`
	// EvictingSwapsPerHour counts only swaps that stopped a resident engine —
	// the subset the swap rate bound (citadel-cli#687) governs.
	EvictingSwapsPerHour int `json:"evicting_swaps_per_hour"`
	// MaxEvictingPerHour is the ceiling in force, so a reader can tell how close
	// to refusing the node is without knowing the build's defaults.
	MaxEvictingPerHour int `json:"max_evicting_per_hour"`
	// Recent holds the most recent swap records, oldest first.
	Recent []SwapRecord `json:"recent,omitempty"`
}

// SwapRecord is one swap this node attempted, mirroring worker.SwapRecord.
//
// Deliberately absent: "whether a pull was required" (citadel-cli#717, part
// 2). The swap manager issues a SERVICE_START and, for docker-based engines,
// any weights pull happens opaquely inside the container's own startup —
// invisible to the Go code driving `docker compose up`. Reporting it honestly
// needs the start path itself to observe and report a pull, which is
// straightforward for the native ollama path but not for the docker path
// without new engine-specific instrumentation; a guessed field here is worse
// than an absent one, so it stays out until that instrumentation exists.
type SwapRecord struct {
	// Backend is the engine swapped in.
	Backend string `json:"backend"`
	// Model is the model it was started for.
	Model string `json:"model,omitempty"`
	// Evicted names the engines stopped to make room, in stop order. Empty when
	// the swap fit in free VRAM.
	Evicted []string `json:"evicted,omitempty"`
	// StartedAt is when the swap began.
	StartedAt time.Time `json:"started_at"`
	// Wait is how long the swap ran before reaching its outcome.
	Wait time.Duration `json:"wait"`
	// Outcome is one of the swap outcome values: "ready", "warming", "failed",
	// "blocked", "rate_limited".
	Outcome string `json:"outcome"`
}

// ReconcileHealth is the heartbeat-facing mirror of reconcile.HealthState
// (citadel-cli#742). Unlike SwapActivity above, this mirror is not forced by
// an import cycle -- internal/status could import internal/reconcile without
// one -- it follows the same hand-maintained-mirror-plus-conversion
// convention as a deliberate choice, keeping this package's heartbeat types
// free of a dependency on the reconcile engine's own types. The conversion is
// reconcileHealthFrom in cmd/work.go, and the "HealthState/ReconcileHealth"
// subtest of TestSwapShapeParity (cmd/work_test.go) keeps the two struct
// shapes from silently drifting apart.
//
// Present on NodeStatus ONLY while Refused is true (see NodeStatus.Reconcile's
// doc); every field here is therefore non-zero whenever this struct is
// reachable at all.
type ReconcileHealth struct {
	// Refused is always true when this block is present.
	Refused bool `json:"refused"`
	// Reason is the full-wipe guard's error text for the CURRENT (most
	// recent) refused pass.
	Reason string `json:"reason,omitempty"`
	// Since is when the refused streak began (the false->true transition).
	Since time.Time `json:"since,omitempty"`
	// Count is the number of consecutive refused passes since Since.
	Count int `json:"count,omitempty"`
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
	// MemoryAvailableGB is RAM available for programs to allocate (gopsutil's
	// kernel-computed "available", e.g. Linux MemAvailable — it already
	// accounts for reclaimable page cache, unlike a naive total-used).
	// This is the number the routing gate needs (citadel #833): the incident
	// that motivated it was a node with plenty of free VRAM but ~0 free RAM
	// (a CPU-offloaded text encoder needing ~19GB RAM), OOM-killed because
	// nothing surfaced RAM headroom for scheduling to gate on. Zero when
	// unavailable (no fabricated value), matching the sibling fields above.
	MemoryAvailableGB float64 `json:"memory_available_gb"`
	DiskUsedGB        float64 `json:"disk_used_gb"`
	DiskTotalGB       float64 `json:"disk_total_gb"`
	DiskPercent       float64 `json:"disk_percent"`
	// DiskAvailableGB is free space on the root filesystem (gopsutil's Free),
	// the same "/" mount the sibling Disk* fields above already measure.
	// Zero when unavailable, matching the sibling fields.
	DiskAvailableGB float64 `json:"disk_available_gb"`
}

// GPUMetrics contains GPU utilization information.
type GPUMetrics struct {
	Index         int    `json:"index"`
	Name          string `json:"name"`
	MemoryUsedMB  int    `json:"memory_used_mb,omitempty"`
	MemoryTotalMB int    `json:"memory_total_mb,omitempty"`
	// MemoryFreeMB is free VRAM as reported directly by nvidia-smi's
	// memory.free query (citadel #833; platform.GPUInfo.MemoryFree). It is
	// deliberately NOT MemoryTotalMB-MemoryUsedMB: nvidia-smi reserves memory
	// that counts against neither, so a derived value systematically
	// overstates what's actually free — the wrong direction of error for a
	// signal meant to prevent an OOM/placement mistake. Omitted when unknown
	// (e.g. macOS/Metal, which does not report it).
	//
	// Zero is ambiguous by construction (0 = "genuinely no free VRAM" OR
	// "unset/unknown"), same as MemoryUsedMB/MemoryTotalMB's existing
	// omitempty convention on this struct. A consumer that must distinguish
	// "full GPU" from "no signal" needs to gate on MemoryTotalMB>0 as well
	// (internal/jobs.freeVRAMBytes already does, treating <=0 as "fall back
	// to the derived total-used value" for exactly this reason).
	MemoryFreeMB       int     `json:"memory_free_mb,omitempty"`
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

	// Pinned reports whether this service is in the node's pinned_services
	// allowlist and therefore NEVER preemptible to make room for another deploy
	// (citadel-cli#577). Omitted (false) for preemptible services — the default.
	// The platform reads this to show pinned vs preemptible per node.
	Pinned bool `json:"pinned,omitempty"`

	// Resident reports whether an installed serving engine's model is currently
	// loaded and serving (container up) as opposed to merely installed-and-
	// swappable (container stopped). Populated ONLY when model hotswap is enabled
	// (CITADEL_MODEL_HOTSWAP, citadel-cli#632); nil otherwise, so the omitempty on
	// this *bool keeps the flag-off heartbeat byte-identical. nil = residency not
	// tracked; true = resident; false = installed but not resident (a swap-in
	// candidate resolve_fabric_target may route to and let the node swap in).
	Resident *bool `json:"resident,omitempty"`

	// VRAMEstimateMB is the approximate GPU memory (MB) this engine's model
	// occupies (running) or would occupy (stopped) when resident. Populated only
	// under model hotswap (citadel-cli#632) so the platform can reason about swap
	// fit: the live footprint VRAM for a running engine, else a coarse per-engine
	// estimate. Omitted (0) when unknown or hotswap off.
	VRAMEstimateMB int `json:"vram_estimate_mb,omitempty"`

	// Readiness is a four-valued lifecycle/protocol classification, PURELY
	// ADDITIVE to Status/Health (citadel-cli#684): one of ReadinessDown,
	// ReadinessStarting, ReadinessReady, ReadinessUnhealthy. Status and Health
	// keep their pre-#684 values byte-for-byte for every case that was already
	// classified correctly -- Readiness exists because "up but not serving yet"
	// (a loading vLLM/TEI) previously had no honest lifecycle name of its own and
	// either vanished from the heartbeat entirely or was indistinguishable from a
	// fully stopped/never-installed engine. Omitted when the producer performed
	// no readiness classification (e.g. the collectRunningEmbeddedServices
	// backstop, which runs no probe at all) -- omission means "not evaluated",
	// never "down".
	Readiness string `json:"readiness,omitempty"`

	// ProbedAt is when the live readiness probe that produced Readiness last
	// ran. nil when Readiness was derived without a live protocol probe (e.g.
	// the installed-but-not-running disk branch, collectInstalledEngines) or
	// when Readiness itself is unset. This is the expiry marker the issue calls
	// out: without it a stale Readiness value from a paused heartbeat has
	// nothing marking it as old.
	ProbedAt *time.Time `json:"probed_at,omitempty"`

	// Reason is a short, stable string explaining why Readiness holds its
	// current value, e.g. "model discovery probe timed out after 2s" or
	// "installed_not_running". Omitted when Readiness is unset.
	Reason string `json:"reason,omitempty"`
}

// HealthResponse is the response for /health endpoint.
type HealthResponse struct {
	Status  string `json:"status"` // "ok", "degraded", "unhealthy"
	Version string `json:"version"`
}

// ServiceType constants for service classification.
const (
	ServiceTypeLLM       = "llm"
	ServiceTypeEmbedding = "embedding"
	ServiceTypeDatabase  = "database"
	ServiceTypeOther     = "other"
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
	// HealthStatusStarting means the service's container is UP but the service
	// is not answering yet: a serving engine still loading weights, a TEI whose
	// /health is not yet 200. Status is still "running" (the container really is
	// running, which is what SERVICE_STATUS reports), so the node no longer
	// under-reports a service it was just told to start (aceteam#7148). Consumers
	// that need READINESS, for example the platform's TEI embedding-node resolver,
	// must gate on this health value, not on Status alone.
	HealthStatusStarting = "starting"
)

// Readiness constants (citadel-cli#684). Four-valued, purely additive to
// Status/Health (see ServiceInfo.Readiness):
//   - ReadinessDown:      no container and no process.
//   - ReadinessStarting:  lifecycle up, protocol probe not passing yet.
//   - ReadinessReady:     serving the declared models now.
//   - ReadinessUnhealthy: lifecycle up, probe failing past a threshold.
const (
	ReadinessDown      = "down"
	ReadinessStarting  = "starting"
	ReadinessReady     = "ready"
	ReadinessUnhealthy = "unhealthy"
)

// StatusVersion is the current version of the status payload format.
const StatusVersion = "1.0"
