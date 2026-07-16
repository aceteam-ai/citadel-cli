package status

import (
	"testing"

	"github.com/aceteam-ai/citadel-cli/services"
)

func TestFirstComposeHostPort(t *testing.T) {
	cases := []struct {
		name    string
		compose string
		want    int
	}{
		{
			name: "simple mapping",
			compose: `services:
  vllm:
    image: vllm/vllm-openai:latest
    ports:
      - "8100:8000"
`,
			want: 8100,
		},
		{
			name: "ip-qualified mapping",
			compose: `services:
  svc:
    ports:
      - "127.0.0.1:9000:8000"
`,
			want: 9000,
		},
		{
			name: "no ports",
			compose: `services:
  svc:
    image: foo
`,
			want: 0,
		},
		{
			name:    "malformed yaml",
			compose: "::: not yaml",
			want:    0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := firstComposeHostPort(c.compose); got != c.want {
				t.Fatalf("got %d, want %d", got, c.want)
			}
		})
	}
}

// TestManagedEngineHostPort_VLLM confirms vLLM resolves to its citadel-owned
// published host port. vLLM's host publish is managed via
// ${CITADEL_VLLM_HOST_PORT} (services/ports.go), so the port comes from the
// registry rather than a literal in the compose file. This guards against the
// registry and the idle scraper (which scrapes the host port) drifting apart.
func TestManagedEngineHostPort_VLLM(t *testing.T) {
	if got := managedEngineHostPort("vllm"); got != services.VLLMHostPort {
		t.Fatalf("expected vllm host port %d from registry, got %d", services.VLLMHostPort, got)
	}
	if got := managedEngineHostPort("does-not-exist"); got != 0 {
		t.Fatalf("expected 0 for unknown engine, got %d", got)
	}
}

// TestManagedProbeEnginesSupersetOfIdleCapable guards against the drift where
// an engine added to the idle list is silently dropped from the heartbeat's
// managed-engine sweep (which now iterates managedProbeEngines, #529).
func TestManagedProbeEnginesSupersetOfIdleCapable(t *testing.T) {
	for _, name := range idleCapableEngines {
		if !engineInList(managedProbeEngines, name) {
			t.Errorf("idle-capable engine %q missing from managedProbeEngines; it would vanish from the heartbeat", name)
		}
	}
	// Every probed engine must be a type DiscoverModels understands, so the
	// discovery call in collectManagedEngineStatus can never hit the
	// "unsupported service type" branch.
	for _, name := range managedProbeEngines {
		if got := EngineTypeFromName(name); got != name {
			t.Errorf("EngineTypeFromName(%q) = %q; managedProbeEngines entries must map to themselves", name, got)
		}
	}
}

func TestIdleEngineType_MatchesImage(t *testing.T) {
	// A catalog slug that doesn't contain "vllm" in the name but whose image does.
	if got := idleEngineType("llm-server vllm/vllm-openai:latest"); got != "vllm" {
		t.Fatalf("expected vllm engine from image hint, got %q", got)
	}
	if got := idleEngineType("postgres postgres:16"); got != "" {
		t.Fatalf("expected empty engine for non-inference hint, got %q", got)
	}
}
