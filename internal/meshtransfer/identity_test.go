package meshtransfer

import (
	"context"
	"testing"
)

// recordingResolver returns a fixed identity/error and records the address it was
// asked to resolve, so tests can assert identity comes ONLY from the resolver (the
// verified mesh identity), never from a caller-supplied name or a request header.
type recordingResolver struct {
	id     *PeerIdentity
	err    error
	called []string
}

func (r *recordingResolver) resolve(_ context.Context, addr string) (*PeerIdentity, error) {
	r.called = append(r.called, addr)
	return r.id, r.err
}

func sameOwnerIdentity() *PeerIdentity {
	return &PeerIdentity{
		NodeName:  "peer-node",
		LoginName: "owner@example.com",
		StableID:  "nodekey-stable-abc",
		OwnerID:   "userid:7",
		SameOwner: true,
	}
}

// Identity reaches the client resolver: a verified peer carries the stable device
// id and owner id straight from the resolver.
func TestVerify_CarriesStableIdentityToClientResolver(t *testing.T) {
	r := &recordingResolver{id: sameOwnerIdentity()}
	peer, err := Verify(context.Background(), r.resolve, "100.64.0.9", Expected{})
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if peer.Identity.StableID != "nodekey-stable-abc" || peer.Identity.OwnerID != "userid:7" {
		t.Errorf("verified identity = %+v, want stable/owner id preserved", peer.Identity)
	}
	if peer.MeshIP.String() != "100.64.0.9" {
		t.Errorf("MeshIP = %s, want 100.64.0.9", peer.MeshIP)
	}
	// Identity was resolved from the mesh IP, not from any display name/header.
	if len(r.called) != 1 || r.called[0] != "100.64.0.9" {
		t.Errorf("resolver called with %v, want exactly [100.64.0.9]", r.called)
	}
}

// A renamed peer with the SAME stable id still verifies — the node NAME is never
// a trust key, so a legitimate rename does not lock the caller out.
func TestVerify_RenameKeepsSameStableIDStillVerifies(t *testing.T) {
	id := sameOwnerIdentity()
	id.NodeName = "brand-new-name" // renamed since the caller last saw it
	r := &recordingResolver{id: id}
	if _, err := Verify(context.Background(), r.resolve, "100.64.0.9", Expected{StableID: "nodekey-stable-abc"}); err != nil {
		t.Fatalf("Verify() with matching StableID but changed name = %v, want success", err)
	}
}

func TestVerify_FailsClosed(t *testing.T) {
	tests := []struct {
		name     string
		meshIP   string
		id       *PeerIdentity
		err      error
		want     Expected
		resolved bool // whether the resolver should have been consulted at all
	}{
		{
			name:   "non-mesh public IP never resolves",
			meshIP: "8.8.8.8",
			id:     sameOwnerIdentity(),
		},
		{
			name:   "hostname is not a mesh address",
			meshIP: "peer.example.com",
			id:     sameOwnerIdentity(),
		},
		{
			name:   "RFC1918 LAN IP is not a mesh address",
			meshIP: "192.168.2.9",
			id:     sameOwnerIdentity(),
		},
		{
			name:     "resolver error is unverified",
			meshIP:   "100.64.0.9",
			err:      context.DeadlineExceeded,
			resolved: true,
		},
		{
			name:     "nil identity is unverified",
			meshIP:   "100.64.0.9",
			id:       nil,
			resolved: true,
		},
		{
			name:     "different owner fails closed",
			meshIP:   "100.64.0.9",
			id:       &PeerIdentity{StableID: "s", OwnerID: "userid:99", SameOwner: false},
			resolved: true,
		},
		{
			name:     "missing stable device id fails closed",
			meshIP:   "100.64.0.9",
			id:       &PeerIdentity{StableID: "", OwnerID: "userid:7", SameOwner: true},
			resolved: true,
		},
		{
			name:     "missing owner id fails closed",
			meshIP:   "100.64.0.9",
			id:       &PeerIdentity{StableID: "s", OwnerID: "", SameOwner: true},
			resolved: true,
		},
		{
			name:     "wrong device (IP reuse / renamed impostor) fails closed",
			meshIP:   "100.64.0.9",
			id:       sameOwnerIdentity(), // StableID nodekey-stable-abc
			want:     Expected{StableID: "nodekey-stable-DIFFERENT"},
			resolved: true,
		},
		{
			name:     "wrong owner fails closed",
			meshIP:   "100.64.0.9",
			id:       sameOwnerIdentity(), // OwnerID userid:7
			want:     Expected{OwnerID: "userid:999"},
			resolved: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &recordingResolver{id: tt.id, err: tt.err}
			peer, err := Verify(context.Background(), r.resolve, tt.meshIP, tt.want)
			if err == nil {
				t.Fatalf("Verify() = %+v, nil error; want fail closed", peer)
			}
			if peer != nil {
				t.Errorf("Verify() returned peer %+v on failure; want nil", peer)
			}
			if !tt.resolved && len(r.called) != 0 {
				t.Errorf("resolver was consulted (%v) for a non-mesh address; want no resolution", r.called)
			}
		})
	}
}

func TestVerify_NilResolverFailsClosed(t *testing.T) {
	if _, err := Verify(context.Background(), nil, "100.64.0.9", Expected{}); err == nil {
		t.Fatal("Verify() with nil resolver = nil error, want fail closed")
	}
}
