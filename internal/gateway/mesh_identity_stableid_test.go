package gateway

import (
	"context"
	"net/http"
	"testing"
)

// recordingResolver captures the address it was asked to resolve so a test can
// assert identity is derived from the connection's RemoteAddr, never from a
// spoofable forwarding header.
type recordingResolver struct {
	id      *MeshPeerIdentity
	gotAddr string
}

func (r *recordingResolver) ResolvePeer(_ context.Context, remoteAddr string) (*MeshPeerIdentity, error) {
	r.gotAddr = remoteAddr
	return r.id, nil
}

// resolvePeer (the shared gate helper behind both org exposures and the #1013
// cache-serving gate) must key on RemoteAddr and carry the stable device/owner
// id through — a spoofed X-Forwarded-For must not change which peer is resolved.
func TestResolvePeer_UsesRemoteAddrNotForwardingHeaderAndCarriesStableIdentity(t *testing.T) {
	r := &recordingResolver{id: &MeshPeerIdentity{
		NodeName:  "peer",
		LoginName: "owner@example.com",
		StableID:  "nodekey-stable-abc",
		OwnerID:   "userid:7",
		SameOwner: true,
	}}

	req, err := http.NewRequest(http.MethodGet, "https://node/cache/index", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.RemoteAddr = "100.64.0.9:5555"
	req.Header.Set("X-Forwarded-For", "100.64.0.10") // spoof attempt: a different mesh IP
	req.Header.Set("X-Real-IP", "203.0.113.7")

	id, ok := resolvePeer(r, req)
	if !ok || id == nil {
		t.Fatalf("resolvePeer() ok=%v id=%v, want a resolved identity", ok, id)
	}
	if r.gotAddr != "100.64.0.9:5555" {
		t.Errorf("resolver saw addr %q, want the connection RemoteAddr (spoofed headers ignored)", r.gotAddr)
	}
	if id.StableID != "nodekey-stable-abc" || id.OwnerID != "userid:7" {
		t.Errorf("resolved identity did not carry stable device/owner id: %+v", id)
	}
}

// A nil resolver (mesh identity unavailable) fails closed.
func TestResolvePeer_NilResolverFailsClosed(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://node/cache/index", nil)
	req.RemoteAddr = "100.64.0.9:5555"
	if id, ok := resolvePeer(nil, req); ok || id != nil {
		t.Errorf("resolvePeer(nil) = (%v, %v), want fail closed", id, ok)
	}
}
