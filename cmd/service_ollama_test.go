package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	svcports "github.com/aceteam-ai/citadel-cli/services"
)

func ollamaPolicyEntries(env []string) []string {
	var entries []string
	for _, entry := range env {
		if strings.HasPrefix(entry, svcports.EnvOllamaMaxLoadedModels+"=") {
			entries = append(entries, entry)
		}
	}
	return entries
}

func ollamaTestIntPtr(n int) *int { return &n }

// TestComposeEnvForServiceValuesNormalizesOllamaPolicy pins the boot-time
// port-drift/restart environment builder without reading the live manifest.
func TestComposeEnvForServiceValuesNormalizesOllamaPolicy(t *testing.T) {
	t.Setenv("CITADEL_WORKSPACE", t.TempDir())
	t.Setenv(svcports.EnvOllamaMaxLoadedModels, "73")
	t.Setenv("CITADEL_OLLAMA_TEST_UNRELATED", "keep")

	cases := []struct {
		name      string
		service   string
		policy    *int
		wantEntry string
	}{
		{"valid", "ollama", ollamaTestIntPtr(1), svcports.EnvOllamaMaxLoadedModels + "=1"},
		{"explicit zero", "ollama", ollamaTestIntPtr(0), svcports.EnvOllamaMaxLoadedModels + "=0"},
		{"unset", "ollama", nil, svcports.EnvOllamaMaxLoadedModels + "="},
		{"invalid best effort", "ollama", ollamaTestIntPtr(-1), svcports.EnvOllamaMaxLoadedModels + "="},
		{"non ollama unchanged", "vllm", ollamaTestIntPtr(1), svcports.EnvOllamaMaxLoadedModels + "=73"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := composeEnvForServiceValues(tc.service, Service{
				Name: tc.service, OllamaMaxLoadedModels: tc.policy,
			})
			entries := ollamaPolicyEntries(env)
			if len(entries) != 1 || entries[0] != tc.wantEntry {
				t.Fatalf("policy entries = %v, want [%q]", entries, tc.wantEntry)
			}
			if !envContains(env, "CITADEL_OLLAMA_TEST_UNRELATED=keep") {
				t.Fatal("unrelated inherited environment was not preserved")
			}
			if tc.service == "ollama" && env[len(env)-1] != tc.wantEntry {
				t.Fatalf("ollama policy must be the final entry, got %q", env[len(env)-1])
			}
		})
	}
}

func TestComposeEnvForStrictServiceStartRejectsNegativeBeforeRuntimeWork(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "must-not-exist")
	t.Setenv("CITADEL_WORKSPACE", workspace)
	t.Setenv(svcports.EnvOllamaMaxLoadedModels, "73")
	t.Setenv("CITADEL_OLLAMA_TEST_UNRELATED", "keep")

	negative := -1
	_, err := composeEnvForStrictServiceStart("ollama", Service{
		Name: "ollama", OllamaMaxLoadedModels: &negative,
	}, map[string]string{"CITADEL_OLLAMA_BIND": "127.0.0.1"})
	if err == nil {
		t.Fatal("negative policy must be a strict error")
	}
	if _, statErr := os.Stat(workspace); !os.IsNotExist(statErr) {
		t.Fatalf("negative policy performed workspace resolution: stat error = %v", statErr)
	}

	one := 1
	env, err := composeEnvForStrictServiceStart("ollama", Service{
		Name: "ollama", OllamaMaxLoadedModels: &one,
	}, map[string]string{"CITADEL_OLLAMA_BIND": "127.0.0.1"})
	if err != nil {
		t.Fatalf("valid policy: %v", err)
	}
	if !envContains(env, "CITADEL_OLLAMA_BIND=127.0.0.1") || !envContains(env, "CITADEL_OLLAMA_TEST_UNRELATED=keep") || !envContains(env, svcports.EnvOllamaMaxLoadedModels+"=1") {
		t.Fatalf("strict builder lost unrelated/bind entries: %v", env)
	}

	// This source-order assertion is a regression guard, not an executable proof
	// of side-effect behavior: the test never invokes a runtime. It makes review
	// fail if validation moves below the known adoption/inspect/removal sites.
	data, err := os.ReadFile(filepath.Join("service.go"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	start := strings.Index(source, "func startService(")
	if start < 0 {
		t.Fatal("startService source not found")
	}
	body := source[start:]
	policy := strings.Index(body, "composeEnvForStrictServiceStart(")
	if policy < 0 {
		t.Fatal("startService does not call strict environment builder")
	}
	for _, later := range []string{"catalog.SelectContainerRuntime(", "maybeAdoptExternalEngineOnStart(", "exec.Command(rt.EngineBin, \"inspect\"", "exec.Command(rt.EngineBin, \"rm\""} {
		index := strings.Index(body, later)
		if index < 0 {
			t.Fatalf("startService side-effect marker %q not found", later)
		}
		if policy > index {
			t.Errorf("strict policy validation at byte %d occurs after %q at byte %d", policy, later, index)
		}
	}
}

func envContains(env []string, want string) bool {
	for _, entry := range env {
		if entry == want {
			return true
		}
	}
	return false
}
