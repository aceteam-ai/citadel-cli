package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDeviceCreds(t *testing.T) {
	dir := t.TempDir()
	yaml := "device_api_token: dat_abc123\napi_base_url: https://aceteam.ai\norg_id: 00000000-0000-0000-0000-000000000000\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yaml), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	c := LoadDeviceCreds(dir)
	if c.Token != "dat_abc123" {
		t.Errorf("Token = %q, want dat_abc123", c.Token)
	}
	if c.APIBaseURL != "https://aceteam.ai" {
		t.Errorf("APIBaseURL = %q, want https://aceteam.ai", c.APIBaseURL)
	}
}

func TestLoadDeviceCreds_MissingFile(t *testing.T) {
	c := LoadDeviceCreds(t.TempDir())
	if c.Token != "" || c.APIBaseURL != "" {
		t.Errorf("missing file should yield empty creds, got %+v", c)
	}
}

func TestLoadDeviceCreds_PartialFile(t *testing.T) {
	dir := t.TempDir()
	// Token present, base URL absent (older config): caller falls back to its
	// own default for the base URL.
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("device_api_token: dat_only\n"), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	c := LoadDeviceCreds(dir)
	if c.Token != "dat_only" {
		t.Errorf("Token = %q, want dat_only", c.Token)
	}
	if c.APIBaseURL != "" {
		t.Errorf("APIBaseURL should be empty, got %q", c.APIBaseURL)
	}
}

func TestLoadDeviceCreds_InvalidYAML(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("not: [valid: yaml: {{{"), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	c := LoadDeviceCreds(dir)
	if c.Token != "" {
		t.Errorf("invalid yaml should yield empty creds, got %+v", c)
	}
}

// TestLoadDeviceCredsFromDirs_ConvergedDirWins simulates citadel-cli#845's
// core scenario: a root-owned systemd `citadel work` and an interactive
// non-root reader have DIFFERENT platform.ConfigDir() results, but BOTH
// resolve network.GetNodeConfigDir() to the SAME machine-convergent
// directory (that is the whole point of GetNodeConfigDir's pointer-file /
// SUDO_USER-aware resolution). Modeling that: dirs[0] stands in for the
// shared GetNodeConfigDir() both contexts agree on; dirs[1] stands in for a
// platform.ConfigDir() that would have diverged between them. A config
// written to dirs[0] (the converged dir) must be found even though dirs[1]
// (what a naive invoker-scoped read would have used) has nothing.
func TestLoadDeviceCredsFromDirs_ConvergedDirWins(t *testing.T) {
	convergedDir := t.TempDir() // what both contexts resolve as GetNodeConfigDir()
	diverged := t.TempDir()     // a ConfigDir() that would NOT agree between contexts

	yaml := "device_api_token: dat_converged\napi_base_url: https://aceteam.ai\n"
	if err := os.WriteFile(filepath.Join(convergedDir, "config.yaml"), []byte(yaml), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	// diverged has nothing written to it -- e.g. the root writer's
	// ConfigDir() never got a config.yaml because the write went to
	// GetNodeConfigDir() instead (post-#845 behavior).

	c := loadDeviceCredsFromDirs([]string{convergedDir, diverged})
	if c.Token != "dat_converged" {
		t.Errorf("Token = %q, want dat_converged (should read from the converged GetNodeConfigDir, not the diverged ConfigDir)", c.Token)
	}
}

// TestLoadDeviceCredsFromDirs_LegacyFallback pins the backward-compat path:
// a node registered before #845 has its device config ONLY at the legacy
// platform.ConfigDir() location (nothing at the new, preferred
// GetNodeConfigDir() location yet, since it hasn't re-run citadel
// init/login/reauth). The converged loader must still find it via fallback,
// so such a node is never stranded.
func TestLoadDeviceCredsFromDirs_LegacyFallback(t *testing.T) {
	preferredDir := t.TempDir() // GetNodeConfigDir() -- empty, nothing written here yet
	legacyDir := t.TempDir()    // platform.ConfigDir() -- pre-#845 write lives here

	yaml := "device_api_token: dat_legacy\napi_base_url: https://aceteam.ai\n"
	if err := os.WriteFile(filepath.Join(legacyDir, "config.yaml"), []byte(yaml), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	c := loadDeviceCredsFromDirs([]string{preferredDir, legacyDir})
	if c.Token != "dat_legacy" {
		t.Errorf("Token = %q, want dat_legacy (legacy-path fallback should have found it)", c.Token)
	}
}

// TestLoadDeviceCredsFromDirs_PreferredOverLegacy verifies preference order
// when BOTH locations have data (e.g. a stale legacy file left behind after
// the node re-registered post-#845): the preferred (new, converged)
// directory wins over the legacy one.
func TestLoadDeviceCredsFromDirs_PreferredOverLegacy(t *testing.T) {
	preferredDir := t.TempDir()
	legacyDir := t.TempDir()

	if err := os.WriteFile(filepath.Join(preferredDir, "config.yaml"), []byte("device_api_token: dat_new\n"), 0600); err != nil {
		t.Fatalf("write preferred config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, "config.yaml"), []byte("device_api_token: dat_stale\n"), 0600); err != nil {
		t.Fatalf("write legacy config: %v", err)
	}

	c := loadDeviceCredsFromDirs([]string{preferredDir, legacyDir})
	if c.Token != "dat_new" {
		t.Errorf("Token = %q, want dat_new (preferred dir should win over stale legacy data)", c.Token)
	}
}

// TestLoadDeviceCredsFromDirs_NeitherHasData verifies the empty-list-of-hits
// case returns zero-valued creds rather than panicking or erroring.
func TestLoadDeviceCredsFromDirs_NeitherHasData(t *testing.T) {
	c := loadDeviceCredsFromDirs([]string{t.TempDir(), t.TempDir()})
	if c.Token != "" {
		t.Errorf("Token = %q, want empty", c.Token)
	}
}

// TestLoadDeviceCredsConverged_UsesHook verifies LoadDeviceCredsConverged
// routes through DeviceConfigDirsHook (the leaf-safe indirection cmd wires at
// init -- internal/config must not import internal/network directly; see
// cmd/devicecreds_hooks.go) rather than resolving directories itself.
func TestLoadDeviceCredsConverged_UsesHook(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("device_api_token: dat_via_hook\n"), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	prev := DeviceConfigDirsHook
	t.Cleanup(func() { DeviceConfigDirsHook = prev })
	DeviceConfigDirsHook = func() []string { return []string{dir} }

	c := LoadDeviceCredsConverged()
	if c.Token != "dat_via_hook" {
		t.Errorf("Token = %q, want dat_via_hook", c.Token)
	}
}

// TestLoadDeviceCredsConverged_NilHookFallsBackToConfigDir verifies a nil
// hook (a caller that never went through cmd.Execute, e.g. a standalone test
// binary) degrades to platform.ConfigDir() alone rather than panicking --
// the pre-#845, leaf-safe behavior.
func TestLoadDeviceCredsConverged_NilHookFallsBackToConfigDir(t *testing.T) {
	prev := DeviceConfigDirsHook
	t.Cleanup(func() { DeviceConfigDirsHook = prev })
	DeviceConfigDirsHook = nil

	// Should not panic with a nil hook. The actual directory it resolves to
	// (platform.ConfigDir()) is exercised by platform's own tests; here we
	// only need "doesn't crash, returns zero-valued creds when nothing is
	// there" -- LoadDeviceCreds handles the missing-file case already.
	c := LoadDeviceCredsConverged()
	_ = c // no assertion on Token: platform.ConfigDir() is environment-dependent
}
