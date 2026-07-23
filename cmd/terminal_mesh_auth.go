// cmd/terminal_mesh_auth.go
//
// cmd-side adapter that lets the terminal endpoint authorize a tokenless
// connection by verified mesh-peer identity (citadel #585). It bridges the
// network layer's WhoIs to the terminal server's MeshIdentityResolver
// interface. It lives here (not in internal/terminal) because it depends on
// internal/network; keeping the dependency on this side is what lets
// internal/terminal stay standalone and unit-testable behind the interface.
package cmd

import (
	"context"

	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/terminal"
)

// meshIdentityResolver implements terminal.MeshIdentityResolver using the mesh
// control plane. It is wired into every terminal server via SetMeshResolver so
// that connections arriving on the VPN listener from a verified same-owner
// tailnet peer are trusted without a platform-minted token. The localhost/LAN
// bind is unaffected and still requires a token (see terminal.resolveAuth).
type meshIdentityResolver struct{}

// ResolvePeer resolves an inbound connection's remote address to its verified
// tailnet identity via network.WhoIsPeer. The peer's login becomes the terminal
// session UserID, which drives the per-user tmux session name so a reconnecting
// peer re-attaches to its own shell. Any error is returned verbatim so the
// terminal server falls back to the token requirement (fail-safe: an
// unverifiable peer is never granted an unauthenticated session).
func (meshIdentityResolver) ResolvePeer(ctx context.Context, remoteAddr string) (*terminal.MeshPeerIdentity, error) {
	id, err := network.WhoIsPeer(ctx, remoteAddr)
	if err != nil {
		return nil, err
	}

	// Prefer the tailnet login as the stable per-user session key; fall back to
	// the node name when no login is available so re-attach still works. Note:
	// on a single-Headscale-user tailnet all peers share one login and therefore
	// one per-user session — that matches the org-as-tailnet trust boundary.
	userID := id.LoginName
	if userID == "" {
		userID = id.NodeName
	}

	return &terminal.MeshPeerIdentity{
		NodeName:  id.NodeName,
		UserID:    userID,
		SameOwner: id.SameOwner,
	}, nil
}
