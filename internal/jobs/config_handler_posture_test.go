package jobs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	citadelconfig "github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

// postureHandler is an APPLY_DEVICE_CONFIG handler over a fresh node dir whose
// permissions file is base, with HOME sandboxed (meeting.yaml lives in
// platform.ConfigDir()).
func postureHandler(t *testing.T, base *citadelconfig.Permissions) (*ConfigHandler, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SUDO_USER", "")
	dir := filepath.Join(home, "node")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if base != nil {
		if err := citadelconfig.SavePermissions(dir, base); err != nil {
			t.Fatal(err)
		}
	}
	h := NewConfigHandler(dir)
	h.PermissionsDir = dir
	return h, dir
}

func runDeviceConfig(t *testing.T, h *ConfigHandler, cfg map[string]any) string {
	t.Helper()
	payload, _ := json.Marshal(cfg)
	out, err := h.Execute(JobContext{}, &nexus.Job{Type: "APPLY_DEVICE_CONFIG", Payload: map[string]string{"config": string(payload)}})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return string(out)
}

func setupPosturePerms() *citadelconfig.Permissions {
	return &citadelconfig.Permissions{CobrowsePublicOnly: true, SetupPosture: citadelconfig.SetupPostureCobrowseOnly}
}

// TestApplyDeviceConfigRespectsSetupPosture: on a setup-page PC no remote
// config turns Console, Desktop, Files or Shell on or sets a passcode, and
// each refusal is in the job result. Turning a surface off still works.
func TestApplyDeviceConfigRespectsSetupPosture(t *testing.T) {
	for _, c := range []struct{ key, label string }{
		{"consoleEnabled", "Console"},
		{"desktopEnabled", "Desktop"},
		{"filesEnabled", "Files"},
		{"shellEnabled", "Shell"},
	} {
		t.Run(c.key, func(t *testing.T) {
			h, dir := postureHandler(t, setupPosturePerms())
			out := runDeviceConfig(t, h, map[string]any{"deviceName": "pc", c.key: true})
			if !strings.Contains(out, "Refused: "+c.label) {
				t.Fatalf("no refusal line for %s: %q", c.key, out)
			}
			p, err := citadelconfig.LoadPermissionsStrict(dir)
			if err != nil {
				t.Fatal(err)
			}
			if p.Console || p.Desktop || p.Files || p.Shell || p.SetupPosture == "" || !p.CobrowsePublicOnly {
				t.Fatalf("%s=true changed the posture: %+v", c.key, p)
			}
		})
	}
	t.Run("passcode", func(t *testing.T) {
		h, dir := postureHandler(t, setupPosturePerms())
		out := runDeviceConfig(t, h, map[string]any{"deviceName": "pc", "nodePasscode": "2468"})
		if !strings.Contains(out, "Refused: the node passcode") {
			t.Fatalf("no refusal line: %q", out)
		}
		if p, _ := citadelconfig.LoadPermissionsStrict(dir); p.HasPasscode() {
			t.Fatal("a remote passcode push was applied")
		}
	})
	t.Run("turning off still applies", func(t *testing.T) {
		base := setupPosturePerms()
		base.Files, base.Shell = true, true // the human allowed them on the PC
		h, dir := postureHandler(t, base)
		out := runDeviceConfig(t, h, map[string]any{"deviceName": "pc", "filesEnabled": false, "shellEnabled": false})
		if strings.Contains(out, "Refused") {
			t.Fatalf("turning off was refused: %q", out)
		}
		p, _ := citadelconfig.LoadPermissionsStrict(dir)
		if p.Files || p.Shell || p.SetupPosture == "" {
			t.Fatalf("turning off did not apply, or dropped the marker: %+v", p)
		}
	})
	t.Run("meeting, relay, services and vnc", func(t *testing.T) {
		h, dir := postureHandler(t, setupPosturePerms())
		out := runDeviceConfig(t, h, map[string]any{
			"deviceName": "pc", "meetingEnabled": true, "egressRelay": true, "egressAllowLan": true,
			"defaultServe": true, "services": []string{"ollama"}, "autoStartServices": true,
		})
		for _, want := range []string{"the meeting capability", "the egress relay", "egress relay LAN destinations", "default-serve", "services"} {
			if !strings.Contains(out, "Refused: "+want) {
				t.Errorf("no refusal for %s: %q", want, out)
			}
		}
		if citadelconfig.LoadMeeting(platform.ConfigDir()).MeetingEnabled && fileExists(filepath.Join(platform.ConfigDir(), "meeting.yaml")) {
			t.Error("the meeting capability was turned on")
		}
		if b, _ := os.ReadFile(filepath.Join(dir, "citadel.yaml")); strings.Contains(string(b), "ollama") {
			t.Errorf("a service was added to the manifest:\n%s", b)
		}
	})
	t.Run("damaged posture file is never rewritten", func(t *testing.T) {
		h, dir := postureHandler(t, nil)
		bad := []byte("setup_posture: cobrowse_only\nfiles: [false\n")
		if err := os.WriteFile(filepath.Join(dir, "permissions.yaml"), bad, 0o600); err != nil {
			t.Fatal(err)
		}
		out := runDeviceConfig(t, h, map[string]any{"deviceName": "pc", "filesEnabled": false, "shellEnabled": true})
		if !strings.Contains(out, "could not be read") {
			t.Fatalf("no refusal: %q", out)
		}
		if b, _ := os.ReadFile(filepath.Join(dir, "permissions.yaml")); string(b) != string(bad) {
			t.Fatalf("a damaged posture file was rewritten: %q", b)
		}
	})
}

