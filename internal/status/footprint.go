package status

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/apps"
	"github.com/aceteam-ai/citadel-cli/internal/catalog"
)

// footprintCollectTimeout bounds the batched stats/nvidia-smi execs so a hung
// container runtime under overload can't stall the heartbeat.
const footprintCollectTimeout = 5 * time.Second

// ServiceFootprint captures the live resource footprint of a single
// citadel-managed serving container (vllm, diffusers, ollama, ...). It rides
// the heartbeat (embedded in ServiceInfo/AppInfo) so the platform can see idle
// GPU hogs, and is rendered in the Control Center services panel.
//
// Motivation (citadel #421): a leftover diffusers container held ~7.4 GB RAM
// while completely idle and drove the node to load average 585 before a human
// noticed. The TUI showed it as "running" with no footprint and no idle signal.
// This makes the managed-service view resource-aware so the heavy-and-idle
// eviction candidate is obvious at a glance.
type ServiceFootprint struct {
	// CPUPercent is the container's CPU utilization as reported by
	// `docker/podman stats` (can exceed 100 on multi-core). -1 means unknown.
	CPUPercent float64 `json:"cpu_percent"`
	// RAMBytes is the container's resident memory (RSS) in bytes. 0 when unknown.
	RAMBytes uint64 `json:"ram_bytes"`
	// VRAMBytes is GPU memory attributed to this container's process subtree,
	// summed across GPUs, in bytes. 0 when the node has no GPU, nvidia-smi is
	// absent, or no compute process maps to the container.
	VRAMBytes uint64 `json:"vram_bytes"`
	// GPUUtilPercent is the whole-GPU utilization (device-level, not
	// per-process — nvidia-smi does not expose reliable per-process SM%).
	// -1 means n/a (no GPU / nvidia-smi absent).
	GPUUtilPercent float64 `json:"gpu_util_percent"`
	// HasGPU reports whether GPU stats were available at all, so a "0.0G / 0%"
	// footprint can be rendered as "n/a" instead of a misleading zero.
	HasGPU bool `json:"has_gpu"`
	// NetBytes is the container's cumulative network I/O (rx + tx) in bytes as
	// reported by `docker/podman stats` NetIO. It is a monotonic counter (reset
	// only on container restart) and is the request-agnostic activity signal the
	// generic idle path uses for services that expose no vLLM-style request
	// metrics (Claude Code, OpenClaw, Hermes, ...). 0 when the stats read did not
	// include a NetIO column. See citadel-cli#433.
	NetBytes uint64 `json:"net_bytes"`
	// HasNet reports whether NetBytes came from an actual NetIO reading (as
	// opposed to a runtime/format that omitted the column). This is the fail-safe
	// discriminator for the generic idle path: a genuinely-zero NetBytes with
	// HasNet=true is a real "no traffic" signal, but NetBytes=0 with HasNet=false
	// means "unknown" and MUST NOT be fed to the idle tracker (an unread counter
	// would look flat forever and false-idle a busy container). Not serialized;
	// it is an internal collection detail.
	HasNet bool `json:"-"`
}

// heavyRAMBytes is the RSS threshold above which an idle managed service is
// flagged as an eviction candidate. ~4 GiB — large enough to exclude small
// sidecars, low enough to catch a stuck diffusers/vLLM worker.
const heavyRAMBytes uint64 = 4 * 1024 * 1024 * 1024

// heavyVRAMBytes is the VRAM threshold above which an idle managed service is
// flagged as an eviction candidate regardless of its RAM. Any multi-GB VRAM
// reservation held while idle is worth surfacing on a contended GPU node.
const heavyVRAMBytes uint64 = 2 * 1024 * 1024 * 1024

// IsHeavyAndIdle reports whether a service is both idle and holding enough
// RAM or VRAM to be worth evicting. This is the highlight predicate for the
// TUI and the signal that would have caught the #421 incident: a diffusers
// container idle for 38m while pinning 7.4 GB RAM.
//
// A nil footprint or nil idle state means "not enough signal to warn" (false).
func IsHeavyAndIdle(fp *ServiceFootprint, idle *IdleState) bool {
	if fp == nil || idle == nil || !idle.Idle {
		return false
	}
	return fp.RAMBytes >= heavyRAMBytes || fp.VRAMBytes >= heavyVRAMBytes
}

