package compose

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestStripGPUDevices(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantGPU bool // whether output should contain GPU specifications
		wantErr bool
	}{
		{
			name: "strips nvidia gpu devices",
			input: `services:
  vllm:
    image: vllm/vllm-openai:latest
    ports:
      - "8000:8000"
    deploy:
      resources:
        reservations:
          devices:
            - driver: nvidia
              count: all
              capabilities: [gpu]
`,
			wantGPU: false,
			wantErr: false,
		},
		{
			name: "preserves other deploy config",
			input: `services:
  app:
    image: myapp:latest
    deploy:
      replicas: 3
      resources:
        reservations:
          devices:
            - driver: nvidia
              count: all
              capabilities: [gpu]
`,
			wantGPU: false,
			wantErr: false,
		},
		{
			name: "handles compose without gpu",
			input: `services:
  web:
    image: nginx:latest
    ports:
      - "80:80"
`,
			wantGPU: false,
			wantErr: false,
		},
		{
			name:    "handles invalid yaml",
			input:   `not: valid: yaml: here`,
			wantGPU: false,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := StripGPUDevices([]byte(tt.input))
			if (err != nil) != tt.wantErr {
				t.Errorf("StripGPUDevices() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr {
				return
			}

			output := string(got)
			hasGPU := strings.Contains(output, "driver: nvidia") ||
				strings.Contains(output, "capabilities:") ||
				strings.Contains(output, "devices:")

			if hasGPU != tt.wantGPU {
				t.Errorf("StripGPUDevices() GPU presence = %v, want %v\nOutput:\n%s", hasGPU, tt.wantGPU, output)
			}
		})
	}
}

func TestStripGPUDevices_PreservesOtherConfig(t *testing.T) {
	input := `services:
  vllm:
    image: vllm/vllm-openai:latest
    container_name: citadel-vllm
    ports:
      - "8000:8000"
    volumes:
      - ~/citadel-cache/huggingface:/root/.cache/huggingface
    command: --host 0.0.0.0 --port 8000
    deploy:
      resources:
        reservations:
          devices:
            - driver: nvidia
              count: all
              capabilities: [gpu]
`

	got, err := StripGPUDevices([]byte(input))
	if err != nil {
		t.Fatalf("StripGPUDevices() error = %v", err)
	}

	output := string(got)

	// Should preserve these
	checks := []string{
		"vllm/vllm-openai:latest",
		"citadel-vllm",
		"8000:8000",
		"citadel-cache/huggingface",
		"--host 0.0.0.0 --port 8000",
	}

	for _, check := range checks {
		if !strings.Contains(output, check) {
			t.Errorf("StripGPUDevices() should preserve %q\nOutput:\n%s", check, output)
		}
	}

	// Should remove these
	removed := []string{
		"driver: nvidia",
		"capabilities:",
	}

	for _, check := range removed {
		if strings.Contains(output, check) {
			t.Errorf("StripGPUDevices() should remove %q\nOutput:\n%s", check, output)
		}
	}
}

