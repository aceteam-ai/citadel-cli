package compose

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/composerefresh"
	"github.com/aceteam-ai/citadel-cli/services"
)

func TestCPUComposeReconcileTracksBothDirectionsAndStamp(t *testing.T) {
	for _, name := range []string{"ollama", "llamacpp"} {
		for _, namespaced := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/namespaced=%v", name, namespaced), func(t *testing.T) {
				dir := t.TempDir()
				path := filepath.Join(dir, name+".yml")
				override := ""
				if namespaced {
					override = dir
				}
				cpu, _ := services.ComposeForHost(name, "linux", func() int { return 0 })
				gpu, err := os.ReadFile(filepath.Join("..", "..", "services", "compose", name+".yml"))
				if err != nil {
					t.Fatal(err)
				}
				current, _, err := EnsureNamespacedContainerName(string(gpu), name, override)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(current), 0600); err != nil {
					t.Fatal(err)
				}
				stampPath := filepath.Join(dir, composerefresh.StampFileName)
				stamp := map[string]any{"version": "same-version", "hashes": map[string]string{name: fmt.Sprintf("%x", sha256.Sum256([]byte(current))), "vllm": "untouched"}}
				b, _ := json.Marshal(stamp)
				if err := os.WriteFile(stampPath, b, 0600); err != nil {
					t.Fatal(err)
				}
				for _, selected := range []string{cpu, string(gpu), cpu} {
					changed, err := ReconcileCPUComposeOnDisk(path, name, selected, override)
					if err != nil || !changed {
						t.Fatalf("transition: %v %v", changed, err)
					}
					want, _, _ := EnsureNamespacedContainerName(selected, name, override)
					got, err := os.ReadFile(path)
					if err != nil || string(got) != want {
						t.Fatalf("wrong selected content: %v", err)
					}
					info, err := os.Stat(path)
					if err != nil || info.Mode().Perm() != 0600 {
						t.Fatalf("permissions: %v %v", info, err)
					}
					var recorded struct {
						Version string            `json:"version"`
						Hashes  map[string]string `json:"hashes"`
					}
					b, err := os.ReadFile(stampPath)
					if err != nil {
						t.Fatal(err)
					}
					if err := json.Unmarshal(b, &recorded); err != nil {
						t.Fatal(err)
					}
					if recorded.Version != "same-version" || recorded.Hashes["vllm"] != "untouched" || recorded.Hashes[name] != fmt.Sprintf("%x", sha256.Sum256(got)) {
						t.Fatalf("stamp drift: %+v", recorded)
					}
					fixedTime := time.Unix(100, 0)
					if err := os.Chtimes(path, fixedTime, fixedTime); err != nil {
						t.Fatal(err)
					}
					changed, err = ReconcileCPUComposeOnDisk(path, name, selected, override)
					if err != nil || changed {
						t.Fatalf("repeat rewrote: %v %v", changed, err)
					}
					info, _ = os.Stat(path)
					if !info.ModTime().Equal(fixedTime) {
						t.Fatal("idempotent call rewrote file")
					}
				}
			})
		}
	}
}

func TestCPUComposeReconcilePreservesEditsAndForeignNamespaces(t *testing.T) {
	gpu, err := os.ReadFile(filepath.Join("..", "..", "services", "compose", "ollama.yml"))
	if err != nil {
		t.Fatal(err)
	}
	cpu, _ := services.ComposeForHost("ollama", "linux", func() int { return 0 })
	for _, fixture := range []string{string(gpu) + "\n# operator change\n", rewriteHostVariantNameForTest(t, string(gpu), "ollama", "/foreign"), "services: {}\n"} {
		dir := t.TempDir()
		path := filepath.Join(dir, "ollama.yml")
		if err := os.WriteFile(path, []byte(fixture), 0600); err != nil {
			t.Fatal(err)
		}
		changed, err := ReconcileCPUComposeOnDisk(path, "ollama", cpu, dir)
		if err != nil || changed {
			t.Fatalf("edited/foreign file changed: %v %v", changed, err)
		}
		got, _ := os.ReadFile(path)
		if string(got) != fixture {
			t.Fatal("operator content lost")
		}
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "operator.yml")
	path := filepath.Join(dir, "ollama.yml")
	if err := os.WriteFile(target, gpu, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := ReconcileCPUComposeOnDisk(path, "ollama", cpu, ""); err == nil {
		t.Fatal("symlink accepted")
	}
	got, _ := os.ReadFile(target)
	if string(got) != string(gpu) {
		t.Fatal("symlink target changed")
	}
}

func rewriteHostVariantNameForTest(t *testing.T, content, name, override string) string {
	t.Helper()
	got, _, err := EnsureNamespacedContainerName(content, name, override)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestCPUFlavorFlipIsLoggedExactlyWhenChanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ollama.yml")
	if err := os.WriteFile(path, []byte(services.ServiceMap["ollama"]), 0600); err != nil {
		t.Fatal(err)
	}
	cpu, _ := services.ComposeForHost("ollama", "linux", func() int { return 0 })
	calls := 0
	log := func(format string, args ...any) {
		calls++
		if !strings.Contains(fmt.Sprintf(format, args...), "ollama switched to CPU") {
			t.Fatal("missing flip evidence")
		}
	}
	if changed, err := ReconcileCPUComposeOnDisk(path, "ollama", cpu, "", log); err != nil || !changed || calls != 1 {
		t.Fatal("flip was not logged", err, calls)
	}
	if changed, err := ReconcileCPUComposeOnDisk(path, "ollama", cpu, "", log); err != nil || changed || calls != 1 {
		t.Fatal("unchanged flavor logged again", err, calls)
	}
}