// CollectFootprints returns a per-container resource footprint keyed by
// container name, for the given set of container names. It makes exactly one
// `stats` call and (at most) one pair of nvidia-smi calls per invocation,
// joining the results in memory — never one exec per service. Callers pass the
// managed container names they care about (e.g. "citadel-vllm",
// "citadel-diffusers"); unknown containers in the stats output are ignored.
//
// It degrades gracefully: with no GPU / no nvidia-smi, VRAM/GPU fields are
// zeroed and HasGPU is false; with no container runtime, an empty map is
// returned. It never errors — a missing signal is reported as "unknown", not a
// failure, so the TUI/heartbeat keep working on non-GPU and non-container hosts.
func CollectFootprints(ctx context.Context, containerNames []string) map[string]ServiceFootprint {
	if len(containerNames) == 0 {
		return map[string]ServiceFootprint{}
	}

	engineBin := catalog.SelectContainerRuntime().EngineBin
	if engineBin == "" {
		engineBin = "docker"
	}

	// One batched stats read for CPU% + RAM across ALL running containers. We
	// deliberately pass no container names: `docker stats <name>` hard-fails the
	// whole batch (non-zero exit, empty stdout) if ANY named container is
	// absent, and we probe both "citadel-<n>" and bare "<n>" candidates — the
	// bare one usually doesn't exist. Listing all and filtering by name in
	// memory is the robust batch, and still ignores unrelated containers.
	stats := collectContainerStats(ctx, engineBin)

	// One nvidia-smi compute-apps read (pid -> vram) + one gpu-util read.
	pidVRAM, gpuUtil, hasGPU := collectGPUFootprint(ctx)

	// Build the /proc parent->children map once (not per container): pidSubtree
	// would otherwise re-walk all of /proc for every GPU container, which adds
	// up under the exact overload this feature targets.
	var procChildren map[int][]int
	if hasGPU {
		procChildren = readProcChildren()
	}

	out := make(map[string]ServiceFootprint, len(containerNames))
	for _, name := range containerNames {
		fp := ServiceFootprint{CPUPercent: -1, GPUUtilPercent: -1, HasGPU: hasGPU}
		if s, ok := stats[name]; ok {
			fp.CPUPercent = s.cpuPercent
			fp.RAMBytes = s.ramBytes
			fp.NetBytes = s.netBytes
			fp.HasNet = s.hasNet
		}
		if hasGPU {
			fp.GPUUtilPercent = gpuUtil
			if pid, ok := containerMainPID(ctx, engineBin, name); ok {
				fp.VRAMBytes = attributeVRAM(pidVRAM, pidSubtree(pid, procChildren))
			}
		}
		out[name] = fp
	}
	return out
}

// containerStats holds the CPU%, RAM, and cumulative network I/O parsed from
// one `stats` row.
type containerStats struct {
	cpuPercent float64
	ramBytes   uint64
	netBytes   uint64
	hasNet     bool
}

// statsFormat is the Go-template format string passed to `stats --format`. Tab
// separated so a container name with a space can't break parsing. NetIO (rx/tx)
// is included for the generic, request-agnostic idle signal (citadel-cli#433);
// both docker and podman expose the {{.NetIO}} field.
const statsFormat = "{{.Name}}\t{{.CPUPerc}}\t{{.MemUsage}}\t{{.NetIO}}"

// collectContainerStats runs one `<engine> stats --no-stream` over ALL running
// containers and returns a name -> {cpu%, ram} map. No container names are
// passed: naming a missing container makes docker/podman fail the whole batch,
// so callers filter the returned map by name instead. Any exec error yields an
// empty map (unknown), never a panic.
func collectContainerStats(ctx context.Context, engineBin string) map[string]containerStats {
	cmd := exec.CommandContext(ctx, engineBin, "stats", "--no-stream", "--format", statsFormat)
	out, err := cmd.Output()
	if err != nil {
		return map[string]containerStats{}
	}
	return parseStatsOutput(string(out))
}

