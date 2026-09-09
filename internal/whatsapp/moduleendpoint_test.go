package whatsapp

import (
	"strings"
	"testing"

	fabricpb "github.com/aceteam-ai/fabric-protocol/gen/go/aceteam/fabric/v1"
	"google.golang.org/protobuf/proto"
)

// TestSourceNamesBridge pins citadel#624 FIX D's canonicalization: the sanctioned
// catalog NAME and every git-SOURCE form of the bridge collapse to ServiceName,
// while unrelated modules (including a same-prefix decoy) do not.
func TestSourceNamesBridge(t *testing.T) {
	match := []string{
		"whatsapp-bridge",
		"sunapi386/whatsapp-bridge",
		"sunapi386/whatsapp-bridge@v1.2.0",
		"https://github.com/sunapi386/whatsapp-bridge.git",
		"https://github.com/sunapi386/whatsapp-bridge.git@main",
		"git@github.com:sunapi386/whatsapp-bridge.git",
	}
	for _, s := range match {
		if !sourceNamesBridge(s) {
			t.Errorf("sourceNamesBridge(%q) = false, want true", s)
		}
	}
	no := []string{"", "vllm", "owner/other", "sunapi386/whatsapp-bridge-extra", "whatsapp"}
	for _, s := range no {
		if sourceNamesBridge(s) {
			t.Errorf("sourceNamesBridge(%q) = true, want false", s)
		}
	}
}

// TestAttachBridgeModule_GitSourceRealRowCollapses pins citadel#624 FIX D at the
// decorator: a git-source-installed bridge records lockfile Source
// "sunapi386/whatsapp-bridge", while the synthetic row's Source is ServiceName.
// The decorator must collapse them into ONE row (canonical-name match) and attach
// the endpoints onto the real row -- NOT append a permanent duplicate the
// upsert-by-source ingest would flap on.
func TestAttachBridgeModule_GitSourceRealRowCollapses(t *testing.T) {
	state := &fabricpb.ActualState{Modules: []*fabricpb.ActualModule{
		{Source: "sunapi386/whatsapp-bridge", Status: fabricpb.ModuleStatus_MODULE_STATUS_RUNNING},
	}}
	synthetic := &fabricpb.ActualModule{
		Source: ServiceName,
		Status: fabricpb.ModuleStatus_MODULE_STATUS_RUNNING,
		Health: fabricpb.ModuleHealth_MODULE_HEALTH_HEALTHY,
		Endpoints: []*fabricpb.ModuleEndpoint{{
			Name: BridgeService, Kind: "rest", Scheme: "https", Port: 8443,
			Path: "/modules/whatsapp", HealthPath: HealthPath, AdminKeyFingerprint: "sha256:abcd",
		}},
	}
	AttachBridgeModule(state, synthetic)
	if len(state.Modules) != 1 {
		t.Fatalf("git-source real row must collapse to ONE bridge row, got %d", len(state.Modules))
	}
	if got := state.Modules[0].GetSource(); got != "sunapi386/whatsapp-bridge" {
		t.Errorf("the real (git-source) row must be kept, got Source %q", got)
	}
	if len(state.Modules[0].GetEndpoints()) != 1 {
		t.Fatal("the synthetic endpoints must attach to the git-source real row")
	}
}

// TestAdminKeyFingerprint_EmptyIsNeverHashOfEmpty pins the required contract:
// a missing/empty admin key reads "", never the hash of an empty string.
func TestAdminKeyFingerprint_EmptyIsNeverHashOfEmpty(t *testing.T) {
	if got := AdminKeyFingerprint(""); got != "" {
		t.Fatalf("AdminKeyFingerprint(\"\") = %q, want \"\"", got)
	}
}

