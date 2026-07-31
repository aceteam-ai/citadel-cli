package services

import (
	_ "embed"
	"os"
	"path/filepath"
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
	for _, name := range []string{"NVR_RETENTION_DAYS", "NVR_DETECTOR", "NVR_STORAGE_MODE", "NVR_STORAGE_TARGET", "NVR_CAMERAS"} {
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
		Image       string   `yaml:"image"`
		NetworkMode string   `yaml:"network_mode"`
		Ports       []string `yaml:"ports"`
		Devices     []string `yaml:"devices"`
		Volumes     []string `yaml:"volumes"`
		DependsOn   map[string]struct {
			Condition string `yaml:"condition"`
		} `yaml:"depends_on"`
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

	// /config must be a LOCAL path (the nvr config dir), never the media dir;
	// /media is a separate local bind. This is the SQLite-stays-local scar.
	// Match by container-mount suffix (the host side may contain colons).
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
	if !strings.Contains(mediaHost, "nvr/media") {
		t.Errorf("/media host mount = %q, want the local nvr/media dir", mediaHost)
	}
	if configHost == mediaHost {
		t.Errorf("/config and /media must be distinct paths; both = %q", configHost)
	}

	// The nvr-config init container must exist and frigate must wait for it to
	// complete successfully (config.yml generation + nas verify gate).
	initSvc, ok := c.Services["nvr-config"]
	if !ok {
		t.Fatalf("compose is missing the nvr-config init container")
	}
	if !strings.Contains(initSvc.Image, "nvr-config") {
		t.Errorf("nvr-config image = %q, want the ghcr nvr-config image", initSvc.Image)
	}
	dep, ok := frigate.DependsOn["nvr-config"]
	if !ok || dep.Condition != "service_completed_successfully" {
		t.Errorf("frigate must depend_on nvr-config with condition service_completed_successfully; got %+v", frigate.DependsOn)
	}
}

// TestNVRComposeShmSize pins the /dev/shm cap. Frigate passes RAW DECODED frames
// through /dev/shm (a 1080p YUV420 frame is ~3MB, buffered ~9 deep per camera),
// so it scales with cameras x resolution, not with the compressed bitrate. At
// 256mb with 3x1080p, Frigate warned it needed >=306MB and measured steady-state
// use was 322MB (#637); undersizing causes frame-write failures and
// capture-process restart loops.
func TestNVRComposeShmSize(t *testing.T) {
	body := readNVRCompose(t)
	if strings.Contains(body, "shm_size: 256mb") {
		t.Error("shm_size is back to 256mb, which is too small for 3x1080p cameras (#637)")
	}
	if !strings.Contains(body, "shm_size: 512mb") {
		t.Error("expected frigate shm_size: 512mb")
	}
}

// TestNVRMosquittoIsNotReachableOffTheComposeNetwork is the security-critical
// property of the node-local broker: it must publish NO host port. Binding
// 127.0.0.1 would NOT be equivalent, because wyze-bridge runs host-networked in
// this module, which would make any bound port LAN-reachable.
func TestNVRMosquittoIsNotReachableOffTheComposeNetwork(t *testing.T) {
	var compose struct {
		Services map[string]struct {
			Ports       []any  `yaml:"ports"`
			NetworkMode string `yaml:"network_mode"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(readNVRCompose(t)), &compose); err != nil {
		t.Fatalf("parse compose: %v", err)
	}
	mosq, ok := compose.Services["mosquitto"]
	if !ok {
		t.Fatal("mosquitto service missing from the nvr module compose")
	}
	if len(mosq.Ports) != 0 {
		t.Errorf("mosquitto must publish no host port, got %v", mosq.Ports)
	}
	if mosq.NetworkMode == "host" {
		t.Error("mosquitto must not use host networking")
	}
}

// TestNVRMosquittoRequiresCredentials pins that the broker never starts
// anonymously: mosquitto permits anonymous connections BY DEFAULT, so the
// config must disable that and the password must be a required input.
func TestNVRMosquittoRequiresCredentials(t *testing.T) {
	body := readNVRCompose(t)
	if !strings.Contains(body, "allow_anonymous false") {
		t.Error("mosquitto config must set allow_anonymous false")
	}
	// `:?` makes compose fail loudly when the password is unset, rather than
	// starting a broker with an empty credential.
	if !strings.Contains(body, "NVR_MQTT_PASSWORD:?") {
		t.Error("NVR_MQTT_PASSWORD must be a required compose variable")
	}
	// mosquitto drops privileges; without this it cannot read its own pwfile.
	if !strings.Contains(body, "chown -R mosquitto:mosquitto") {
		t.Error("the generated password file must be chowned to the mosquitto user")
	}
}

// TestNVRMqttPasswordIsNodeGenerated pins that the broker credential the module
// requires is one the NODE mints for itself. It is purely internal (frigate ->
// mosquitto on the module's private network), so nothing upstream has a value to
// send: a fabric MODULE_SET assignment and `citadel module update` both resolve
// config non-interactively, and a required-but-ungenerated var makes BOTH paths
// hard-fail — the module becomes undeployable. A static default is equally wrong
// (same password on every node), which is why this asserts `generate:` and not
// merely "has some value".
func TestNVRMqttPasswordIsNodeGenerated(t *testing.T) {
	var m catalog.ServiceManifest
	if err := yaml.Unmarshal(nvrServiceYAML, &m); err != nil {
		t.Fatalf("parse nvr service.yaml: %v", err)
	}

	var found bool
	for _, cv := range m.Config {
		if cv.Name != "NVR_MQTT_PASSWORD" {
			continue
		}
		found = true
		if cv.Generate != catalog.GenerateSecret {
			t.Errorf("NVR_MQTT_PASSWORD generate = %q, want %q — otherwise assignment and update fail with "+
				"'required config has no value'", cv.Generate, catalog.GenerateSecret)
		}
		if !cv.Required {
			t.Error("NVR_MQTT_PASSWORD must stay required so the compose `:?` guard keeps failing closed")
		}
		if cv.Default != "" {
			t.Errorf("NVR_MQTT_PASSWORD must have no default (%q) — a shared default ships one password to every node", cv.Default)
		}
	}
	if !found {
		t.Fatal("NVR_MQTT_PASSWORD missing from the nvr manifest config")
	}
}

// readNVRCompose returns the nvr module compose file body.
func readNVRCompose(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("nvr-service", "compose.yml"))
	if err != nil {
		t.Fatalf("read nvr compose: %v", err)
	}
	return string(b)
}
