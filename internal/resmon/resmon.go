// Package resmon (resource monitor) collects the FULL resource-consumer picture
// of a node — every GPU compute process, not just the citadel-managed ones that
// #421's footprint collector sees.
//
// Motivation (citadel #427): during a live cleanup on node 1054 (RTX 3090) a
// leftover `tei-gte` embedding container plus other non-citadel GPU users
// (`docker-model-runner`, `open-webui`) were pinning VRAM completely invisibly
// to citadel. Finding them meant doing by hand exactly what citadel should do
// for us: `nvidia-smi --query-compute-apps` → map each PID to a container via
// cgroup → `docker ps` → `docker stats`. This package automates that: it
// enumerates ALL GPU processes, classifies each by owner
// (citadel-managed | container:<name> | host:<comm>), attributes VRAM/RSS,
// and flags heavy-and-idle UNMANAGED consumers as reclaimable — the slice of
// the picture #421 was blind to.
//
// It is the mirror image of internal/status/footprint.go: #421 walks
// name→mainPID→subtree to attribute VRAM to a KNOWN container; resmon walks
// pid→owner for EVERY compute pid, so it needs no /proc subtree walk. The whole
// snapshot costs 2 nvidia-smi calls + 1 container-runtime `ps` + cheap /proc
// reads, mirroring #421's ONE-stats-exec + ONE-nvidia-smi-per-tick discipline.
package resmon

import (
	"context"
	"os/exec"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
)

// collectTimeout bounds the nvidia-smi / container-runtime execs so a hung
// runtime under overload (the #421 incident hit load average 585) can't stall
// a caller. Matches internal/status.footprintCollectTimeout.
const collectTimeout = 5 * time.Second

// OwnerKind classifies who a resource consumer belongs to. It drives the
// reclaimable heuristic (only non-managed consumers are candidates) and the
// operator-facing label ("not managed by citadel — free manually").
type OwnerKind string

const (
	// OwnerCitadelManaged is a container citadel started (name begins with
	// "citadel-" or is in the manifest). Managed heavy-idle eviction is already
	// covered by #421; these are never flagged reclaimable here.
	OwnerCitadelManaged OwnerKind = "citadel-managed"
	// OwnerContainer is a container citadel did NOT start. Owner is
	// "container:<name>". This is the #427 blind spot (the tei-gte leftover).
	OwnerContainer OwnerKind = "container"
	// OwnerHost is a bare host process with no container. Owner is
	// "host:<comm>".
	OwnerHost OwnerKind = "host"
)

// Consumer is a single resource-consuming process in the snapshot.
type Consumer struct {
	// PID is the host process id as reported by nvidia-smi.
	PID int `json:"pid"`
	// Owner is the human-readable owner string: "citadel-managed",
	// "container:<name>", or "host:<comm>".
	Owner string `json:"owner"`
	// Kind is the machine-readable owner classification.
	Kind OwnerKind `json:"kind"`
	// VRAMBytes is GPU memory attributed to this pid, summed across GPUs. 0 when
	// the pid holds no VRAM (it may still be heavy on RSS).
	VRAMBytes uint64 `json:"vram_bytes"`
	// RSSBytes is the process's resident set size from /proc/<pid>/statm. 0 when
	// unknown (e.g. process exited between the nvidia-smi read and the /proc
	// read, or on a non-Linux host).
	RSSBytes uint64 `json:"rss_bytes"`
	// CPUPercent is a best-effort per-process CPU utilization. -1 means unknown;
	// per-process CPU is not sampled in v1 (util is device-level only), so this
	// is -1 for now and reserved for a future continuous sampler.
	CPUPercent float64 `json:"cpu_percent"`
	// GPUUtil is whole-GPU utilization (device-level, not per-process —
	// nvidia-smi does not expose reliable per-process SM%). -1 when no GPU.
	GPUUtil float64 `json:"gpu_util"`
	// Reclaimable reports whether this consumer is a heavy, idle, UNMANAGED
	// candidate worth freeing. Managed consumers are never reclaimable here
	// (#421 owns those).
	Reclaimable bool `json:"reclaimable"`
	// Reason is a short human-readable justification when Reclaimable is true,
	// empty otherwise.
	Reason string `json:"reason,omitempty"`
}

