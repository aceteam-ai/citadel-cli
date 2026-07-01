package footprint

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const (
	// DefaultInterval is the sampling cadence when CITADEL_FOOTPRINT_INTERVAL is
	// unset. 60s keeps the sampler negligibly cheap (one stats exec + one GPU
	// read per minute) while still catching multi-minute idle-hoarding stretches.
	DefaultInterval = 60 * time.Second
	// DefaultRetentionDays bounds on-disk history so the log never becomes a disk
	// hog. One week of per-minute rows is a few MB.
	DefaultRetentionDays = 7
)

// Config configures the background footprint sampler. It deliberately takes
// primitives (not cmd types) so internal/footprint never imports cmd.
type Config struct {
	// NodeID is the node identity stamped on every row (Headscale hostname).
	NodeID string
	// Services is the list of managed service names to attribute stats to
	// (typically the manifest's service names).
	Services []string
	// EngineBin is the container engine CLI ("docker" or "podman") for the stats
	// exec, resolved by the caller from catalog.SelectContainerRuntime().
	EngineBin string
	// Dir is the footprints directory (~/citadel-node/footprints).
	Dir string
	// Interval is the sampling cadence. Zero uses DefaultInterval.
	Interval time.Duration
	// RetentionDays prunes files older than this many days. Zero uses
	// DefaultRetentionDays; negative disables pruning.
	RetentionDays int
	// Logf is an optional structured log sink (e.g. cmd.Log). May be nil.
	Logf func(format string, args ...any)
}

// IntervalFromEnv resolves the sampling interval, honoring
// CITADEL_FOOTPRINT_INTERVAL (seconds). A value <= 0 disables the sampler
// entirely (returns ok=false). An unset/invalid env var falls back to
// DefaultInterval.
func IntervalFromEnv() (interval time.Duration, enabled bool) {
	raw := os.Getenv("CITADEL_FOOTPRINT_INTERVAL")
	if raw == "" {
		return DefaultInterval, true
	}
	secs, err := strconv.Atoi(raw)
	if err != nil {
		return DefaultInterval, true
	}
	if secs <= 0 {
		return 0, false
	}
	return time.Duration(secs) * time.Second, true
}

// RetentionFromEnv resolves the retention window, honoring
// CITADEL_FOOTPRINT_RETENTION_DAYS. Unset/invalid falls back to
// DefaultRetentionDays.
func RetentionFromEnv() int {
	raw := os.Getenv("CITADEL_FOOTPRINT_RETENTION_DAYS")
	if raw == "" {
		return DefaultRetentionDays
	}
	days, err := strconv.Atoi(raw)
	if err != nil {
		return DefaultRetentionDays
	}
	return days
}

// Run samples footprints on cfg.Interval until ctx is cancelled. It prunes stale
// files once at startup and opportunistically after each tick, so the log stays
// bounded even for a long-lived node. Errors are logged (via cfg.Logf when set)
// and never abort the loop — a transient stats failure must not kill the sampler.
func Run(ctx context.Context, cfg Config) {
	interval := cfg.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	retentionDays := cfg.RetentionDays
	if retentionDays == 0 {
		retentionDays = DefaultRetentionDays
	}

	logf := cfg.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}

	store, err := NewStore(cfg.Dir)
	if err != nil {
		logf("footprint: sampler disabled: %v", err)
		return
	}

	sampler := NewSampler(cfg.NodeID, cfg.Services, cfg.EngineBin)

	// Prune once at startup so a node that was offline past the retention window
	// cleans up immediately rather than after the first tick.
	if _, err := store.Prune(time.Now(), retentionDays); err != nil {
		logf("footprint: startup prune: %v", err)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	tick := func() {
		now := time.Now()
		samples := sampler.Sample(ctx, now)
		if err := store.Append(samples); err != nil {
			logf("footprint: append: %v", err)
		}
		if _, err := store.Prune(now, retentionDays); err != nil {
			logf("footprint: prune: %v", err)
		}
	}

	// Emit one sample immediately so `citadel footprints` has data without waiting
	// a full interval.
	tick()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tick()
		}
	}
}

// DefaultDir returns the footprints directory under the given node dir.
func DefaultDir(nodeDir string) string {
	return filepath.Join(nodeDir, "footprints")
}