func TestRewriteGPUDevicesForPodman(t *testing.T) {
	input := `services:
  vllm:
    image: vllm/vllm-openai:latest
    deploy:
      replicas: 1
      resources:
        reservations:
          devices:
            - driver: nvidia
              count: all
              capabilities: [gpu]
            - driver: example
              count: 1
              capabilities: [tpu]
  selected:
    image: selected
    devices:
      - /dev/dri:/dev/dri
    deploy:
      resources:
        reservations:
          devices:
            - driver: nvidia
              device_ids: ["1", "GPU-deadbeef"]
              capabilities: [gpu]
  shorthand:
    image: short
    gpus: 2
  request-list:
    image: listed
    gpus:
      - driver: nvidia
        device_ids: ["GPU-a", "GPU-b"]
  legacy:
    image: old
    runtime: nvidia
`

	got, err := RewriteGPUDevicesForPodman([]byte(input))
	if err != nil {
		t.Fatalf("RewriteGPUDevicesForPodman() error = %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(got, &doc); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	services := doc["services"].(map[string]any)
	assertDevices := func(service string, want []any) {
		t.Helper()
		svc := services[service].(map[string]any)
		if got := svc["devices"]; !reflect.DeepEqual(got, want) {
			t.Errorf("%s devices = %#v, want %#v", service, got, want)
		}
	}
	assertDevices("vllm", []any{"nvidia.com/gpu=all"})
	assertDevices("selected", []any{"/dev/dri:/dev/dri", "nvidia.com/gpu=1", "nvidia.com/gpu=GPU-deadbeef"})
	assertDevices("shorthand", []any{"nvidia.com/gpu=0", "nvidia.com/gpu=1"})
	assertDevices("request-list", []any{"nvidia.com/gpu=GPU-a", "nvidia.com/gpu=GPU-b"})
	assertDevices("legacy", []any{"nvidia.com/gpu=all"})

	vllm := services["vllm"].(map[string]any)
	deploy := vllm["deploy"].(map[string]any)
	if deploy["replicas"] != 1 {
		t.Errorf("deploy.replicas = %#v, want 1", deploy["replicas"])
	}
	reservations := deploy["resources"].(map[string]any)["reservations"].(map[string]any)
	remaining := reservations["devices"].([]any)
	if len(remaining) != 1 || remaining[0].(map[string]any)["driver"] != "example" {
		t.Errorf("non-GPU reservation was not preserved: %#v", remaining)
	}
	if _, ok := services["shorthand"].(map[string]any)["gpus"]; ok {
		t.Error("gpus shorthand survived CDI rewrite")
	}
	if _, ok := services["legacy"].(map[string]any)["runtime"]; ok {
		t.Error("legacy nvidia runtime survived CDI rewrite")
	}
}

func TestRewriteGPUDevicesForPodmanFailsClosedOnAmbiguousReservation(t *testing.T) {
	input := `services:
  app:
    image: app
    deploy:
      resources:
        reservations:
          devices:
            - driver: nvidia
              capabilities: [gpu]
`
	if _, err := RewriteGPUDevicesForPodman([]byte(input)); err == nil || !strings.Contains(err.Error(), "neither count nor device_ids") {
		t.Fatalf("expected an actionable refusal, got %v", err)
	}
}

func TestRewriteGPUDevicesForPodmanNoGPUIsByteIdentical(t *testing.T) {
	input := []byte("services:\n  web:\n    image: nginx\n")
	got, err := RewriteGPUDevicesForPodman(input)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, input) {
		t.Fatalf("GPU-free compose was rewritten:\n%s", got)
	}
}

func TestMaterializePodmanGPUComposeUsesPrivateTemporaryFile(t *testing.T) {
	original := filepath.Join(t.TempDir(), "service.yml")
	input := []byte("services:\n  app:\n    image: app\n    gpus: all\n")
	if err := os.WriteFile(original, input, 0o644); err != nil {
		t.Fatal(err)
	}
	actual, cleanup, err := MaterializePodmanGPUCompose(original)
	if err != nil {
		t.Fatal(err)
	}
	if actual == original {
		t.Fatal("GPU compose was not materialized")
	}
	if filepath.Dir(actual) != filepath.Dir(original) {
		t.Fatalf("temporary compose directory = %q, want %q", filepath.Dir(actual), filepath.Dir(original))
	}
	info, err := os.Stat(actual)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("temporary mode = %o, want 600", got)
	}
	if source, err := os.ReadFile(original); err != nil || !reflect.DeepEqual(source, input) {
		t.Fatalf("installed compose changed: %q, %v", source, err)
	}
	cleanup()
	if _, err := os.Stat(actual); !os.IsNotExist(err) {
		t.Fatalf("temporary file survived cleanup: %v", err)
	}
}

func TestRequiredLimitControllers(t *testing.T) {
	base := []byte(`services:
  app:
    image: app
    cpus: "1.5"
    pids_limit: 128
`)
	override := []byte(`services:
  app:
    mem_limit: 2g
`)
	got, err := RequiredLimitControllers(base, override)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"cpu", "memory", "pids"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("RequiredLimitControllers() = %v, want %v", got, want)
	}
}
