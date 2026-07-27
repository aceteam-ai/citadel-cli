package footprint

import (
	"context"
	"math"
	"testing"
	"time"
)

func TestSamplerBuildsServiceAndNodeRows(t *testing.T) {
	s := &Sampler{
		nodeID:    "node-x",
		services:  []string{"vllm", "diffusers"},
		engineBin: "docker",
		stats: func(ctx context.Context, engineBin string) ([]containerStat, error) {
			if engineBin != "docker" {
				t.Errorf("expected engineBin docker, got %q", engineBin)
			}
			// Only vllm is running; diffusers absent.
			return []containerStat{
				{Name: "proj-vllm-1", CPUPerc: "42.0%", MemUsage: "7.4GiB / 62GiB"},
			}, nil
		},
		gpu: func() GPUSnapshot {
			return GPUSnapshot{HasGPU: true, VRAMUsedMB: 7400, GPUUtilPercent: 3}
		},
		idle: func() (int, bool) { return 0, false },
	}

	ts := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	rows := s.Sample(context.Background(), ts)
	if len(rows) != 3 { // 2 services + node
		t.Fatalf("expected 3 rows, got %d", len(rows))
	}

	byService := map[string]Sample{}
	for _, r := range rows {
		byService[r.Service] = r
	}

	vllm := byService["vllm"]
	if !vllm.Running {
		t.Error("vllm should be running")
	}
	if vllm.RSSMB == nil || math.Abs(*vllm.RSSMB-7.4*1024) > 1 {
		t.Errorf("vllm RSS = %v, want ~7577 MB", vllm.RSSMB)
	}
	if vllm.CPUPercent == nil || *vllm.CPUPercent != 42 {
		t.Errorf("vllm CPU = %v, want 42", vllm.CPUPercent)
	}
	// Per-service VRAM is intentionally not attributed.
	if vllm.VRAMMB != nil {
		t.Errorf("per-service VRAM must be nil, got %v", vllm.VRAMMB)
	}

	diff := byService["diffusers"]
	if diff.Running {
		t.Error("diffusers should not be running (no matching container)")
	}
	if diff.RSSMB != nil {
		t.Errorf("absent service should have nil RSS, got %v", diff.RSSMB)
	}

	node := byService[NodeService]
	if !node.Running {
		t.Error("node row should be marked running")
	}
	if node.VRAMMB == nil || *node.VRAMMB != 7400 {
		t.Errorf("node VRAM = %v, want 7400", node.VRAMMB)
	}
	if node.GPUUtilPercent == nil || *node.GPUUtilPercent != 3 {
		t.Errorf("node GPU util = %v, want 3", node.GPUUtilPercent)
	}
}

func TestSamplerStatsErrorStillEmitsNodeRow(t *testing.T) {
	s := &Sampler{
		nodeID:    "n",
		services:  []string{"vllm"},
		engineBin: "docker",
		stats: func(ctx context.Context, engineBin string) ([]containerStat, error) {
			return nil, context.DeadlineExceeded
		},
		gpu:  func() GPUSnapshot { return GPUSnapshot{} }, // no GPU
		idle: func() (int, bool) { return 0, false },
	}
	rows := s.Sample(context.Background(), time.Now())
	if len(rows) != 2 {
		t.Fatalf("expected service + node rows even on stats error, got %d", len(rows))
	}
	for _, r := range rows {
		if r.Service == "vllm" && r.Running {
			t.Error("vllm should report not running when stats fails")
		}
		if r.Service == NodeService && r.VRAMMB != nil {
			t.Error("node row should have nil VRAM when no GPU")
		}
	}
}

func TestSamplerNodeRowCarriesMeasuredPower(t *testing.T) {
	s := &Sampler{
		nodeID:    "n",
		services:  []string{"vllm"},
		engineBin: "docker",
		energy:    true,
		interval:  60 * time.Second,
		powerCfg:  PowerConfig{CPUTDPWatts: 65},
		stats:     func(ctx context.Context, _ string) ([]containerStat, error) { return nil, nil },
		gpu: func() GPUSnapshot {
			return GPUSnapshot{
				HasGPU: true, VRAMUsedMB: 8000, GPUUtilPercent: 55,
				PowerWatts: 210, PowerMeasured: true, PowerLimitWatts: 350,
			}
		},
		idle: func() (int, bool) { return 0, false },
	}
	rows := s.Sample(context.Background(), time.Now())
	var node *Sample
	for i := range rows {
		if rows[i].Service == NodeService {
			node = &rows[i]
		}
		if rows[i].Service == "vllm" {
			// Per-service rows never carry power in this increment.
			if rows[i].PowerW != nil || rows[i].EnergyWh != nil || rows[i].PowerSource != PowerSourceUnknown {
				t.Errorf("service row must not carry power, got %+v", rows[i])
			}
		}
	}
	if node == nil {
		t.Fatal("no node row")
	}
	if node.PowerW == nil || *node.PowerW != 210 {
		t.Errorf("node power_w = %v, want 210 (measured draw wins)", node.PowerW)
	}
	if node.PowerSource != PowerSourceMeasured {
		t.Errorf("node power_source = %q, want measured", node.PowerSource)
	}
	// 210W for 60s = 210/60 Wh.
	if node.EnergyWh == nil || math.Abs(*node.EnergyWh-210.0/60.0) > 1e-6 {
		t.Errorf("node energy_wh = %v, want %v", node.EnergyWh, 210.0/60.0)
	}
}

