package catalog

import (
	"path/filepath"
	"strings"
	"testing"
)

const gpuComposeYAML = `services:
  vllm:
    image: vllm/vllm-openai:latest
    deploy:
      resources:
        reservations:
          devices:
            - driver: nvidia
              capabilities: [gpu]
`

const nonGPUComposeYAML = `services:
  redis:
    image: redis:7
`

const gpuComposeAlreadyLimitedYAML = `services:
  vllm:
    image: vllm/vllm-openai:latest
    mem_limit: 4g
    deploy:
      resources:
        reservations:
          devices:
            - driver: nvidia
              capabilities: [gpu]
`

func TestGenerateGPUMemoryOverride_AppliesToGPUService(t *testing.T) {
	yml, err := GenerateGPUMemoryOverride(gpuComposeYAML, 20*1024*1024*1024, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	svcs := parseOverride(t, yml)
	vllm, ok := svcs["vllm"]
	if !ok {
		t.Fatalf("expected vllm in override, got %+v", svcs)
	}
	got, ok := vllm["mem_limit"].(string)
	if !ok {
		t.Fatalf("expected mem_limit string, got %T (%v)", vllm["mem_limit"], vllm["mem_limit"])
	}
	if want := "21474836480b"; got != want { // 20 GiB in bytes
		t.Errorf("mem_limit = %q, want %q", got, want)
	}
	// Narrower than the Tier-2 hardening override: no cap_drop/read_only/cpus.
	for _, forbidden := range []string{"cap_drop", "read_only", "cpus", "security_opt", "tmpfs"} {
		if _, set := vllm[forbidden]; set {
			t.Errorf("GPU RAM override must not set %q (that's GenerateHardeningOverride's job, and GPU services are exempt from it)", forbidden)
		}
	}
}

func TestGenerateGPUMemoryOverride_SkipsNonGPUService(t *testing.T) {
	yml, err := GenerateGPUMemoryOverride(nonGPUComposeYAML, 20*1024*1024*1024, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if yml != "" {
		t.Fatalf("expected empty override for a non-GPU compose, got:\n%s", yml)
	}
}

func TestGenerateGPUMemoryOverride_SkipsWhenHostHasNoGPU(t *testing.T) {
	// Fail-safe direction mirrors GenerateHardeningOverride: a compose-declared
	// GPU signal is author-controlled, so it must not exempt a service from
	// scrutiny on a host that turns out to have no GPU. Here that means: no
	// override is generated at all (there's nothing to protect against on a
	// GPU-less host via THIS mechanism).
	yml, err := GenerateGPUMemoryOverride(gpuComposeYAML, 20*1024*1024*1024, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if yml != "" {
		t.Fatalf("expected empty override when hostHasGPU=false, got:\n%s", yml)
	}
}

func TestGenerateGPUMemoryOverride_InjectOnlyWhereAbsent(t *testing.T) {
	// A service that already declares its own mem_limit is left alone.
	yml, err := GenerateGPUMemoryOverride(gpuComposeAlreadyLimitedYAML, 20*1024*1024*1024, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if yml != "" {
		t.Fatalf("expected empty override when base already declares mem_limit, got:\n%s", yml)
	}
}

func TestGenerateGPUMemoryOverride_RejectsNonPositiveLimit(t *testing.T) {
	if _, err := GenerateGPUMemoryOverride(gpuComposeYAML, 0, true); err == nil {
		t.Fatal("expected an error for memLimitBytes<=0")
	}
	if _, err := GenerateGPUMemoryOverride(gpuComposeYAML, -1, true); err == nil {
		t.Fatal("expected an error for negative memLimitBytes")
	}
}

func TestGenerateGPUMemoryOverride_RejectsEmptyCompose(t *testing.T) {
	if _, err := GenerateGPUMemoryOverride("services: {}", 1024, true); err == nil {
		t.Fatal("expected an error for a compose declaring no services")
	}
}

func TestGPURAMOverridePath_MirrorsSandboxNamingConvention(t *testing.T) {
	got := GPURAMOverridePath("/svc", "vllm")
	want := filepath.Join("/svc", "vllm.ram.yml")
	if got != want {
		t.Errorf("GPURAMOverridePath = %q, want %q", got, want)
	}
	// Distinct filename from the Tier-2 sandbox override -- the two can coexist
	// as separate `-f` files with no key collision.
	if got == SandboxOverridePath("/svc", "vllm") {
		t.Errorf("GPU RAM override path must differ from the sandbox override path")
	}
}

func TestExistingGPURAMOverride(t *testing.T) {
	dir := t.TempDir()
	if got := ExistingGPURAMOverride(dir, "vllm"); got != "" {
		t.Fatalf("expected \"\" when no override file exists, got %q", got)
	}
	path := GPURAMOverridePath(dir, "vllm")
	if err := writeFileTest(t, path, "services: {}"); err != nil {
		t.Fatalf("write override: %v", err)
	}
	if got := ExistingGPURAMOverride(dir, "vllm"); got != path {
		t.Fatalf("ExistingGPURAMOverride = %q, want %q", got, path)
	}
}

func TestComposeDeclaresGPU(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want bool
	}{
		{"gpu deploy reservation", gpuComposeYAML, true},
		{"non-gpu", nonGPUComposeYAML, false},
		{"gpus shorthand", "services:\n  x:\n    gpus: all\n", true},
		{"runtime nvidia", "services:\n  x:\n    runtime: nvidia\n", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ComposeDeclaresGPU(c.yaml)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Errorf("ComposeDeclaresGPU(%s) = %v, want %v", c.name, got, c.want)
			}
		})
	}
}

func TestComposeDeclaresGPU_MalformedYAML(t *testing.T) {
	if _, err := ComposeDeclaresGPU("services: [this is not a map"); err == nil {
		t.Fatal("expected an error for malformed YAML")
	}
	if !strings.Contains("services: [this is not a map", "services") {
		t.Fatal("sanity check on test fixture failed")
	}
}
