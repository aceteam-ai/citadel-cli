package serviceborrow

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"

	"github.com/aceteam-ai/citadel-cli/internal/meshtransfer"
)

// PeerConn is an authenticated connection to ONE verified peer's gateway over
// the mesh. HTTP reaches ONLY that peer (meshtransfer's mesh-only dial guard +
// pinned self-signed leaf), so a redirect, public DNS name, proxy, or any other
// mesh peer cannot steer a borrow request off the intended peer. BaseURL is the
// peer's gateway base ("https://<meshIP>:<port>"); request URLs MUST use the
// mesh IP literal (the transport refuses a hostname by design — the no-public-DNS
// property). StableID/OwnerID/NodeName are the verified identity, for the
// requester's attribution (delegated_node_id is the serving receipt's node_id,
// carried over the wire, not this StableID — see client.go).
type PeerConn struct {
	HTTP     *http.Client
	BaseURL  string
	MeshIP   string
	StableID string
	OwnerID  string
	NodeName string
}

// MeshGateway connects a requester to a verified same-owner peer's gateway over
// the mesh. It is an interface so the requester Client is adversarially testable
// with an httptest-backed fake (the e2e test) while production uses the real
// verified mesh transport (NewMeshGateway).
type MeshGateway interface {
	// Connect verifies the peer at meshIP as a same-owner mesh peer and returns
	// an authenticated connection to its gateway. It MUST verify BEFORE dialing
	// (an unverified, foreign, or non-mesh target never has its certificate
	// captured or a byte sent to it).
	Connect(ctx context.Context, meshIP string) (*PeerConn, error)
}

// meshGateway is the production MeshGateway: it wraps internal/meshtransfer
// (verify-before-dial + bootstrapped-leaf pin) so the client-side authorization
// crux lives in one tested place. The cmd layer wires resolver=network.WhoIsPeer
// adapter and dial=network.Dial.
type meshGateway struct {
	resolver    meshtransfer.PeerResolver
	dial        meshtransfer.Dialer
	gatewayPort int
}

// NewMeshGateway builds the production MeshGateway. resolver verifies a peer's
// mesh identity (production: network.WhoIsPeer); dial dials over the mesh
// (production: network.Dial); gatewayPort is the peer's gateway TLS port.
func NewMeshGateway(resolver meshtransfer.PeerResolver, dial meshtransfer.Dialer, gatewayPort int) MeshGateway {
	return &meshGateway{resolver: resolver, dial: dial, gatewayPort: gatewayPort}
}

// Connect implements MeshGateway: Verify(same-owner) -> discover the stable
// device id -> BootstrapPeerCert pinned to that device + the gateway port ->
// build a client pinned to the captured leaf. Every step fails closed; a
// non-mesh or non-same-owner meshIP returns an error from Verify before any dial.
func (g *meshGateway) Connect(ctx context.Context, meshIP string) (*PeerConn, error) {
	if g.gatewayPort <= 0 || g.gatewayPort > 65535 {
		return nil, fmt.Errorf("serviceborrow: invalid gateway port %d", g.gatewayPort)
	}
	// 1. Verify same-owner identity + discover the stable device id (no dial yet).
	vp, err := meshtransfer.Verify(ctx, g.resolver, meshIP, meshtransfer.Expected{})
	if err != nil {
		return nil, fmt.Errorf("serviceborrow: verify peer %s: %w", meshIP, err)
	}
	// 2. Bootstrap the peer's gateway cert, pinned to the device discovered above
	//    (BootstrapPeerCert requires want.StableID — the trust-commitment point).
	peer, leaf, err := meshtransfer.BootstrapPeerCert(ctx, g.resolver, g.dial, meshIP, g.gatewayPort, meshtransfer.Expected{StableID: vp.Identity.StableID})
	if err != nil {
		return nil, fmt.Errorf("serviceborrow: bootstrap peer %s gateway cert: %w", meshIP, err)
	}
	// 3. A client that can only ever reach this verified peer's gateway.
	httpc, err := peer.Client(g.dial, leaf)
	if err != nil {
		return nil, fmt.Errorf("serviceborrow: build pinned client for %s: %w", meshIP, err)
	}
	ipLit := peer.MeshIP.String()
	return &PeerConn{
		HTTP:     httpc,
		BaseURL:  "https://" + net.JoinHostPort(ipLit, strconv.Itoa(g.gatewayPort)),
		MeshIP:   ipLit,
		StableID: peer.Identity.StableID,
		OwnerID:  peer.Identity.OwnerID,
		NodeName: peer.Identity.NodeName,
	}, nil
}
