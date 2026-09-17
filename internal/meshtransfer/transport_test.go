package meshtransfer

import (
	"context"
	"crypto/x509"
	"net"
	"net/netip"
	"testing"
)

// recordingDialer records the address it was asked to dial and returns a dummy
// (unconnected) net.Conn via a pipe so the caller has something to close.
type recordingDialer struct {
	dialed []string
}

func (d *recordingDialer) dial(_ context.Context, _, addr string) (net.Conn, error) {
	d.dialed = append(d.dialed, addr)
	c, _ := net.Pipe()
	return c, nil
}

func TestMeshOnlyDial_OnlyDialsThePinnedPeer(t *testing.T) {
	peer := netip.MustParseAddr("100.64.0.9")

	t.Run("pinned peer passes through to the underlying dialer", func(t *testing.T) {
		d := &recordingDialer{}
		conn, err := meshOnlyDial(peer, d.dial)(context.Background(), "tcp", "100.64.0.9:8080")
		if err != nil {
			t.Fatalf("dial pinned peer = %v, want success", err)
		}
		_ = conn.Close()
		if len(d.dialed) != 1 || d.dialed[0] != "100.64.0.9:8080" {
			t.Errorf("underlying dialer saw %v, want [100.64.0.9:8080]", d.dialed)
		}
	})

	rejects := []struct {
		name string
		addr string
	}{
		{"different mesh peer", "100.64.0.10:8080"},
		{"public IP", "8.8.8.8:443"},
		{"RFC1918 LAN IP", "192.168.2.9:8080"},
		{"loopback", "127.0.0.1:8080"},
		{"hostname (would need DNS)", "peer.example.com:443"},
		{"malformed address", "not-an-address"},
	}
	for _, tt := range rejects {
		t.Run("refuses "+tt.name, func(t *testing.T) {
			d := &recordingDialer{}
			conn, err := meshOnlyDial(peer, d.dial)(context.Background(), "tcp", tt.addr)
			if err == nil {
				_ = conn.Close()
				t.Fatalf("dial %q = nil error, want refusal", tt.addr)
			}
			if len(d.dialed) != 0 {
				t.Errorf("underlying dialer was reached (%v) for a refused address", d.dialed)
			}
		})
	}
}

func TestVerifiedPeer_ClientFailsClosedOnNilInputs(t *testing.T) {
	peer := &VerifiedPeer{MeshIP: netip.MustParseAddr("100.64.0.9")}
	if _, err := peer.Client(nil, &x509.Certificate{}); err == nil {
		t.Error("Client(nil dialer) = nil error, want failure")
	}
	d := &recordingDialer{}
	if _, err := peer.Client(d.dial, nil); err == nil {
		t.Error("Client(nil leaf) = nil error, want failure")
	}
}
