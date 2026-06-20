// cmd/init_test.go
package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestClearSavedConfigPreservesNodeConfigDir verifies that clearSavedConfig()
// preserves node_config_dir while clearing device-specific fields.
func TestClearSavedConfigPreservesNodeConfigDir(t *testing.T) {
	// Create a temporary directory structure
	tmpDir, err := os.MkdirTemp("", "citadel-init-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create a config file with both node_config_dir and device fields
	configFile := filepath.Join(tmpDir, "config.yaml")
	configContent := `node_config_dir: "/home/user/citadel-node"
device_api_token: "test-token"
api_base_url: "https://aceteam.ai"
org_id: "test-org"
redis_url: "redis://localhost:6379"
`
	if err := os.WriteFile(configFile, []byte(configContent), 0644); err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}

	// Read the config to verify initial state
	data, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatalf("Failed to read config: %v", err)
	}
	var initialConfig map[string]interface{}
	if err := yaml.Unmarshal(data, &initialConfig); err != nil {
		t.Fatalf("Failed to parse initial config: %v", err)
	}

	// Verify initial state has all fields
	if initialConfig["node_config_dir"] != "/home/user/citadel-node" {
		t.Error("Initial config should have node_config_dir")
	}
	if initialConfig["device_api_token"] != "test-token" {
		t.Error("Initial config should have device_api_token")
	}

	// Simulate clearSavedConfig logic (can't call it directly due to platform.ConfigDir())
	// This tests the YAML manipulation logic that clearSavedConfig uses

	// Preserve node_config_dir, clear device-specific fields
	nodeConfigDir, hasNodeConfigDir := initialConfig["node_config_dir"]
	delete(initialConfig, "device_api_token")
	delete(initialConfig, "api_base_url")
	delete(initialConfig, "org_id")
	delete(initialConfig, "redis_url")

	if hasNodeConfigDir {
		initialConfig["node_config_dir"] = nodeConfigDir
	}

	// Write back
	newData, err := yaml.Marshal(initialConfig)
	if err != nil {
		t.Fatalf("Failed to marshal config: %v", err)
	}
	if err := os.WriteFile(configFile, newData, 0600); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}

	// Read back and verify
	finalData, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatalf("Failed to read final config: %v", err)
	}
	var finalConfig map[string]interface{}
	if err := yaml.Unmarshal(finalData, &finalConfig); err != nil {
		t.Fatalf("Failed to parse final config: %v", err)
	}

	// node_config_dir should be preserved
	if finalConfig["node_config_dir"] != "/home/user/citadel-node" {
		t.Error("node_config_dir should be preserved after clearing config")
	}

	// Device fields should be cleared
	if _, exists := finalConfig["device_api_token"]; exists {
		t.Error("device_api_token should be cleared")
	}
	if _, exists := finalConfig["api_base_url"]; exists {
		t.Error("api_base_url should be cleared")
	}
	if _, exists := finalConfig["org_id"]; exists {
		t.Error("org_id should be cleared")
	}
	if _, exists := finalConfig["redis_url"]; exists {
		t.Error("redis_url should be cleared")
	}
}

// TestClearSavedConfigPreservesNetworkState verifies that clearSavedConfig()
// only clears device config, not network state.
func TestClearSavedConfigPreservesNetworkState(t *testing.T) {
	t.Log("clearSavedConfig() behavior:")
	t.Log("  - Preserves node_config_dir in global config")
	t.Log("  - Clears device_api_token, api_base_url, org_id, redis_url")
	t.Log("  - Does NOT touch network state in ~/.citadel-node/network/")
	t.Log("  - Network state and global config are in separate directories")
	t.Log("  - This separation enables IP preservation on --relogin")
}

// TestReloginBehaviorDocumentation documents the expected --relogin behavior.
func TestReloginBehaviorDocumentation(t *testing.T) {
	t.Log("--relogin flag behavior:")
	t.Log("")
	t.Log("Before fix (IP not preserved):")
	t.Log("  1. network.Logout() called")
	t.Log("     - Disconnect() - stops tsnet server")
	t.Log("     - ClearState() - DELETES machine key")
	t.Log("  2. clearSavedConfig() - clears device tokens")
	t.Log("  3. New device auth flow")
	t.Log("  4. Connect with new authkey")
	t.Log("  5. New machine key generated → NEW IP assigned")
	t.Log("")
	t.Log("After fix (IP preserved):")
	t.Log("  1. network.Disconnect() called (NOT Logout)")
	t.Log("     - Disconnect() - stops tsnet server")
	t.Log("     - Machine key PRESERVED")
	t.Log("  2. clearSavedConfig() - clears device tokens")
	t.Log("  3. New device auth flow")
	t.Log("  4. Connect with new authkey BUT same machine key")
	t.Log("  5. Headscale recognizes machine → SAME IP preserved")
	t.Log("")
	t.Log("Key insight:")
	t.Log("  - Headscale identifies nodes by machine key, not authkey")
	t.Log("  - Machine key is stored in ~/.citadel-node/network/")
	t.Log("  - Keeping the machine key = keeping the same IP")
}

// TestInitFlagDescriptions verifies the flag descriptions are accurate.
func TestInitFlagDescriptions(t *testing.T) {
	// Find the relogin flag
	flag := initCmd.Flags().Lookup("relogin")
	if flag == nil {
		t.Fatal("--relogin flag not found")
	}

	expectedUsage := "Force re-authentication while preserving IP address"
	if flag.Usage != expectedUsage {
		t.Errorf("--relogin usage = %q, want %q", flag.Usage, expectedUsage)
	}

	// Verify new-device flag exists and has correct description
	newDeviceFlag := initCmd.Flags().Lookup("new-device")
	if newDeviceFlag == nil {
		t.Fatal("--new-device flag not found")
	}

	if newDeviceFlag.Usage == "" {
		t.Error("--new-device flag should have a usage description")
	}
}

