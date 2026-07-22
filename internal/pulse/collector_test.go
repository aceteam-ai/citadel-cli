package pulse

import (
	"context"
	"testing"
	"time"
)

func TestCollectorOmitsEmptyArrays(t *testing.T) {
	// CPU-only node with no inference engine: the block still ships (v/ts
	// prove the collector is alive) but carries no gpus/inference arrays.
	c := NewCollector(CollectorConfig{
		Targets: []EngineTarget{},
		GPUFn:   func(context.Context) []GPUStat { return nil },
	})
	c.collectOnce(context.Background())

	block := c.Latest()
	if block == nil {
		t.Fatal("expected a block after collection")
	}
	if block.V != StatsVersion {
		t.Errorf("v: got %d, want %d", block.V, StatsVersion)
	}
	if block.TS == 0 {
		t.Error("ts must be set")
	}
	if block.GPUs != nil || block.Inference != nil {
		t.Errorf("empty collections must be nil (omitted on the wire): gpus=%v inference=%v", block.GPUs, block.Inference)
	}
}

func TestCollectorLatestBeforeFirstCollection(t *testing.T) {
	c := NewCollector(CollectorConfig{Targets: []EngineTarget{}})
	if got := c.Latest(); got != nil {
		t.Errorf("expected nil before first collection, got %+v", got)
	}
}

func TestCollectorLatestGoesStale(t *testing.T) {
	c := NewCollector(CollectorConfig{
		Interval: 10 * time.Second,
		Targets:  []EngineTarget{},
		GPUFn:    func(context.Context) []GPUStat { return []GPUStat{{Index: 0}} },
	})
	base := time.Unix(1753142400, 0)
	c.now = func() time.Time { return base }
	c.collectOnce(context.Background())

	// Fresh: served.
	if c.Latest() == nil {
		t.Fatal("fresh block must be served")
	}
	// Within 3 intervals: still served (stale beats absent).
	c.now = func() time.Time { return base.Add(29 * time.Second) }
	if c.Latest() == nil {
		t.Error("block within the staleness bound must be served")
	}
	// Beyond 3 intervals: the collector is wedged; ship nothing rather than
	// ancient numbers.
	c.now = func() time.Time { return base.Add(31 * time.Second) }
	if got := c.Latest(); got != nil {
		t.Errorf("stale block must not be served, got %+v", got)
	}
}

func TestCollectorSurvivesPanickingSource(t *testing.T) {
	c := NewCollector(CollectorConfig{
		Targets: []EngineTarget{},
		GPUFn:   func(context.Context) []GPUStat { panic("driver went sideways") },
	})
	// Must not propagate: the heartbeat process stays alive and simply has no
	// stats block.
	c.collectOnce(context.Background())
	if got := c.Latest(); got != nil {
		t.Errorf("panicking collection must leave no block, got %+v", got)
	}
}

func TestDefaultEngineTargets(t *testing.T) {
	targets := DefaultEngineTargets()
	if len(targets) != 2 {
		t.Fatalf("expected vllm + sglang, got %+v", targets)
	}
	// Sorted by engine name for deterministic block ordering.
	if targets[0].Engine != "sglang" || targets[1].Engine != "vllm" {
		t.Errorf("ordering: got %+v", targets)
	}
	for _, tgt := range targets {
		if tgt.Port <= 0 {
			t.Errorf("%s has no port: %+v", tgt.Engine, tgt)
		}
		if _, ok := dialects[tgt.Engine]; !ok {
			t.Errorf("%s has no metrics dialect", tgt.Engine)
		}
	}
}

func TestDisabled(t *testing.T) {
	cases := []struct {
		val  string
		want bool
	}{
		{"", false},
		{"1", false},
		{"true", false},
		{"anything", false},
		{"0", true},
		{"false", true},
		{"no", true},
		{"off", true},
		{" FALSE ", true},
	}
	for _, tc := range cases {
		t.Setenv(EnvVar, tc.val)
		if got := Disabled(); got != tc.want {
			t.Errorf("Disabled() with %q = %v, want %v", tc.val, got, tc.want)
		}
	}
}

func TestResolveInterval(t *testing.T) {
	t.Setenv(IntervalEnvVar, "")
	if got := ResolveInterval(""); got != DefaultInterval {
		t.Errorf("default: got %v", got)
	}
	if got := ResolveInterval("30s"); got != 30*time.Second {
		t.Errorf("flag: got %v", got)
	}
	t.Setenv(IntervalEnvVar, "1m")
	if got := ResolveInterval(""); got != time.Minute {
		t.Errorf("env: got %v", got)
	}
	// Flag beats env.
	if got := ResolveInterval("15s"); got != 15*time.Second {
		t.Errorf("flag precedence: got %v", got)
	}
	// Malformed and non-positive fall through to the default.
	t.Setenv(IntervalEnvVar, "bogus")
	if got := ResolveInterval("-5s"); got != DefaultInterval {
		t.Errorf("malformed: got %v", got)
	}
}
