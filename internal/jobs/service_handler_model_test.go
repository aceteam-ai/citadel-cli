// internal/jobs/service_handler_model_test.go
//
// SERVICE_START model selection (#530): the backend's model-deploy contract
// dispatches MODEL_CACHE_PULL (weights) then SERVICE_START {service, model}.
// These tests cover the serve half: the model is persisted to the sibling
// <name>.env next to the compose file (the file BOTH start paths — this
// handler and the cmd/ boot path via composeFileArgs — pass to compose with
// --env-file), engines with no serve-time model parameter ignore it
// gracefully, and a plain restart without a model still serves the persisted
// one.
package jobs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/compose"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	embeddedservices "github.com/aceteam-ai/citadel-cli/services"
)

// TestVLLMComposeParameterizesModel pins the contract on the embedded compose
// file: the served model must be interpolated from VLLM_MODEL with the
// pre-#530 model as the default, so nodes without a persisted selection keep
// serving exactly what they served before.
func TestVLLMComposeParameterizesModel(t *testing.T) {
	content, ok := embeddedservices.ServiceMap["vllm"]
	if !ok {
		t.Fatal("vllm missing from embedded ServiceMap")
	}
	if !strings.Contains(content, "${VLLM_MODEL:-Qwen/Qwen3-8B}") {
		t.Errorf("embedded vllm compose does not interpolate the model via ${VLLM_MODEL:-Qwen/Qwen3-8B}:\n%s", content)
	}
}

