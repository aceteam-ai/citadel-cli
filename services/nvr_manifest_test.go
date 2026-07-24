package services

import (
	_ "embed"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
	"gopkg.in/yaml.v3"
)

// nvrServiceYAML / nvrComposeYAML are the on-disk module manifest + compose the
// installer will consume. Embedding them makes the test fail if a file is deleted
// or renamed, and lets us parse the manifest through the REAL
// catalog.ServiceManifest struct so a schema mismatch is caught at build time
// rather than at install time on a node.
//
//go:embed nvr-service/service.yaml
var nvrServiceYAML []byte

//go:embed nvr-service/compose.yml
var nvrComposeYAML []byte

// TestNVRServiceManifest validates the nvr module manifest (#597) against the
// catalog schema and the load-bearing invariants: host networking for
// wyze-bridge, the iGPU render device for Frigate, the assignment config vars,
// the published-host-port health check, and the (data-only) gateway block.
func TestNVRServiceManifest(t *testing.T) {
	var m catalog.ServiceManifest
	if err := yaml.Unmarshal(nvrServiceYAML, &m); err != nil {
		t.Fatalf("nvr service.yaml does not parse as a ServiceManifest: %v", err)
	}

	if m.Name != "nvr" {
		t.Errorf("name = %q, want %q", m.Name, "nvr")
	}
	if m.SchemaVersion != catalog.CurrentSchemaVersion {
		t.Errorf("schema_version = %d, want %d", m.SchemaVersion, catalog.CurrentSchemaVersion)
	}

	// Host networking is the #1 non-negotiable gotcha (TUTK discovery timeout
	// otherwise). It must be declared in the sandbox block.
	if !m.Sandbox.HostNetwork {
		t.Errorf("sandbox.host_network must be true — wyze-bridge TUTK P2P needs it or every camera fails discovery")
	}

	// Frigate hardware decode + OpenVINO detection needs the iGPU render node.
	foundDRI := false
	for _, d := range m.Sandbox.Devices {
		if strings.Contains(d, "renderD128") {
			foundDRI = true
		}
	}
	if !foundDRI {
		t.Errorf("sandbox.devices must include /dev/dri/renderD128 for Frigate hw decode; got %v", m.Sandbox.Devices)
	}

	// The assignment config schema must be present. Secrets are required (no
	// default) so an unset value fails the assignment instead of silently falling
	// back.
	cfg := map[string]catalog.ConfigVar{}
	for _, c := range m.Config {
		cfg[c.Name] = c
	}
	for _, secret := range []string{"WYZE_EMAIL", "WYZE_PASSWORD", "API_ID", "API_KEY"} {
		c, ok := cfg[secret]
		if !ok {
			t.Errorf("config is missing required secret %q", secret)
			continue
		}
		if !c.Required {
			t.Errorf("secret %q must be required (no silent default)", secret)
		}
		if c.Default != "" {
			t.Errorf("secret %q must not carry a default value", secret)
		}
	}
	for _, name := range []string{"NVR_RETENTION_DAYS", "NVR_DETECTOR", "NVR_STORAGE_MODE", "NVR_STORAGE_TARGET"} {
		if _, ok := cfg[name]; !ok {
			t.Errorf("config is missing %q", name)
		}
	}

	// Ports: Frigate host 8212 -> container 5000, matching the registry.
	if len(m.Ports) != 1 {
		t.Fatalf("expected exactly 1 published port (frigate), got %d", len(m.Ports))
	}
	if m.Ports[0].Host != FrigateHostPort || m.Ports[0].Container != 5000 {
		t.Errorf("port = host %d -> container %d, want host %d -> container 5000", m.Ports[0].Host, m.Ports[0].Container, FrigateHostPort)
	}

	// Health check must target the PUBLISHED HOST port (ProbeHealth runs on the
	// host), not the container port 5000.
	if !catalog.HasHealthProbe(m.HealthCheck) {
		t.Errorf("health_check is not probeable")
	}
	if m.HealthCheck.Port != FrigateHostPort {
		t.Errorf("health_check.port = %d, want the published HOST port %d", m.HealthCheck.Port, FrigateHostPort)
	}

	// The gateway block is data-only (#598 owns exposure) but must be valid and
	// name the registry's port env.
	if m.Gateway == nil {
		t.Errorf("expected a data-only gateway block (port env declaration for #598)")
	} else {
		if err := m.Gateway.Validate(); err != nil {
			t.Errorf("gateway block does not validate: %v", err)
		}
		if m.Gateway.PortEnv != EnvFrigateHostPort {
			t.Errorf("gateway.port_env = %q, want %q", m.Gateway.PortEnv, EnvFrigateHostPort)
		}
	}
}

