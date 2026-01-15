package compose

import (
	"strings"
	"testing"
)

func TestStripGPUDevices(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantGPU  bool // whether output should contain GPU specifications
		wantErr  bool
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
			name: "handles invalid yaml",
			input: `not: valid: yaml: here`,
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
