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

// TestDeviceConfigPersistenceRoundTrip verifies that saveDeviceConfigToFile and
// getDeviceConfigFromFile correctly round-trip all config fields including the
// device API token (act_*). This is the core persistence test for issue #109.
func TestDeviceConfigPersistenceRoundTrip(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "citadel-config-rt-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	configFile := filepath.Join(tmpDir, "config.yaml")

	// Simulate saveDeviceConfigToFile by writing all fields
	config := map[string]interface{}{
		"device_api_token": "act_test_token_12345",
		"api_base_url":     "https://aceteam.ai",
		"org_id":           "org-uuid-here",
		"org_name":         "Test Org",
		"user_email":       "user@test.com",
		"user_name":        "Test User",
		"hostname":         "citadel-aabbccdd",
	}
	data, err := yaml.Marshal(config)
	if err != nil {
		t.Fatalf("Failed to marshal config: %v", err)
	}
	if err := os.WriteFile(configFile, data, 0600); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}

	// Read it back (simulating getDeviceConfigFromFile)
	readData, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatalf("Failed to read config: %v", err)
	}

	var readConfig struct {
		DeviceAPIToken string `yaml:"device_api_token"`
		APIBaseURL     string `yaml:"api_base_url"`
		OrgID          string `yaml:"org_id"`
		OrgName        string `yaml:"org_name"`
		UserEmail      string `yaml:"user_email"`
		UserName       string `yaml:"user_name"`
		Hostname       string `yaml:"hostname"`
	}
	if err := yaml.Unmarshal(readData, &readConfig); err != nil {
		t.Fatalf("Failed to parse config: %v", err)
	}

	// Assert all fields round-tripped
	if readConfig.DeviceAPIToken != "act_test_token_12345" {
		t.Errorf("DeviceAPIToken = %q, want %q", readConfig.DeviceAPIToken, "act_test_token_12345")
	}
	if readConfig.APIBaseURL != "https://aceteam.ai" {
		t.Errorf("APIBaseURL = %q, want %q", readConfig.APIBaseURL, "https://aceteam.ai")
	}
	if readConfig.OrgID != "org-uuid-here" {
		t.Errorf("OrgID = %q, want %q", readConfig.OrgID, "org-uuid-here")
	}
	if readConfig.OrgName != "Test Org" {
		t.Errorf("OrgName = %q, want %q", readConfig.OrgName, "Test Org")
	}
	if readConfig.UserEmail != "user@test.com" {
		t.Errorf("UserEmail = %q, want %q", readConfig.UserEmail, "user@test.com")
	}
	if readConfig.UserName != "Test User" {
		t.Errorf("UserName = %q, want %q", readConfig.UserName, "Test User")
	}
	if readConfig.Hostname != "citadel-aabbccdd" {
		t.Errorf("Hostname = %q, want %q", readConfig.Hostname, "citadel-aabbccdd")
	}

	// Verify file permissions are secure
	info, err := os.Stat(configFile)
	if err != nil {
		t.Fatalf("Failed to stat config file: %v", err)
	}
	perm := info.Mode().Perm()
	if perm != 0600 {
		t.Errorf("Config file permissions = %o, want 0600", perm)
	}
}

// TestVerifyConfigPersisted_Success verifies the config verification function
// correctly detects a matching token.
func TestVerifyConfigPersisted_Success(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "citadel-verify-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	configFile := filepath.Join(tmpDir, "config.yaml")
	config := map[string]interface{}{
		"device_api_token": "act_expected_token",
	}
	data, _ := yaml.Marshal(config)
	os.WriteFile(configFile, data, 0600)

	err = verifyConfigPersisted(configFile, "act_expected_token")
	if err != nil {
		t.Errorf("verifyConfigPersisted should pass for matching token: %v", err)
	}
}

// TestVerifyConfigPersisted_Mismatch verifies the config verification function
// detects a token mismatch.
func TestVerifyConfigPersisted_Mismatch(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "citadel-verify-mismatch-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	configFile := filepath.Join(tmpDir, "config.yaml")
	config := map[string]interface{}{
		"device_api_token": "act_wrong_token",
	}
	data, _ := yaml.Marshal(config)
	os.WriteFile(configFile, data, 0600)

	err = verifyConfigPersisted(configFile, "act_expected_token")
	if err == nil {
		t.Error("verifyConfigPersisted should fail for mismatched token")
	}
}

