package jobs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	citadelconfig "github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

// TestApplyCobrowsePublicOnlyOnlyTightens: remote config may enable the
// public-only guard, never disable it, and never rewrites a damaged file.
func TestApplyCobrowsePublicOnlyOnlyTightens(t *testing.T) {
	dir := t.TempDir()
	p := citadelconfig.DefaultPermissions()
	p.Files = true
	if err := citadelconfig.SavePermissions(dir, p); err != nil {
		t.Fatal(err)
	}
	if msg := applyCobrowsePublicOnly(dir, true); !strings.Contains(msg, "enabled") {
		t.Fatalf("enable: %q", msg)
	}
	got := citadelconfig.LoadPermissions(dir)
	if !got.CobrowsePublicOnly || !got.Files {
		t.Fatalf("after enable: %+v", got)
	}
	if msg := applyCobrowsePublicOnly(dir, false); !strings.Contains(msg, "remote request ignored") {
		t.Fatalf("disable: %q", msg)
	}
	if !citadelconfig.LoadPermissions(dir).CobrowsePublicOnly {
		t.Fatal("a remote request turned the guard off")
	}

	bad := t.TempDir()
	path := filepath.Join(bad, "permissions.yaml")
	if err := os.WriteFile(path, []byte("files: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if msg := applyCobrowsePublicOnly(bad, true); !strings.Contains(msg, "Warning") {
		t.Fatalf("damaged file: %q", msg)
	}
	if data, _ := os.ReadFile(path); string(data) != "files: [\n" {
		t.Fatalf("damaged file rewritten: %q", data)
	}
}

func TestApplyDeviceConfig_CobrowsePublicOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SUDO_USER", "")
	workerDir := filepath.Join(home, "resolved-node")
	if err := os.MkdirAll(workerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	h := NewConfigHandler(workerDir)
	h.PermissionsDir = workerDir
	run := func(v any) string {
		payload, _ := json.Marshal(map[string]any{"cobrowsePublicOnly": v})
		out, err := h.Execute(JobContext{}, &nexus.Job{Type: "APPLY_DEVICE_CONFIG", Payload: map[string]string{"config": string(payload)}})
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		return string(out)
	}
	run(true)
	if !citadelconfig.LoadPermissions(workerDir).CobrowsePublicOnly {
		t.Fatal("cobrowsePublicOnly=true was not applied")
	}
	if out := run(false); !strings.Contains(out, "remote request ignored") {
		t.Fatalf("disable result: %q", out)
	}
	if !citadelconfig.LoadPermissions(workerDir).CobrowsePublicOnly {
		t.Fatal("cobrowsePublicOnly=false loosened the guard remotely")
	}

	var c DeviceConfig
	if err := json.Unmarshal([]byte(`{"deviceName":"n"}`), &c); err != nil || c.CobrowsePublicOnly != nil {
		t.Fatalf("absent field must be nil: %+v %v", c.CobrowsePublicOnly, err)
	}
}
