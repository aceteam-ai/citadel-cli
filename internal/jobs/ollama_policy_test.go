package jobs

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aceteam-ai/citadel-cli/services"
)

// TestOllamaMaxLoadedModelsFromFile pins the #1209 best-effort manifest reader
// the APPLY_DEVICE_CONFIG compose-up (config_handler.go) uses to thread an
// operator's ollama_max_loaded_models: policy into OLLAMA_MAX_LOADED_MODELS,
// plus the ollamaMaxLoadedModelsEnvForService flattening. Hermetic: it reads a
// manifest written into t.TempDir(), never network.GetNodeConfigDir().
func TestOllamaMaxLoadedModelsFromFile(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "citadel.yaml")
	manifest := `node:
  name: n
services:
  - name: ollama
    compose_file: ./services/ollama.yml
    ollama_max_loaded_models: 1
  - name: vllm
    compose_file: ./services/vllm.yml
`
	if err := os.WriteFile(manifestPath, []byte(manifest), 0600); err != nil {
		t.Fatalf("seed manifest: %v", err)
	}

	if got := ollamaMaxLoadedModelsFromFile(manifestPath, "ollama"); got == nil || *got != 1 {
		t.Errorf("ollama policy = %v, want 1", got)
	}
	if got := ollamaMaxLoadedModelsFromFile(manifestPath, "vllm"); got != nil {
		t.Errorf("vllm policy (unset) = %v, want nil", got)
	}
	if got := ollamaMaxLoadedModelsFromFile(manifestPath, "absent"); got != nil {
		t.Errorf("absent service policy = %v, want nil", got)
	}
	if got := ollamaMaxLoadedModelsFromFile(filepath.Join(dir, "nope.yaml"), "ollama"); got != nil {
		t.Errorf("policy from missing file = %v, want nil", got)
	}

	// ollamaMaxLoadedModelsEnvForService: set -> the injected entry; nil -> nil;
	// negative (invalid) -> nil (best-effort: engine default, not a start failure);
	// non-ollama service -> nil.
	one := 1
	if got := ollamaMaxLoadedModelsEnvForService("ollama", &one); len(got) != 1 || got[0] != services.EnvOllamaMaxLoadedModels+"=1" {
		t.Errorf("env for ollama=1 = %v, want [%s=1]", got, services.EnvOllamaMaxLoadedModels)
	}
	if got := ollamaMaxLoadedModelsEnvForService("ollama", nil); got != nil {
		t.Errorf("env for ollama nil = %v, want nil", got)
	}
	neg := -1
	if got := ollamaMaxLoadedModelsEnvForService("ollama", &neg); got != nil {
		t.Errorf("env for ollama=-1 = %v, want nil (best-effort skip)", got)
	}
	if got := ollamaMaxLoadedModelsEnvForService("vllm", &one); got != nil {
		t.Errorf("env for vllm=1 = %v, want nil (ollama-only)", got)
	}
}

// TestComposeEnvForEngineStart_InjectsOllamaPolicyOnlyWhenSet pins the
// SERVICE_START docker-branch env builder: a non-nil policy adds exactly one
// OLLAMA_MAX_LOADED_MODELS=<n> entry (last, so it wins over any inherited value),
// a nil policy adds none, and a negative value is a hard error the caller
// surfaces as a start failure. Hermetic: composeEnv() is os.Environ()+HostPortEnv().
func TestComposeEnvForEngineStart_InjectsOllamaPolicyOnlyWhenSet(t *testing.T) {
	h := &ServiceHandler{ConfigDir: t.TempDir()}
	countPrefix := func(env []string) int {
		n := 0
		for _, e := range env {
			if len(e) >= len(services.EnvOllamaMaxLoadedModels)+1 &&
				e[:len(services.EnvOllamaMaxLoadedModels)+1] == services.EnvOllamaMaxLoadedModels+"=" {
				n++
			}
		}
		return n
	}

	one := 1
	env, err := h.composeEnvForEngineStart("ollama", "", &one)
	if err != nil {
		t.Fatalf("set: unexpected error: %v", err)
	}
	if got := countPrefix(env); got != 1 {
		t.Errorf("set: want exactly 1 OLLAMA_MAX_LOADED_MODELS entry, got %d", got)
	}
	if env[len(env)-1] != services.EnvOllamaMaxLoadedModels+"=1" {
		t.Errorf("set: policy entry must be last (so it wins), got %q", env[len(env)-1])
	}

	envNil, err := h.composeEnvForEngineStart("ollama", "", nil)
	if err != nil {
		t.Fatalf("unset: unexpected error: %v", err)
	}
	if got := countPrefix(envNil); got != 0 {
		t.Errorf("unset: want 0 OLLAMA_MAX_LOADED_MODELS entries, got %d", got)
	}

	neg := -1
	if _, err := h.composeEnvForEngineStart("ollama", "", &neg); err == nil {
		t.Errorf("negative: want a hard error, got nil")
	}

	// A non-ollama engine never gets the policy entry even with a value set.
	envVLLM, err := h.composeEnvForEngineStart("vllm", "", &one)
	if err != nil {
		t.Fatalf("vllm: unexpected error: %v", err)
	}
	if got := countPrefix(envVLLM); got != 0 {
		t.Errorf("vllm: want 0 OLLAMA_MAX_LOADED_MODELS entries, got %d", got)
	}
}