// composeShape captures the compose fields the nvr invariants assert on.
type composeShape struct {
	Services map[string]struct {
		NetworkMode string   `yaml:"network_mode"`
		Ports       []string `yaml:"ports"`
		Devices     []string `yaml:"devices"`
		Volumes     []string `yaml:"volumes"`
	} `yaml:"services"`
}

// TestNVRComposeInvariants pins the load-bearing compose scars: wyze-bridge on
// host networking with NO published ports; frigate mapping the iGPU render node,
// publishing the citadel-owned host port, and keeping /config on local disk while
// /media follows the storage target.
func TestNVRComposeInvariants(t *testing.T) {
	var c composeShape
	if err := yaml.Unmarshal(nvrComposeYAML, &c); err != nil {
		t.Fatalf("nvr compose.yml does not parse: %v", err)
	}

	wyze, ok := c.Services["wyze-bridge"]
	if !ok {
		t.Fatalf("compose is missing the wyze-bridge service")
	}
	if wyze.NetworkMode != "host" {
		t.Errorf("wyze-bridge network_mode = %q, want host (TUTK P2P discovery)", wyze.NetworkMode)
	}
	if len(wyze.Ports) != 0 {
		t.Errorf("wyze-bridge must NOT publish ports under host networking; got %v", wyze.Ports)
	}

	frigate, ok := c.Services["frigate"]
	if !ok {
		t.Fatalf("compose is missing the frigate service")
	}
	foundDRI := false
	for _, d := range frigate.Devices {
		if strings.Contains(d, "renderD128") {
			foundDRI = true
		}
	}
	if !foundDRI {
		t.Errorf("frigate must map /dev/dri/renderD128 for hw decode; got %v", frigate.Devices)
	}
	// Publishes the citadel-owned host port -> container 5000.
	pubOK := false
	for _, p := range frigate.Ports {
		if strings.Contains(p, EnvFrigateHostPort) && strings.HasSuffix(p, ":5000") {
			pubOK = true
		}
	}
	if !pubOK {
		t.Errorf("frigate must publish ${%s}:5000; got %v", EnvFrigateHostPort, frigate.Ports)
	}

	// /config must be a LOCAL path (the nvr config dir), never the media target;
	// /media must be the resolved media dir. This is the SQLite-stays-local scar.
	// Match by container-mount suffix: the HOST side may be a ${VAR:?msg}
	// expansion that itself contains colons, so a naive split on ":" is wrong.
	var configHost, mediaHost string
	for _, v := range frigate.Volumes {
		if h, ok := strings.CutSuffix(v, ":/config"); ok {
			configHost = h
		}
		if h, ok := strings.CutSuffix(v, ":/media"); ok {
			mediaHost = h
		}
	}
	if !strings.Contains(configHost, "nvr/config") {
		t.Errorf("/config host mount = %q, want the local nvr/config dir (SQLite must stay local)", configHost)
	}
	if strings.Contains(configHost, "NVR_MEDIA_DIR") {
		t.Errorf("/config must NOT be on the media target (%q) — SQLite corrupts over NFS", configHost)
	}
	if !strings.Contains(mediaHost, "NVR_MEDIA_DIR") {
		t.Errorf("/media host mount = %q, want the resolved ${NVR_MEDIA_DIR}", mediaHost)
	}
}
