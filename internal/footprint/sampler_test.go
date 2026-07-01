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
