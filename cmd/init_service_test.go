package cmd

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/service"
	"gopkg.in/yaml.v3"
)

// TestDecideLinuxWorkerSetup pins the pure root-vs-non-root branch selection for
// `citadel init`'s Linux worker-setup hook (citadel-cli#1080). The injected
// installedUnit stands in for service.InstalledManagedUnit so the decision is
// exercised without touching systemctl or the filesystem.
func TestDecideLinuxWorkerSetup(t *testing.T) {
	workerUnit := service.ManagedUnit{Name: "citadel-worker", UserMode: false}
	userUnit := service.ManagedUnit{Name: "citadel", UserMode: true}
	none := func() (service.ManagedUnit, bool) { return service.ManagedUnit{}, false }
	found := func(u service.ManagedUnit) func() (service.ManagedUnit, bool) {
		return func() (service.ManagedUnit, bool) { return u, true }
	}

	t.Run("root + existing unit -> enable it, never install", func(t *testing.T) {
		d := decideLinuxWorkerSetup(true, found(workerUnit))
		if d.action != linuxWorkerEnableExisting {
			t.Fatalf("action = %v, want linuxWorkerEnableExisting", d.action)
		}
		if !d.unitFound || d.unit.Name != "citadel-worker" {
			t.Fatalf("unit = %+v (found=%v), want citadel-worker", d.unit, d.unitFound)
		}
	})

	t.Run("root + no unit -> install a fresh system unit", func(t *testing.T) {
		d := decideLinuxWorkerSetup(true, none)
		if d.action != linuxWorkerInstallNew {
			t.Fatalf("action = %v, want linuxWorkerInstallNew", d.action)
		}
	})

	t.Run("non-root + existing system unit -> print sudo enable command", func(t *testing.T) {
		d := decideLinuxWorkerSetup(false, found(workerUnit))
		if d.action != linuxWorkerPrintCommand {
			t.Fatalf("action = %v, want linuxWorkerPrintCommand", d.action)
		}
		if d.nextCommand != "sudo systemctl enable --now citadel-worker" {
			t.Fatalf("nextCommand = %q", d.nextCommand)
		}
	})

	t.Run("non-root + existing user unit -> print sudo-free --user command", func(t *testing.T) {
		d := decideLinuxWorkerSetup(false, found(userUnit))
		if d.action != linuxWorkerPrintCommand {
			t.Fatalf("action = %v, want linuxWorkerPrintCommand", d.action)
		}
		if d.nextCommand != "systemctl --user enable --now citadel" {
			t.Fatalf("nextCommand = %q", d.nextCommand)
		}
	})

	t.Run("non-root + no unit -> print system-service install command", func(t *testing.T) {
		d := decideLinuxWorkerSetup(false, none)
		if d.action != linuxWorkerPrintCommand {
			t.Fatalf("action = %v, want linuxWorkerPrintCommand", d.action)
		}
		if d.nextCommand != "sudo citadel service install --system" {
			t.Fatalf("nextCommand = %q", d.nextCommand)
		}
	})
}

// TestApplyLinuxWorkerSetup_PrintCommandNamesExactCommand verifies the non-root
// success output actually names the exact next command (the issue's explicit
// requirement that a step left separate must name the exact command to run).
func TestApplyLinuxWorkerSetup_PrintCommandNamesExactCommand(t *testing.T) {
	out := captureStdoutForTest(t, func() {
		applyLinuxWorkerSetup(linuxWorkerDecision{
			action:      linuxWorkerPrintCommand,
			nextCommand: "sudo citadel service install --system",
		})
	})
	if !strings.Contains(out, "sudo citadel service install --system") {
		t.Fatalf("output did not name the next command:\n%s", out)
	}
}

