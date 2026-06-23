package chat

import (
	"context"
	"sort"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/network"
)

// Broker-less peer discovery.
//
// Today, chat messages flow through a central WSS endpoint (the AceTeam Redis
// API proxy): every node subscribes to an org-scoped channel and the broker
// fans messages out. This is simple but couples chat liveness to a single
// hosted service — exactly the dependency that left the chat pane stuck on
// "connecting…" when the endpoint was unreachable.
//
// Broker-less discovery is the alternative: because every node in an org is
// already a member of the same Headscale/nexus mesh, a node can enumerate its
// org peers directly from the local tailnet (no central directory) and probe
// each one for a chat listener, then exchange messages peer-to-peer over the
// VPN mesh. This file implements the enumerate + probe half of that design.
//
// Status of each step:
//   - Enumerate (DiscoverPeers): IMPLEMENTED. Uses network.GetGlobalPeers, which
//     returns only same-org peers (Headscale filters by Headscale user, and
//     each org maps to user "org_<organization_id>").
//   - Reachability probe (probeReachable): IMPLEMENTED as a stub. It pings the
//     peer over the mesh to confirm it is reachable. Reachability is NOT proof
//     that the peer speaks the chat protocol.
//   - Protocol probe: NOT YET IMPLEMENTED. There is no per-node chat listener
//     today (chat is broker-mediated), so there is no endpoint to probe for
//     "speaks chat". Wiring a small per-node chat listener and replacing the
//     reachability probe with a real protocol handshake is the follow-up that
//     completes broker-less chat. See ProbeResult.SpeaksChat (always false for
//     now) and the chatProbePort placeholder below.

// chatProbePort is the (future) TCP port a node would expose for a local chat
// listener. It is a placeholder: no listener binds it yet, so the protocol
// probe is intentionally not performed.
const chatProbePort = 8473

// probeTimeout bounds how long we wait when probing a single peer.
const probeTimeout = 2 * time.Second

// ProbeResult captures what we learned about one peer during discovery.
type ProbeResult struct {
	// NodeName is the peer's mesh hostname.
	NodeName string
	// IP is the peer's tailnet IPv4 address, if known.
	IP string
	// Online reflects the peer's last-known mesh presence.
	Online bool
	// Reachable is true if the peer responded to a mesh ping.
	Reachable bool
	// LatencyMs is the ping round-trip time when Reachable is true.
	LatencyMs float64
	// SpeaksChat is whether the peer was confirmed to speak the chat protocol.
	// Always false until a per-node chat listener exists to probe against.
	SpeaksChat bool
}

// DiscoverPeers enumerates the org-scoped peers visible on the Headscale mesh
// and probes each for reachability. It does NOT use a central broker: the peer
// list comes from the local tailnet and probes go directly over the VPN.
//
// This is the broker-less discovery primitive. It is currently informational
// (the chat client still uses the central WSS transport); a future change can
// route messages to peers returned here once they expose a chat listener.
func DiscoverPeers(ctx context.Context) ([]ProbeResult, error) {
	peers, err := network.GetGlobalPeers(ctx)
	if err != nil {
		return nil, err
	}

	results := make([]ProbeResult, 0, len(peers))
	for _, peer := range peers {
		r := ProbeResult{
			NodeName: peer.Hostname,
			IP:       peer.IP,
			Online:   peer.Online,
		}

		// Only probe peers we have an address for and that the mesh reports
		// as online — probing offline nodes just burns the timeout.
		if peer.IP != "" && peer.Online {
			reachable, latency := probeReachable(ctx, peer.IP)
			r.Reachable = reachable
			r.LatencyMs = latency
			// SpeaksChat stays false: no per-node chat listener to probe yet.
		}

		results = append(results, r)
	}

	// Stable ordering: reachable first, then by name.
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Reachable != results[j].Reachable {
			return results[i].Reachable
		}
		return results[i].NodeName < results[j].NodeName
	})

	return results, nil
}

// probeReachable pings a peer over the mesh to confirm basic reachability.
// This is the reachability stub: it proves the peer is on the VPN and routable,
// not that it speaks chat. Returns (reachable, latencyMs).
func probeReachable(ctx context.Context, ip string) (bool, float64) {
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	latency, _, _, err := network.PingPeer(pctx, ip)
	if err != nil {
		return false, 0
	}
	return true, latency
}
