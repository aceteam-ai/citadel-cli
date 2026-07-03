package whatsapp

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/services"
)

// TestSelectHostPortHonorsExplicitOverride verifies that an explicit port is
// returned verbatim and NOT probed -- the operator asked for exactly this port.
func TestSelectHostPortHonorsExplicitOverride(t *testing.T) {
	probed := false
	got, err := SelectHostPort(9099, 0, func(int) bool { probed = true; return true })
	if err != nil {
		t.Fatalf("SelectHostPort() error = %v", err)
	}
	if got != 9099 {
		t.Errorf("port = %d, want 9099 (explicit override honored verbatim)", got)
	}
	if probed {
		t.Error("an explicit override must not be probed")
	}
}

// TestSelectHostPortAvoidsOccupied8080 is the core guarantee for issue #438: when
// 8080 (DefaultPort, held by citadel's own listener) is occupied, auto-selection
// must skip it and return a DIFFERENT free port.
func TestSelectHostPortAvoidsOccupied8080(t *testing.T) {
	// Simulate 8080 taken (as it is on any node running the citadel agent). Every
	// other candidate is free.
	probe := func(port int) bool { return port != DefaultPort }

	got, err := SelectHostPort(0, 0, probe)
	if err != nil {
		t.Fatalf("SelectHostPort() error = %v", err)
	}
	if got == DefaultPort {
		t.Fatalf("port = %d, want a port other than the occupied DefaultPort", got)
	}
	// It must not be a citadel-reserved port either.
	if name, taken := services.ReservedCitadelPorts[got]; taken {
		t.Errorf("selected port %d is reserved by citadel (%q)", got, name)
	}
}

// TestSelectHostPortSkipsReserved verifies auto-selection never returns any
// citadel-reserved port even when every port is "free" to bind.
func TestSelectHostPortSkipsReserved(t *testing.T) {
	got, err := SelectHostPort(0, 0, func(int) bool { return true })
	if err != nil {
		t.Fatalf("SelectHostPort() error = %v", err)
	}
	if name, taken := services.ReservedCitadelPorts[got]; taken {
		t.Errorf("selected port %d collides with reserved citadel port %q", got, name)
	}
	// DefaultPort (8080) is itself reserved, so a free-everywhere host must not
	// hand it back.
	if got == DefaultPort {
		t.Errorf("selected DefaultPort %d, which is reserved by citadel's gateway", got)
	}
}

// TestSelectHostPortExhausted verifies a saturated host yields a clear error
// rather than a bogus port.
func TestSelectHostPortExhausted(t *testing.T) {
	_, err := SelectHostPort(0, 0, func(int) bool { return false }) // nothing is free
	if err == nil {
		t.Fatal("expected an error when no host port is free, got nil")
	}
	if !strings.Contains(err.Error(), "no free host port") {
		t.Errorf("error = %q, want it to mention no free host port", err.Error())
	}
}

// TestProvisionAutoSelectsFreePortWhenDefaultOccupied is the Provision-level
// mirror of hostport_collision_test.go: with 8080 occupied, the provision picks a
// different free port and that port flows into the returned api_url and the
// bridge client / BRIDGE_PORT env, exactly as it would on a real node.
func TestProvisionAutoSelectsFreePortWhenDefaultOccupied(t *testing.T) {
	const chosen = 8092 // a stand-in "next free port"; 8080 is occupied below.

	bridge := &fakeBridge{health: &Health{LoggedIn: false}, qr: "2@qr"}
	deps, _ := baseDeps(t, bridge)

	// api_url must reflect whatever port Provision selects (this is what proves
	// the dynamic port reaches the backend + stored credential).
	deps.MeshAPIURL = func(port int) string {
		return fmt.Sprintf("http://100.64.0.7:%d", port)
	}
	// The port the bridge client is built for must also be the chosen one.
	var clientPort int
	deps.NewBridgeClient = func(port int, adminKey string) BridgeClient {
		clientPort = port
		return bridge
	}
	// Simulate 8080 taken (citadel's own listener) and steer auto-selection to a
	// deterministic free port.
	deps.SelectHostPort = func(preferred, floor int) (int, error) {
		if preferred > 0 {
			return preferred, nil
		}
		return chosen, nil
	}

	res, err := Provision(context.Background(), ProvisionRequest{}, deps)
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	if res.Port != chosen {
		t.Errorf("result Port = %d, want the auto-selected %d", res.Port, chosen)
	}
	wantURL := fmt.Sprintf("http://100.64.0.7:%d", chosen)
	if res.APIURL != wantURL {
		t.Errorf("api_url = %q, want %q (dynamic port must flow into api_url)", res.APIURL, wantURL)
	}
	if clientPort != chosen {
		t.Errorf("bridge client built for port %d, want %d", clientPort, chosen)
	}
}