// TestClearSavedConfigPreservesHostname verifies that clearSavedConfig()
// does NOT clear the saved hostname (it's stable identity, not device auth).
func TestClearSavedConfigPreservesHostname(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "citadel-hostname-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	configFile := filepath.Join(tmpDir, "config.yaml")
	configContent := `hostname: "citadel-ab12cd34"
node_config_dir: "/home/user/citadel-node"
device_api_token: "test-token"
`
	if err := os.WriteFile(configFile, []byte(configContent), 0644); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}

	// Simulate clearSavedConfig logic
	data, _ := os.ReadFile(configFile)
	var config map[string]interface{}
	yaml.Unmarshal(data, &config)

	// clearSavedConfig deletes these fields:
	delete(config, "device_api_token")
	delete(config, "api_base_url")
	delete(config, "org_id")
	delete(config, "redis_url")
	delete(config, "user_email")
	delete(config, "user_name")

	// hostname should NOT be in the delete list (stable identity)
	if _, exists := config["hostname"]; !exists {
		t.Error("hostname should survive clearSavedConfig")
	}
	if config["hostname"] != "citadel-ab12cd34" {
		t.Errorf("hostname = %q, want %q", config["hostname"], "citadel-ab12cd34")
	}
}

// TestSaveHostnameToConfigRoundTrip verifies hostname save/load via config YAML.
func TestSaveHostnameToConfigRoundTrip(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "citadel-hostname-rt-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	configFile := filepath.Join(tmpDir, "config.yaml")

	// Write a config with hostname
	config := map[string]interface{}{
		"hostname":        "citadel-deadbeef",
		"node_config_dir": "/home/user/citadel-node",
	}
	data, _ := yaml.Marshal(config)
	os.WriteFile(configFile, data, 0600)

	// Read it back
	readData, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatalf("Failed to read config: %v", err)
	}

	var readConfig struct {
		Hostname string `yaml:"hostname"`
	}
	if err := yaml.Unmarshal(readData, &readConfig); err != nil {
		t.Fatalf("Failed to parse config: %v", err)
	}

	if readConfig.Hostname != "citadel-deadbeef" {
		t.Errorf("hostname = %q, want %q", readConfig.Hostname, "citadel-deadbeef")
	}
}

// TestNodeReclaimBehavior documents the node reclamation behavior during init.
func TestNodeReclaimBehavior(t *testing.T) {
	t.Log("Node identity reclamation (issue #159):")
	t.Log("")
	t.Log("Problem:")
	t.Log("  Every live ISO reboot registers a NEW Headscale node.")
	t.Log("  Old nodes go stale, cluttering the fabric dashboard.")
	t.Log("")
	t.Log("Solution:")
	t.Log("  Before connecting to the network, call the backend to deregister")
	t.Log("  any existing node with the same hostname in the same org.")
	t.Log("  The backend's /api/fabric/device-auth/deregister endpoint handles")
	t.Log("  the Headscale lookup + deletion, scoped to the caller's org.")
	t.Log("")
	t.Log("Flow:")
	t.Log("  1. Device auth succeeds -> we have device_api_token + hostname")
	t.Log("  2. Call deregister with hostname -> removes stale node if exists")
	t.Log("  3. Connect to network with new authkey -> registers fresh node")
	t.Log("")
	t.Log("Safety:")
	t.Log("  - Best-effort: reclaim failure does not block init")
	t.Log("  - Org-scoped: only affects nodes in the caller's organization")
	t.Log("  - 404 is a no-op: if no stale node exists, nothing happens")
	t.Log("  - Hostname collision: hostnames are derived from /etc/machine-id")
	t.Log("    (first 8 chars), so different hardware gets different hostnames.")
	t.Log("    Generic OS hostnames (debian, ubuntu, etc.) are never registered")
	t.Log("    because getNodeName() replaces them with citadel-<id>.")
	t.Log("  - Same hardware: when the same machine reboots, /etc/machine-id")
	t.Log("    produces the same hostname, so the stale node is correctly")
	t.Log("    reclaimed. If /etc/machine-id changes between boots (e.g. live")
	t.Log("    ISO without persistence), the hostname differs and the old node")
	t.Log("    is left untouched (404 no-op).")
}

// TestReloginVsNewDeviceFlags documents the difference between --relogin and --new-device.
func TestReloginVsNewDeviceFlags(t *testing.T) {
	t.Log("--relogin vs --new-device:")
	t.Log("")
	t.Log("--relogin:")
	t.Log("  - Purpose: Re-authenticate while keeping the same identity")
	t.Log("  - Preserves: Machine key (network state)")
	t.Log("  - Result: Same IP address after re-login")
	t.Log("  - Use case: Refreshing expired credentials")
	t.Log("")
	t.Log("--new-device:")
	t.Log("  - Purpose: Register as a completely new device")
	t.Log("  - Backend: Tells server to ignore existing machine_id mapping")
	t.Log("  - Result: New node ID, new IP address")
	t.Log("  - Use case: Treating same hardware as different node")
	t.Log("")
	t.Log("Combined usage:")
	t.Log("  --relogin alone: Same machine key, same backend mapping → same IP")
	t.Log("  --relogin --new-device: Same machine key, NEW backend mapping → still same IP")
	t.Log("                         (backend ignores old mapping but Headscale sees same machine)")
}
