// cmd/egress_relay_mesh_auth.go
//
// cmd-side adapter that lets the egress relay (citadel #787) authorize an
// inbound connection by verified mesh-peer identity. It bridges the network
// layer's WhoIs to internal/egressrelay's IdentityResolver interface. It
// lives here (not in internal/egressrelay) because it depends on
// internal/network; keeping the dependency on this side is what lets
// internal/egressrelay stay standalone and unit-testable behind the
// interface (see cmd/gateway_mesh_auth.go and cmd/terminal_mesh_auth.go, the
// same pattern for the gateway's exposure middleware and the terminal
// endpoint).
package cmd

import (
	"context"

	"github.com/aceteam-ai/citadel-cli/internal/egressrelay"
	"github.com/aceteam-ai/citadel-cli/internal/network"
)

// egressRelayMeshResolver implements egressrelay.IdentityResolver using the
// mesh control plane. It is the ONLY authorization gate the relay has: a
// peer must resolve to a verified tailnet identity AND be same-owner
// (citadel #787's "same-org verified peers only" posture) or the connection
// is refused before any SOCKS5 bytes are relayed.
type egressRelayMeshResolver struct{}

// ResolvePeer resolves an inbound connection's remote address to its
// verified tailnet identity via network.WhoIsPeer. Any error is returned
// verbatim so egressrelay.Authorize fails closed (an unverifiable caller is
// never granted access to egress through this node).
func (egressRelayMeshResolver) ResolvePeer(ctx context.Context, remoteAddr string) (*egressrelay.PeerIdentity, error) {
	id, err := network.WhoIsPeer(ctx, remoteAddr)
	if err != nil {
		return nil, err
	}
	return &egressrelay.PeerIdentity{
		NodeName:  id.NodeName,
		LoginName: id.LoginName,
		SameOwner: id.SameOwner,
	}, nil
}
