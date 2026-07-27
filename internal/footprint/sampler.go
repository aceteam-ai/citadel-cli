package footprint

import (
	"context"
	"os/exec"
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

	// PowerWatts is the summed measured board power across GPUs (nvidia-smi
	// power.draw). Valid only when PowerMeasured is true.
	PowerWatts float64
	// PowerMeasured is true when at least one GPU reported a real power.draw
	// reading (not "[N/A]" / "[Not Supported]").
	PowerMeasured bool
	// PowerLimitWatts is the summed enforced power cap across GPUs (nvidia-smi
	// power.limit). Used as the TDP for the utilisation-based power estimate when
	// power.draw is unavailable. Zero when unknown.
	PowerLimitWatts float64
}

// statsFunc runs the single container-stats exec for a tick. Injected so the
// sampler is testable without a container daemon.
type statsFunc func(ctx context.Context, engineBin string) ([]containerStat, error)

// gpuFunc reads the node-level GPU snapshot for a tick. Injected so the sampler
// is testable without a GPU. It carries VRAM/util only; power is read separately
// (gpuPowerFunc) and ONLY when energy sampling is enabled, so a plain footprint
// tick runs no extra probe.
type gpuFunc func() GPUSnapshot

// gpuPowerFunc reads GPU board power draw and the enforced power limit for a
// tick, summed across GPUs. Injected so the sampler is testable, and called ONLY
// when energy sampling is enabled and a GPU is present (so the default footprint
// path runs zero nvidia-smi power probes).
type gpuPowerFunc func() (watts float64, measured bool, limitWatts float64)

// idleFunc returns the node's current idle-seconds signal and whether it is
// available. Injected; the default returns (0, false) because #420's idle signal
// is not wired into this branch and this package must NOT reimplement idle
// detection.
type idleFunc func() (int, bool)

// Sampler builds one batch of footprint samples per tick: one row per managed
// service (from stats) plus one node-level row (host CPU/RSS + GPU + energy).
type Sampler struct {
	nodeID    string
	services  []string
	engineBin string

	// energy gates the whole energy estimate. When false (the default), no power
	// probe runs and the node row carries no power_w / energy_wh / power_source.
	energy bool
	// powerCfg holds the resolved power-estimation knobs (TDP overrides). Resolved
	// once so no env parsing happens per tick.
	powerCfg PowerConfig
	// interval is the sampling cadence, used to convert instantaneous power_w into
	// per-interval energy_wh. Zero leaves energy_wh blank.
	interval time.Duration

	stats    statsFunc
	gpu      gpuFunc
	gpuPower gpuPowerFunc
	idle     idleFunc
}

// NewSampler wires a Sampler to the real host probes. interval is the sampling
// cadence (used for energy_wh); powerCfg carries the resolved TDP knobs; energy
// turns the power estimate (and its nvidia-smi power probe) on. When energy is
// false the sampler behaves exactly as before the energy feature existed.
func NewSampler(nodeID string, services []string, engineBin string, interval time.Duration, powerCfg PowerConfig, energy bool) *Sampler {
	return &Sampler{
		nodeID:    nodeID,
		services:  services,
		engineBin: engineBin,
		energy:    energy,
		powerCfg:  powerCfg,
		interval:  interval,
		stats:     sampleContainerStats,
		gpu:       sampleGPU,
		gpuPower:  readGPUPowerReading,
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
	cpuPct, cpuOK := hostCPUPercent()
	if cpuOK {
		node.CPUPercent = &cpuPct
	}
	if rss, ok := hostRSSMB(); ok {
		node.RSSMB = &rss
	}
	snap := s.gpu()
	if snap.HasGPU {
		vram := snap.VRAMUsedMB
		util := snap.GPUUtilPercent
		node.VRAMMB = &vram
		node.GPUUtilPercent = &util
	}

	// Node-level energy estimate: opt-in (default OFF). Only when enabled do we run
	// the GPU power probe and stamp power_w / energy_wh / power_source. When off,
	// this is a no-op so the tick is byte-identical to the pre-energy footprint.
	if s.energy {
		if snap.HasGPU && s.gpuPower != nil {
			watts, measured, limit := s.gpuPower()
			snap.PowerWatts = watts
			snap.PowerMeasured = measured
			snap.PowerLimitWatts = limit
		}
		s.fillNodePower(&node, snap, cpuPct, cpuOK)
	}

	rows = append(rows, node)
	return rows
}

// fillNodePower runs the power waterfall for the node row and, when a figure is
// available, sets power_w / energy_wh / power_source. It never fails: an absent
// signal simply leaves the fields blank.
func (s *Sampler) fillNodePower(node *Sample, snap GPUSnapshot, cpuPct float64, cpuOK bool) {
	gpuTDP := s.powerCfg.GPUTDPWattsOverride
	if gpuTDP <= 0 {
		gpuTDP = snap.PowerLimitWatts
	}
	est := EstimateNodePower(PowerInputs{
		HasGPU:           snap.HasGPU,
		GPUPowerWatts:    snap.PowerWatts,
		GPUPowerMeasured: snap.PowerMeasured,
		GPUUtilKnown:     snap.HasGPU,
		GPUUtilPercent:   snap.GPUUtilPercent,
		GPUTDPWatts:      gpuTDP,
		CPUKnown:         cpuOK,
		CPUPercent:       cpuPct,
		CPUTDPWatts:      s.powerCfg.CPUTDPWatts,
	})
	if !est.Known {
		return
	}
	watts := est.Watts
	node.PowerW = &watts
	node.PowerSource = est.Source
	if wh := energyWh(watts, s.interval); wh > 0 {
		node.EnergyWh = &wh
	}
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

// readGPUPowerReading runs a single read-only nvidia-smi query for board power
// draw and the enforced power limit, summed across GPUs. It is intentionally
// footprint-local (not a change to the shared internal/platform GPU detector) so
// this package stays self-contained per its package doc. Any failure is silent:
// it returns measured=false and zero watts, so the estimator falls through to a
// util or CPU model. This never prompts for privileges. It is called ONLY when
// energy sampling is enabled.
func readGPUPowerReading() (watts float64, measured bool, limitWatts float64) {
	cmd := exec.Command(
		"nvidia-smi",
		"--query-gpu=power.draw,power.limit",
		"--format=csv,noheader,nounits",
	)
	out, err := cmd.Output()
	if err != nil {
		return 0, false, 0
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.Split(line, ",")
		if len(parts) < 2 {
			continue
		}
		if w, ok := parseWattsField(parts[0]); ok {
			watts += w
			measured = true
		}
		if l, ok := parseWattsField(parts[1]); ok {
			limitWatts += l
		}
	}
	return watts, measured, limitWatts
}

// parseWattsField parses an nvidia-smi power field (already "nounits", e.g.
// "142.35"). It rejects the sentinel values nvidia-smi emits when a sensor is
// absent ("[N/A]", "[Not Supported]", "[Insufficient Permissions]"), returning
// ok=false so the caller falls through to an estimate.
func parseWattsField(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasPrefix(s, "[") {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 {
		return 0, false
	}
	return v, true
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