// parseStatsOutput parses the tab-separated `stats --format` output into a
// name -> {cpu%, ram, net} map. Lines that don't parse are skipped. Exported
// field units follow docker/podman conventions: CPUPerc like "74.31%", MemUsage
// like "6.1GiB / 62.0GiB" (we take the used side), NetIO like "1.2kB / 3.4MB"
// (rx / tx, summed). The NetIO column is optional: a 3-field row (older format
// or a runtime that omits NetIO) still parses with netBytes=0, so this stays
// backward compatible with the pre-#433 statsFormat.
func parseStatsOutput(out string) map[string]containerStats {
	res := map[string]containerStats{}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 3 {
			continue
		}
		name := strings.TrimSpace(fields[0])
		if name == "" || strings.EqualFold(name, "NAME") {
			continue // skip a header row if one slips through
		}
		cs := containerStats{cpuPercent: parsePercent(fields[1])}
		// MemUsage is "<used> / <limit>"; take the used side.
		usedStr := fields[2]
		if idx := strings.Index(usedStr, "/"); idx >= 0 {
			usedStr = usedStr[:idx]
		}
		cs.ramBytes = parseMemBytes(usedStr)
		// NetIO is "<rx> / <tx>"; sum both sides. Optional field. hasNet is set
		// ONLY when a real byte value parsed: a runtime that reports "--" / "-- /
		// --" (host-networked or stats-unavailable containers) is an UNKNOWN
		// reading, not a real zero, and must not drive the idle tracker toward a
		// false idle. So an unparseable NetIO cell leaves hasNet=false and the
		// generic idle path falls through to the footprint heuristic.
		if len(fields) >= 4 {
			cs.netBytes, cs.hasNet = parseNetIO(fields[3])
		}
		res[name] = cs
	}
	return res
}

// parseNetIO parses a docker/podman NetIO cell like "1.2kB / 3.4MB" (received /
// transmitted) into the summed byte total. The bool reports whether at least
// one side parsed to a real byte value: a cell like "--", "-- / --", or empty
// (host-networked or stats-unavailable containers) yields (0, false) so callers
// treat it as UNKNOWN rather than a real "zero traffic" reading. This is the
// fail-safe gate for the generic idle path -- an unknown reading must never
// look like a flat zero counter that goes idle. A genuine "0B / 0B" reading
// parses as (0, true): a real, quiet container.
func parseNetIO(s string) (uint64, bool) {
	rx, tx := s, ""
	if idx := strings.Index(s, "/"); idx >= 0 {
		rx = s[:idx]
		tx = s[idx+1:]
	}
	rxB, rxOK := parseNetSide(strings.TrimSpace(rx))
	txB, txOK := parseNetSide(strings.TrimSpace(tx))
	if !rxOK && !txOK {
		return 0, false
	}
	return rxB + txB, true
}

// parseNetSide parses one side of a NetIO cell into bytes, reporting whether it
// was a real numeric value. An empty or non-numeric token (e.g. "--", the
// placeholder docker/podman print when NetIO is unavailable) returns (0,false).
// A real "0B" returns (0,true). It reuses parseMemBytes for the unit math but
// distinguishes "parsed to zero" from "could not parse" by requiring a leading
// digit, which parseMemBytes alone (which maps garbage to 0) cannot.
func parseNetSide(s string) (uint64, bool) {
	if s == "" {
		return 0, false
	}
	// A real value starts with a digit (optionally after a sign, though NetIO is
	// never negative). "--" and other placeholders do not.
	if s[0] < '0' || s[0] > '9' {
		return 0, false
	}
	return parseMemBytes(s), true
}

// parsePercent parses a value like "74.31%" or "74.31" into a float. Returns
// -1 for unparseable input so an unknown reading is distinguishable from 0.
func parsePercent(s string) float64 {
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "%"))
	if s == "" || s == "--" {
		return -1
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return -1
	}
	return v
}

// parseMemBytes parses a docker/podman memory string like "6.1GiB", "512MiB",
// "6.1GB", "1.5kB", or "0B" into bytes. Returns 0 on failure. Both binary
// (GiB/MiB) and decimal (GB/MB) suffixes are accepted since podman and docker
// differ; both are treated with their respective multipliers.
func parseMemBytes(s string) uint64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	// Split the trailing unit from the number.
	i := 0
	for i < len(s) && (s[i] == '.' || s[i] == '-' || (s[i] >= '0' && s[i] <= '9')) {
		i++
	}
	numStr := strings.TrimSpace(s[:i])
	unit := strings.ToLower(strings.TrimSpace(s[i:]))
	val, err := strconv.ParseFloat(numStr, 64)
	if err != nil || val < 0 {
		return 0
	}
	var mult float64
	switch unit {
	case "b", "":
		mult = 1
	case "kb", "k":
		mult = 1e3
	case "kib":
		mult = 1024
	case "mb", "m":
		mult = 1e6
	case "mib":
		mult = 1 << 20
	case "gb", "g":
		mult = 1e9
	case "gib":
		mult = 1 << 30
	case "tb", "t":
		mult = 1e12
	case "tib":
		mult = 1 << 40
	default:
		return 0
	}
	return uint64(val * mult)
}

