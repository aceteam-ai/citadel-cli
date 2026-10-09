package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/status"
	"github.com/aceteam-ai/citadel-cli/services"
)

// TestOllamaMaxLoadedModelsFromFile pins the #1209 best-effort manifest reader
// the APPLY_DEVICE_CONFIG compose-up (config_handler.go) uses to thread an
// operator's ollama_max_loaded_models policy into the shared compose-env
// normalizer. Hermetic: it reads a manifest written into t.TempDir(), never
// network.GetNodeConfigDir().
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
}

// TestComposeEnvForAppliedServiceNormalizesPolicy pins the fourth production
// compose-up path without executing a container runtime. Missing/unreadable and
// invalid-best-effort manifests must still shield the process from inherited
// policy, while unrelated variables and non-Ollama services remain unchanged.
func TestComposeEnvForAppliedServiceNormalizesPolicy(t *testing.T) {
	dir := t.TempDir()
	validPath := filepath.Join(dir, "valid.yaml")
	negativePath := filepath.Join(dir, "negative.yaml")
	invalidPath := filepath.Join(dir, "invalid.yaml")
	if err := os.WriteFile(validPath, []byte("services:\n  - name: ollama\n    ollama_max_loaded_models: 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(negativePath, []byte("services:\n  - name: ollama\n    ollama_max_loaded_models: -1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(invalidPath, []byte("services: [\n"), 0600); err != nil {
		t.Fatal(err)
	}

	base := []string{"UNRELATED=keep", services.EnvOllamaMaxLoadedModels + "=73"}
	cases := []struct {
		name      string
		manifest  string
		service   string
		want      []string
		wantError bool
	}{
		{"valid", validPath, "ollama", []string{"UNRELATED=keep", services.EnvOllamaMaxLoadedModels + "=1"}, false},
		{"missing", filepath.Join(dir, "missing.yaml"), "ollama", []string{"UNRELATED=keep", services.EnvOllamaMaxLoadedModels + "="}, false},
		{"invalid yaml", invalidPath, "ollama", []string{"UNRELATED=keep", services.EnvOllamaMaxLoadedModels + "="}, false},
		{"negative", negativePath, "ollama", []string{"UNRELATED=keep", services.EnvOllamaMaxLoadedModels + "="}, true},
		{"non ollama unchanged", validPath, "vllm", base, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := composeEnvForAppliedService(base, tc.manifest, tc.service)
			if (err != nil) != tc.wantError {
				t.Fatalf("error = %v, wantError %v", err, tc.wantError)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("env = %#v, want %#v", got, tc.want)
			}
		})
	}
}

// TestComposeEnvForEngineStart_InjectsOllamaPolicyOnlyWhenSet pins the
// SERVICE_START docker-branch env builder: a non-nil policy adds exactly one
// OLLAMA_MAX_LOADED_MODELS=<n> entry (last, so it wins over any inherited value),
// a nil policy adds explicit empty, and a negative value is a hard error the caller
// surfaces as a start failure. Hermetic: composeEnv() is os.Environ()+HostPortEnv().
func TestComposeEnvForEngineStart_InjectsOllamaPolicyOnlyWhenSet(t *testing.T) {
	h := &ServiceHandler{ConfigDir: t.TempDir()}
	t.Setenv("OLLAMA_MAX_LOADED_MODELS", "73")
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
	if got := countPrefix(envNil); got != 1 {
		t.Errorf("unset: want exactly 1 OLLAMA_MAX_LOADED_MODELS entry, got %d", got)
	}
	if envNil[len(envNil)-1] != "OLLAMA_MAX_LOADED_MODELS=" {
		t.Errorf("unset: policy entry must be explicit empty and last, got %q", envNil[len(envNil)-1])
	}

	neg := -1
	if _, err := h.composeEnvForEngineStart("ollama", "", &neg); err == nil {
		t.Errorf("negative: want a hard error, got nil")
	}

	// A non-ollama engine remains byte-for-byte outside this policy authority;
	// the hostile inherited value therefore remains present and unchanged.
	envVLLM, err := h.composeEnvForEngineStart("vllm", "", &one)
	if err != nil {
		t.Fatalf("vllm: unexpected error: %v", err)
	}
	if got := countPrefix(envVLLM); got != 1 {
		t.Errorf("vllm: want inherited OLLAMA_MAX_LOADED_MODELS unchanged, got %d entries", got)
	}
	if !slices.Contains(envVLLM, services.EnvOllamaMaxLoadedModels+"=73") {
		t.Errorf("vllm: inherited policy value was modified: %v", envVLLM)
	}
}

// TestServiceStartRejectsNegativeOllamaPolicyBeforeEngineSideEffects exercises
// the actual strict docker branch. Execute intentionally records desired-state
// intent before serviceStart; this direct test pins that serviceStart itself
// performs no engine/model/trust/resource/cleanup work for an invalid policy.
func TestServiceStartRejectsNegativeOllamaPolicyBeforeEngineSideEffects(t *testing.T) {
	configDir := t.TempDir()
	fakePath := t.TempDir()
	t.Setenv("PATH", fakePath)

	probeCalls := 0
	h := &ServiceHandler{
		ConfigDir: configDir,
		dockerServiceRunningFn: func(string) bool {
			probeCalls++
			return false
		},
		externalEngineServingFn: func(_ context.Context, _ string, _ int) ([]string, bool) {
			probeCalls++
			return nil, false
		},
		collectStatus: func() (*status.NodeStatus, error) {
			probeCalls++
			return nil, errors.New("unexpected status collection")
		},
	}
	logCalls := 0
	ctx := JobContext{LogFn: func(string, string) { logCalls++ }}
	negative := -1
	svc := manifestService{
		Name:                  "ollama",
		Type:                  "docker",
		ComposeFile:           "./services/ollama.yml",
		OllamaMaxLoadedModels: &negative,
	}

	before, err := os.ReadDir(configDir)
	if err != nil {
		t.Fatal(err)
	}
	out, err := h.serviceStart(ctx, svc, "hostile/model", 1, 1, trustRemoteCodeEnable)
	if err != nil {
		t.Fatalf("serviceStart changed its serviceResult error contract: %v", err)
	}
	var result serviceResult
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("decode result: %v (%s)", err, out)
	}
	if result.Name != "ollama" || result.Kind != "docker" || result.Action != "start" || result.Running ||
		!strings.Contains(result.Error, "invalid ollama_max_loaded_models -1") {
		t.Fatalf("unexpected strict-error result: %+v", result)
	}
	if probeCalls != 0 {
		t.Fatalf("invalid policy reached engine/runtime/resource probe seams: %d calls", probeCalls)
	}
	if logCalls != 0 {
		t.Fatalf("invalid policy reached model/trust/adoption paths: %d log calls", logCalls)
	}
	after, err := os.ReadDir(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("invalid policy mutated config dir: before=%v after=%v", before, after)
	}
}
