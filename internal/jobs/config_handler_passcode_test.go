package jobs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	citadelconfig "github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

// TestDeviceConfig_SensitivePermsUnmarshal verifies the new sensitive-surface
// fields carry absent(nil)-vs-explicit pointer semantics, matching the meeting
// fields — so applying a device config that omits them never silently flips a
// surface.
func TestDeviceConfig_SensitivePermsUnmarshal(t *testing.T) {
	var c DeviceConfig
	if err := json.Unmarshal([]byte(`{"deviceName":"n"}`), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c.ConsoleEnabled != nil || c.DesktopEnabled != nil || c.FilesEnabled != nil || c.NodePasscode != nil {
		t.Errorf("absent sensitive fields must be nil, got %+v", c)
	}

	if err := json.Unmarshal([]byte(`{"consoleEnabled":true,"nodePasscode":"1379"}`), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c.ConsoleEnabled == nil || !*c.ConsoleEnabled {
		t.Error("explicit consoleEnabled:true should be non-nil true")
	}
	if c.NodePasscode == nil || *c.NodePasscode != "1379" {
		t.Error("nodePasscode should round-trip")
	}
}

// TestApplyDeviceConfig_EnablesConsoleAndSetsPasscode is the programmatic opt-in
// path (aceteam#6524): APPLY_DEVICE_CONFIG with consoleEnabled + nodePasscode
// must write permissions.yaml so console is enabled and the passcode verifies —
// without storing the plaintext PIN.
func TestApplyDeviceConfig_EnablesConsoleAndSetsPasscode(t *testing.T) {
	dir := t.TempDir()
	// platform.ConfigDir() (where permissions.yaml is written) resolves via HOME.
	t.Setenv("HOME", dir)
	t.Setenv("SUDO_USER", "")
	cfgDir := filepath.Join(dir, ".citadel-cli")
	if err := os.MkdirAll(cfgDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte("node:\n  name: t\n"), 0600); err != nil {
		t.Fatalf("marker: %v", err)
	}

	// Sanity: a fresh node starts locked down.
	if p := citadelconfig.LoadPermissions(platform.ConfigDir()); p.Console || p.HasPasscode() {
		t.Fatalf("precondition: fresh node should be locked down, got %+v", p)
	}

	// Manifest dir for the handler (separate from the per-concern config dir).
	manifestDir := filepath.Join(dir, "citadel-node")
	if err := os.MkdirAll(manifestDir, 0755); err != nil {
		t.Fatalf("mkdir manifest: %v", err)
	}

	h := NewConfigHandler(manifestDir)
	payload, _ := json.Marshal(map[string]any{
		"deviceName":     "peters-macbook",
		"consoleEnabled": true,
		"nodePasscode":   "1379",
	})
	job := &nexus.Job{
		Type:    "APPLY_DEVICE_CONFIG",
		Payload: map[string]string{"config": string(payload)},
	}
	if _, err := h.Execute(JobContext{}, job); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	p := citadelconfig.LoadPermissions(platform.ConfigDir())
	if !p.Console {
		t.Error("console should be enabled after APPLY_DEVICE_CONFIG")
	}
	if !p.VerifyPasscode("1379") {
		t.Error("passcode should verify after APPLY_DEVICE_CONFIG")
	}
	if p.Desktop || p.Files {
		t.Error("desktop/files should remain disabled (only console was pushed)")
	}

	// The plaintext PIN must never be written to disk.
	raw, err := os.ReadFile(filepath.Join(cfgDir, "permissions.yaml"))
	if err != nil {
		t.Fatalf("read perms: %v", err)
	}
	if containsSub(string(raw), "1379") {
		t.Error("permissions.yaml must not contain the plaintext PIN")
	}
}

func containsSub(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
