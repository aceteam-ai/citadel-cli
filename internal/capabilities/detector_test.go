package capabilities

import (
	"testing"
)

func TestValidateTag(t *testing.T) {
	tests := []struct {
		tag  string
		want bool
	}{
		{"gpu:rtx4090", true},
		{"llm:llama3", true},
		{"cpu:general", true},
		{"vram:24gb", true},
		{"gpu:a100-sxm", true},
		{"llm:mistral-7b.v0.1", true},
		{"", false},
		{"GPU:RTX4090", false},       // uppercase not allowed
		{"../etc/passwd", false},      // path traversal
		{" gpu:rtx4090", false},       // leading space
		{"gpu rtx4090", false},        // space in middle
	}
	for _, tt := range tests {
		t.Run(tt.tag, func(t *testing.T) {
			if got := ValidateTag(tt.tag); got != tt.want {
				t.Errorf("ValidateTag(%q) = %v, want %v", tt.tag, got, tt.want)
			}
		})
	}
}

func TestNormalizeGPUName(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{"NVIDIA GeForce RTX 4090", "rtx4090"},
		{"NVIDIA GeForce RTX 3080 Ti", "rtx3080ti"},
		{"Tesla A100", "a100"},
		{"NVIDIA A100-SXM4-80GB", "a100"},
		{"NVIDIA H100", "h100"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NormalizeGPUName(tt.name)
			if got != tt.want {
				t.Errorf("NormalizeGPUName(%q) = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}

func TestNormalizeVRAM(t *testing.T) {
	tests := []struct {
		mb   string
		want string
	}{
		{"24576", "24"},
		{"81920", "80"},
		{"16384", "16"},
		{"", ""},
		{"0", ""},
	}
	for _, tt := range tests {
		t.Run(tt.mb, func(t *testing.T) {
			got := NormalizeVRAM(tt.mb)
			if got != tt.want {
				t.Errorf("NormalizeVRAM(%q) = %q, want %q", tt.mb, got, tt.want)
			}
		})
	}
}

func TestNormalizeModelName(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{"llama3", "llama3"},
		{"mistral-7b", "mistral-7b"},
		{"codellama/CodeLlama-13b", "codellama-codellama-13b"},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NormalizeModelName(tt.name)
			if got != tt.want {
				t.Errorf("NormalizeModelName(%q) = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}

func TestParseTags(t *testing.T) {
	caps := ParseTags("gpu:rtx4090,llm:llama3,cpu:general")
	if len(caps) != 3 {
		t.Fatalf("expected 3 capabilities, got %d", len(caps))
	}
	if caps[0].Tag != "gpu:rtx4090" {
		t.Errorf("first tag = %q, want %q", caps[0].Tag, "gpu:rtx4090")
	}
	if caps[0].Category != "gpu" {
		t.Errorf("first category = %q, want %q", caps[0].Category, "gpu")
	}
}

func TestResolveQueues(t *testing.T) {
	caps := []Capability{
		{Tag: "gpu:rtx4090", Category: "gpu"},
		{Tag: "llm:llama3", Category: "llm"},
		{Tag: "cpu:general", Category: "cpu"},
	}
	queues := ResolveQueues(caps, "jobs:v1:gpu-general")
	// Should have gpu:rtx4090 queue, llm:llama3 queue, and base queue
	if len(queues) != 3 {
		t.Fatalf("expected 3 queues, got %d: %v", len(queues), queues)
	}
	expected := map[string]bool{
		"jobs:v1:tag:gpu:rtx4090": true,
		"jobs:v1:tag:llm:llama3":  true,
		"jobs:v1:gpu-general":     true,
	}
	for _, q := range queues {
		if !expected[q] {
			t.Errorf("unexpected queue: %q", q)
		}
	}
}

func TestTagsHelper(t *testing.T) {
	caps := []Capability{
		{Tag: "gpu:rtx4090"},
		{Tag: "llm:llama3"},
	}
	tags := Tags(caps)
	if len(tags) != 2 || tags[0] != "gpu:rtx4090" || tags[1] != "llm:llama3" {
		t.Errorf("Tags() = %v, want [gpu:rtx4090, llm:llama3]", tags)
	}
}

func TestGPUDevice(t *testing.T) {
	dev := GPUDevice{
		Name:    "NVIDIA GeForce RTX 3090",
		VRAMMb:  24576,
		Tag:     "rtx3090",
		VRAMTag: "24gb",
	}

	if dev.Name != "NVIDIA GeForce RTX 3090" {
		t.Errorf("Name = %q, want %q", dev.Name, "NVIDIA GeForce RTX 3090")
	}
	if dev.VRAMMb != 24576 {
		t.Errorf("VRAMMb = %d, want %d", dev.VRAMMb, 24576)
	}
	if dev.Tag != "rtx3090" {
		t.Errorf("Tag = %q, want %q", dev.Tag, "rtx3090")
	}
	if dev.VRAMTag != "24gb" {
		t.Errorf("VRAMTag = %q, want %q", dev.VRAMTag, "24gb")
	}
}

func TestGPUCapabilities(t *testing.T) {
	caps := GPUCapabilities{
		Devices: []GPUDevice{
			{Name: "RTX 3090", VRAMMb: 24576, Tag: "rtx3090", VRAMTag: "24gb"},
			{Name: "RTX 3090", VRAMMb: 24576, Tag: "rtx3090", VRAMTag: "24gb"},
		},
		Count: 2,
	}

	if caps.Count != 2 {
		t.Errorf("Count = %d, want 2", caps.Count)
	}
	if len(caps.Devices) != 2 {
		t.Errorf("len(Devices) = %d, want 2", len(caps.Devices))
	}
}

func TestNodeCapabilities(t *testing.T) {
	nodeCaps := NodeCapabilities{
		GPU: &GPUCapabilities{
			Devices: []GPUDevice{
				{Name: "RTX 4090", VRAMMb: 24576, Tag: "rtx4090", VRAMTag: "24gb"},
			},
			Count: 1,
		},
		Engines: []string{"vllm"},
		Tags:    []string{"gpu:rtx4090", "vram:24gb", "engine:vllm", "cpu:general"},
	}

	if nodeCaps.GPU == nil {
		t.Fatal("GPU should not be nil")
	}
	if nodeCaps.GPU.Count != 1 {
		t.Errorf("GPU.Count = %d, want 1", nodeCaps.GPU.Count)
	}
	if len(nodeCaps.Engines) != 1 || nodeCaps.Engines[0] != "vllm" {
		t.Errorf("Engines = %v, want [vllm]", nodeCaps.Engines)
	}
	if len(nodeCaps.Tags) != 4 {
		t.Errorf("len(Tags) = %d, want 4", len(nodeCaps.Tags))
	}
}

func TestDetectNodeCapabilities_NoGPU(t *testing.T) {
	// When nvidia-smi is not available, GPU should be nil but function should not panic
	caps := DetectNodeCapabilities()
	if caps == nil {
		t.Fatal("DetectNodeCapabilities should never return nil")
	}
	// Should always have cpu:general tag
	hasCPU := false
	for _, tag := range caps.Tags {
		if tag == "cpu:general" {
			hasCPU = true
			break
		}
	}
	if !hasCPU {
		t.Error("Tags should always contain cpu:general")
	}
}

func TestMatchEngines(t *testing.T) {
	tests := []struct {
		name       string
		containers []string
		want       []string
		notWant    []string
	}{
		{
			name:       "ollama container does not produce llamacpp",
			containers: []string{"ollama-server"},
			want:       []string{"ollama"},
			notWant:    []string{"llamacpp"},
		},
		{
			name:       "llama container without ollama produces llamacpp",
			containers: []string{"llama-cpp-server"},
			want:       []string{"llamacpp"},
			notWant:    []string{"ollama"},
		},
		{
			name:       "llamacpp keyword matches directly",
			containers: []string{"citadel-llamacpp"},
			want:       []string{"llamacpp"},
			notWant:    nil,
		},
		{
			name:       "multiple engines detected independently",
			containers: []string{"citadel-vllm", "citadel-ollama"},
			want:       []string{"vllm", "ollama"},
			notWant:    []string{"llamacpp"},
		},
		{
			name:       "empty input returns nil",
			containers: []string{""},
			want:       nil,
			notWant:    nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchEngines(tt.containers)

			gotSet := make(map[string]bool)
			for _, e := range got {
				gotSet[e] = true
			}

			for _, w := range tt.want {
				if !gotSet[w] {
					t.Errorf("matchEngines(%v) missing expected engine %q, got %v", tt.containers, w, got)
				}
			}
			for _, nw := range tt.notWant {
				if gotSet[nw] {
					t.Errorf("matchEngines(%v) should NOT contain %q, got %v", tt.containers, nw, got)
				}
			}
		})
	}
}

func TestResolveQueuesWithEngines(t *testing.T) {
	caps := []Capability{
		{Tag: "gpu:rtx3090", Category: "gpu"},
		{Tag: "vram:24gb", Category: "vram"},
		{Tag: "engine:vllm", Category: "engine"},
		{Tag: "cpu:general", Category: "cpu"},
	}
	queues := ResolveQueues(caps, "jobs:v1:gpu-general")

	// Should have gpu:rtx3090 queue, vram:24gb queue, engine:vllm queue, and base queue
	// cpu:general is excluded (handled by base queue)
	expected := map[string]bool{
		"jobs:v1:tag:gpu:rtx3090":  true,
		"jobs:v1:tag:vram:24gb":    true,
		"jobs:v1:tag:engine:vllm":  true,
		"jobs:v1:gpu-general":      true,
	}
	if len(queues) != len(expected) {
		t.Fatalf("expected %d queues, got %d: %v", len(expected), len(queues), queues)
	}
	for _, q := range queues {
		if !expected[q] {
			t.Errorf("unexpected queue: %q", q)
		}
	}
}
