package jobs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

func TestContainerDevicePermissionIsOwnerLocalAndRemotelyRevocable(t *testing.T) {
	dir := t.TempDir()
	p := config.DefaultPermissions()
	p.ContainerRuntime = "runsc"
	p.ContainerEgressProxyURL = "http://host.docker.internal:3128"
	yes, no := true, false
	for _, local := range []bool{false, true} {
		p.ContainerHost = local
		if config.SavePermissions(dir, p) != nil {
			t.Fatal("fixture")
		}
		before, err := os.ReadFile(filepath.Join(dir, "permissions.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if persistContainerHostPermission(dir, &yes) == nil {
			t.Fatal("device config enabled owner-local hosting")
		}
		after, err := os.ReadFile(filepath.Join(dir, "permissions.yaml"))
		if err != nil || string(before) != string(after) {
			t.Fatal("refused enable changed local policy")
		}
		var omitted DeviceConfig
		if json.Unmarshal([]byte(`{"containerEgressProxyURL":"http://device-controlled.invalid:3128"}`), &omitted) != nil {
			t.Fatal("fixture")
		}
		if persistContainerHostPermission(dir, omitted.ContainerHostEnabled) != nil || config.LoadPermissions(dir).ContainerHost != local {
			t.Fatal("omission changed owner policy")
		}
		if persistContainerHostPermission(dir, &no) != nil {
			t.Fatal("revocation failed")
		}
		got := config.LoadPermissions(dir)
		if got.ContainerHost || got.ContainerRuntime != "runsc" || got.ContainerEgressProxyURL != p.ContainerEgressProxyURL || got.Files || got.Shell {
			t.Fatal("revocation broadened another capability")
		}
	}
	if persistContainerHostPermission("", &no) == nil {
		t.Fatal("invoker path fallback allowed")
	}
	if os.Remove(filepath.Join(dir, "permissions.yaml")) != nil || os.Mkdir(filepath.Join(dir, "permissions.yaml"), 0700) != nil {
		t.Fatal("fixture")
	}
	if persistContainerHostPermission(dir, &no) == nil {
		t.Fatal("unreadable permission silently restored")
	}
}

func TestContainerDeviceEnableRefusesBeforeManifestDiscovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-directory")
	if os.WriteFile(path, []byte("owned sentinel"), 0600) != nil {
		t.Fatal("fixture")
	}
	// If the early guard regresses, manifest discovery fails in the owned fixture
	// before any other config/native effect. The expected owner-local refusal then fails.
	handler := &ConfigHandler{ConfigDir: path, PermissionsDir: t.TempDir()}
	_, err := handler.Execute(JobContext{}, &nexus.Job{Type: "APPLY_DEVICE_CONFIG", Payload: map[string]string{"config": `{"containerHostEnabled":true}`}})
	if err == nil || !strings.Contains(err.Error(), "can only be enabled locally") {
		t.Fatal("remote enable reached manifest discovery", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "owned sentinel" {
		t.Fatal("refused enable changed the fixture")
	}
}