// TestAdminKeyFingerprint_Shape checks the sha256:<hex16> shape and that the
// fingerprint changes when the key changes (the one thing it exists to
// detect) but is stable for the same key.
func TestAdminKeyFingerprint_Shape(t *testing.T) {
	fp1 := AdminKeyFingerprint("wab_admin_deadbeef")
	fp2 := AdminKeyFingerprint("wab_admin_deadbeef")
	fp3 := AdminKeyFingerprint("wab_admin_different")

	if !strings.HasPrefix(fp1, "sha256:") {
		t.Fatalf("fingerprint = %q, want sha256: prefix", fp1)
	}
	hexPart := strings.TrimPrefix(fp1, "sha256:")
	if len(hexPart) != adminKeyFingerprintHexLen {
		t.Fatalf("hex part len = %d, want %d", len(hexPart), adminKeyFingerprintHexLen)
	}
	if fp1 != fp2 {
		t.Fatalf("fingerprint not stable for the same key: %q != %q", fp1, fp2)
	}
	if fp1 == fp3 {
		t.Fatalf("fingerprint did not change for a different key")
	}
	if fp1 == AdminKeyFingerprint("") {
		t.Fatalf("a real key's fingerprint must never equal the empty-key fingerprint")
	}
}

// TestBuildBridgeModule_NotDeployedNotRegistered: nothing to report.
func TestBuildBridgeModule_NotDeployedNotRegistered(t *testing.T) {
	if m := BuildBridgeModule(BridgeModuleInputs{}); m != nil {
		t.Fatalf("want nil module, got %+v", m)
	}
}

// TestBuildBridgeModule_KeyedBySource: ActualModule has no name field, so the
// synthetic row must be keyed by Source = ServiceName.
func TestBuildBridgeModule_KeyedBySource(t *testing.T) {
	m := BuildBridgeModule(BridgeModuleInputs{Deployed: true, ContainerRunning: true})
	if m == nil {
		t.Fatal("want non-nil module")
	}
	if m.GetSource() != ServiceName {
		t.Errorf("source = %q, want %q", m.GetSource(), ServiceName)
	}
}

// TestBuildBridgeModule_HealthReflectsContainer pins that health/status come
// from the live, bridge-specific container check -- not a hardcoded value.
func TestBuildBridgeModule_HealthReflectsContainer(t *testing.T) {
	running := BuildBridgeModule(BridgeModuleInputs{Deployed: true, ContainerRunning: true})
	if running.GetStatus() != fabricpb.ModuleStatus_MODULE_STATUS_RUNNING {
		t.Errorf("running: status = %v, want RUNNING", running.GetStatus())
	}
	if running.GetHealth() != fabricpb.ModuleHealth_MODULE_HEALTH_HEALTHY {
		t.Errorf("running: health = %v, want HEALTHY", running.GetHealth())
	}

	stopped := BuildBridgeModule(BridgeModuleInputs{Deployed: true, ContainerRunning: false})
	if stopped.GetStatus() != fabricpb.ModuleStatus_MODULE_STATUS_STOPPED {
		t.Errorf("stopped: status = %v, want STOPPED", stopped.GetStatus())
	}

	absent := BuildBridgeModule(BridgeModuleInputs{Deployed: false, Registered: true})
	if absent.GetStatus() != fabricpb.ModuleStatus_MODULE_STATUS_ABSENT {
		t.Errorf("undeployed-but-registered: status = %v, want ABSENT", absent.GetStatus())
	}
}

// TestBuildBridgeModule_PortZeroSkipsEndpoint: a registry entry with Port==0
// ("declared but not yet deployed") must not produce an endpoint.
func TestBuildBridgeModule_PortZeroSkipsEndpoint(t *testing.T) {
	m := BuildBridgeModule(BridgeModuleInputs{
		Deployed: true, ContainerRunning: true,
		Registered: true, Prefix: "whatsapp", BridgePort: 0,
		GatewayPort: 8443, GatewayUseTLS: true,
	})
	if len(m.GetEndpoints()) != 0 {
		t.Fatalf("want no endpoints when BridgePort==0, got %+v", m.GetEndpoints())
	}
}

// TestBuildBridgeModule_UnknownGatewaySkipsEndpoint: GatewayPort<=0 (facts
// unknown) must not fabricate a portless endpoint.
func TestBuildBridgeModule_UnknownGatewaySkipsEndpoint(t *testing.T) {
	m := BuildBridgeModule(BridgeModuleInputs{
		Deployed: true, ContainerRunning: true,
		Registered: true, Prefix: "whatsapp", BridgePort: 18210,
		GatewayPort: 0,
	})
	if len(m.GetEndpoints()) != 0 {
		t.Fatalf("want no endpoints when GatewayPort is unknown, got %+v", m.GetEndpoints())
	}
}

