// internal/terminal/meshauth.go
//
// Mesh-peer identity trust for the terminal endpoint (citadel #585).
//
// The terminal server historically authenticated EVERY connection with a
// platform-minted `?token=` (validated by TokenValidator). But any peer that
// can dial this node's <vpn_ip>:7860 over the WireGuard mesh is *already* an
// authenticated member of the org's tailnet — the authentication happened at
// the mesh layer. This file defines the trust primitive that lets the server
// accept such a connection based on its verified mesh identity instead of a
// separately-provisioned token, so `citadel connect <name>` works with no
// manual token.
//
// Security posture (READ THIS): mesh-identity trust is GATED to the VPN
// listener. The localhost/LAN bind and any public exposure still require a
// token (see Server.resolveAuth). The token path is preserved and takes
// precedence; mesh identity is an ADDITIONAL authorizer used only when no token
// is presented on a VPN connection. Resolution is fail-safe — an unresolvable
// or same-owner-mismatched peer yields an error and the server falls back to
// the token requirement, never an unauthenticated session.
package terminal

import "context"

// MeshPeerIdentity is the verified identity of a peer that connected over the
// mesh/VPN listener. It is produced by a MeshIdentityResolver (implemented in
// the cmd layer against the tsnet control plane) and consumed by the terminal
// server to authorize a connection WITHOUT a platform-minted token.
type MeshPeerIdentity struct {
	// NodeName is the peer's node name, included in the audit log.
	NodeName string

	// UserID identifies the peer for session ownership. It becomes the session
	// TokenInfo.UserID, which drives the per-user tmux session name
	// (sessionNameForUser) so a reconnecting peer re-attaches to its own shell
	// rather than sharing one with every other peer.
	UserID string

	// SameOwner reports whether the peer belongs to the same tailnet owner/org
	// as this node. The server rejects mesh trust when this is false.
	SameOwner bool
}

// MeshIdentityResolver resolves an inbound connection's remote address to a
// verified mesh peer identity. It is injected into the server (SetMeshResolver)
// so the terminal package stays standalone and unit-testable without a live
// mesh — tests pass a MockMeshResolver, production wires network.WhoIsPeer via
// the cmd layer.
type MeshIdentityResolver interface {
	// ResolvePeer resolves remoteAddr ("ip:port") to a verified identity, or an
	// error if the peer cannot be verified (the caller then falls back to token
	// auth).
	ResolvePeer(ctx context.Context, remoteAddr string) (*MeshPeerIdentity, error)
}

// MockMeshResolver is a MeshIdentityResolver for tests: it returns a fixed
// identity (or error) regardless of the remote address, so auth-decision tests
// need no live mesh.
type MockMeshResolver struct {
	Identity *MeshPeerIdentity
	Err      error
}

// ResolvePeer implements MeshIdentityResolver for the mock.
func (m *MockMeshResolver) ResolvePeer(ctx context.Context, remoteAddr string) (*MeshPeerIdentity, error) {
	if m.Err != nil {
		return nil, m.Err
	}
	return m.Identity, nil
}
