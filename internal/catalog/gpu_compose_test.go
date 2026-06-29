package catalog

import "testing"

func TestServiceRequestsGPU(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want bool
	}{
		{
			name: "no gpu signal",
			yaml: "services:\n  svc:\n    image: nginx:latest\n",
			want: false,
		},
		{
			name: "gpus shorthand all",
			yaml: "services:\n  svc:\n    image: x\n    gpus: all\n",
			want: true,
		},
		{
			name: "runtime nvidia",
			yaml: "services:\n  svc:\n    image: x\n    runtime: nvidia\n",
			want: true,
		},
		{
			name: "runtime nvidia case-insensitive",
			yaml: "services:\n  svc:\n    image: x\n    runtime: NVIDIA\n",
			want: true,
		},
		{
			name: "runtime non-nvidia is not gpu",
			yaml: "services:\n  svc:\n    image: x\n    runtime: runc\n",
			want: false,
		},
		{
			name: "deploy reservation driver nvidia",
			yaml: `services:
  svc:
    image: x
    deploy:
      resources:
        reservations:
          devices:
            - driver: nvidia
              count: all
`,
			want: true,
		},
		{
			name: "deploy reservation gpu capability without driver",
			yaml: `services:
  svc:
    image: x
    deploy:
      resources:
        reservations:
          devices:
            - capabilities: [gpu]
`,
			want: true,
		},
		{
			name: "deploy without gpu device is not gpu",
			yaml: `services:
  svc:
    image: x
    deploy:
      replicas: 3
      resources:
        reservations:
          cpus: "2"
`,
			want: false,
		},
		{
			name: "deploy reservation non-gpu device is not gpu",
			yaml: `services:
  svc:
    image: x
    deploy:
      resources:
        reservations:
          devices:
            - driver: foo
              capabilities: [tpu]
`,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svcs, err := decodeComposeServices(tt.yaml)
			if err != nil {
				t.Fatalf("decode error: %v", err)
			}
			svc, ok := svcs["svc"]
			if !ok {
				t.Fatalf("test compose missing service 'svc'")
			}
			if got := serviceRequestsGPU(svc); got != tt.want {
				t.Errorf("serviceRequestsGPU = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestServiceRequestsGPU_NilSafe(t *testing.T) {
	if serviceRequestsGPU(nil) {
		t.Error("nil service must not be reported as GPU")
	}
}

func TestDecodeComposeServices(t *testing.T) {
	// No services section -> empty map, no error.
	svcs, err := decodeComposeServices("name: not-a-compose\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(svcs) != 0 {
		t.Errorf("expected empty services, got %v", svcs)
	}

	// Malformed YAML -> error.
	if _, err := decodeComposeServices("services:\n  - a\n  b"); err == nil {
		t.Error("expected an error for malformed YAML")
	}
}