// TestBuildBridgeModule_EndpointFacts pins the endpoint field sourcing: port
// is the GATEWAY's port (not the bridge's raw loopback port), path is the
// gateway module route, scheme/health_path come from this package (not the
// registry, which has no concept of either).
func TestBuildBridgeModule_EndpointFacts(t *testing.T) {
	m := BuildBridgeModule(BridgeModuleInputs{
		Deployed: true, ContainerRunning: true,
		Registered: true, Prefix: "whatsapp", BridgePort: 18210,
		GatewayPort: 8443, GatewayUseTLS: true,
		AdminKeyFingerprint: "sha256:abcdef0123456789",
	})
	if len(m.GetEndpoints()) != 1 {
		t.Fatalf("want 1 endpoint, got %d", len(m.GetEndpoints()))
	}
	ep := m.GetEndpoints()[0]
	if ep.GetName() != BridgeService {
		t.Errorf("name = %q, want %q", ep.GetName(), BridgeService)
	}
	if ep.GetScheme() != "https" {
		t.Errorf("scheme = %q, want https", ep.GetScheme())
	}
	if ep.GetPort() != 8443 {
		t.Errorf("port = %d, want the GATEWAY port 8443, not the bridge port 18210", ep.GetPort())
	}
	if ep.GetPath() != "/modules/whatsapp" {
		t.Errorf("path = %q, want /modules/whatsapp", ep.GetPath())
	}
	if ep.GetHealthPath() != HealthPath {
		t.Errorf("health_path = %q, want %q", ep.GetHealthPath(), HealthPath)
	}
	if ep.GetHealth() != fabricpb.ModuleHealth_MODULE_HEALTH_HEALTHY {
		t.Errorf("endpoint health = %v, want HEALTHY (mirrors module health)", ep.GetHealth())
	}
	if ep.GetAdminKeyFingerprint() != "sha256:abcdef0123456789" {
		t.Errorf("admin_key_fingerprint = %q", ep.GetAdminKeyFingerprint())
	}

	// no-TLS gateway -> http scheme.
	http := BuildBridgeModule(BridgeModuleInputs{
		Deployed: true, Registered: true, Prefix: "whatsapp", BridgePort: 1,
		GatewayPort: 8080, GatewayUseTLS: false,
	})
	if http.GetEndpoints()[0].GetScheme() != "http" {
		t.Errorf("scheme = %q, want http for a no-TLS gateway", http.GetEndpoints()[0].GetScheme())
	}
}

// TestBuildBridgeModule_MarshaledStateNeverContainsSecretBytes is the required
// structural-safety pin (citadel#624 design review): a marshaled ActualModule
// for the bridge, built from an input carrying a real secret admin key, must
// never contain those secret bytes on the wire -- only the one-way
// fingerprint may appear.
func TestBuildBridgeModule_MarshaledStateNeverContainsSecretBytes(t *testing.T) {
	const secretAdminKey = "wab_admin_TOTALLY-SECRET-DO-NOT-LEAK-9f8e7d6c5b4a"

	m := BuildBridgeModule(BridgeModuleInputs{
		Deployed: true, ContainerRunning: true,
		Registered: true, Prefix: "whatsapp", BridgePort: 18210,
		GatewayPort: 8443, GatewayUseTLS: true,
		AdminKeyFingerprint: AdminKeyFingerprint(secretAdminKey),
	})

	wire, err := proto.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(wire), secretAdminKey) {
		t.Fatalf("marshaled ActualModule contains the raw secret admin key bytes")
	}
	// Sanity: the fingerprint itself (derived, one-way) is expected to appear.
	fp := AdminKeyFingerprint(secretAdminKey)
	if !strings.Contains(string(wire), strings.TrimPrefix(fp, "sha256:")) {
		t.Fatalf("expected the derived fingerprint to be present on the wire")
	}
}