// TestEnsureNodeScaffoldAt covers the network-only scaffold: it must create a
// manifest so findAndReadManifest resolves, record node_config_dir, and NOT
// clobber a pre-existing hostname key or an existing manifest.
func TestEnsureNodeScaffoldAt(t *testing.T) {
	t.Run("fresh box: writes manifest + node_config_dir pointer", func(t *testing.T) {
		nodeDir := t.TempDir()
		globalCfg := filepath.Join(t.TempDir(), "config.yaml")

		if err := ensureNodeScaffoldAt(nodeDir, "rm-01", globalCfg); err != nil {
			t.Fatalf("ensureNodeScaffoldAt: %v", err)
		}

		// Manifest exists with the resolved node name.
		var m CitadelManifest
		data, err := os.ReadFile(filepath.Join(nodeDir, "citadel.yaml"))
		if err != nil {
			t.Fatalf("read manifest: %v", err)
		}
		if err := yaml.Unmarshal(data, &m); err != nil {
			t.Fatalf("parse manifest: %v", err)
		}
		if m.Node.Name != "rm-01" {
			t.Fatalf("manifest node name = %q, want rm-01", m.Node.Name)
		}

		// Global config points at the node dir.
		got := readGlobalNodeConfigDir(t, globalCfg)
		if got != nodeDir {
			t.Fatalf("node_config_dir = %q, want %q", got, nodeDir)
		}
	})

	t.Run("preserves a pre-existing hostname key in the global config", func(t *testing.T) {
		nodeDir := t.TempDir()
		globalCfg := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(globalCfg, []byte("hostname: my-node\n"), 0600); err != nil {
			t.Fatalf("seed global config: %v", err)
		}

		if err := ensureNodeScaffoldAt(nodeDir, "rm-01", globalCfg); err != nil {
			t.Fatalf("ensureNodeScaffoldAt: %v", err)
		}

		var cfg map[string]interface{}
		data, err := os.ReadFile(globalCfg)
		if err != nil {
			t.Fatalf("read global config: %v", err)
		}
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			t.Fatalf("parse global config: %v", err)
		}
		if cfg["hostname"] != "my-node" {
			t.Fatalf("hostname clobbered: got %v, want my-node", cfg["hostname"])
		}
		if cfg["node_config_dir"] != nodeDir {
			t.Fatalf("node_config_dir = %v, want %q", cfg["node_config_dir"], nodeDir)
		}
	})

	t.Run("idempotent: never clobbers an existing manifest", func(t *testing.T) {
		nodeDir := t.TempDir()
		globalCfg := filepath.Join(t.TempDir(), "config.yaml")
		manifestPath := filepath.Join(nodeDir, "citadel.yaml")

		// A provisioned manifest with a real service already present.
		existing := &CitadelManifest{
			Node: struct {
				Name  string   `yaml:"name"`
				Tags  []string `yaml:"tags"`
				OrgID string   `yaml:"org_id,omitempty"`
			}{Name: "provisioned", Tags: []string{"gpu"}},
			Services: []Service{{Name: "vllm", ComposeFile: "./services/vllm.yml"}},
		}
		if err := os.MkdirAll(nodeDir, 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := writeManifest(manifestPath, existing); err != nil {
			t.Fatalf("seed manifest: %v", err)
		}

		if err := ensureNodeScaffoldAt(nodeDir, "different-name", globalCfg); err != nil {
			t.Fatalf("ensureNodeScaffoldAt: %v", err)
		}

		var m CitadelManifest
		data, _ := os.ReadFile(manifestPath)
		if err := yaml.Unmarshal(data, &m); err != nil {
			t.Fatalf("parse manifest: %v", err)
		}
		if m.Node.Name != "provisioned" {
			t.Fatalf("existing manifest name clobbered: got %q, want provisioned", m.Node.Name)
		}
		if len(m.Services) != 1 || m.Services[0].Name != "vllm" {
			t.Fatalf("existing manifest services clobbered: %+v", m.Services)
		}
	})

	t.Run("empty node name falls back rather than writing a blank name", func(t *testing.T) {
		nodeDir := t.TempDir()
		globalCfg := filepath.Join(t.TempDir(), "config.yaml")
		if err := ensureNodeScaffoldAt(nodeDir, "", globalCfg); err != nil {
			t.Fatalf("ensureNodeScaffoldAt: %v", err)
		}
		var m CitadelManifest
		data, _ := os.ReadFile(filepath.Join(nodeDir, "citadel.yaml"))
		if err := yaml.Unmarshal(data, &m); err != nil {
			t.Fatalf("parse manifest: %v", err)
		}
		if strings.TrimSpace(m.Node.Name) == "" {
			t.Fatal("manifest node name is blank; expected a hostname fallback")
		}
	})

	t.Run("empty node config dir is an error, not a silent no-op", func(t *testing.T) {
		if err := ensureNodeScaffoldAt("", "rm-01", filepath.Join(t.TempDir(), "config.yaml")); err == nil {
			t.Fatal("expected an error for an empty node config dir")
		}
	})
}

// TestWriteGlobalConfigFilePreservesExistingKeys pins the merge-preserving
// behavior directly (the clobber this replaced wiped hostname -- citadel-cli#1080).
func TestWriteGlobalConfigFilePreservesExistingKeys(t *testing.T) {
	globalCfg := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(globalCfg, []byte("hostname: keep-me\noriginal_hostname: orig\n"), 0600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := writeGlobalConfigFile(globalCfg, "/home/user/citadel-node"); err != nil {
		t.Fatalf("writeGlobalConfigFile: %v", err)
	}
	var cfg map[string]interface{}
	data, _ := os.ReadFile(globalCfg)
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg["hostname"] != "keep-me" || cfg["original_hostname"] != "orig" {
		t.Fatalf("existing keys not preserved: %+v", cfg)
	}
	if cfg["node_config_dir"] != "/home/user/citadel-node" {
		t.Fatalf("node_config_dir = %v", cfg["node_config_dir"])
	}
}

// readGlobalNodeConfigDir reads node_config_dir out of a global config file.
func readGlobalNodeConfigDir(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var cfg struct {
		NodeConfigDir string `yaml:"node_config_dir"`
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return cfg.NodeConfigDir
}

// captureStdoutForTest runs fn with os.Stdout redirected to a pipe and returns
// what it wrote.
func captureStdoutForTest(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	_ = w.Close()
	os.Stdout = orig
	return <-done
}
