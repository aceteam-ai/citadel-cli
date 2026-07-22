package pulse

import (
	"context"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aceteam-ai/citadel-cli/services"
)

// EnvVar is the operator KILL SWITCH for heartbeat stats collection
// (citadel-cli#587). Collection is ON by default; set CITADEL_HEARTBEAT_STATS
// to a falsy value ("0", "false", "no", "off") to disable it fleet-wide
// without a binary rollback. Mirrors the CITADEL_RECONCILE_PULL pattern
// (internal/reconcile/config_env.go).
const EnvVar = "CITADEL_HEARTBEAT_STATS"

// IntervalEnvVar overrides the collection interval (Go duration syntax, e.g.
// "10s", "1m"). The --heartbeat-stats-interval flag takes precedence.
const IntervalEnvVar = "CITADEL_HEARTBEAT_STATS_INTERVAL"

// DefaultInterval is the default collection interval. Kept comfortably under
// the 30s heartbeat interval so the cached block a heartbeat picks up is
// always fresh.
const DefaultInterval = 10 * time.Second

// scrapeTimeout bounds each engine /metrics fetch.
const scrapeTimeout = 3 * time.Second

// Disabled reports whether the operator has explicitly turned stats collection
// OFF via the kill switch. Default (unset or any non-falsy value) is enabled.
func Disabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(EnvVar))) {
	case "0", "false", "no", "off":
		return true
	default:
		return false
	}
}

// ResolveInterval returns the collection interval: flag value > env var >
// DefaultInterval. A malformed or non-positive duration falls back to the
// default rather than failing node startup over a telemetry knob.
func ResolveInterval(flagValue string) time.Duration {
	for _, v := range []string{flagValue, os.Getenv(IntervalEnvVar)} {
		if v = strings.TrimSpace(v); v == "" {
			continue
		}
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return DefaultInterval
}

// EngineTarget names a local inference engine metrics endpoint to scrape.
type EngineTarget struct {
	Engine string
	Port   int
}

// DefaultEngineTargets returns the scrape targets for every engine registered
// in the host-port registry with a Prometheus /metrics endpoint
// (services.InferenceMetricsPorts), sorted by engine name for deterministic
// block ordering.
func DefaultEngineTargets() []EngineTarget {
	ports := services.InferenceMetricsPorts()
	targets := make([]EngineTarget, 0, len(ports))
	for engine, port := range ports {
		targets = append(targets, EngineTarget{Engine: engine, Port: port})
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].Engine < targets[j].Engine })
	return targets
}

// Collector periodically gathers GPU + inference stats on its own goroutine
// and caches the latest block. The heartbeat publishers read the cache via
// Latest() and therefore NEVER block on (or fail because of) collection — a
// stale block beats a late heartbeat.
type Collector struct {
	interval time.Duration
	targets  []EngineTarget
	gpuFn    func(context.Context) []GPUStat
	now      func() time.Time

	scrapers []*engineScraper

	mu          sync.RWMutex
	latest      *StatsBlock
	collectedAt time.Time
}

// CollectorConfig configures a Collector. Zero values select the defaults.
type CollectorConfig struct {
	// Interval between collection cycles (default DefaultInterval).
	Interval time.Duration
	// Targets are the engine metrics endpoints to scrape (default
	// DefaultEngineTargets()).
	Targets []EngineTarget
	// GPUFn overrides GPU collection (tests). Default: nvidia-smi.
	GPUFn func(context.Context) []GPUStat
}

// NewCollector creates a stats collector. Call Run on its own goroutine.
func NewCollector(cfg CollectorConfig) *Collector {
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	targets := cfg.Targets
	if targets == nil {
		targets = DefaultEngineTargets()
	}
	gpuFn := cfg.GPUFn
	if gpuFn == nil {
		gpuFn = collectGPUStats
	}
	c := &Collector{
		interval: cfg.Interval,
		targets:  targets,
		gpuFn:    gpuFn,
		now:      time.Now,
	}
	for _, t := range targets {
		if _, ok := dialects[t.Engine]; !ok {
			continue
		}
		c.scrapers = append(c.scrapers, newEngineScraper(t.Engine, t.Port, scrapeTimeout))
	}
	return c
}

// Interval returns the configured collection interval.
func (c *Collector) Interval() time.Duration { return c.interval }

// Run collects immediately and then on every interval tick until the context
// is cancelled. It never returns an error and never panics out: telemetry
// must not be able to take down the node process.
func (c *Collector) Run(ctx context.Context) {
	c.collectOnce(ctx)

	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.collectOnce(ctx)
		}
	}
}

// collectOnce runs one collection cycle and swaps the cached block. Any panic
// in collection (a parser bug on a hostile /metrics response, a driver
// oddity) is swallowed: the cache simply keeps its previous value and ages
// out via the Latest() staleness bound.
func (c *Collector) collectOnce(ctx context.Context) {
	defer func() {
		_ = recover()
	}()

	block := &StatsBlock{
		V:    StatsVersion,
		TS:   c.now().Unix(),
		GPUs: c.gpuFn(ctx),
	}
	for _, s := range c.scrapers {
		if stat := s.Observe(ctx); stat != nil {
			block.Inference = append(block.Inference, *stat)
		}
	}

	c.mu.Lock()
	c.latest = block
	c.collectedAt = c.now()
	c.mu.Unlock()
}

// Latest returns the most recently collected stats block, or nil when nothing
// has been collected yet or the cache has gone stale (the collector stopped
// refreshing for over 3 intervals — e.g. wedged behind a hung subprocess).
// Nil means the heartbeat ships no stats field at all, exactly like a legacy
// node. Never blocks.
func (c *Collector) Latest() *StatsBlock {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.latest == nil {
		return nil
	}
	maxAge := 3 * c.interval
	if minAge := 30 * time.Second; maxAge < minAge {
		maxAge = minAge
	}
	if c.now().Sub(c.collectedAt) > maxAge {
		return nil
	}
	return c.latest
}