func TestSamplerNodeRowEstimatesFromUtilWhenNoDraw(t *testing.T) {
	s := &Sampler{
		nodeID:    "n",
		services:  nil,
		engineBin: "docker",
		energy:    true,
		interval:  60 * time.Second,
		powerCfg:  PowerConfig{CPUTDPWatts: 65},
		stats:     func(ctx context.Context, _ string) ([]containerStat, error) { return nil, nil },
		gpu: func() GPUSnapshot {
			// No measured draw, but a power.limit is known -> tier 2 estimate.
			return GPUSnapshot{HasGPU: true, GPUUtilPercent: 40, PowerLimitWatts: 350}
		},
		idle: func() (int, bool) { return 0, false },
	}
	rows := s.Sample(context.Background(), time.Now())
	node := rows[len(rows)-1]
	if node.Service != NodeService {
		t.Fatalf("last row should be node, got %q", node.Service)
	}
	if node.PowerSource != PowerSourceEstimated {
		t.Errorf("power_source = %q, want estimated", node.PowerSource)
	}
	if node.PowerW == nil || math.Abs(*node.PowerW-140) > 1e-6 { // 40% of 350
		t.Errorf("power_w = %v, want 140", node.PowerW)
	}
}

// TestSamplerEnergyDisabledSkipsProbeAndColumns verifies the default-OFF contract:
// with energy off, the GPU power probe is never invoked and the node row carries
// no power_w / energy_wh / power_source, even though a GPU with power is present.
func TestSamplerEnergyDisabledSkipsProbeAndColumns(t *testing.T) {
	probeCalled := false
	s := &Sampler{
		nodeID:    "n",
		services:  []string{"vllm"},
		engineBin: "docker",
		energy:    false, // disabled
		interval:  60 * time.Second,
		powerCfg:  PowerConfig{CPUTDPWatts: 65},
		stats:     func(ctx context.Context, _ string) ([]containerStat, error) { return nil, nil },
		gpu: func() GPUSnapshot {
			return GPUSnapshot{HasGPU: true, VRAMUsedMB: 8000, GPUUtilPercent: 55}
		},
		gpuPower: func() (float64, bool, float64) {
			probeCalled = true
			return 210, true, 350
		},
		idle: func() (int, bool) { return 0, false },
	}
	rows := s.Sample(context.Background(), time.Now())
	if probeCalled {
		t.Error("GPU power probe must NOT be invoked when energy sampling is disabled")
	}
	node := rows[len(rows)-1]
	if node.Service != NodeService {
		t.Fatalf("last row should be node, got %q", node.Service)
	}
	// VRAM/util still recorded (unchanged behavior); power columns stay blank.
	if node.VRAMMB == nil || *node.VRAMMB != 8000 {
		t.Errorf("VRAM should still be recorded when energy off, got %v", node.VRAMMB)
	}
	if node.PowerW != nil || node.EnergyWh != nil || node.PowerSource != PowerSourceUnknown {
		t.Errorf("no power fields when energy off, got power_w=%v energy_wh=%v source=%q", node.PowerW, node.EnergyWh, node.PowerSource)
	}
}

// TestSamplerEnergyEnabledInvokesProbe verifies the enabled path calls the
// injected power probe and stamps the resulting measured figure.
func TestSamplerEnergyEnabledInvokesProbe(t *testing.T) {
	probeCalled := false
	s := &Sampler{
		nodeID:    "n",
		engineBin: "docker",
		energy:    true,
		interval:  time.Hour,
		powerCfg:  PowerConfig{CPUTDPWatts: 65},
		stats:     func(ctx context.Context, _ string) ([]containerStat, error) { return nil, nil },
		gpu: func() GPUSnapshot {
			return GPUSnapshot{HasGPU: true, GPUUtilPercent: 50}
		},
		gpuPower: func() (float64, bool, float64) {
			probeCalled = true
			return 300, true, 350
		},
		idle: func() (int, bool) { return 0, false },
	}
	rows := s.Sample(context.Background(), time.Now())
	if !probeCalled {
		t.Error("GPU power probe should be invoked when energy sampling is enabled")
	}
	node := rows[len(rows)-1]
	if node.PowerW == nil || *node.PowerW != 300 || node.PowerSource != PowerSourceMeasured {
		t.Errorf("expected measured 300W from probe, got power_w=%v source=%q", node.PowerW, node.PowerSource)
	}
	// 300W for 1h = 300 Wh.
	if node.EnergyWh == nil || math.Abs(*node.EnergyWh-300) > 1e-6 {
		t.Errorf("energy_wh = %v, want 300", node.EnergyWh)
	}
}

func TestSamplerIdleSignalWiredThrough(t *testing.T) {
	s := &Sampler{
		nodeID:    "n",
		services:  []string{"svc"},
		engineBin: "docker",
		stats:     func(ctx context.Context, _ string) ([]containerStat, error) { return nil, nil },
		gpu:       func() GPUSnapshot { return GPUSnapshot{} },
		idle:      func() (int, bool) { return 300, true },
	}
	rows := s.Sample(context.Background(), time.Now())
	for _, r := range rows {
		if r.IdleSeconds == nil || *r.IdleSeconds != 300 {
			t.Errorf("row %s: idle = %v, want 300", r.Service, r.IdleSeconds)
		}
	}
}