// collectGPUFootprint runs one nvidia-smi compute-apps query (pid -> vram
// bytes) and one gpu-utilization query. It returns hasGPU=false when nvidia-smi
// is absent or errors, so callers render "n/a" rather than a misleading zero.
func collectGPUFootprint(ctx context.Context) (pidVRAM map[int]uint64, gpuUtil float64, hasGPU bool) {
	if _, err := exec.LookPath("nvidia-smi"); err != nil {
		return nil, -1, false
	}
	appsOut, err := exec.CommandContext(ctx, "nvidia-smi",
		"--query-compute-apps=pid,used_memory", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return nil, -1, false
	}
	pidVRAM = parseComputeApps(string(appsOut))

	utilOut, err := exec.CommandContext(ctx, "nvidia-smi",
		"--query-gpu=utilization.gpu", "--format=csv,noheader,nounits").Output()
	if err != nil {
		// compute-apps worked, so a GPU is present; util is just unknown.
		return pidVRAM, -1, true
	}
	return pidVRAM, parseGPUUtil(string(utilOut)), true
}

// parseComputeApps parses nvidia-smi `--query-compute-apps=pid,used_memory`
// CSV output (units stripped, so used_memory is in MiB) into a pid -> vram
// bytes map. When two rows share a pid (multi-GPU process), their VRAM is
// summed. Unparseable rows are skipped.
func parseComputeApps(out string) map[int]uint64 {
	res := map[int]uint64{}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		parts := strings.Split(line, ",")
		if len(parts) < 2 {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(parts[0]))
		if err != nil {
			continue
		}
		mib, err := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
		if err != nil || mib < 0 {
			continue
		}
		res[pid] += uint64(mib * (1 << 20))
	}
	return res
}

// parseGPUUtil parses nvidia-smi `--query-gpu=utilization.gpu` CSV (units
// stripped) and returns the max utilization across GPUs, or -1 if none parse.
// Max (not sum/avg) so a busy GPU isn't diluted by an idle second card.
func parseGPUUtil(out string) float64 {
	max := -1.0
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		v := parsePercent(sc.Text())
		if v > max {
			max = v
		}
	}
	return max
}

// attributeVRAM sums the VRAM of every compute-app PID that falls inside the
// container's process subtree. This is the crux of VRAM attribution: nvidia-smi
// reports *host* PIDs, and the GPU process is typically a child of the
// container's main PID (PID 1 inside the namespace), so a naive join on the
// main PID alone misses the real VRAM holder. We match any compute PID in the
// full descendant set.
func attributeVRAM(pidVRAM map[int]uint64, subtree map[int]struct{}) uint64 {
	var total uint64
	for pid, vram := range pidVRAM {
		if _, ok := subtree[pid]; ok {
			total += vram
		}
	}
	return total
}

// containerMainPID returns the host PID of a container's main process via
// `<engine> inspect --format {{.State.Pid}}`. Returns (0,false) on any error or
// a zero/absent PID (stopped container).
func containerMainPID(ctx context.Context, engineBin, name string) (int, bool) {
	out, err := exec.CommandContext(ctx, engineBin, "inspect", "--format", "{{.State.Pid}}", name).Output()
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

// pidSubtree returns the set of host PIDs rooted at (and including) root, given
// a prebuilt parent->children adjacency map (from readProcChildren). This
// captures GPU worker children of a container's main PID that nvidia-smi
// attributes by host PID. With an empty/nil map (e.g. non-Linux, no /proc) it
// returns just {root} so the main-PID case still works.
func pidSubtree(root int, children map[int][]int) map[int]struct{} {
	subtree := map[int]struct{}{root: {}}
	if len(children) == 0 {
		return subtree
	}
	// BFS from root over the child adjacency list.
	queue := []int{root}
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		for _, c := range children[p] {
			if _, seen := subtree[c]; seen {
				continue
			}
			subtree[c] = struct{}{}
			queue = append(queue, c)
		}
	}
	return subtree
}

