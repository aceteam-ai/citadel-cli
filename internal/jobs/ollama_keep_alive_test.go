package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/status"
	"github.com/aceteam-ai/citadel-cli/services"
	"gopkg.in/yaml.v3"
)

func requireKeepAlive(t *testing.T, env []string, value string) {
	t.Helper()
	count := 0
	for _, entry := range env {
		if strings.HasPrefix(entry, services.EnvOllamaKeepAlive+"=") {
			count++
			if entry != services.EnvOllamaKeepAlive+"="+value {
				t.Fatalf("wrong keep-alive %q", entry)
			}
		}
	}
	if count != 1 {
		t.Fatalf("keep-alive count %d", count)
	}
}

func TestOllamaKeepAliveAppliedAndDirectStartEnvironments(t *testing.T) {
	for _, value := range []string{"60m", "-1", "0"} {
		t.Run(value, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "citadel.yaml")
			// Numeric -1/0 YAML inputs must survive the string policy representation.
			input := "services:\n  - name: ollama\n    ollama_keep_alive: " + value + "\n"
			if err := os.WriteFile(path, []byte(input), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv(services.EnvOllamaKeepAlive, "hostile")
			applied, err := composeEnvForAppliedService(os.Environ(), path, "ollama")
			if err != nil {
				t.Fatal(err)
			}
			requireKeepAlive(t, applied, value)
			_, policy := ollamaResidencyFromFile(path, "ollama")
			if policy == nil || *policy != value {
				t.Fatal("reader dropped keep-alive")
			}
			h := &ServiceHandler{ConfigDir: dir}
			workerManifest, err := h.loadManifest()
			if err != nil {
				t.Fatal(err)
			}
			service, found := h.findService(workerManifest, "ollama")
			if !found || service.OllamaKeepAlive == nil || *service.OllamaKeepAlive != value {
				t.Fatal("worker manifest dropped keep-alive")
			}
			direct, err := h.composeEnvForEngineStart(service.Name, service.Bind, service.OllamaMaxLoadedModels, service.Threads, service.OllamaKeepAlive)
			if err != nil {
				t.Fatal(err)
			}
			requireKeepAlive(t, direct, value)
		})
	}
	path := filepath.Join(t.TempDir(), "invalid.yaml")
	if err := os.WriteFile(path, []byte("services:\n  - name: ollama\n    ollama_keep_alive: -2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	applied, err := composeEnvForAppliedService([]string{services.EnvOllamaKeepAlive + "=hostile"}, path, "ollama")
	if err == nil {
		t.Fatal("invalid applied policy had no warning error")
	}
	requireKeepAlive(t, applied, "")
	missing, err := composeEnvForAppliedService([]string{services.EnvOllamaKeepAlive + "=hostile"}, path+".missing", "ollama")
	if err != nil {
		t.Fatal(err)
	}
	requireKeepAlive(t, missing, "")
}

func TestOllamaKeepAliveSurvivesDeviceConfigRoundTrip(t *testing.T) {
	for _, value := range []string{"60m", "-1", "0"} {
		t.Run(value, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "citadel.yaml")
			input := "node:\n  name: fixture\nservices:\n  - name: ollama\n    compose_file: ./services/ollama.yml\n    ollama_keep_alive: " + value + "\n"
			if err := os.WriteFile(path, []byte(input), 0600); err != nil {
				t.Fatal(err)
			}
			if err := NewConfigHandler(dir).updateManifest(dir, &DeviceConfig{DeviceName: "fixture"}); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var manifest CitadelManifest
			if err := yaml.Unmarshal(data, &manifest); err != nil {
				t.Fatal(err)
			}
			if len(manifest.Services) != 1 || manifest.Services[0].OllamaKeepAlive == nil || *manifest.Services[0].OllamaKeepAlive != value {
				t.Fatal("device update dropped keep-alive")
			}
			env, err := composeEnvForAppliedService(nil, path, "ollama")
			if err != nil {
				t.Fatal(err)
			}
			requireKeepAlive(t, env, value)
		})
	}
}

func TestServiceStartRefusesInvalidOllamaKeepAliveBeforeEffects(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", t.TempDir())
	probes := 0
	h := &ServiceHandler{ConfigDir: dir,
		dockerServiceRunningFn:  func(string) bool { probes++; return false },
		externalEngineServingFn: func(context.Context, string, int) ([]string, bool) { probes++; return nil, false },
		collectStatus:           func() (*status.NodeStatus, error) { probes++; return nil, errors.New("unexpected status") },
	}
	bad := "-2"
	output, err := h.serviceStart(JobContext{}, manifestService{Name: "ollama", Type: "docker", ComposeFile: "./services/ollama.yml", OllamaKeepAlive: &bad}, "fixture/model", 1, 1, trustRemoteCodeEnable)
	if err != nil {
		t.Fatal(err)
	}
	var result serviceResult
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatal(err)
	}
	if result.Running || !strings.Contains(result.Error, "invalid ollama_keep_alive") || probes != 0 {
		t.Fatalf("invalid policy reached engine effects: %+v / %d", result, probes)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatal("invalid policy modified node fixture", entries, err)
	}
}
