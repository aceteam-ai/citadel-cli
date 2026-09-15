package cmd

import (
	"strings"
	"testing"

	svcports "github.com/aceteam-ai/citadel-cli/services"
)

// TestEngineExposedOnAllInterfaces exercises the pure exposure decision against
// the REAL embedded compose templates (aceteam-ai/citadel-cli#1023), so it stays
// honest to what the compose files actually publish.
func TestEngineExposedOnAllInterfaces(t *testing.T) {
	tests := []struct {
		name    string
		service string
		bindEnv map[string]string
		want    bool
	}{
		{"vllm default is loopback", "vllm", nil, false},
		{"vllm bind:all is exposed", "vllm", map[string]string{svcports.EnvVLLMBind: svcports.AllInterfacesBindAddr}, true},
		{"ollama default is exposed", "ollama", nil, true},
		{"ollama bind:loopback is not exposed", "ollama", map[string]string{svcports.EnvOllamaBind: svcports.LoopbackBindAddr}, false},
		{"kokoro is always loopback (no hatch)", "kokoro", nil, false},
		{"non-embedded service is never flagged", "whatsapp-bridge", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content := svcports.ServiceMap[tt.service] // "" for the non-embedded case
			got := engineExposedOnAllInterfaces(tt.service, content, tt.bindEnv)
			if got != tt.want {
				t.Errorf("engineExposedOnAllInterfaces(%q) = %v, want %v", tt.service, got, tt.want)
			}
			warning := engineBindExposureWarning(tt.service, content, tt.bindEnv)
			if tt.want && warning == "" {
				t.Errorf("expected a non-empty exposure warning for %q", tt.service)
			}
			if !tt.want && warning != "" {
				t.Errorf("expected NO exposure warning for %q, got %q", tt.service, warning)
			}
			if tt.want && !strings.Contains(warning, tt.service) {
				t.Errorf("exposure warning must name the service %q; got %q", tt.service, warning)
			}
		})
	}
}

// TestServiceBindEnvMap pins the cmd-side resolution wrapper: unset -> nil,
// bind:all -> the injected 0.0.0.0 entry, unrecognized -> error.
func TestServiceBindEnvMap(t *testing.T) {
	if m, err := serviceBindEnvMap("vllm", ""); err != nil || m != nil {
		t.Errorf("serviceBindEnvMap(vllm, \"\") = (%v,%v), want (nil,nil)", m, err)
	}
	m, err := serviceBindEnvMap("vllm", "all")
	if err != nil || m[svcports.EnvVLLMBind] != svcports.AllInterfacesBindAddr {
		t.Errorf("serviceBindEnvMap(vllm, all) = (%v,%v), want {%s:0.0.0.0}", m, err, svcports.EnvVLLMBind)
	}
	if _, err := serviceBindEnvMap("vllm", "bogus"); err == nil {
		t.Error("serviceBindEnvMap(vllm, bogus) should error")
	}
	// Non-hatch service: never injects, never errors.
	if m, err := serviceBindEnvMap("kokoro", "all"); err != nil || m != nil {
		t.Errorf("serviceBindEnvMap(kokoro, all) = (%v,%v), want (nil,nil)", m, err)
	}
}

// TestBindEnvEntries pins the map->[]string flattening the compose-up sites use.
func TestBindEnvEntries(t *testing.T) {
	if got := bindEnvEntries(nil); got != nil {
		t.Errorf("bindEnvEntries(nil) = %v, want nil", got)
	}
	got := bindEnvEntries(map[string]string{svcports.EnvVLLMBind: "0.0.0.0"})
	if len(got) != 1 || got[0] != svcports.EnvVLLMBind+"=0.0.0.0" {
		t.Errorf("bindEnvEntries = %v, want [%s=0.0.0.0]", got, svcports.EnvVLLMBind)
	}
}
