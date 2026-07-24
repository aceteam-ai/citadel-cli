// cmd/gateway_mesh_auth.go
//
// cmd-side adapter that lets the gateway authorize a private/org exposure by
// verified mesh-peer identity (issue #598). It bridges the network layer's
// WhoIs to the gateway's MeshIdentityResolver interface. It lives here (not in
// internal/gateway) because it depends on internal/network; keeping the
// dependency on this side is what lets internal/gateway stay standalone and
// unit-testable behind the interface (see cmd/terminal_mesh_auth.go, the same
// pattern for the terminal endpoint).
package cmd

import (
	"context"

	"github.com/aceteam-ai/citadel-cli/internal/gateway"
	"github.com/aceteam-ai/citadel-cli/internal/network"
)

// gatewayMeshResolver implements gateway.MeshIdentityResolver using the mesh
// control plane. It is wired into the gateway via SetMeshResolver so an exposed
// service with `private`/`org` visibility can authorize a caller arriving on the
// VPN listener by its verified tailnet login + same-owner status — no
// platform-minted token needed. `link` exposures do not use it (they verify a
// signed token instead), so this only lights up private/org.
type gatewayMeshResolver struct{}

// ResolvePeer resolves an inbound connection's remote address to its verified
// tailnet identity via network.WhoIsPeer. Any error is returned verbatim so the
// gateway's exposure middleware fails the private/org check closed (an
// unverifiable caller is never granted access).
func (gatewayMeshResolver) ResolvePeer(ctx context.Context, remoteAddr string) (*gateway.MeshPeerIdentity, error) {
	id, err := network.WhoIsPeer(ctx, remoteAddr)
	if err != nil {
		return nil, err
	}
	return &gateway.MeshPeerIdentity{
		NodeName:  id.NodeName,
		LoginName: id.LoginName,
		SameOwner: id.SameOwner,
	}, nil
}
