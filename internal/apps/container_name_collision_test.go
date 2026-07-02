package apps

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestWhatsAppBridgeNoHardcodedContainerName is the container-NAME analogue of
// TestHostPortNoCollisions (the host-PORT case in hostport_collision_test.go).
//
// Docker container names are GLOBAL, so a hardcoded `container_name:` in a module
// compose collides ("The container name ... is already in use") on any node that
// already runs a stack with that name -- a dev stack, a second tenant, or a
// half-succeeded provision -- and permanently wedges `citadel whatsapp up` /
// WHATSAPP_PROVISION on that node (aceteam-ai/citadel-cli#436,
// sunapi386/whatsapp-bridge#4).
//
// The fix removes the hardcoded names and relies on the compose PROJECT prefix
// (`docker compose -p <project>` yields `<project>-<service>-<index>`). This test
// guards that invariant against regression and proves the coexistence property:
//
//  1. the WhatsApp bridge module compose declares NO `container_name` on any
//     service (otherwise the project prefix is bypassed and the collision is
//     back), and
//  2. two distinct compose projects derive DISTINCT container names for every
//     service, so a citadel-provisioned stack can run alongside another bridge
//     (e.g. a dev `watest` stack) on the same node without a name conflict.
//
// The testdata compose is a committed copy of the module's citadel/compose.yml
// (sunapi386/whatsapp-bridge). It is intentionally NOT the citadel binary's own
// embedded services (the bridge is a ToS-gray external module the CLI never
// embeds); the copy lets this test pin the coupled contract from the CLI side.
func TestWhatsAppBridgeNoHardcodedContainerName(t *testing.T) {
	path := filepath.Join("testdata", "whatsapp-bridge.compose.yml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	var cf struct {
		Services map[string]struct {
			ContainerName string `yaml:"container_name"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(data, &cf); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if len(cf.Services) == 0 {
		t.Fatalf("%s declares no services; testdata is stale", path)
	}

	// 1. No service may hardcode a container_name.
	for svc, spec := range cf.Services {
		if spec.ContainerName != "" {
			t.Errorf("service %q hardcodes container_name %q; container names are global, so this collides on any node already running a bridge stack. Remove it and rely on the `docker compose -p <project>` prefix.", svc, spec.ContainerName)
		}
	}

	// 2. Coexistence: two distinct projects must derive distinct container names
	// for every service. This mirrors how a citadel-provisioned stack (its pinned
	// project, "services") coexists with a dev stack (project "watest").
	const projA, projB = "services", "watest"
	seen := map[string]string{} // derived container name -> owner "project/service"
	claim := func(project, svc string) {
		t.Helper()
		name := derivedContainerName(project, svc)
		owner := project + "/" + svc
		if prev, taken := seen[name]; taken {
			t.Errorf("derived container name %q claimed by both %q and %q -- projects do not namespace containers", name, prev, owner)
			return
		}
		seen[name] = owner
	}
	for svc := range cf.Services {
		claim(projA, svc)
		claim(projB, svc)
	}
}

// derivedContainerName mirrors how docker compose (v2) names a service's
// container when no explicit container_name is set: "<project>-<service>-<index>"
// (index 1 for a single, unscaled replica).
func derivedContainerName(project, service string) string {
	return fmt.Sprintf("%s-%s-1", project, service)
}
