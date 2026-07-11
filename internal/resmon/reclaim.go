package resmon

import (
	"fmt"
	"sync"
	"time"
)

// heavyVRAMBytes is the VRAM threshold above which an idle consumer is a
// reclaim candidate regardless of RSS. Mirrors internal/status.heavyVRAMBytes
// (~2 GiB): any multi-GB VRAM reservation held while idle is worth surfacing on
// a contended GPU node.
const heavyVRAMBytes uint64 = 2 * 1024 * 1024 * 1024

// heavyRSSBytes is the RSS threshold above which an idle consumer is a reclaim
// candidate regardless of VRAM. Mirrors internal/status.heavyRAMBytes (~4 GiB):
// large enough to exclude small sidecars, low enough to catch a stuck worker
// (the #421 diffusers case).
const heavyRSSBytes uint64 = 4 * 1024 * 1024 * 1024

// reclaimable reports whether a consumer is a heavy, idle, UNMANAGED candidate
// worth freeing, and a short human-readable reason when so. This is #427's
// value-add: managed heavy-idle is already covered by #421, so managed
// consumers are never reclaimable here. The predicate is
// heavy AND idle AND NOT citadel-managed.
//
// It is a pure function over (vram, rss, idle, kind) so it is unit-tested
// directly against the three cases the issue calls out: unmanaged heavy+idle →
// true, managed → false, heavy+active → false.
func reclaimable(vram, rss uint64, idle bool, kind OwnerKind) (reason string, ok bool) {
	if kind == OwnerCitadelManaged {
		return "", false // #421 owns managed eviction
	}
	if !idle {
		return "", false
	}
	heavyVRAM := vram >= heavyVRAMBytes
	heavyRSS := rss >= heavyRSSBytes
	if !heavyVRAM && !heavyRSS {
		return "", false
	}
	switch {
	case heavyVRAM && heavyRSS:
		reason = fmt.Sprintf("idle, holding %s VRAM + %s RAM, not tracked by citadel — free manually",
			formatGB(vram), formatGB(rss))
	case heavyVRAM:
		reason = fmt.Sprintf("idle, holding %s VRAM, not tracked by citadel — free manually", formatGB(vram))
	default:
		reason = fmt.Sprintf("idle, holding %s RAM, not tracked by citadel — free manually", formatGB(rss))
	}
	return reason, true
}

// formatGB renders a byte count as a compact "6.1G" string, matching
// internal/status.FormatBytesGB.
func formatGB(b uint64) string {
	return fmt.Sprintf("%.1fG", float64(b)/(1<<30))
}

// idleCPUThresholdPercent is the per-process CPU% at or below which a consumer
// is considered inactive. Mirrors internal/status.idleCPUThresholdPercent.
const idleCPUThresholdPercent = 2.0

// idleGPUThresholdPercent is the whole-GPU utilization at or below which a GPU
// consumer is considered inactive. Mirrors internal/status.idleGPUThresholdPercent.
const idleGPUThresholdPercent = 5.0

// idleThreshold is how long a consumer must stay inactive before it is flagged
// idle. Mirrors internal/status.footprintIdleThreshold.
const idleThreshold = 60 * time.Second

// idleTracker derives a per-pid idle signal from CPU + GPU utilization, reusing
// the #421 FootprintIdleTracker heuristic (thresholds + first-inactive time
// accumulation) but self-contained so this package does not import
// internal/status. It is stateful and must be long-lived: idle duration
// accumulates from the first-inactive timestamp, so a single on-demand observe
// reports idle=false (idleFor=0). Safe for concurrent use.
type idleTracker struct {
	mu            sync.Mutex
	inactiveSince map[int]time.Time
	threshold     time.Duration
	now           func() time.Time // injectable for tests
}

// newIdleTracker returns a tracker with the default idle threshold.
func newIdleTracker() *idleTracker {
	return &idleTracker{
		inactiveSince: map[int]time.Time{},
		threshold:     idleThreshold,
		now:           time.Now,
	}
}

// active reports whether a consumer is doing work: per-process CPU above the
// idle threshold, or (when a GPU is present) whole-GPU utilization above its
// threshold. Unknown CPU (-1) is treated as not-active-on-CPU so a missing CPU
// read alone doesn't mask idleness — matching internal/status.footprintActive.
func active(cpuPercent, gpuUtil float64, hasGPU bool) bool {
	if cpuPercent > idleCPUThresholdPercent {
		return true
	}
	if hasGPU && gpuUtil > idleGPUThresholdPercent {
		return true
	}
	return false
}

// observe updates the tracker for a pid's current utilization and returns
// whether it is now idle (inactive for at least the threshold). Active pids
// reset the idle clock. Inactive pids accumulate from when they first went
// quiet, flipping idle=true once past the threshold.
func (t *idleTracker) observe(pid int, cpuPercent, gpuUtil float64, hasGPU bool) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := t.now()
	if active(cpuPercent, gpuUtil, hasGPU) {
		delete(t.inactiveSince, pid)
		return false
	}
	since, ok := t.inactiveSince[pid]
	if !ok {
		t.inactiveSince[pid] = now
		return false
	}
	return now.Sub(since) >= t.threshold
}
