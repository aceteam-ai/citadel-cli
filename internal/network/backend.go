// internal/network/backend.go
// Transport abstraction behind NetworkServer: userspace (tsnet) vs machine-wide (TUN).
package network

import (
	"context"
	"net"
	"net/netip"

	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
)

// Backend is the mesh transport a NetworkServer runs on.
//
// Two implementations are planned (issue #643):
//
//   - userspaceBackend (this file): today's embedded tsnet. The citadel
//     *process* gets a mesh identity; Dial/Listen go through tsnet's
//     userspace netstack. No root required. This is what `citadel login`
//     uses and it must keep behaving exactly as it did before the split.
//   - tunBackend: a real kernel TUN device, so the whole *machine* routes
//     100.64.0.0/10. Requires root/admin and is strictly opt-in.
//
// The interface is deliberately expressed in terms of `ipnstate` types
// rather than `*tailscale.LocalClient`. tsnet reaches its backend through a
// LocalClient, but a TUN backend drives an in-process `ipnlocal.LocalBackend`
// directly and has no local API socket to talk to. Both can produce an
// *ipnstate.Status, so that is the contract. `LocalClient` has no callers
// outside this package (verified), so nothing is lost by hiding it.
type Backend interface {
	// Up starts the backend and returns once the underlying engine is
	// running. It does NOT wait for the control plane to mark the node
	// Running — waitForConnection polls Status for that.
	Up(ctx context.Context) error

	// Close tears the backend down. For the TUN backend this must also
	// remove the interface, its routes, and any DNS configuration.
	Close() error

	// Status reports the current backend state, self node, and peers.
	Status(ctx context.Context) (*ipnstate.Status, error)

	// TailscaleIPs returns the node's assigned mesh addresses. Either may
	// be invalid before the netmap arrives.
	TailscaleIPs() (ip4, ip6 netip.Addr)

	// Dial opens a connection to a mesh address.
	Dial(ctx context.Context, network, addr string) (net.Conn, error)

	// Listen accepts connections from the mesh.
	Listen(network, addr string) (net.Listener, error)

	// ListenTLS accepts TLS connections with automatic certificate
	// management.
	ListenTLS(network, addr string) (net.Listener, error)

	// Ping probes a peer, reporting latency and whether the path is direct
	// or DERP-relayed.
	Ping(ctx context.Context, ip netip.Addr, pingType tailcfg.PingType) (*ipnstate.PingResult, error)

	// WhoIs resolves a remote address to the node and user the coordination
	// server vouches for. It is the trust primitive behind mesh-native
	// authorization (citadel #585), so an error must never be read as
	// "allowed" — see NetworkServer.WhoIs for the fail-safe contract.
	WhoIs(ctx context.Context, remoteAddr string) (*apitype.WhoIsResponse, error)

	// Reauth applies a fresh authkey to the already-running backend,
	// re-authorizing the node key in place. The machine key (and therefore
	// the node's IP) and all open listeners survive. This is the mechanism
	// behind online key renewal — see keyhealth.go.
	Reauth(ctx context.Context, authKey string) error
}
