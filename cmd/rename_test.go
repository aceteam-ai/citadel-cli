package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"gopkg.in/yaml.v3"
)

// withTempConfigHome points platform.ConfigDir() at a temp directory by
// overriding HOME, and returns the resolved config directory. It skips the
// test when running as root, since ConfigDir() ignores HOME in that case.
func withTempConfigHome(t *testing.T) string {
	t.Helper()
	if platform.IsRoot() {
		t.Skip("skipping config-dir test when running as root (ConfigDir ignores HOME)")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)

	configDir := platform.ConfigDir()
	if configDir == "" {
		t.Fatal("ConfigDir() returned empty path")
	}
	if err := os.MkdirAll(configDir, 0755); err != nil {
		t.Fatalf("failed to create config dir: %v", err)
	}
	return configDir
}

func TestOriginalHostnameRoundTrip(t *testing.T) {
	withTempConfigHome(t)

	if got := getOriginalHostname(); got != "" {
		t.Fatalf("getOriginalHostname() = %q before any save, want empty", got)
	}

	if err := saveOriginalHostnameToConfig("aceteamvm"); err != nil {
		t.Fatalf("saveOriginalHostnameToConfig failed: %v", err)
	}

	if got := getOriginalHostname(); got != "aceteamvm" {
		t.Errorf("getOriginalHostname() = %q, want %q", got, "aceteamvm")
	}
}

func TestSaveOriginalHostnamePreservesFirstValue(t *testing.T) {
	withTempConfigHome(t)

	if err := saveOriginalHostnameToConfig("original-name"); err != nil {
		t.Fatalf("first save failed: %v", err)
	}
	// A subsequent save must NOT overwrite the genuine original.
	if err := saveOriginalHostnameToConfig("renamed-later"); err != nil {
		t.Fatalf("second save failed: %v", err)
	}

	if got := getOriginalHostname(); got != "original-name" {
		t.Errorf("getOriginalHostname() = %q, want %q (original must be preserved)", got, "original-name")
	}
}

func TestSaveOriginalHostnameIgnoresEmpty(t *testing.T) {
	withTempConfigHome(t)

	if err := saveOriginalHostnameToConfig(""); err != nil {
		t.Fatalf("saving empty hostname returned error: %v", err)
	}
	if got := getOriginalHostname(); got != "" {
		t.Errorf("getOriginalHostname() = %q, want empty after saving empty value", got)
	}
}

// TestSaveOriginalHostnamePreservesOtherFields verifies that recording the
// original hostname does not clobber unrelated config keys.
func TestSaveOriginalHostnamePreservesOtherFields(t *testing.T) {
	configDir := withTempConfigHome(t)

	configFile := filepath.Join(configDir, "config.yaml")
	initial := map[string]interface{}{
		"node_config_dir":  "/home/user/citadel-node",
		"device_api_token": "secret-token",
		"hostname":         "citadel-deadbeef",
	}
	data, _ := yaml.Marshal(initial)
	if err := os.WriteFile(configFile, data, 0600); err != nil {
		t.Fatalf("failed to seed config: %v", err)
	}

	if err := saveOriginalHostnameToConfig("real-host"); err != nil {
		t.Fatalf("saveOriginalHostnameToConfig failed: %v", err)
	}

	readData, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatalf("failed to read config: %v", err)
	}
	var result map[string]interface{}
	if err := yaml.Unmarshal(readData, &result); err != nil {
		t.Fatalf("failed to parse config: %v", err)
	}

	if result["original_hostname"] != "real-host" {
		t.Errorf("original_hostname = %v, want %q", result["original_hostname"], "real-host")
	}
	if result["device_api_token"] != "secret-token" {
		t.Errorf("device_api_token was clobbered: got %v", result["device_api_token"])
	}
	if result["node_config_dir"] != "/home/user/citadel-node" {
		t.Errorf("node_config_dir was clobbered: got %v", result["node_config_dir"])
	}
	if result["hostname"] != "citadel-deadbeef" {
		t.Errorf("hostname was clobbered: got %v", result["hostname"])
	}
}