// newModelTestHandler writes a citadel.yaml declaring vllm+ollama (type:
// docker so resolveKind never probes native binaries) and materializes the
// embedded vllm compose file, returning the handler and its config dir.
func newModelTestHandler(t *testing.T) (*ServiceHandler, string) {
	t.Helper()
	dir := t.TempDir()
	manifest := `node:
  name: test-node
services:
  - name: vllm
    type: docker
    compose_file: services/vllm.yml
  - name: ollama
    type: docker
    compose_file: services/ollama.yml
`
	if err := os.WriteFile(filepath.Join(dir, "citadel.yaml"), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "services"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"vllm", "ollama"} {
		if err := os.WriteFile(filepath.Join(dir, "services", name+".yml"),
			[]byte(embeddedservices.ServiceMap[name]), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return NewServiceHandler(dir), dir
}

func TestPersistServiceModel(t *testing.T) {
	h, dir := newModelTestHandler(t)
	svc := manifestService{Name: "vllm", Type: "docker", ComposeFile: "services/vllm.yml"}
	envPath := filepath.Join(dir, "services", "vllm.env")

	// First persist: file created, VLLM_MODEL recorded, changed=true.
	envVar, changed, err := h.persistServiceModel(JobContext{}, svc, "Qwen/Qwen2.5-0.5B-Instruct")
	if err != nil {
		t.Fatalf("persistServiceModel: %v", err)
	}
	if envVar != "VLLM_MODEL" || !changed {
		t.Errorf("persist = (%q, %v), want (VLLM_MODEL, true)", envVar, changed)
	}
	if v, ok := compose.ReadEnvVar(envPath, "VLLM_MODEL"); !ok || v != "Qwen/Qwen2.5-0.5B-Instruct" {
		t.Errorf("persisted VLLM_MODEL = %q, %v", v, ok)
	}

	// The sibling env is what compose interpolation will read: the jobs-side up
	// (and the cmd/ boot path) must now receive --env-file for it.
	composePath, err := h.resolveComposePath(svc)
	if err != nil {
		t.Fatalf("resolveComposePath: %v", err)
	}
	if args := compose.EnvFileArgs(composePath); len(args) != 2 || args[0] != "--env-file" || args[1] != envPath {
		t.Errorf("EnvFileArgs = %v, want [--env-file %s]", args, envPath)
	}

	// Re-persisting the same model reports changed=false, so a re-dispatched
	// identical SERVICE_START does not force-recreate a running engine.
	if _, changed, err = h.persistServiceModel(JobContext{}, svc, "Qwen/Qwen2.5-0.5B-Instruct"); err != nil || changed {
		t.Errorf("identical re-persist = (changed=%v, err=%v), want (false, nil)", changed, err)
	}

	// A different model updates the value and reports changed=true.
	if _, changed, err = h.persistServiceModel(JobContext{}, svc, "Qwen/Qwen3-8B"); err != nil || !changed {
		t.Errorf("changed re-persist = (changed=%v, err=%v), want (true, nil)", changed, err)
	}
	if v, _ := compose.ReadEnvVar(envPath, "VLLM_MODEL"); v != "Qwen/Qwen3-8B" {
		t.Errorf("VLLM_MODEL after change = %q, want Qwen/Qwen3-8B", v)
	}
}

// TestPersistServiceModel_UnmappedEngine: ollama loads models on demand, so a
// model on SERVICE_START is ignored gracefully — no env var, no file, no error.
func TestPersistServiceModel_UnmappedEngine(t *testing.T) {
	h, dir := newModelTestHandler(t)
	svc := manifestService{Name: "ollama", Type: "docker", ComposeFile: "services/ollama.yml"}

	envVar, changed, err := h.persistServiceModel(JobContext{}, svc, "llama3.2")
	if err != nil {
		t.Fatalf("persistServiceModel(ollama): %v", err)
	}
	if envVar != "" || changed {
		t.Errorf("persist(ollama) = (%q, %v), want (\"\", false)", envVar, changed)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "services", "ollama.env")); statErr == nil {
		t.Error("an env file was created for an engine with no serve-time model parameter")
	}
}

// TestPersistServiceModel_InvalidModel: the model is written into a file
// compose parses, so identifiers that could corrupt it are rejected.
func TestPersistServiceModel_InvalidModel(t *testing.T) {
	h, dir := newModelTestHandler(t)
	svc := manifestService{Name: "vllm", Type: "docker", ComposeFile: "services/vllm.yml"}

	for _, bad := range []string{"a b", "a\nb", "$(rm -rf /)", "a\"b", "a#b", "-leading-dash", ""} {
		if _, _, err := h.persistServiceModel(JobContext{}, svc, bad); err == nil {
			t.Errorf("persistServiceModel accepted invalid model %q", bad)
		}
	}
	if _, statErr := os.Stat(filepath.Join(dir, "services", "vllm.env")); statErr == nil {
		t.Error("env file was created despite invalid models")
	}
}

// TestServiceStartModelFlow drives the full Execute path with PATH neutered
// (no docker binary reachable) so nothing real is launched: the compose up
// fails safely AFTER the persistence step, which is what we assert. It then
// re-drives a plain SERVICE_START without a model — the restart path — and
// verifies the previously persisted model is untouched and still fed to
// compose via --env-file.
func TestServiceStartModelFlow(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no docker, no native binaries
	h, dir := newModelTestHandler(t)
	envPath := filepath.Join(dir, "services", "vllm.env")

	// SERVICE_START with a model: persists it before attempting the up.
	out, err := h.Execute(JobContext{}, &nexus.Job{
		ID:   "job-model-1",
		Type: "SERVICE_START",
		Payload: map[string]string{
			"service": "vllm",
			"model":   "Qwen/Qwen2.5-0.5B-Instruct",
		},
	})
	if err != nil {
		t.Fatalf("Execute SERVICE_START with model: %v", err)
	}
	// The up itself must have failed (docker unreachable) — proving the test
	// never started anything — but the model is already durable.
	if !strings.Contains(string(out), "docker compose up failed") {
		t.Errorf("expected compose-up failure with neutered PATH, got: %s", out)
	}
	if v, ok := compose.ReadEnvVar(envPath, "VLLM_MODEL"); !ok || v != "Qwen/Qwen2.5-0.5B-Instruct" {
		t.Fatalf("model not persisted by SERVICE_START: %q, %v", v, ok)
	}

	// Restart path: a plain SERVICE_START without a model must leave the
	// persisted selection intact and still pass it to compose.
	if _, err := h.Execute(JobContext{}, &nexus.Job{
		ID:      "job-model-2",
		Type:    "SERVICE_START",
		Payload: map[string]string{"service": "vllm"},
	}); err != nil {
		t.Fatalf("Execute plain SERVICE_START: %v", err)
	}
	if v, _ := compose.ReadEnvVar(envPath, "VLLM_MODEL"); v != "Qwen/Qwen2.5-0.5B-Instruct" {
		t.Errorf("plain restart clobbered persisted model: %q", v)
	}
	composePath := filepath.Join(dir, "services", "vllm.yml")
	if args := compose.EnvFileArgs(composePath); len(args) != 2 {
		t.Errorf("EnvFileArgs after restart = %v, want --env-file pair", args)
	}

	// Without any model ever set, no env file exists, so nothing is injected
	// and the compose default (:-) applies.
	if args := compose.EnvFileArgs(filepath.Join(dir, "services", "ollama.yml")); args != nil {
		t.Errorf("EnvFileArgs for ollama = %v, want nil (no model persisted)", args)
	}
}