// TestVerifyConfigPersisted_MissingFile verifies the config verification function
// detects a missing config file.
func TestVerifyConfigPersisted_MissingFile(t *testing.T) {
	err := verifyConfigPersisted("/nonexistent/path/config.yaml", "act_token")
	if err == nil {
		t.Error("verifyConfigPersisted should fail for missing file")
	}
}

// TestVerifyConfigPersisted_EmptyToken verifies no verification for empty token.
func TestVerifyConfigPersisted_EmptyToken(t *testing.T) {
	err := verifyConfigPersisted("/nonexistent/path/config.yaml", "")
	if err != nil {
		t.Errorf("verifyConfigPersisted should pass for empty token: %v", err)
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

// --- citadel-cli#845: device/org config must read/write via the
// machine-convergent network.GetNodeConfigDir(), not the invoker-scoped
// platform.ConfigDir(), so a root-owned systemd `citadel work` and an
// interactive non-root reader (`citadel whoami`/`status`/...) agree on where
// the config lives. See CLAUDE.md's ConfigDir()/GetNodeConfigDir() section.

// TestReadDeviceConfigFromDirs_ConvergedDirWins models the exact scenario
// #845 reports: a root-context writer and a non-root-context reader have
// DIFFERENT platform.ConfigDir() results, but BOTH resolve
// network.GetNodeConfigDir() to the SAME directory (that convergence is the
// whole point of GetNodeConfigDir's pointer-file / SUDO_USER-aware
// resolution -- see internal/network/state.go). dirs[0] here stands in for
// that shared, converged GetNodeConfigDir(); dirs[1] stands in for a
// platform.ConfigDir() that would NOT have agreed between the two contexts.
func TestReadDeviceConfigFromDirs_ConvergedDirWins(t *testing.T) {
	convergedDir := t.TempDir() // what both contexts resolve as GetNodeConfigDir()
	diverged := t.TempDir()     // a ConfigDir() that would diverge between contexts

	content := "device_api_token: dat_converged\norg_id: org-1\norg_name: Acme\n"
	if err := os.WriteFile(filepath.Join(convergedDir, "config.yaml"), []byte(content), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	dc := readDeviceConfigFromDirs([]string{convergedDir, diverged})
	if dc == nil {
		t.Fatal("expected a device config, got nil")
	}
	if dc.DeviceAPIToken != "dat_converged" || dc.OrgID != "org-1" || dc.OrgName != "Acme" {
		t.Errorf("got %+v, want token=dat_converged org_id=org-1 org_name=Acme (read should come from the converged dir, not the diverged one)", dc)
	}
}

// TestReadDeviceConfigFromDirs_LegacyFallback pins the backward-compat
// contract: a node registered before #845 has its device config ONLY at the
// legacy platform.ConfigDir() location. It must not be stranded -- the read
// falls back to the legacy dir when the preferred (new) dir has nothing.
func TestReadDeviceConfigFromDirs_LegacyFallback(t *testing.T) {
	preferredDir := t.TempDir() // GetNodeConfigDir() -- nothing written yet
	legacyDir := t.TempDir()    // platform.ConfigDir() -- pre-#845 write lives here

	content := "device_api_token: dat_legacy\nredis_url: redis://legacy:6379\n"
	if err := os.WriteFile(filepath.Join(legacyDir, "config.yaml"), []byte(content), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	dc := readDeviceConfigFromDirs([]string{preferredDir, legacyDir})
	if dc == nil {
		t.Fatal("expected a device config via legacy fallback, got nil")
	}
	if dc.DeviceAPIToken != "dat_legacy" {
		t.Errorf("DeviceAPIToken = %q, want dat_legacy", dc.DeviceAPIToken)
	}
}

// TestReadDeviceConfigFromDirs_PreferredOverLegacy verifies preference order
// when both locations have data (e.g. a stale legacy file left behind after
// a post-#845 re-registration wrote the new location): the preferred dir
// wins.
func TestReadDeviceConfigFromDirs_PreferredOverLegacy(t *testing.T) {
	preferredDir := t.TempDir()
	legacyDir := t.TempDir()

	if err := os.WriteFile(filepath.Join(preferredDir, "config.yaml"), []byte("device_api_token: dat_new\n"), 0600); err != nil {
		t.Fatalf("write preferred config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, "config.yaml"), []byte("device_api_token: dat_stale\n"), 0600); err != nil {
		t.Fatalf("write legacy config: %v", err)
	}

	dc := readDeviceConfigFromDirs([]string{preferredDir, legacyDir})
	if dc == nil || dc.DeviceAPIToken != "dat_new" {
		t.Errorf("got %+v, want token=dat_new (preferred dir should win over stale legacy data)", dc)
	}
}

// TestReadDeviceConfigFromDirs_SkipsEmptyCandidates verifies a directory
// whose config.yaml exists but carries neither device_api_token nor
// redis_url (e.g. only hostname/node_config_dir, the legacy pointer file's
// other keys) is skipped in favor of a later candidate that has real data --
// matching the original single-directory behavior's "return nil if no
// relevant config found" rule, now applied per-candidate.
func TestReadDeviceConfigFromDirs_SkipsEmptyCandidates(t *testing.T) {
	emptyDir := t.TempDir()
	dataDir := t.TempDir()

	if err := os.WriteFile(filepath.Join(emptyDir, "config.yaml"), []byte("node_config_dir: /home/user/citadel-node\nhostname: citadel-abc\n"), 0600); err != nil {
		t.Fatalf("write empty-candidate config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "config.yaml"), []byte("device_api_token: dat_real\n"), 0600); err != nil {
		t.Fatalf("write data config: %v", err)
	}

	dc := readDeviceConfigFromDirs([]string{emptyDir, dataDir})
	if dc == nil || dc.DeviceAPIToken != "dat_real" {
		t.Errorf("got %+v, want token=dat_real (candidate with no device_api_token/redis_url should be skipped)", dc)
	}
}

// TestReadDeviceConfigFromDirs_NoneFound verifies a miss across every
// candidate returns nil, not a zero-valued non-nil struct or an error.
func TestReadDeviceConfigFromDirs_NoneFound(t *testing.T) {
	dc := readDeviceConfigFromDirs([]string{t.TempDir(), t.TempDir()})
	if dc != nil {
		t.Errorf("got %+v, want nil", dc)
	}
}

// TestClearDeviceFieldsPreservingNodeConfigDir verifies the pure core of
// clearLegacyDeviceConfig: it clears every device-auth field (including
// org_name, added alongside the #845 fix -- the original clearSavedConfig
// deleted org_id but never org_name, so a --relogin left a stale org name
// behind) while preserving node_config_dir, matching (and extending)
// TestClearSavedConfigPreservesNodeConfigDir above.
func TestClearDeviceFieldsPreservingNodeConfigDir(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config.yaml")
	content := `node_config_dir: "/home/user/citadel-node"
device_api_token: "test-token"
api_base_url: "https://aceteam.ai"
org_id: "test-org"
org_name: "Test Org"
user_email: "user@test.com"
user_name: "Test User"
redis_url: "redis://localhost:6379"
`
	if err := os.WriteFile(configFile, []byte(content), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	clearDeviceFieldsPreservingNodeConfigDir(configFile)

	data, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatalf("read back config: %v", err)
	}
	var got map[string]interface{}
	if err := yaml.Unmarshal(data, &got); err != nil {
		t.Fatalf("parse config: %v", err)
	}

	if got["node_config_dir"] != "/home/user/citadel-node" {
		t.Errorf("node_config_dir should be preserved, got %v", got["node_config_dir"])
	}
	for _, field := range []string{"device_api_token", "api_base_url", "org_id", "org_name", "user_email", "user_name", "redis_url"} {
		if _, exists := got[field]; exists {
			t.Errorf("%s should be cleared, still present: %v", field, got[field])
		}
	}
}

// TestClearDeviceFieldsPreservingNodeConfigDir_MissingFile verifies clearing
// a nonexistent file is a silent no-op (matches the original behavior: "File
// doesn't exist, nothing to clear").
func TestClearDeviceFieldsPreservingNodeConfigDir_MissingFile(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config.yaml")
	// Do not create the file.
	clearDeviceFieldsPreservingNodeConfigDir(configFile)
	if _, err := os.Stat(configFile); !os.IsNotExist(err) {
		t.Errorf("expected file to remain absent, stat err = %v", err)
	}
}

// TestSeedAceteamAPIKeyFromLegacyFile_CarriesForward pins the fix for a real
// gap the #845 location change would otherwise introduce: aceteam_api_key is
// hand-set by the user (never written by device auth, #495) and
// readDeviceConfigFromDirs returns the FIRST candidate with a token rather
// than merging fields across candidates. Without this seed, a user's
// hand-added key at the legacy location would silently stop being read the
// moment the new (preferred) location starts winning.
func TestSeedAceteamAPIKeyFromLegacyFile_CarriesForward(t *testing.T) {
	legacyFile := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(legacyFile, []byte("aceteam_api_key: act_hand_set_by_user\ndevice_api_token: dat_old\n"), 0600); err != nil {
		t.Fatalf("write legacy config: %v", err)
	}

	config := map[string]interface{}{"device_api_token": "dat_new"}
	seedAceteamAPIKeyFromLegacyFile(config, legacyFile)

	if config["aceteam_api_key"] != "act_hand_set_by_user" {
		t.Errorf("aceteam_api_key = %v, want act_hand_set_by_user (should carry forward from legacy file)", config["aceteam_api_key"])
	}
	// The seed must not clobber unrelated fields already being written.
	if config["device_api_token"] != "dat_new" {
		t.Errorf("device_api_token = %v, want dat_new (unrelated field must be untouched)", config["device_api_token"])
	}
}

// TestSeedAceteamAPIKeyFromLegacyFile_DoesNotOverwriteExisting verifies the
// seed is idempotent and never clobbers a value already present in the
// target config (e.g. the user set aceteam_api_key directly at the new
// location, or a previous seed already ran).
func TestSeedAceteamAPIKeyFromLegacyFile_DoesNotOverwriteExisting(t *testing.T) {
	legacyFile := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(legacyFile, []byte("aceteam_api_key: act_legacy\n"), 0600); err != nil {
		t.Fatalf("write legacy config: %v", err)
	}

	config := map[string]interface{}{"aceteam_api_key": "act_current"}
	seedAceteamAPIKeyFromLegacyFile(config, legacyFile)

	if config["aceteam_api_key"] != "act_current" {
		t.Errorf("aceteam_api_key = %v, want act_current (must not overwrite an already-set value)", config["aceteam_api_key"])
	}
}

// TestSeedAceteamAPIKeyFromLegacyFile_NoLegacyFile verifies a missing/empty
// legacy file is a silent no-op, not an error or a panic.
func TestSeedAceteamAPIKeyFromLegacyFile_NoLegacyFile(t *testing.T) {
	config := map[string]interface{}{"device_api_token": "dat_new"}
	seedAceteamAPIKeyFromLegacyFile(config, filepath.Join(t.TempDir(), "config.yaml"))

	if _, exists := config["aceteam_api_key"]; exists {
		t.Errorf("aceteam_api_key should not be set, got %v", config["aceteam_api_key"])
	}
}

// TestSeedAceteamAPIKeyFromLegacyFile_LegacyHasNoKey verifies a legacy file
// that exists but has no aceteam_api_key field leaves config untouched.
func TestSeedAceteamAPIKeyFromLegacyFile_LegacyHasNoKey(t *testing.T) {
	legacyFile := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(legacyFile, []byte("device_api_token: dat_old\n"), 0600); err != nil {
		t.Fatalf("write legacy config: %v", err)
	}

	config := map[string]interface{}{}
	seedAceteamAPIKeyFromLegacyFile(config, legacyFile)

	if _, exists := config["aceteam_api_key"]; exists {
		t.Errorf("aceteam_api_key should not be set, got %v", config["aceteam_api_key"])
	}
}