// GPUTotals holds whole-node GPU memory, summed across all devices.
type GPUTotals struct {
	// UsedBytes is total GPU memory in use across all devices.
	UsedBytes uint64 `json:"used"`
	// TotalBytes is total GPU memory capacity across all devices.
	TotalBytes uint64 `json:"total"`
}

// Snapshot is the full resource picture returned by Collect: host GPU totals
// plus a per-consumer breakdown. It is what /resources returns and what the
// RESOURCE_SNAPSHOT job returns, so the fabric can make honest placement
// decisions ("is this node's GPU actually free?") without SSH.
type Snapshot struct {
	// HasGPU reports whether GPU stats were available. When false, GPU/VRAM
	// fields are zero and callers render "n/a" rather than a misleading zero.
	HasGPU bool `json:"has_gpu"`
	// GPU holds whole-node GPU memory totals.
	GPU GPUTotals `json:"gpu"`
	// GPUUtil is the max whole-GPU utilization across devices. -1 when no GPU.
	GPUUtil float64 `json:"gpu_util"`
	// Consumers is every GPU compute process, one entry per pid.
	Consumers []Consumer `json:"consumers"`
	// CollectedAt is the snapshot time (RFC3339, UTC).
	CollectedAt string `json:"collected_at"`
}

// idleTracker is the process-level idle window used by Collect to decide
// reclaimability. It reuses the #421 FootprintIdleTracker heuristic (CPU/GPU
// thresholds + first-inactive time accumulation) but keyed by pid and kept
// self-contained so this package does not import internal/status. A single
// on-demand Collect() call sees a consumer for the first time and reports
// idleFor=0 (not reclaimable); reclaimable only flips true once a consumer has
// been observed inactive across enough polls to cross the threshold. Callers
// wanting continuous reclaimable signal keep one Collector alive and poll it.
var defaultTracker = newIdleTracker()

// Collect gathers the full resource snapshot. It never errors: a missing signal
// (no GPU, no nvidia-smi, no container runtime, no /proc) degrades to "unknown"
// so the snapshot is always serializable. It runs the collection under a bounded
// context so a hung runtime can't stall the caller.
//
// It classifies managed containers by the "citadel-" name prefix only. Callers
// that know the manifest-declared service names (e.g. the CLI) should use
// CollectWithManaged so bare-named managed services are never mis-flagged as
// reclaimable.
func Collect(ctx context.Context) Snapshot {
	return collectWith(ctx, realProbe{}, defaultTracker, nil)
}

// CollectWithManaged is like Collect but also treats any container whose name
// exactly matches a manifest-declared service name as citadel-managed, closing
// the gap where a managed service runs under a bare (non-"citadel-"-prefixed)
// name. managedNames may be empty.
func CollectWithManaged(ctx context.Context, managedNames []string) Snapshot {
	set := make(map[string]struct{}, len(managedNames))
	for _, n := range managedNames {
		if n != "" {
			set[n] = struct{}{}
		}
	}
	return collectWith(ctx, realProbe{}, defaultTracker, set)
}

