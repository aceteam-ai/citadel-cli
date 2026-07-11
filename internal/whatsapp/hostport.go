// internal/whatsapp/hostport.go
//
// Host-port selection for the WhatsApp bridge.
//
// The bridge publishes on a HOST port so the AceTeam backend can reach it over
// the mesh at http://<mesh-ip>:<port>. The historical default was 8080
// (whatsapp.DefaultPort), but on any node running the citadel agent that port is
// already held by citadel's own gateway / status listener (services.GatewayPort
// == 8080), plus nexus-server-local. So `docker compose up` failed with
//
//	failed to bind host port 0.0.0.0:8080/tcp: address already in use
//
// on essentially every real node (aceteam-ai/citadel-cli#438). The container-name
// collision fix (#436/#437, v2.59.0) got the containers to come up; this is the
// next wall.
//
// The fix is to pick a FREE host port when the caller does not pin one, instead
// of hardcoding 8080. We probe candidates by actually binding them (the exact
// operation `docker compose up` will attempt), so the selection reflects live
// process ownership -- a static registry check would miss citadel's own listener
// and nexus-server-local, which is precisely what bites here. Candidates that
// belong to citadel's own reserved set (services.ReservedCitadelPorts, e.g. 8080)
// are skipped up front so we never even try them.
package whatsapp

import (
	"fmt"
	"net"
	"strings"

	"github.com/aceteam-ai/citadel-cli/services"
)

// hostPortScanLimit bounds how many candidate ports SelectHostPort probes before
// giving up, so a pathologically busy host fails with a clear error rather than
// scanning forever.
const hostPortScanLimit = 512

// hostPortFree reports whether tcpPort can be bound on the host right now. It
// binds on 0.0.0.0 (all interfaces) because that is what the bridge's compose
// publish (`${BRIDGE_PORT}:8080`) does -- a port free only on loopback would
// still fail the real publish. The listener is closed immediately; this is an
// availability probe, not a reservation, so a caller must bind (via compose up)
// promptly after and be prepared to retry on the inherent TOCTOU race.
func hostPortFree(tcpPort int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", tcpPort))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// SelectHostPort chooses a bindable host port for the bridge publish.
//
//   - preferred > 0: an explicit operator override. It is honored verbatim and
//     NOT probed -- the operator asked for exactly this port, and failing their
//     explicit choice with a surprise substitution would be worse than surfacing
//     the eventual bind error from compose. (This preserves the documented
//     ProvisionRequest.Port override.) floor is ignored in this case.
//   - preferred <= 0: auto-select. Probe candidates from a starting point,
//     skipping any port in citadel's own reserved set, and return the first that
//     binds. The scan starts at max(DefaultPort, floor): floor lets a
//     bind-collision retry resume ABOVE the port that just failed instead of
//     re-picking it. Because DefaultPort (8080) is itself reserved by citadel's
//     gateway, a fresh auto-selection effectively lands on the next free port
//     above it.
//
// probe is the availability check, injected so tests can simulate an occupied
// port without touching the real network stack; nil uses the real hostPortFree.
func SelectHostPort(preferred, floor int, probe func(int) bool) (int, error) {
	if preferred > 0 {
		return preferred, nil
	}
	if probe == nil {
		probe = hostPortFree
	}

	start := DefaultPort
	if floor > start {
		start = floor
	}

	reserved := services.ReservedCitadelPorts
	// 65535 is the max valid TCP port; stop there even if the scan limit would
	// otherwise carry us past it.
	for offset := 0; offset < hostPortScanLimit; offset++ {
		port := start + offset
		if port > 65535 {
			break
		}
		if _, taken := reserved[port]; taken {
			// Never hand out a citadel-owned port (8080 gateway, etc.).
			continue
		}
		if probe(port) {
			return port, nil
		}
	}
	return 0, fmt.Errorf("no free host port found in %d ports starting at %d (is the host saturated?)", hostPortScanLimit, start)
}

// isHostPortCollision reports whether err looks like a host-port bind conflict
// from `docker compose up` (or the dockerd it drives). The bridge publish fails
// with a message like `failed to bind host port 0.0.0.0:8080/tcp: address
// already in use`, which is the exact race SelectHostPort's probe can lose. We
// match on the stable substrings docker/OS emit rather than a typed error
// because DeployCompose surfaces the failure as a wrapped, formatted string.
func isHostPortCollision(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "address already in use") ||
		strings.Contains(msg, "port is already allocated") ||
		(strings.Contains(msg, "bind") && strings.Contains(msg, "in use"))
}
