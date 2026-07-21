package jobs

import (
	"reflect"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/status"
)

const testGiB = uint64(1) << 30

func TestParseRequiredVRAMBytes(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]string
		want    uint64
	}{
		{"absent", map[string]string{"service": "vllm"}, 0},
		{"blank", map[string]string{"vram_mb": "  "}, 0},
		{"zero", map[string]string{"vram_mb": "0"}, 0},
		{"negative", map[string]string{"vram_gb": "-4"}, 0},
		{"garbage", map[string]string{"vram_mb": "lots"}, 0},
		{"mb", map[string]string{"vram_mb": "8192"}, 8192 * 1024 * 1024},
		{"gb", map[string]string{"vram_gb": "6"}, 6 * 1024 * 1024 * 1024},
		{"mb_wins_over_gb", map[string]string{"vram_mb": "1024", "vram_gb": "40"}, 1024 * 1024 * 1024},
		{"fractional_gb", map[string]string{"vram_gb": "5.5"}, uint64(5.5 * 1024 * 1024 * 1024)},
	}
	for _, c := range cases {
		if got := parseRequiredVRAMBytes(c.payload); got != c.want {
			t.Errorf("%s: parseRequiredVRAMBytes = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestFreeVRAMBytes(t *testing.T) {
	// No GPU reporting a total => unknown (found=false), so callers skip the fit
	// check rather than treat unknown as zero-free.
	if _, ok := freeVRAMBytes(nil); ok {
		t.Fatalf("nil GPUs must report unknown")
	}
	if _, ok := freeVRAMBytes([]status.GPUMetrics{{MemoryTotalMB: 0}}); ok {
		t.Fatalf("a GPU with no total must report unknown")
	}
	// total 24GB, used 20GB => 4GB free.
	free, ok := freeVRAMBytes([]status.GPUMetrics{{MemoryTotalMB: 24576, MemoryUsedMB: 20480}})
	if !ok || free != 4096*1024*1024 {
		t.Fatalf("expected 4GB free, got %d ok=%v", free, ok)
	}
	// Used > total (transient over-report) clamps to 0, doesn't underflow.
	free, ok = freeVRAMBytes([]status.GPUMetrics{{MemoryTotalMB: 1000, MemoryUsedMB: 2000}})
	if !ok || free != 0 {
		t.Fatalf("expected clamp to 0 free, got %d ok=%v", free, ok)
	}
}

func TestBuildPreemptCandidates(t *testing.T) {
	st := &status.NodeStatus{
		Services: []status.ServiceInfo{
			{Name: "vllm", Status: status.ServiceStatusRunning,
				Footprint: &status.ServiceFootprint{VRAMBytes: 20 * testGiB, CPUPercent: 0.5, HasGPU: true, GPUUtilPercent: 0}},
			{Name: "bonsai", Status: status.ServiceStatusRunning,
				Footprint: &status.ServiceFootprint{VRAMBytes: 6 * testGiB, CPUPercent: 90, HasGPU: true, GPUUtilPercent: 80}},
			{Name: "stopped-one", Status: status.ServiceStatusStopped},
			{Name: "self", Status: status.ServiceStatusRunning,
				Footprint: &status.ServiceFootprint{VRAMBytes: 3 * testGiB}},
		},
	}
	got := buildPreemptCandidates(st, "self", map[string]bool{"bonsai": true})

	want := []status.PreemptCandidate{
		// vllm: low CPU + low GPU => instantaneously idle.
		{Name: "vllm", VRAMBytes: 20 * testGiB, Idle: true, Pinned: false},
		// bonsai: busy (high CPU/GPU) and pinned.
		{Name: "bonsai", VRAMBytes: 6 * testGiB, Idle: false, Pinned: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("candidates mismatch:\n got=%+v\nwant=%+v", got, want)
	}
	// Excluded target and stopped services must not appear.
	for _, c := range got {
		if c.Name == "self" || c.Name == "stopped-one" {
			t.Fatalf("unexpected candidate %q", c.Name)
		}
	}
}
