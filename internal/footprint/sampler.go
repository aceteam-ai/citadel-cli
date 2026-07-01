package footprint

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

// GPUSnapshot is the node-level GPU reading captured once per tick: total VRAM
// used across all GPUs and their aggregate (max) utilisation.
type GPUSnapshot struct {
	VRAMUsedMB     int
	GPUUtilPercent float64
	// HasGPU is false on nodes without an NVIDIA/Metal GPU, in which case the
	// node-level row leaves vram_mb / gpu_util_pct empty.
	HasGPU bool
}

// statsFunc runs the single container-stats exec for a tick. Injected so the
// sampler is testable without a container daemon.
type statsFunc func(ctx context.Context, engineBin string) ([]containerStat, error)

// gpuFunc reads the node-level GPU snapshot for a tick. Injected so the sampler
// is testable without a GPU.
type gpuFunc func() GPUSnapshot

// idleFunc returns the node's current idle-seconds signal and whether it is
// available. Injected; the default returns (0, false) because #420's idle signal
// is not wired into this branch and this package must NOT reimplement idle
// detection.
type idleFunc func() (int, bool)

// Sampler builds one batch of footprint samples per tick: one row per managed
// service (from stats) plus one node-level row (host CPU/RSS + GPU).
type Sampler struct {
	nodeID    string
	services  []string
	engineBin string

	stats statsFunc
	gpu   gpuFunc
	idle  idleFunc
}

// NewSampler wires a Sampler to the real host probes.
func NewSampler(nodeID string, services []string, engineBin string) *Sampler {
	return &Sampler{
		nodeID:    nodeID,
		services:  services,
		engineBin: engineBin,
		stats:     sampleContainerStats,
		gpu:       sampleGPU,
		idle:      func() (int, bool) { return 0, false },
	}
}

// SetIdleFunc lets callers supply a readily-available idle signal (#420). When
// unset, idle_seconds is left empty in the CSV.
func (s *Sampler) SetIdleFunc(f idleFunc) {
	if f != nil {
		s.idle = f
	}
}

// Sample builds the footprint rows for a single tick at time ts. It performs at
// most ONE container-stats exec and ONE GPU read — never per-service execs.
func (s *Sampler) Sample(ctx context.Context, ts time.Time) []Sample {
	var idlePtr *int
	if secs, ok := s.idle(); ok {
		idlePtr = &secs
	}

	// One stats exec for the whole tick. On error (daemon down, engine missing)
	// treat as "no containers": services report running=false, and we still emit
	// the node-level row so host/GPU history is never lost.
	stats, _ := s.stats(ctx, s.engineBin)

	rows := make([]Sample, 0, len(s.services)+1)
	for _, svc := range s.services {
		row := Sample{
			Timestamp:   ts,
			NodeID:      s.nodeID,
			Service:     svc,
			IdleSeconds: idlePtr,
		}
		if cs, ok := matchContainer(stats, svc); ok {
			row.Running = true
			if cpu, ok := parseCPUPercent(cs.CPUPerc); ok {
				row.CPUPercent = &cpu
			}
			if rss, ok := parseMemUsageMB(cs.MemUsage); ok {
				row.RSSMB = &rss
			}
		}
		rows = append(rows, row)
	}

	// Node-level row: host CPU/RSS from gopsutil + GPU util/VRAM. This is where
	// VRAM lives — per-service VRAM is intentionally NOT attributed (nvidia-smi is
	// not container-aware; PID->container mapping would be a per-service exec
	// storm). The GPU-hoarding half of the incident is covered node-level here.
	node := Sample{
		Timestamp:   ts,
		NodeID:      s.nodeID,
		Service:     NodeService,
		Running:     true,
		IdleSeconds: idlePtr,
	}
	if cpu, ok := hostCPUPercent(); ok {
		node.CPUPercent = &cpu
	}
	if rss, ok := hostRSSMB(); ok {
		node.RSSMB = &rss
	}
	if snap := s.gpu(); snap.HasGPU {
		vram := snap.VRAMUsedMB
		util := snap.GPUUtilPercent
		node.VRAMMB = &vram
		node.GPUUtilPercent = &util
	}
	rows = append(rows, node)
	return rows
}

// matchContainer returns the first stats row whose container name contains the
// service name (case-insensitive). Compose containers are named like
// "<project>-<service>-1", so a substring match reliably attributes them to the
// manifest service without an extra `compose ps` exec.
func matchContainer(stats []containerStat, service string) (containerStat, bool) {
	needle := strings.ToLower(service)
	for _, cs := range stats {
		if strings.Contains(strings.ToLower(cs.Name), needle) {
			return cs, true
		}
	}
	return containerStat{}, false
}

// sampleGPU reads the node-level GPU snapshot via the read-only platform GPU
// detector (the same source #421 reads). Returns HasGPU=false when no GPU is
// present or the read fails.
func sampleGPU() GPUSnapshot {
	detector, err := platform.GetGPUDetector()
	if err != nil || !detector.HasGPU() {
		return GPUSnapshot{}
	}
	infos, err := detector.GetGPUInfo()
	if err != nil || len(infos) == 0 {
		return GPUSnapshot{}
	}
	snap := GPUSnapshot{HasGPU: true}
	for _, gpu := range infos {
		if mb, ok := parseMBField(gpu.MemoryUsed); ok {
			snap.VRAMUsedMB += mb
		}
		if util, ok := parsePercentField(gpu.Utilization); ok && util > snap.GPUUtilPercent {
			snap.GPUUtilPercent = util
		}
	}
	return snap
}

// parseMBField parses a platform GPUInfo memory string like "8192 MB" into MB.
func parseMBField(s string) (int, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, "MB")
	s = strings.TrimSuffix(s, " MB")
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return v, true
}

// parsePercentField parses a platform GPUInfo utilisation string like "85%".
func parsePercentField(s string) (float64, bool) {
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "%"))
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}
