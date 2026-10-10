package services

import (
	"reflect"
	"testing"
)

// TestTEICUDATagForComputeCap pins the compute-cap -> CUDA tag mapping against
// the TEI v1.6 README Docker table (see tei.go header). The sm86 case is the
// node-1795 / RTX 3090 verified data point (aceteam-ai/citadel-cli#1269).
func TestTEICUDATagForComputeCap(t *testing.T) {
	cases := map[string]string{
		"7.5": "turing-1.6", // Turing
		"8.0": "1.6",        // Ampere 80 (bare version tag)
		"8.6": "86-1.6",     // Ampere 86 (RTX 3090 — verified)
		"8.9": "89-1.6",     // Ada Lovelace
		"9.0": "hopper-1.6", // Hopper
		// Unmapped capabilities fall back to the CPU image (empty tag).
		"7.0":  "", // Volta — no published 1.6 CUDA image
		"10.0": "", // Blackwell — not on the 1.6 pin
		"":     "",
		"junk": "",
	}
	for cap, want := range cases {
		if got := TEICUDATagForComputeCap(cap); got != want {
			t.Errorf("TEICUDATagForComputeCap(%q) = %q, want %q", cap, got, want)
		}
	}
	// Surrounding whitespace (nvidia-smi CSV rows can carry it) is trimmed.
	if got := TEICUDATagForComputeCap("  8.6 "); got != "86-1.6" {
		t.Errorf("TEICUDATagForComputeCap with whitespace = %q, want \"86-1.6\"", got)
	}
}

// TestResolveTEIImageTag verifies GPU 0 is authoritative and an unmapped /
// absent GPU 0 falls back to the CPU image.
func TestResolveTEIImageTag(t *testing.T) {
	cases := []struct {
		name  string
		caps  []string
		tag   string
		isGPU bool
	}{
		{"rtx3090", []string{"8.6"}, "86-1.6", true},
		{"a100", []string{"8.0"}, "1.6", true},
		{"ada", []string{"8.9"}, "89-1.6", true},
		{"no-gpu", nil, "", false},
		{"empty-cap", []string{""}, "", false},
		{"unmapped-gpu0", []string{"7.0"}, "", false},
		// GPU 0 is authoritative: an unmapped device 0 falls back even if a later
		// device would map (TEI binds device 0; a 86 kernel can't run on Volta).
		{"gpu0-unmapped-gpu1-mapped", []string{"7.0", "8.6"}, "", false},
		{"homogeneous-multi", []string{"8.6", "8.6"}, "86-1.6", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tag, gpu := ResolveTEIImageTag(tc.caps)
			if tag != tc.tag || gpu != tc.isGPU {
				t.Errorf("ResolveTEIImageTag(%v) = (%q,%v), want (%q,%v)", tc.caps, tag, gpu, tc.tag, tc.isGPU)
			}
		})
	}
}

// TestTEICPUThreads pins the CPU thread-count default: cores minus a small
// headroom, floored at 1.
func TestTEICPUThreads(t *testing.T) {
	cases := map[int]int{
		0:  1,  // nonsensical input floors to 1
		1:  1,  // one core: can't reserve headroom, floor to 1
		2:  1,  // 2 - 1 headroom
		8:  7,  // 8 - 1
		32: 31, // 32 - 1
	}
	for numCPU, want := range cases {
		if got := TEICPUThreads(numCPU); got != want {
			t.Errorf("TEICPUThreads(%d) = %d, want %d", numCPU, got, want)
		}
	}
}

// TestTEIComposeEnv is the injection-at-compose-up contract: the GPU image tag
// is injected only when a usable GPU resolves ON a GPU-capable runtime; the CPU
// thread count only when the operator opted in (threads > 0); nothing otherwise;
// and nothing at all for a non-tei service.
func TestTEIComposeEnv(t *testing.T) {
	cases := []struct {
		name               string
		service            string
		caps               []string
		runtimeProvidesGPU bool
		threads            int
		want               []string
	}{
		{
			name: "gpu-docker-injects-tag", service: "tei",
			caps: []string{"8.6"}, runtimeProvidesGPU: true, threads: 0,
			want: []string{"CITADEL_TEI_IMAGE_TAG=86-1.6"},
		},
		{
			// GPU present but runtime (Podman) can't hand it to the reservation-less
			// tei.yml: fall to CPU, no tag injected.
			name: "gpu-podman-no-tag", service: "tei",
			caps: []string{"8.6"}, runtimeProvidesGPU: false, threads: 0,
			want: nil,
		},
		{
			// CPU node, operator did NOT opt into threads: inject nothing (compose
			// default 1 applies, byte-compatible).
			name: "cpu-no-threads-opt-in", service: "tei",
			caps: nil, runtimeProvidesGPU: true, threads: 0,
			want: nil,
		},
		{
			name: "cpu-threads-opt-in", service: "tei",
			caps: nil, runtimeProvidesGPU: true, threads: 7,
			want: []string{"CITADEL_TEI_NUM_THREADS=7"},
		},
		{
			// GPU wins over a threads opt-in: on the GPU image threads are moot, so
			// only the tag is injected.
			name: "gpu-ignores-threads", service: "tei",
			caps: []string{"8.6"}, runtimeProvidesGPU: true, threads: 7,
			want: []string{"CITADEL_TEI_IMAGE_TAG=86-1.6"},
		},
		{
			name: "non-tei-service", service: "vllm",
			caps: []string{"8.6"}, runtimeProvidesGPU: true, threads: 7,
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := TEIComposeEnv(tc.service, tc.caps, tc.runtimeProvidesGPU, tc.threads)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("TEIComposeEnv(%q,%v,%v,%d) = %v, want %v", tc.service, tc.caps, tc.runtimeProvidesGPU, tc.threads, got, tc.want)
			}
		})
	}
}

// TestParseTEIServingImage pins the reporting-path parse of a resolved container
// image ref into (tag, device).
func TestParseTEIServingImage(t *testing.T) {
	cases := []struct {
		image, tag, device string
	}{
		{"ghcr.io/huggingface/text-embeddings-inference:86-1.6", "86-1.6", "cuda"},
		{"ghcr.io/huggingface/text-embeddings-inference:cpu-1.6", "cpu-1.6", "cpu"},
		{"ghcr.io/huggingface/text-embeddings-inference:1.6", "1.6", "cuda"},
		{"ghcr.io/huggingface/text-embeddings-inference:hopper-1.6", "hopper-1.6", "cuda"},
		// Not a TEI image.
		{"ghcr.io/aceteam-ai/kokoro-service:latest", "", ""},
		{"", "", ""},
		// TEI repo with no tag component.
		{"ghcr.io/huggingface/text-embeddings-inference", "", ""},
	}
	for _, tc := range cases {
		tag, device := ParseTEIServingImage(tc.image)
		if tag != tc.tag || device != tc.device {
			t.Errorf("ParseTEIServingImage(%q) = (%q,%q), want (%q,%q)", tc.image, tag, device, tc.tag, tc.device)
		}
	}
}
