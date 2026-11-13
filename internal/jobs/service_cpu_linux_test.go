//go:build linux

package jobs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/compose"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	embeddedservices "github.com/aceteam-ai/citadel-cli/services"
)

func TestEmbeddedComposeTracksCPUAndGPUWithoutTouchingOtherEngines(t *testing.T) {
	for _, name := range []string{"ollama", "llamacpp"} {
		for _, namespaced := range []bool{false, true} {
			t.Run(name+map[bool]string{false: "/default", true: "/override"}[namespaced], func(t *testing.T) {
				dir := t.TempDir()
				override := ""
				if namespaced {
					override = dir
				}
				t.Setenv("CITADEL_NODE_DIR", override)
				cpu, err := os.ReadFile(filepath.Join("..", "..", "services", "compose", name+".darwin.yml"))
				if err != nil {
					t.Fatal(err)
				}
				count, probes := 0, 0
				h := NewServiceHandler(dir)
				h.gpuCount = func() int { probes++; return count }
				for _, gpuCount := range []int{0, 1, 0} {
					count = gpuCount
					if err := h.ensureEmbeddedComposeFile(name); err != nil {
						t.Fatal(err)
					}
					want := string(cpu)
					if count > 0 {
						want = embeddedservices.ServiceMap[name]
					}
					want, _, err = compose.EnsureNamespacedContainerName(want, name, override)
					if err != nil {
						t.Fatal(err)
					}
					path := filepath.Join(dir, "services", name+".yml")
					got, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					if string(got) != want {
						t.Fatalf("%s with GPU count %d did not materialize the selected template", name, count)
					}
					info, err := os.Stat(path)
					if err != nil || info.Mode().Perm() != 0600 {
						t.Fatalf("compose permissions: %v, %v", info, err)
					}
				}
				if probes != 3 {
					t.Fatalf("GPU policy was not resolved at each start: got %d probes", probes)
				}
			})
		}
	}

	for _, name := range []string{"vllm", "sglang"} {
		t.Run(name+"/unchanged", func(t *testing.T) {
			t.Setenv("CITADEL_NODE_DIR", "")
			dir := t.TempDir()
			h := NewServiceHandler(dir)
			h.gpuCount = func() int { t.Fatal("CUDA-only engine must not use the CPU fallback"); return 0 }
			if err := h.ensureEmbeddedComposeFile(name); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(filepath.Join(dir, "services", name+".yml"))
			if err != nil || string(got) != embeddedservices.ServiceMap[name] {
				t.Fatalf("changed CUDA-only template: %v", err)
			}
		})
	}
}

func TestExistingManifestComposeUsesHostSelectionAtEveryStart(t *testing.T) {
	for _, name := range []string{"ollama", "llamacpp"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("CITADEL_NODE_DIR", "")
			dir := t.TempDir()
			h := NewServiceHandler(dir)
			count := 1
			h.gpuCount = func() int { return count }
			svc := manifestService{Name: name, Type: "docker", ComposeFile: "services/" + name + ".yml"}
			if err := h.prepareEmbeddedComposeForStart(svc); err != nil {
				t.Fatal(err)
			}
			count = 0
			if err := h.prepareEmbeddedComposeForStart(svc); err != nil {
				t.Fatal(err)
			}
			want, _ := embeddedservices.ComposeForHost(name, "linux", func() int { return 0 })
			got, err := os.ReadFile(filepath.Join(dir, svc.ComposeFile))
			if err != nil || string(got) != want {
				t.Fatalf("existing manifest kept GPU template: %v", err)
			}
			custom := filepath.Join(dir, "custom.yml")
			original := embeddedservices.ServiceMap[name]
			if err := os.WriteFile(custom, []byte(original), 0600); err != nil {
				t.Fatal(err)
			}
			h.gpuCount = func() int { t.Fatal("custom compose probed GPU"); return 0 }
			svc.ComposeFile = "custom.yml"
			if err := h.prepareEmbeddedComposeForStart(svc); err != nil {
				t.Fatal(err)
			}
			got, err = os.ReadFile(custom)
			if err != nil || string(got) != original {
				t.Fatalf("custom compose changed: %v", err)
			}
		})
	}
}

// An already-running service still reaches materialization through Execute.
// Runtime and serving probes are fakes; PATH contains no host commands.
func TestServiceStartExistingManifestReconcilesBeforeRunningShortcut(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("CITADEL_NODE_DIR", "")
	for _, name := range []string{"ollama", "llamacpp"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			h := NewServiceHandler(dir)
			count := 1
			probes := 0
			h.gpuCount = func() int { probes++; return count }
			h.dockerServiceRunningFn = func(string) bool { return true }
			h.externalEngineServingFn = func(context.Context, string, int) ([]string, bool) {
				t.Fatal("external engine probe reached")
				return nil, false
			}
			manifest := fmt.Sprintf("services:\n  - name: %s\n    type: docker\n    compose_file: services/%s.yml\n", name, name)
			if err := os.WriteFile(filepath.Join(dir, "citadel.yaml"), []byte(manifest), 0600); err != nil {
				t.Fatal(err)
			}
			if err := h.ensureEmbeddedComposeFile(name); err != nil {
				t.Fatal(err)
			}
			probes = 0
			for _, n := range []int{0, 1, 0} {
				count = n
				_, err := h.Execute(testCtx(), &nexus.Job{ID: "cpu-start-fixture", Type: "SERVICE_START", Payload: map[string]string{"service": name}})
				if err != nil {
					t.Fatal(err)
				}
				want, _ := embeddedservices.ComposeForHost(name, "linux", func() int { return count })
				got, err := os.ReadFile(filepath.Join(dir, "services", name+".yml"))
				if err != nil || string(got) != want {
					t.Fatalf("start shortcut kept wrong variant for count %d: %v", count, err)
				}
			}
			if probes != 3 {
				t.Fatalf("existing manifest did not sample every start: %d", probes)
			}
		})
	}
}
