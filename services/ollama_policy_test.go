package services

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func intPtr(n int) *int { return &n }

// TestOllamaMaxLoadedModelsComposeEnv pins the single pure #1209 environment
// authority. Ollama gets exactly one final policy entry even with hostile
// inherited duplicates; nil and invalid-best-effort values fail closed to the
// semantic default. Non-ollama environments stay byte-for-byte unchanged.
func TestOllamaMaxLoadedModelsComposeEnv(t *testing.T) {
	base := []string{
		"HOME=/tmp/isolated",
		EnvOllamaMaxLoadedModels + "=73",
		"UNRELATED=keep",
		EnvOllamaMaxLoadedModels + "=91",
	}
	cases := []struct {
		name      string
		service   string
		maxLoaded *int
		want      []string
		wantErr   bool
	}{
		{"ollama set to 1", "ollama", intPtr(1), []string{"HOME=/tmp/isolated", "UNRELATED=keep", "OLLAMA_MAX_LOADED_MODELS=1"}, false},
		{"ollama explicit 0", "ollama", intPtr(0), []string{"HOME=/tmp/isolated", "UNRELATED=keep", "OLLAMA_MAX_LOADED_MODELS=0"}, false},
		{"ollama unset is explicit empty", "ollama", nil, []string{"HOME=/tmp/isolated", "UNRELATED=keep", "OLLAMA_MAX_LOADED_MODELS="}, false},
		{"ollama negative is safe and errors", "ollama", intPtr(-1), []string{"HOME=/tmp/isolated", "UNRELATED=keep", "OLLAMA_MAX_LOADED_MODELS="}, true},
		{"non-ollama with value is unchanged", "vllm", intPtr(1), base, false},
		{"non-ollama nil is unchanged", "llamacpp", nil, base, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			original := append([]string(nil), base...)
			got, err := OllamaMaxLoadedModelsComposeEnv(base, tc.service, tc.maxLoaded)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tc.wantErr)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("env = %#v, want %#v", got, tc.want)
			}
			if !reflect.DeepEqual(base, original) {
				t.Fatalf("input env mutated: got %#v, want %#v", base, original)
			}
		})
	}
}

// TestOllamaComposeExplicitEmptyDefeatsEnvFile proves the Compose precedence
// relied on by the normalizer without starting a daemon or container. A hostile
// .env/--env-file value is visible when the process key is absent, but an
// explicit empty process value wins and renders empty in both shipped templates.
func TestOllamaComposeExplicitEmptyDefeatsEnvFile(t *testing.T) {
	docker, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("docker CLI unavailable")
	}
	probeDir := t.TempDir()
	probeCtx, cancelProbe := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelProbe()
	probe := exec.CommandContext(probeCtx, docker, "compose", "version")
	probe.Env = []string{
		"HOME=" + probeDir,
		"XDG_CONFIG_HOME=" + filepath.Join(probeDir, "xdg"),
		"DOCKER_CONFIG=" + filepath.Join(probeDir, "docker"),
	}
	if err := probe.Run(); err != nil {
		t.Skipf("docker compose unavailable: %v", err)
	}

	for _, template := range []string{"ollama.yml", "ollama.darwin.yml"} {
		t.Run(template, func(t *testing.T) {
			dir := t.TempDir()
			source := filepath.Join("compose", template)
			data, err := os.ReadFile(source)
			if err != nil {
				t.Fatal(err)
			}
			composePath := filepath.Join(dir, template)
			if err := os.WriteFile(composePath, data, 0600); err != nil {
				t.Fatal(err)
			}
			envFile := filepath.Join(dir, "hostile.env")
			if err := os.WriteFile(envFile, []byte(EnvOllamaMaxLoadedModels+"=73\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(EnvOllamaMaxLoadedModels+"=72\n"), 0600); err != nil {
				t.Fatal(err)
			}

			base := []string{
				"HOME=" + dir,
				"XDG_CONFIG_HOME=" + filepath.Join(dir, "xdg"),
				"DOCKER_CONFIG=" + filepath.Join(dir, "docker"),
				"CITADEL_WORKSPACE=" + filepath.Join(dir, "workspace"),
				"UNRELATED=keep",
			}
			if got := renderOllamaPolicy(t, docker, dir, composePath, "", base); got != "72" {
				t.Fatalf("stripped process key rendered %q, want hostile .env value 72", got)
			}
			if got := renderOllamaPolicy(t, docker, dir, composePath, envFile, base); got != "73" {
				t.Fatalf("stripped process key rendered %q, want hostile env-file value 73", got)
			}

			normalized, err := OllamaMaxLoadedModelsComposeEnv(base, "ollama", nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := renderOllamaPolicy(t, docker, dir, composePath, envFile, normalized); got != "" {
				t.Fatalf("explicit-empty process key rendered %q, want empty", got)
			}
			one := 1
			normalized, err = OllamaMaxLoadedModelsComposeEnv(base, "ollama", &one)
			if err != nil {
				t.Fatal(err)
			}
			if got := renderOllamaPolicy(t, docker, dir, composePath, envFile, normalized); got != "1" {
				t.Fatalf("explicit manifest policy rendered %q, want 1", got)
			}
			zero := 0
			normalized, err = OllamaMaxLoadedModelsComposeEnv(base, "ollama", &zero)
			if err != nil {
				t.Fatal(err)
			}
			if got := renderOllamaPolicy(t, docker, dir, composePath, envFile, normalized); got != "0" {
				t.Fatalf("explicit automatic policy rendered %q, want 0", got)
			}
		})
	}
}

func renderOllamaPolicy(t *testing.T, docker, dir, composePath, envFile string, env []string) string {
	t.Helper()
	args := []string{"compose", "--project-directory", dir}
	if envFile != "" {
		args = append(args, "--env-file", envFile)
	}
	args = append(args, "-f", composePath, "config", "--format", "json")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, docker, args...)
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("docker compose config: %v\n%s", err, out)
	}
	var cfg struct {
		Services map[string]struct {
			Environment map[string]*string `json:"environment"`
		} `json:"services"`
	}
	if err := json.Unmarshal(out, &cfg); err != nil {
		t.Fatalf("decode compose config: %v\n%s", err, out)
	}
	value, ok := cfg.Services["ollama"].Environment[EnvOllamaMaxLoadedModels]
	if !ok || value == nil {
		t.Fatalf("rendered environment lacks %s: %s", EnvOllamaMaxLoadedModels, out)
	}
	return *value
}

