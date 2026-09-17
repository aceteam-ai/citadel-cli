// Package meshtransfer holds the CLIENT-side primitives a later cache-transfer
// delegation flow (aceteam-ai/aceteam#8553, S3.1) will use to fetch already-cached
// model artifacts from another same-owner citadel node over the mesh. This slice
// (S3.0) is IDENTITY + TRANSPORT ONLY: it preserves a verified stable device id
// and owner id through a client-side resolver, and provides a strict mesh-only
// HTTP transport plus a same-peer certificate bootstrap. It deliberately does NOT
// add delegation leases, passcode bypasses, Redis credentials, or any
// cache-serving behavior — those are S3.1 and are not folded in here.
//
// # Standalone by design
//
// This package imports only the standard library. Callers inject a PeerResolver
// (production wraps network.WhoIsPeer) and a Dialer (production passes
// network.Dial), which keeps the identity/verification/transport logic pure and
// unit-testable with no live mesh — mirroring internal/mesh and internal/ingress.
// The cmd layer bridges internal/network to these seams; internal/network is
// never imported here (the leaf-package rule).
//
// # Trust model (fail closed everywhere)
//
// Identity binding uses ONLY the coordination server's verified mesh identity —
// the stable device id (network.PeerIdentity.StableID) and owner id (OwnerID) —
// never a hostname, IP, display name, or job payload. A non-mesh address never
// resolves to a peer; an unverified, wrong-device, or different-owner peer never
// yields a VerifiedPeer. The transport dials ONLY the verified peer's pinned mesh
// IP and pins the certificate captured over the authenticated mesh connection, so
// no public DNS, arbitrary URL, proxy, redirect, or unverified certificate can
// steer a cache-transfer request off the intended peer.
package meshtransfer

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
)

// meshPrefixes are the address ranges the coordination server (Headscale/tsnet)
// assigns mesh IPs from: the 100.64.0.0/10 CGNAT range (IPv4, the same range
// internal/ingress and internal/egressrelay reason about) and the Tailscale ULA
// (IPv6). An address outside every range is not a mesh address and is refused
// before any dial or resolve — a non-mesh RemoteAddr must never resolve to a peer.
var meshPrefixes = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("fd7a:115c:a1e0::/48"),
}

// isMeshIP reports whether ip is inside one of the mesh ranges.
func isMeshIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	for _, p := range meshPrefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// PeerIdentity is the client-resolver view of a verified mesh peer. It is the
// mirror of network.PeerIdentity carried across the leaf-package boundary; the
// cmd layer copies the fields from network.WhoIsPeer's result. StableID and
// OwnerID are the rename- and IP-reuse-stable trust keys — an empty value means
// "unverified for that dimension" and makes Verify fail closed.
type PeerIdentity struct {
	// NodeName is the peer's node name. Display-only (changes on rename); never a
	// trust key.
	NodeName string
	// LoginName is the peer's tailnet user login. Display-oriented; OwnerID is the
	// stable owner trust key.
	LoginName string
	// StableID is the coordination server's stable node identifier
	// (tailcfg.StableNodeID). It survives a rename and is not reassigned on IP
	// reuse, so it is the device-binding key.
	StableID string
	// OwnerID is the stable numeric owner identifier (tailcfg.UserID string form).
	OwnerID string
	// SameOwner reports whether the peer belongs to the same tailnet owner/org as
	// this node.
	SameOwner bool
}

// Dialer dials addr ("ip:port") over the mesh. Production passes network.Dial
// (tsnet userspace netstack); tests pass a dialer targeting a local server.
type Dialer func(ctx context.Context, network, addr string) (net.Conn, error)

// PeerResolver resolves a mesh address ("ip" or "ip:port") to its verified mesh
// identity. Production wraps network.WhoIsPeer:
//
//	func(ctx context.Context, addr string) (*meshtransfer.PeerIdentity, error) {
//	    id, err := network.WhoIsPeer(ctx, addr)
//	    if err != nil {
//	        return nil, err
//	    }
//	    return &meshtransfer.PeerIdentity{
//	        NodeName:  id.NodeName,
//	        LoginName: id.LoginName,
//	        StableID:  id.StableID,
//	        OwnerID:   id.OwnerID,
//	        SameOwner: id.SameOwner,
//	    }, nil
//	}
//
// An error (or a nil identity) MUST be treated as unverified: Verify fails closed.
type PeerResolver func(ctx context.Context, meshAddr string) (*PeerIdentity, error)

