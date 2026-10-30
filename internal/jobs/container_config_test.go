package jobs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/config"
)

func TestContainerDevicePermissionPersistsWithoutNativeConfigEffects(t *testing.T) {
	dir := t.TempDir()
	p := config.DefaultPermissions()
	p.ContainerRuntime = "runsc"
	p.ContainerEgressProxyURL = "http://host.docker.internal:3128"
	if config.SavePermissions(dir, p) != nil {
		t.Fatal("fixture")
	}
	for _, body := range []string{`{"containerHostEnabled":true}`, `{"containerEgressProxyURL":"http://device-controlled.invalid:3128"}`, `{}`, `{"containerHostEnabled":false}`} {
		var value DeviceConfig
		if json.Unmarshal([]byte(body), &value) != nil {
			t.Fatal("fixture")
		}
		if err := persistContainerHostPermission(dir, value.ContainerHostEnabled); err != nil {
			t.Fatal(err)
		}
		p = config.LoadPermissions(dir)
		if p.ContainerRuntime != "runsc" || p.ContainerEgressProxyURL != "http://host.docker.internal:3128" || p.Files || p.Shell {
			t.Fatal("unrelated authority changed")
		}
		if body == `{}` && !p.ContainerHost {
			t.Fatal("omitted value revoked permission")
		}
		if body == `{"containerHostEnabled":true}` && !p.ContainerHost || body == `{"containerHostEnabled":false}` && p.ContainerHost {
			t.Fatal("explicit permission ignored")
		}
	}
	yes := true
	if persistContainerHostPermission("", &yes) == nil {
		t.Fatal("invoker path fallback allowed")
	}
	if os.Remove(filepath.Join(dir, "permissions.yaml")) != nil || os.Mkdir(filepath.Join(dir, "permissions.yaml"), 0700) != nil {
		t.Fatal("fixture")
	}
	if persistContainerHostPermission(dir, &yes) == nil {
		t.Fatal("unreadable permission silently restored")
	}
}
