package capabilities

import (
	"testing"
)

func TestBuildAppleGPUCapabilities(t *testing.T) {
	const gb = uint64(1024 * 1024 * 1024)
	caps := buildAppleGPUCapabilities("Apple M2 Max", 24*gb)
	if caps == nil {
		t.Fatal("buildAppleGPUCapabilities returned nil for Apple Silicon")
	}
	if caps.Count != 1 || len(caps.Devices) != 1 {
		t.Fatalf("expected 1 device, got count=%d devices=%d", caps.Count, len(caps.Devices))
	}
	if caps.DriverStatus != "ok" {
		t.Errorf("DriverStatus = %q, want ok", caps.DriverStatus)
	}
	d := caps.Devices[0]
	if d.Tag != "apple-m2-max" {
		t.Errorf("Tag = %q, want apple-m2-max", d.Tag)
	}
	if !d.Unified {
		t.Error("device Unified = false, want true")
	}
	if d.VRAMMb != 18432 { // 24576 * 0.75
		t.Errorf("VRAMMb = %d, want 18432", d.VRAMMb)
	}
	if d.VRAMTag != "18gb" { // round(18432/1024)
		t.Errorf("VRAMTag = %q, want 18gb", d.VRAMTag)
	}
}

func TestBuildAppleGPUCapabilities_NonAppleReturnsNil(t *testing.T) {
	if caps := buildAppleGPUCapabilities("AMD Radeon Pro 5500M", 8*1024*1024*1024); caps != nil {
		t.Errorf("expected nil for a non-Apple-Silicon chipset, got %+v", caps)
	}
}

// appleUnifiedCaps builds a NodeCapabilities carrying only an Apple Silicon
// unified GPU (plus the tags DetectNodeCapabilities would attach), for the
// queue-routing tests below.
func appleUnifiedCaps() *NodeCapabilities {
	gpu := buildAppleGPUCapabilities("Apple M2 Max", 24*1024*1024*1024)
	return &NodeCapabilities{
		GPU: gpu,
		Tags: []string{
			"gpu:apple-m2-max", "vram:18gb", "cpu:general",
			"os:darwin", "os:macos", "arch:arm64",
		},
	}
}

// TestGPUInferenceQueues_UnifiedGPUJoinsNoDiscreteQueues is the load-bearing
// guard (citadel-cli#1042): advertising an apple GPU device must NOT make the
// Mac join the discrete-GPU queues (gpu-general + tag queues), which is exactly
// how a CUDA-shaped job would reach a node that cannot run it. A unified-only
// node returns nil here, identical to the pre-#1042 caps.GPU==nil behavior.
func TestGPUInferenceQueues_UnifiedGPUJoinsNoDiscreteQueues(t *testing.T) {
	if q := GPUInferenceQueues(appleUnifiedCaps()); q != nil {
		t.Errorf("unified-only node joined discrete GPU queues: %v (want nil)", q)
	}
}

func TestGPUInferenceQueues_DiscreteGPUStillJoins(t *testing.T) {
	caps := &NodeCapabilities{
		GPU: &GPUCapabilities{
			Devices: []GPUDevice{{Name: "NVIDIA RTX 3090", Tag: "rtx3090", VRAMTag: "24gb"}},
			Count:   1,
		},
		Tags: []string{"gpu:rtx3090", "vram:24gb"},
	}
	q := GPUInferenceQueues(caps)
	found := false
	for _, name := range q {
		if name == "jobs:v1:gpu-general" {
			found = true
		}
	}
	if !found {
		t.Errorf("discrete GPU node did not join jobs:v1:gpu-general: %v", q)
	}
}

// TestGPUInferenceQueues_BrokenDriverNvidiaStillJoins pins that a discrete
// NVIDIA GPU whose drivers failed (display-only entry: empty Tag, Unified=false)
// still joins gpu-general — today's behavior. This is the row most likely to be
// wrongly "simplified" to gate on d.Tag != "" instead of !d.Unified, which would
// silently stop such a node consuming inference.
func TestGPUInferenceQueues_BrokenDriverNvidiaStillJoins(t *testing.T) {
	caps := &NodeCapabilities{
		GPU: &GPUCapabilities{
			Devices:      []GPUDevice{{Name: "NVIDIA RTX 3090"}}, // Tag/VRAMTag empty, Unified false
			Count:        1,
			DriverStatus: "not_loaded",
		},
		Tags: []string{"os:linux"},
	}
	q := GPUInferenceQueues(caps)
	found := false
	for _, name := range q {
		if name == "jobs:v1:gpu-general" {
			found = true
		}
	}
	if !found {
		t.Errorf("broken-driver NVIDIA node did not join jobs:v1:gpu-general: %v", q)
	}
}

// TestInferenceQueues_AppleSiliconServingStillGetsGpuGeneral pins that the
// Apple Silicon inference path (citadel-cli#606) is unchanged: a serving Mac
// still joins gpu-general via the serving branch, and a non-serving Mac joins
// nothing — exactly as before an apple GPU device was reported.
func TestInferenceQueues_AppleSiliconServingStillGetsGpuGeneral(t *testing.T) {
	caps := appleUnifiedCaps()

	serving := InferenceQueues(caps, true)
	if len(serving) != 1 || serving[0] != "jobs:v1:gpu-general" {
		t.Errorf("serving Mac queues = %v, want [jobs:v1:gpu-general]", serving)
	}

	if idle := InferenceQueues(caps, false); idle != nil {
		t.Errorf("non-serving Mac queues = %v, want nil", idle)
	}
}