// TestApplyDeviceConfigFleetUnchanged: without the marker, every surface,
// the passcode and the services behave as before.
func TestApplyDeviceConfigFleetUnchanged(t *testing.T) {
	h, dir := postureHandler(t, citadelconfig.DefaultPermissions())
	out := runDeviceConfig(t, h, map[string]any{
		"deviceName": "fleet", "consoleEnabled": true, "desktopEnabled": true, "filesEnabled": true,
		"shellEnabled": true, "nodePasscode": "2468", "meetingEnabled": true,
	})
	if strings.Contains(out, "Refused") {
		t.Fatalf("a fleet node refused remote config: %q", out)
	}
	p, err := citadelconfig.LoadPermissionsStrict(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Console || !p.Desktop || !p.Files || !p.Shell || !p.VerifyLegacyPasscode("2468") || p.SetupPosture != "" {
		t.Fatalf("fleet permissions: %+v", p)
	}
	// A damaged file with no marker keeps main's behavior: a warning, the file
	// left as it is, and no posture refusal.
	h2, dir2 := postureHandler(t, nil)
	if err := os.WriteFile(filepath.Join(dir2, "permissions.yaml"), []byte("files: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out = runDeviceConfig(t, h2, map[string]any{"deviceName": "fleet", "filesEnabled": true})
	if strings.Contains(out, "Refused") || !strings.Contains(out, "permissions not changed") {
		t.Fatalf("a damaged fleet file changed behavior: %q", out)
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// TestSetupPostureFilterDropsEveryEnable checks the filter itself: every
// enabling field is dropped, every disabling field is kept.
func TestSetupPostureFilterDropsEveryEnable(t *testing.T) {
	dir := t.TempDir()
	if err := citadelconfig.SavePermissions(dir, setupPosturePerms()); err != nil {
		t.Fatal(err)
	}
	on, off, pin := true, false, "1"
	c := DeviceConfig{
		ConsoleEnabled: &on, DesktopEnabled: &on, FilesEnabled: &on, ShellEnabled: &off,
		NodePasscode: &pin, MeetingEnabled: &on, EgressRelay: &on, EgressAllowLan: &on,
		DefaultServe: &on, VNCEnabled: true, Services: []string{"ollama"}, AutoStartServices: true,
		AudioBackupEnabled: &off,
	}
	applySetupPostureFilter(dir, &c)
	if c.ConsoleEnabled != nil || c.DesktopEnabled != nil || c.FilesEnabled != nil || c.NodePasscode != nil ||
		c.MeetingEnabled != nil || c.EgressRelay != nil || c.EgressAllowLan != nil || c.DefaultServe != nil ||
		c.VNCEnabled || c.Services != nil || c.AutoStartServices {
		t.Fatalf("an enabling field survived: %+v", c)
	}
	if c.ShellEnabled == nil || *c.ShellEnabled || c.AudioBackupEnabled == nil {
		t.Fatalf("a disabling field was dropped: %+v", c)
	}
	// No marker: nothing is touched.
	fleet := t.TempDir()
	c2 := DeviceConfig{FilesEnabled: &on, NodePasscode: &pin, Services: []string{"ollama"}}
	if out := applySetupPostureFilter(fleet, &c2); out != "" || c2.FilesEnabled == nil || c2.NodePasscode == nil || c2.Services == nil {
		t.Fatalf("a fleet config was filtered: %q %+v", out, c2)
	}
}
