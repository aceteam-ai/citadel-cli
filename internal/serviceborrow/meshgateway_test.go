package serviceborrow

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/meshtransfer"
)

// neverDial is a mesh dialer that fails the test if it is ever called — the proof
// that NewMeshGateway verifies identity BEFORE dialing a peer.
func neverDial(t *testing.T) meshtransfer.Dialer {
	return func(context.Context, string, string) (net.Conn, error) {
		t.Fatalf("dialer must not be called: identity must be verified before any dial")
		return nil, errors.New("unreachable")
	}
}

func TestMeshGateway_ForeignPeer_RefusedBeforeDial(t *testing.T) {
	// A verified peer that is NOT same-owner: Verify fails, so Connect refuses
	// and no byte is ever dialed to it.
	resolver := func(context.Context, string) (*meshtransfer.PeerIdentity, error) {
		return &meshtransfer.PeerIdentity{
			StableID:  "nodekey:foreign",
			OwnerID:   "userid:999",
			SameOwner: false, // different owner
		}, nil
	}
	gw := NewMeshGateway(resolver, neverDial(t), 8443)
	if _, err := gw.Connect(context.Background(), "100.64.0.9"); err == nil {
		t.Fatal("Connect to a foreign-owner peer must fail (and never dial)")
	}
}

func TestMeshGateway_EmptyStableID_RefusedBeforeDial(t *testing.T) {
	resolver := func(context.Context, string) (*meshtransfer.PeerIdentity, error) {
		return &meshtransfer.PeerIdentity{StableID: "", OwnerID: "userid:1", SameOwner: true}, nil
	}
	gw := NewMeshGateway(resolver, neverDial(t), 8443)
	if _, err := gw.Connect(context.Background(), "100.64.0.9"); err == nil {
		t.Fatal("Connect to a peer with no stable id must fail (and never dial)")
	}
}

func TestMeshGateway_NonMeshIP_RefusedBeforeDialOrResolve(t *testing.T) {
	// A non-mesh IP is refused by meshtransfer.Verify before the resolver is even
	// consulted — a non-mesh RemoteAddr must never resolve to a peer, and never
	// be dialed (meshOnlyDial would refuse it too). This is the structural
	// "never central, never hosted" guarantee.
	resolver := func(context.Context, string) (*meshtransfer.PeerIdentity, error) {
		t.Fatalf("resolver must not be consulted for a non-mesh IP")
		return nil, nil
	}
	gw := NewMeshGateway(resolver, neverDial(t), 8443)
	if _, err := gw.Connect(context.Background(), "8.8.8.8"); err == nil {
		t.Fatal("Connect to a non-mesh (public) IP must fail")
	}
}

func TestMeshGateway_ResolverError_RefusedBeforeDial(t *testing.T) {
	resolver := func(context.Context, string) (*meshtransfer.PeerIdentity, error) {
		return nil, errors.New("whois failed")
	}
	gw := NewMeshGateway(resolver, neverDial(t), 8443)
	if _, err := gw.Connect(context.Background(), "100.64.0.9"); err == nil {
		t.Fatal("a resolver error must fail closed (and never dial)")
	}
}

func TestMeshGateway_InvalidPort_Refused(t *testing.T) {
	resolver := func(context.Context, string) (*meshtransfer.PeerIdentity, error) {
		return &meshtransfer.PeerIdentity{StableID: "s", OwnerID: "o", SameOwner: true}, nil
	}
	gw := NewMeshGateway(resolver, neverDial(t), 0)
	if _, err := gw.Connect(context.Background(), "100.64.0.9"); err == nil {
		t.Fatal("an invalid gateway port must be refused")
	}
}
