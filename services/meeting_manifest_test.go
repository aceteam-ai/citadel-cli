package services

import (
	_ "embed"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
	"gopkg.in/yaml.v3"
)

// meetingServiceYAML is the on-disk module manifest the installer will consume.
// Embedding it makes the test fail if the file is deleted or renamed, and lets us
// parse it through the REAL catalog.ServiceManifest struct so a schema mismatch is
// caught at build time rather than at install time on a node.
//
//go:embed meeting-service/service.yaml
var meetingServiceYAML []byte

// TestMeetingServiceManifest validates the meeting module manifest against the
// catalog schema and the load-bearing invariants from the design (#514): a real,
// probeable health check on the PUBLISHED HOST port; the two loopback ports
// matching the registry; a least-privilege sandbox with no devices/host-network;
// and no gateway exposure.
func TestMeetingServiceManifest(t *testing.T) {
	var m catalog.ServiceManifest
	if err := yaml.Unmarshal(meetingServiceYAML, &m); err != nil {
		t.Fatalf("meeting service.yaml does not parse as a ServiceManifest: %v", err)
	}

	if m.Name != "meeting" {
		t.Errorf("name = %q, want %q", m.Name, "meeting")
	}
	if m.SchemaVersion != catalog.CurrentSchemaVersion {
		t.Errorf("schema_version = %d, want %d", m.SchemaVersion, catalog.CurrentSchemaVersion)
	}
	if m.Requires.GPU {
		t.Errorf("meeting module must not require a GPU (softwareGL under Xvfb)")
	}

	// Health check must be probeable AND target the published HOST port, because
	// catalog.ProbeHealth GETs 127.0.0.1:<port> on the host. Pointing it at the
	// container port (8102) would make the probe a silent no-op (claudecode's bug).
	if !catalog.HasHealthProbe(m.HealthCheck) {
		t.Errorf("health_check is not probeable; the whole point of #514 is a real health gate")
	}
	if m.HealthCheck.Endpoint != "/health" {
		t.Errorf("health_check.endpoint = %q, want /health", m.HealthCheck.Endpoint)
	}
	if m.HealthCheck.Port != MeetingdHostPort {
		t.Errorf("health_check.port = %d, want the published HOST port %d (ProbeHealth runs on the host)", m.HealthCheck.Port, MeetingdHostPort)
	}

	// Both loopback ports must match the registry so nothing else claims them.
	wantPorts := map[int]int{ // host -> container
		MeetingdHostPort:   8102,
		MeetingCDPHostPort: 9223,
	}
	gotPorts := map[int]int{}
	for _, p := range m.Ports {
		gotPorts[p.Host] = p.Container
	}
	if len(gotPorts) != len(wantPorts) {
		t.Errorf("ports = %v, want host->container %v", gotPorts, wantPorts)
	}
	for host, container := range wantPorts {
		if gotPorts[host] != container {
			t.Errorf("port host %d maps to container %d, want %d", host, gotPorts[host], container)
		}
	}

	// Least-privilege sandbox: no devices (null sink is software), no host network.
	if len(m.Sandbox.Devices) != 0 {
		t.Errorf("sandbox.devices = %v, want none (the null sink needs no /dev/snd)", m.Sandbox.Devices)
	}
	if m.Sandbox.HostNetwork {
		t.Errorf("sandbox.host_network must be false (loopback-only module)")
	}

	// The meeting routing tag must be advertised on install.
	hasMeetingTag := false
	for _, tag := range m.NodeTags {
		if tag == "meeting" {
			hasMeetingTag = true
		}
	}
	if !hasMeetingTag {
		t.Errorf("node_tags must include %q, got %v", "meeting", m.NodeTags)
	}

	// Not exposed on the gateway: the only consumer is the co-located citadel
	// process, nothing should reach this over the mesh.
	if m.Gateway != nil {
		t.Errorf("meeting module must not declare a gateway block; it is loopback-only")
	}
}