// collectWith is the testable core: it takes an injected probe (for mocking
// nvidia-smi / container-runtime / proc reads), an idle tracker, and the set of
// manifest-managed service names, so tests never touch a real GPU.
func collectWith(ctx context.Context, p probe, tracker *idleTracker, managedNames map[string]struct{}) Snapshot {
	snap := Snapshot{
		GPUUtil:     -1,
		CollectedAt: time.Now().UTC().Format(time.RFC3339),
	}

	pidVRAM, gpuUsed, gpuTotal, gpuUtil, hasGPU := p.GPU(ctx)
	snap.HasGPU = hasGPU
	if hasGPU {
		snap.GPU = GPUTotals{UsedBytes: gpuUsed, TotalBytes: gpuTotal}
		snap.GPUUtil = gpuUtil
	}

	if len(pidVRAM) == 0 {
		return snap
	}

	// One container-runtime `ps` to build container-id → name, so we never
	// `inspect` per pid.
	idToName := p.Containers(ctx)

	for pid, vram := range pidVRAM {
		owner, kind := classifyOwner(p.Cgroup(pid), idToName, p.Comm(pid), managedNames)
		c := Consumer{
			PID:        pid,
			Owner:      owner,
			Kind:       kind,
			VRAMBytes:  vram,
			RSSBytes:   p.RSS(pid),
			CPUPercent: -1,
			GPUUtil:    snap.GPUUtil,
		}
		idle := tracker.observe(pid, c.CPUPercent, snap.GPUUtil, hasGPU)
		if reason, ok := reclaimable(c.VRAMBytes, c.RSSBytes, idle, kind); ok {
			c.Reclaimable = true
			c.Reason = reason
		}
		snap.Consumers = append(snap.Consumers, c)
	}
	return snap
}

// realProbe is the production probe backed by nvidia-smi, the selected
// container runtime, and /proc.
type realProbe struct{}

// GPU runs one compute-apps query (pid → vram) and one gpu memory+util query,
// returning per-pid VRAM plus whole-node totals. hasGPU is false when nvidia-smi
// is absent or the compute-apps query fails.
func (realProbe) GPU(ctx context.Context) (pidVRAM map[int]uint64, used, total uint64, util float64, hasGPU bool) {
	if _, err := exec.LookPath("nvidia-smi"); err != nil {
		return nil, 0, 0, -1, false
	}
	appsOut, err := exec.CommandContext(ctx, "nvidia-smi",
		"--query-compute-apps=pid,used_memory", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return nil, 0, 0, -1, false
	}
	pidVRAM = parseComputeApps(string(appsOut))

	gpuOut, err := exec.CommandContext(ctx, "nvidia-smi",
		"--query-gpu=memory.used,memory.total,utilization.gpu", "--format=csv,noheader,nounits").Output()
	if err != nil {
		// compute-apps worked, so a GPU is present; totals/util unknown.
		return pidVRAM, 0, 0, -1, true
	}
	used, total, util = parseGPUTotals(string(gpuOut))
	return pidVRAM, used, total, util, true
}

// Containers runs one `<engine> ps --no-trunc` and returns container-id → name.
// Empty map on any error (no runtime / daemon down) — callers then classify
// every containerized pid as host, which is a safe degradation.
func (realProbe) Containers(ctx context.Context) map[string]string {
	engineBin := catalog.SelectContainerRuntime().EngineBin
	if engineBin == "" {
		engineBin = "docker"
	}
	out, err := exec.CommandContext(ctx, engineBin, "ps", "--no-trunc",
		"--format", "{{.ID}}\t{{.Names}}").Output()
	if err != nil {
		return map[string]string{}
	}
	return parsePsOutput(string(out))
}

// Cgroup returns the raw /proc/<pid>/cgroup contents, or "" when unreadable.
func (realProbe) Cgroup(pid int) string { return readProcFile(pid, "cgroup") }

// Comm returns the process command name from /proc/<pid>/comm, or "" when
// unreadable.
func (realProbe) Comm(pid int) string { return readProcComm(pid) }

// RSS returns resident set size in bytes from /proc/<pid>/statm, or 0 when
// unreadable.
func (realProbe) RSS(pid int) uint64 { return readProcRSS(pid) }

// probe abstracts the OS-level reads so collectWith can be unit-tested against
// string fixtures with no real GPU/proc dependency.
type probe interface {
	GPU(ctx context.Context) (pidVRAM map[int]uint64, used, total uint64, util float64, hasGPU bool)
	Containers(ctx context.Context) map[string]string
	Cgroup(pid int) string
	Comm(pid int) string
	RSS(pid int) uint64
}