// readProcChildren builds a parent -> children adjacency map by scanning
// /proc/<pid>/stat for each process's PPID. Returns an empty map when /proc is
// unavailable (e.g. macOS) so callers fall back to the single-PID match.
func readProcChildren() map[int][]int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	children := map[int][]int{}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue // non-numeric entries (e.g. /proc/cpuinfo)
		}
		ppid, ok := readPPID(pid)
		if !ok {
			continue
		}
		children[ppid] = append(children[ppid], pid)
	}
	return children
}

// readPPID reads the parent PID of a process from /proc/<pid>/stat. The stat
// file's comm field (field 2) can contain spaces and parentheses, so we key off
// the last ')' and take PPID as the second whitespace field after it.
func readPPID(pid int) (int, bool) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, false
	}
	s := string(data)
	close := strings.LastIndex(s, ")")
	if close < 0 || close+1 >= len(s) {
		return 0, false
	}
	// After ") " the fields are: state ppid ...
	rest := strings.Fields(s[close+1:])
	if len(rest) < 2 {
		return 0, false
	}
	ppid, err := strconv.Atoi(rest[1])
	if err != nil {
		return 0, false
	}
	return ppid, true
}

// FormatBytesGB renders a byte count as a compact "6.1G" string used in the TUI
// footprint line. 0 renders as "0.0G".
func FormatBytesGB(b uint64) string {
	gb := float64(b) / (1 << 30)
	return fmt.Sprintf("%.1fG", gb)
}

// idleCPUThresholdPercent is the container CPU% at or below which a managed
// service is considered inactive by the footprint-derived idle heuristic. A
// serving engine that is genuinely handling requests sits well above this; a
// stuck/leftover container (the #421 diffusers case) idles near zero.
const idleCPUThresholdPercent = 2.0

// idleGPUThresholdPercent is the whole-GPU utilization at or below which a GPU
// service is considered inactive. Combined with CPU: a service is inactive only
// when both CPU and (if present) GPU are quiet.
const idleGPUThresholdPercent = 5.0

// footprintIdleThreshold is how long a container must stay inactive before the
// footprint-derived heuristic marks it idle. Mirrors the intent of #420's
// metrics-based threshold for engines that expose no request counters.
const footprintIdleThreshold = 60 * time.Second

// FootprintIdleTracker derives a per-container idle signal purely from resource
// footprint (CPU + GPU utilization), for managed engines that #420's
// metrics-based IdleTracker cannot scrape (diffusers, ollama, llama.cpp, ...).
// It is complementary to — not a replacement for — the request-counter idle
// signal: #420 owns engines with Prometheus counters (vLLM); this covers the
// rest so the "heavy AND idle" eviction warning still fires for them. That is
// the exact gap the #421 incident fell through (idle diffusers, no request
// metrics, invisible).
//
// It is stateful and MUST be long-lived (one instance across refreshes): idle
// duration accumulates from the first-inactive timestamp, so a fresh tracker
// per refresh would never cross the threshold. Safe for concurrent use.
type FootprintIdleTracker struct {
	mu sync.Mutex
	// inactiveSince maps container name -> the time it first went inactive.
	// Absent when the container is currently active. threshold is the idle
	// cutoff.
	inactiveSince map[string]time.Time
	threshold     time.Duration
	now           func() time.Time // injectable for tests
}

// NewFootprintIdleTracker returns a tracker with the default idle threshold.
func NewFootprintIdleTracker() *FootprintIdleTracker {
	return &FootprintIdleTracker{
		inactiveSince: map[string]time.Time{},
		threshold:     footprintIdleThreshold,
		now:           time.Now,
	}
}

// footprintActive reports whether a footprint indicates the container is doing
// work: CPU above the idle threshold, or (when a GPU is present) GPU utilization
// above its threshold. Unknown CPU (-1) is treated as not-active-on-CPU so a
// missing stats read alone doesn't mask idleness.
func footprintActive(fp *ServiceFootprint) bool {
	if fp == nil {
		return false
	}
	if fp.CPUPercent > idleCPUThresholdPercent {
		return true
	}
	if fp.HasGPU && fp.GPUUtilPercent > idleGPUThresholdPercent {
		return true
	}
	return false
}

