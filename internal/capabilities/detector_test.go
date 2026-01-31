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