// Expected is the identity a caller requires the target peer to match. An empty
// field is "no additional constraint on that dimension", but the baseline checks
// in Verify (same-owner, non-empty stable id, non-empty owner id) ALWAYS apply.
// A cache-transfer caller that knows which device it intends to reach sets
// StableID so a renamed impostor or a reused IP fails closed.
type Expected struct {
	// StableID, when set, must equal the peer's StableID exactly.
	StableID string
	// OwnerID, when set, must equal the peer's OwnerID exactly.
	OwnerID string
}

// VerifiedPeer is a mesh peer whose identity Verify has confirmed. It is the
// only value the transport and bootstrap accept — construct one via Verify,
// never by hand.
type VerifiedPeer struct {
	// MeshIP is the verified peer's mesh address. The transport pins dials to it.
	MeshIP netip.Addr
	// Identity is the verified stable identity that reached the client resolver.
	Identity PeerIdentity
}

// Verify resolves the peer at meshIP and confirms it is a same-owner mesh peer
// with a stable device id and owner id, optionally matching a caller-required
// device/owner. It fails closed on every ambiguity:
//
//   - meshIP is not a valid mesh IP (a non-mesh RemoteAddr never resolves);
//   - the resolver errors or returns no identity (unverified);
//   - the peer is not same-owner (a different-owner or shared-in peer);
//   - the peer has no stable device id or no owner id (missing identity);
//   - want.StableID/want.OwnerID is set and does not match (renamed impostor,
//     reused IP, or wrong owner).
//
// Only on success does it return a VerifiedPeer carrying the verified identity.
//
// The coordination server (Headscale) vouches the StableID — it sets
// tailcfg.Node.StableID from the node id in its map response — so a real
// same-owner node resolves with a non-empty StableID; the empty-StableID
// fail-closed branch guards against a peer the control plane did not vouch, not
// the normal case.
func Verify(ctx context.Context, resolver PeerResolver, meshIP string, want Expected) (*VerifiedPeer, error) {
	if resolver == nil {
		return nil, errors.New("meshtransfer: nil peer resolver")
	}
	ip, err := netip.ParseAddr(strings.TrimSpace(meshIP))
	if err != nil {
		return nil, fmt.Errorf("meshtransfer: %q is not an IP address: %w", meshIP, err)
	}
	ip = ip.Unmap()
	if !isMeshIP(ip) {
		return nil, fmt.Errorf("meshtransfer: %s is not a mesh address", ip)
	}
	id, err := resolver(ctx, ip.String())
	if err != nil {
		return nil, fmt.Errorf("meshtransfer: verify peer %s: %w", ip, err)
	}
	if id == nil {
		return nil, fmt.Errorf("meshtransfer: verify peer %s: no identity", ip)
	}
	if !id.SameOwner {
		return nil, fmt.Errorf("meshtransfer: peer %s is not a same-owner mesh peer", ip)
	}
	if id.StableID == "" {
		return nil, fmt.Errorf("meshtransfer: peer %s has no stable device id", ip)
	}
	if id.OwnerID == "" {
		return nil, fmt.Errorf("meshtransfer: peer %s has no owner id", ip)
	}
	if want.StableID != "" && id.StableID != want.StableID {
		return nil, fmt.Errorf("meshtransfer: peer %s device id %q does not match expected %q", ip, id.StableID, want.StableID)
	}
	if want.OwnerID != "" && id.OwnerID != want.OwnerID {
		return nil, fmt.Errorf("meshtransfer: peer %s owner id %q does not match expected %q", ip, id.OwnerID, want.OwnerID)
	}
	return &VerifiedPeer{MeshIP: ip, Identity: *id}, nil
}