// Observe updates the tracker for a container's current footprint and returns
// its derived idle state. Active containers reset the idle clock and report
// idle=false with idle_seconds=0. Inactive containers accumulate idle_seconds
// from when they first went quiet, flipping idle=true once past the threshold.
func (t *FootprintIdleTracker) Observe(name string, fp *ServiceFootprint) IdleState {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := t.now()
	if footprintActive(fp) {
		delete(t.inactiveSince, name)
		return IdleState{Idle: false, IdleSeconds: 0}
	}

	since, ok := t.inactiveSince[name]
	if !ok {
		t.inactiveSince[name] = now
		return IdleState{Idle: false, IdleSeconds: 0}
	}
	idleFor := now.Sub(since)
	return IdleState{
		Idle:        idleFor >= t.threshold,
		IdleSeconds: int64(idleFor.Seconds()),
	}
}

// FormatIdleLabel renders a compact usage label for the TUI from an idle state:
// "busy" when active, "idle <duration>" when idle. Empty when the state is nil.
func FormatIdleLabel(idle *IdleState) string {
	if idle == nil {
		return ""
	}
	if !idle.Idle {
		return "busy"
	}
	return "idle " + formatIdleDuration(idle.IdleSeconds)
}

// formatIdleDuration renders an idle duration compactly: "38m", "2h", "45s".
func formatIdleDuration(seconds int64) string {
	switch {
	case seconds >= 3600:
		return fmt.Sprintf("%dh", seconds/3600)
	case seconds >= 60:
		return fmt.Sprintf("%dm", seconds/60)
	default:
		return fmt.Sprintf("%ds", seconds)
	}
}

// serviceContainerNames returns the candidate container names to probe for a
// managed service by its logical name. Managed engines/services deploy as
// "citadel-<name>" (compose path) but a manifest service may also run under its
// bare name, so both are tried; the first one that appears in the stats output
// wins.
func serviceContainerNames(name string) []string {
	return []string{"citadel-" + name, name}
}

// attachFootprints attaches a live resource footprint to every running managed
// service and app in the status, using a single batched collection. It maps
// each running service/app to its container name(s), collects footprints once,
// and writes the result back. Stopped services/apps are skipped (no container
// to stat).
func (c *Collector) attachFootprints(status *NodeStatus) {
	// Map candidate container name -> back-reference so we can attach the result.
	type ref struct {
		svcIdx int // index into status.Services, or -1
		appIdx int // index into status.Apps, or -1
	}
	refs := map[string]ref{}
	var names []string
	addName := func(cn string, r ref) {
		if _, exists := refs[cn]; exists {
			return
		}
		refs[cn] = r
		names = append(names, cn)
	}

	for i := range status.Services {
		if status.Services[i].Status != ServiceStatusRunning {
			continue
		}
		for _, cn := range serviceContainerNames(status.Services[i].Name) {
			addName(cn, ref{svcIdx: i, appIdx: -1})
		}
	}
	for i := range status.Apps {
		if status.Apps[i].Status != "running" {
			continue
		}
		addName(apps.ContainerName(status.Apps[i].Name), ref{svcIdx: -1, appIdx: i})
	}

	if len(names) == 0 {
		return
	}

	// Bound the stats/nvidia-smi execs: under the exact overload this feature
	// targets (the #421 incident hit load average 585), `docker stats` and
	// `nvidia-smi` can hang for many seconds. Since this runs inside Collect()
	// on the heartbeat path, an unbounded wait would stall the heartbeat during
	// overload — precisely when it's most needed. Degrade to "unknown" instead.
	ctx, cancel := context.WithTimeout(context.Background(), footprintCollectTimeout)
	defer cancel()
	footprints := CollectFootprints(ctx, names)
	for cn, fp := range footprints {
		r, ok := refs[cn]
		if !ok {
			continue
		}
		// Only attach a footprint that actually resolved to a container: a bare
		// name that produced no stats row leaves CPU=-1/RAM=0, which we skip so
		// the "citadel-<name>" variant (or nothing) wins.
		if fp.CPUPercent < 0 && fp.RAMBytes == 0 && fp.VRAMBytes == 0 {
			continue
		}
		fpCopy := fp
		if r.svcIdx >= 0 {
			status.Services[r.svcIdx].Footprint = &fpCopy
			c.attachDerivedIdle(cn, &fpCopy, &status.Services[r.svcIdx].IdleState)
		} else if r.appIdx >= 0 {
			status.Apps[r.appIdx].Footprint = &fpCopy
			c.attachDerivedIdle(cn, &fpCopy, &status.Apps[r.appIdx].IdleState)
		}
	}
}

