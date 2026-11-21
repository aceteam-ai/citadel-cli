//go:build linux

package jobs

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/composerefresh"
	embeddedservices "github.com/aceteam-ai/citadel-cli/services"
)

func TestDeviceConfigMaterializationUsesHostVariantAndRepairsStamp(t *testing.T) {
	t.Setenv("CITADEL_NODE_DIR", "")
	for _, name := range []string{"ollama", "llamacpp"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			h := NewConfigHandler(dir)
			count := 0
			h.gpuCount = func() int { return count }
			config := &DeviceConfig{DeviceName: "fixture", Services: []string{name}}
			// Fresh wizard materialization must choose CPU on proven absence.
			if err := h.updateManifest(dir, config); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "services", name+".yml")
			stampPath := filepath.Join(dir, "services", composerefresh.StampFileName)
			for _, next := range []int{0, 1, 0, -1} {
				previous, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				previousHash := fmt.Sprintf("%x", sha256.Sum256(previous))
				stamp := map[string]any{"version": "keep", "hashes": map[string]string{name: previousHash, "vllm": "peer"}}
				b, _ := json.Marshal(stamp)
				if err := os.WriteFile(stampPath, b, 0600); err != nil {
					t.Fatal(err)
				}
				count = next
				if err := h.updateManifest(dir, config); err != nil {
					t.Fatal(err)
				}
				got, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				want, _ := embeddedservices.ComposeForHost(name, "linux", func() int { return count })
				if next < 0 {
					want = string(previous)
				}
				if string(got) != want {
					t.Fatal("config save reset selected flavor", next)
				}
				b, err = os.ReadFile(stampPath)
				if err != nil {
					t.Fatal(err)
				}
				var actual struct {
					Version string            `json:"version"`
					Hashes  map[string]string `json:"hashes"`
				}
				if err := json.Unmarshal(b, &actual); err != nil {
					t.Fatal(err)
				}
				if actual.Version != "keep" || actual.Hashes["vllm"] != "peer" || actual.Hashes[name] != fmt.Sprintf("%x", sha256.Sum256(got)) {
					t.Fatal("config save left stale or damaged stamp", actual)
				}
			}
		})
	}
}

func TestDeviceConfigStartReconcilesBeforeRuntimePreflight(t *testing.T) {
	t.Setenv("CITADEL_NODE_DIR", "")
	t.Setenv("PATH", "")
	dir := t.TempDir()
	h := NewConfigHandler(dir)
	probes := 0
	h.gpuCount = func() int { probes++; return 0 }
	servicesDir := filepath.Join(dir, "services")
	if err := os.Mkdir(servicesDir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(servicesDir, "ollama.yml")
	if err := os.WriteFile(path, []byte(embeddedservices.ServiceMap["ollama"]), 0600); err != nil {
		t.Fatal(err)
	}
	if err := h.startServices(dir, []string{"ollama"}); err == nil {
		t.Fatal("empty PATH should refuse runtime")
	}
	got, err := os.ReadFile(path)
	want, _ := embeddedservices.ComposeForHost("ollama", "linux", func() int { return 0 })
	if err != nil || string(got) != want || probes != 1 {
		t.Fatal("start bypassed host reconciliation", err, probes)
	}
}