// TestOllamaPolicyNormalizerWiredIntoEveryComposeUp guards the four production
// compose-up paths against drifting back to duplicated, path-local policy code.
func TestOllamaPolicyNormalizerWiredIntoEveryComposeUp(t *testing.T) {
	cases := []struct {
		path   string
		marker string
		needle string
	}{
		{"../cmd/service.go", "func startService(", "composeEnvForStrictServiceStart"},
		{"../cmd/service_ollama.go", "func composeEnvForStrictServiceStart(", "OllamaMaxLoadedModelsComposeEnv"},
		{"../cmd/service_bind.go", "func composeEnvForServiceValues(", "OllamaMaxLoadedModelsComposeEnv"},
		{"../internal/jobs/service_handler.go", "func (h *ServiceHandler) composeEnvForEngineStart(", "OllamaMaxLoadedModelsComposeEnv"},
		{"../internal/jobs/service_handler.go", "func (h *ServiceHandler) serviceStart(", "OllamaMaxLoadedModelsComposeEnv"},
		{"../internal/jobs/config_handler.go", "func (h *ConfigHandler) startServices(", "composeEnvForAppliedService"},
		{"../internal/jobs/ollama_policy.go", "func composeEnvForAppliedService(", "OllamaMaxLoadedModelsComposeEnv"},
	}
	for _, tc := range cases {
		data, err := os.ReadFile(tc.path)
		if err != nil {
			t.Fatalf("read %s: %v", tc.path, err)
		}
		source := string(data)
		start := strings.Index(source, tc.marker)
		if start < 0 {
			t.Fatalf("%s lacks %q", tc.path, tc.marker)
		}
		body := source[start:]
		if next := strings.Index(body[len(tc.marker):], "\nfunc "); next >= 0 {
			body = body[:len(tc.marker)+next]
		}
		if !strings.Contains(body, tc.needle) {
			t.Errorf("%s function %q does not call %q", tc.path, tc.marker, tc.needle)
		}
	}

	for _, legacy := range []string{"OllamaMaxLoadedModelsEnv(", "ollamaMaxLoadedModelsEntries", "ollamaMaxLoadedModelsEnvForService"} {
		for _, path := range []string{"../cmd", "../internal/jobs", "."} {
			err := filepath.WalkDir(path, func(file string, entry os.DirEntry, walkErr error) error {
				if walkErr != nil || entry.IsDir() || !strings.HasSuffix(file, ".go") || strings.HasSuffix(file, "_test.go") {
					return walkErr
				}
				data, readErr := os.ReadFile(file)
				if readErr != nil {
					return readErr
				}
				if strings.Contains(string(data), legacy) {
					t.Errorf("legacy policy helper %q remains in %s", legacy, file)
				}
				return nil
			})
			if err != nil {
				t.Fatalf("walk %s: %v", path, err)
			}
		}
	}
}
