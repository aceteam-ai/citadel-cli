// cmd/darwin_engines_test.go
package cmd

import "testing"

func TestSkipEngineOnDarwin(t *testing.T) {
	cases := []struct {
		name    string
		goos    string
		service string
		force   bool
		want    bool
	}{
		// NVIDIA/docker-only engines are skipped on darwin.
		{"vllm on darwin", "darwin", "vllm", false, true},
		{"sglang on darwin", "darwin", "sglang", false, true},
		{"llamacpp on darwin", "darwin", "llamacpp", false, true},
		{"case-insensitive name", "darwin", "vLLM", false, true},
		{"whitespace tolerated", "darwin", "  vllm  ", false, true},

		// Non-GPU engines are never skipped, even on darwin.
		{"ollama on darwin", "darwin", "ollama", false, false},
		{"lmstudio on darwin", "darwin", "lmstudio", false, false},
		{"unknown service on darwin", "darwin", "something-else", false, false},

		// Other platforms always start these engines.
		{"vllm on linux", "linux", "vllm", false, false},
		{"llamacpp on linux", "linux", "llamacpp", false, false},
		{"vllm on windows", "windows", "vllm", false, false},

		// The force escape hatch overrides the darwin skip.
		{"vllm on darwin forced", "darwin", "vllm", true, false},
		{"llamacpp on darwin forced", "darwin", "llamacpp", true, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := skipEngineOnDarwin(tc.goos, tc.service, tc.force)
			if got != tc.want {
				t.Errorf("skipEngineOnDarwin(%q, %q, %v) = %v, want %v",
					tc.goos, tc.service, tc.force, got, tc.want)
			}
		})
	}
}

func TestForceGPUEngines(t *testing.T) {
	cases := []struct {
		val  string
		want bool
	}{
		{"1", true},
		{"true", true},
		{"YES", true},
		{"On", true},
		{"", false},
		{"0", false},
		{"false", false},
		{"nonsense", false},
	}
	for _, tc := range cases {
		t.Run(tc.val, func(t *testing.T) {
			t.Setenv("CITADEL_FORCE_GPU_ENGINES", tc.val)
			if got := forceGPUEngines(); got != tc.want {
				t.Errorf("forceGPUEngines() with %q = %v, want %v", tc.val, got, tc.want)
			}
		})
	}
}