// attachDerivedIdle fills in a per-service idle signal when the metrics-based
// path (#420, vLLM request counters) left one absent. There are two generic
// fallbacks, in precedence order:
//
//  1. vLLM request-counter idle (#420): authoritative when present, left
//     untouched here.
//  2. Network-activity idle (#433): a request-agnostic signal derived from the
//     container's cumulative NetIO, folded through the shared IdleTracker so it
//     honors SERVICE_IDLE_THRESHOLD_SECONDS. This is the auto-stop parity path
//     for agent-app containers (Claude Code, OpenClaw, Hermes) that emit no
//     vLLM metrics — a Node process idling above the CPU floor would never be
//     caught by the CPU/GPU heuristic below.
//  3. Footprint (CPU/GPU) idle (#421): the last-resort heuristic, on a fixed
//     60s clock, that powers the TUI "heavy AND idle" eviction warning.
//
// The footprint tracker is ALWAYS fed (its per-container 60s clock must stay
// warm so the #421 heavy-and-idle warning keeps working) but its state is only
// EMITTED when neither the vLLM nor the network-activity signal produced one.
// The network signal is preferred over CPU/GPU for auto-stop because it does
// not false-idle a busy-but-low-CPU agent process, and because it respects the
// operator-configured threshold rather than a hardcoded 60s.
func (c *Collector) attachDerivedIdle(containerName string, fp *ServiceFootprint, dst **IdleState) {
	// Feed the footprint tracker unconditionally so its heavy-and-idle clock
	// keeps advancing regardless of which signal is emitted.
	var footprintIdle *IdleState
	if c.fpIdleTracker != nil {
		f := c.fpIdleTracker.Observe(containerName, fp)
		footprintIdle = &f
	}

	if *dst != nil {
		return // vLLM request-counter idle is authoritative
	}

	// Network-activity idle: preferred generic signal (#433). Fed ONLY when the
	// stats read actually produced a NetIO reading for this container. If NetIO
	// was absent (older format / runtime without the column), fp.HasNet is false
	// and we do NOT feed the tracker: an unread, permanently-flat counter would
	// otherwise look idle after the threshold and could auto-stop a busy
	// container. Fail-safe = fall through to the footprint heuristic (or emit
	// nothing), never fabricate an idle signal from an unknown reading.
	if c.netIdleTracker != nil && fp.HasNet {
		n := c.netIdleTracker.RecordActivityCounter(containerName, fp.NetBytes)
		// GPU-busy override: a container can pin the GPU (a batch diffusers /
		// fine-tune job) while sending no network traffic, so a network-silent
		// verdict of "idle" would be a dangerous false idle on a node doing real
		// work. If the GPU is actively utilized, the container is NOT idle
		// regardless of the network counter. We deliberately do NOT fold CPU in
		// here -- an agent process idling above the CPU floor is exactly the case
		// #433 exists to catch, so only the unambiguous GPU-hot signal overrides.
		if n.Idle && fp.HasGPU && fp.GPUUtilPercent > idleGPUThresholdPercent {
			n.Idle = false
		}
		*dst = &n
		return
	}

	if footprintIdle != nil {
		*dst = footprintIdle
	}
}

// FormatFootprint renders a one-line footprint summary for the TUI, e.g.
// "RAM 6.1G   VRAM 21.0G   GPU 74%". VRAM/GPU degrade to "n/a" when the node has
// no GPU. RAM is always shown (from container stats).
func FormatFootprint(fp *ServiceFootprint) string {
	if fp == nil {
		return ""
	}
	ram := FormatBytesGB(fp.RAMBytes)
	if fp.HasGPU {
		gpu := "n/a"
		if fp.GPUUtilPercent >= 0 {
			gpu = fmt.Sprintf("%.0f%%", fp.GPUUtilPercent)
		}
		return fmt.Sprintf("RAM %s  VRAM %s  GPU %s", ram, FormatBytesGB(fp.VRAMBytes), gpu)
	}
	return fmt.Sprintf("RAM %s  VRAM n/a  GPU n/a", ram)
}