// TestProvisionRetriesOnBindCollision defuses the TOCTOU landmine: SelectHostPort
// only *probes* the port, so another process can grab it between probe and
// `compose up`. When that happens on an auto-selected port, Provision must
// re-select a fresh port and retry rather than fail. Here the first deploy fails
// with a host-port bind error and the second succeeds on the next port.
func TestProvisionRetriesOnBindCollision(t *testing.T) {
	bridge := &fakeBridge{health: &Health{LoggedIn: false}, qr: "2@qr"}
	deps, _ := baseDeps(t, bridge)
	deps.MeshAPIURL = func(port int) string { return fmt.Sprintf("http://100.64.0.7:%d", port) }

	// Deterministic auto-selection: first pick 9000, then (floor > 9000) pick 9001.
	deps.SelectHostPort = func(preferred, floor int) (int, error) {
		if preferred > 0 {
			return preferred, nil
		}
		if floor > 9000 {
			return 9001, nil
		}
		return 9000, nil
	}

	var deployPorts []int
	deps.DeployCompose = func(servicesDir string, env map[string]string) error {
		deployPorts = append(deployPorts, mustAtoi(t, env["BRIDGE_PORT"]))
		if len(deployPorts) == 1 {
			// First attempt: simulate the race -- the port got taken after the probe.
			return fmt.Errorf("docker compose up failed:\nfailed to bind host port 0.0.0.0:9000/tcp: address already in use")
		}
		return nil
	}

	res, err := Provision(context.Background(), ProvisionRequest{}, deps)
	if err != nil {
		t.Fatalf("Provision() should retry past a bind collision, got %v", err)
	}
	if len(deployPorts) != 2 || deployPorts[0] != 9000 || deployPorts[1] != 9001 {
		t.Fatalf("deploy ports = %v, want [9000 9001] (retry on the next port)", deployPorts)
	}
	if res.Port != 9001 {
		t.Errorf("result Port = %d, want 9001 (the port the retry succeeded on)", res.Port)
	}
	if res.APIURL != "http://100.64.0.7:9001" {
		t.Errorf("api_url = %q, want it to carry the retry port 9001", res.APIURL)
	}
}

// TestProvisionExplicitPortNotRetriedOnCollision verifies an explicit override is
// never silently moved: a bind collision on an operator-pinned port surfaces the
// error instead of substituting a different port.
func TestProvisionExplicitPortNotRetriedOnCollision(t *testing.T) {
	bridge := &fakeBridge{health: &Health{LoggedIn: false}, qr: "2@qr"}
	deps, _ := baseDeps(t, bridge)
	deps.MeshAPIURL = func(port int) string { return fmt.Sprintf("http://100.64.0.7:%d", port) }

	deployCalls := 0
	deps.DeployCompose = func(servicesDir string, env map[string]string) error {
		deployCalls++
		return fmt.Errorf("failed to bind host port 0.0.0.0:8091/tcp: address already in use")
	}

	_, err := Provision(context.Background(), ProvisionRequest{Port: 8091}, deps)
	if err == nil {
		t.Fatal("expected the bind error to surface for an explicit port, got nil")
	}
	if deployCalls != 1 {
		t.Errorf("deploy attempted %d times, want 1 (explicit port must not retry)", deployCalls)
	}
}

func mustAtoi(t *testing.T, s string) int {
	t.Helper()
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		t.Fatalf("BRIDGE_PORT %q is not an int: %v", s, err)
	}
	return n
}

// TestProvisionHonorsExplicitPort verifies an explicit ProvisionRequest.Port is
// preserved end-to-end (override path).
func TestProvisionHonorsExplicitPort(t *testing.T) {
	const pinned = 8091 // the watest-style pinned dev port.

	bridge := &fakeBridge{health: &Health{LoggedIn: false}, qr: "2@qr"}
	deps, _ := baseDeps(t, bridge)
	deps.MeshAPIURL = func(port int) string { return fmt.Sprintf("http://100.64.0.7:%d", port) }

	// Use the real default selector (nil dep) to prove the override short-circuits
	// probing entirely -- an explicit port is never rejected.
	res, err := Provision(context.Background(), ProvisionRequest{Port: pinned}, deps)
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	if res.Port != pinned {
		t.Errorf("result Port = %d, want the explicit %d", res.Port, pinned)
	}
	if res.APIURL != fmt.Sprintf("http://100.64.0.7:%d", pinned) {
		t.Errorf("api_url = %q, want it to carry the explicit port %d", res.APIURL, pinned)
	}
}
