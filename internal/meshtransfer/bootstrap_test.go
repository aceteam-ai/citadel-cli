package meshtransfer

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"testing"
	"time"
)

// redirectingDialer ignores the mesh address it is handed and dials a fixed real
// address instead (a local test server), recording every call. It stands in for
// network.Dial: the mesh-only guard runs BEFORE it, so it only ever sees the
// pinned peer's address.
type redirectingDialer struct {
	target string
	calls  []string
}

func (d *redirectingDialer) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	d.calls = append(d.calls, addr)
	var nd net.Dialer
	return nd.DialContext(ctx, network, d.target)
}

func newStatusServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	mux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/status", http.StatusFound)
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func serverPort(t *testing.T, srv *httptest.Server) int {
	t.Helper()
	tcp, ok := srv.Listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener addr %T is not *net.TCPAddr", srv.Listener.Addr())
	}
	return tcp.Port
}

// End-to-end: bootstrap captures the peer's leaf over the mesh, and a pinned
// client can then reach that peer's status endpoint — but refuses redirects and
// rejects any other certificate.
func TestBootstrapPeerCert_CapturesLeafAndPinsClient(t *testing.T) {
	srv := newStatusServer(t)
	dialer := &redirectingDialer{target: srv.Listener.Addr().String()}
	resolver := &recordingResolver{id: sameOwnerIdentity()}
	port := serverPort(t, srv)

	peer, leaf, err := BootstrapPeerCert(context.Background(), resolver.resolve, dialer.dial, "100.64.0.9", port, Expected{StableID: "nodekey-stable-abc"})
	if err != nil {
		t.Fatalf("BootstrapPeerCert() error = %v", err)
	}
	if peer.Identity.StableID != "nodekey-stable-abc" {
		t.Errorf("bootstrapped identity StableID = %q, want nodekey-stable-abc", peer.Identity.StableID)
	}
	if !leaf.Equal(srv.Certificate()) {
		t.Error("captured leaf does not match the status server's certificate")
	}
	// The bootstrap dialed only the pinned peer's status address.
	if len(dialer.calls) != 1 || dialer.calls[0] != "100.64.0.9:"+strconv.Itoa(port) {
		t.Errorf("bootstrap dialed %v, want the pinned peer status address only", dialer.calls)
	}

	client, err := peer.Client(dialer.dial, leaf)
	if err != nil {
		t.Fatalf("Client() error = %v", err)
	}

	// A GET to the pinned peer's status endpoint succeeds.
	resp, err := client.Get("https://100.64.0.9:" + strconv.Itoa(port) + "/status")
	if err != nil {
		t.Fatalf("GET /status over pinned client = %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /status status = %d, want 200", resp.StatusCode)
	}

	// A redirect is refused (a covert way off the pinned peer).
	if _, err := client.Get("https://100.64.0.9:" + strconv.Itoa(port) + "/redirect"); err == nil {
		t.Error("GET /redirect = nil error, want redirect refused")
	}
}

// The pinned client rejects a peer that presents a DIFFERENT (unverified)
// certificate than the one bootstrapped.
func TestPinnedClient_RejectsMismatchedCertificate(t *testing.T) {
	real := newStatusServer(t)
	// NOTE: every httptest.NewTLSServer shares one built-in certificate, so a
	// second test server would NOT be a distinct cert. Generate an unrelated
	// self-signed leaf instead.
	other := genSelfSignedLeaf(t)

	dialer := &redirectingDialer{target: real.Listener.Addr().String()}
	peer := &VerifiedPeer{MeshIP: netip.MustParseAddr("100.64.0.9")}

	// Pin the OTHER (unrelated) leaf, then dial the real server: the presented
	// cert won't match the pinned one.
	client, err := peer.Client(dialer.dial, other)
	if err != nil {
		t.Fatalf("Client() error = %v", err)
	}
	if _, err := client.Get("https://100.64.0.9:" + strconv.Itoa(serverPort(t, real)) + "/status"); err == nil {
		t.Error("GET with a mismatched pinned cert = nil error, want rejection")
	}
}

// Identity is verified BEFORE any dial: a resolver failure (or a non-same-owner
// peer) returns without ever touching the peer's certificate.
func TestBootstrapPeerCert_VerifiesIdentityBeforeDialing(t *testing.T) {
	srv := newStatusServer(t)
	port := serverPort(t, srv)

	t.Run("resolver error never dials", func(t *testing.T) {
		dialer := &redirectingDialer{target: srv.Listener.Addr().String()}
		resolver := &recordingResolver{err: context.DeadlineExceeded}
		if _, _, err := BootstrapPeerCert(context.Background(), resolver.resolve, dialer.dial, "100.64.0.9", port, Expected{StableID: "x"}); err == nil {
			t.Fatal("BootstrapPeerCert() = nil error, want fail closed")
		}
		if len(dialer.calls) != 0 {
			t.Errorf("dialer was called (%v) before identity verification", dialer.calls)
		}
	})

	t.Run("different-owner peer never dials", func(t *testing.T) {
		dialer := &redirectingDialer{target: srv.Listener.Addr().String()}
		resolver := &recordingResolver{id: &PeerIdentity{StableID: "s", OwnerID: "userid:99", SameOwner: false}}
		if _, _, err := BootstrapPeerCert(context.Background(), resolver.resolve, dialer.dial, "100.64.0.9", port, Expected{StableID: "s"}); err == nil {
			t.Fatal("BootstrapPeerCert() for a different-owner peer = nil error, want fail closed")
		}
		if len(dialer.calls) != 0 {
			t.Errorf("dialer was called (%v) for an unverified peer", dialer.calls)
		}
	})

	// IP reuse fails closed UNCONDITIONALLY at bootstrap: the caller committed to
	// device "nodekey-stable-abc", but the peer now at the IP is a different
	// device — no cert is ever captured.
	t.Run("wrong device (IP reuse) never dials", func(t *testing.T) {
		dialer := &redirectingDialer{target: srv.Listener.Addr().String()}
		resolver := &recordingResolver{id: sameOwnerIdentity()} // StableID nodekey-stable-abc
		if _, _, err := BootstrapPeerCert(context.Background(), resolver.resolve, dialer.dial, "100.64.0.9", port, Expected{StableID: "nodekey-stable-DIFFERENT"}); err == nil {
			t.Fatal("BootstrapPeerCert() for a reused IP (wrong device) = nil error, want fail closed")
		}
		if len(dialer.calls) != 0 {
			t.Errorf("dialer was called (%v) for a wrong-device peer", dialer.calls)
		}
	})

	// The cert-pin commitment point requires naming the device.
	t.Run("empty StableID is refused before any dial", func(t *testing.T) {
		dialer := &redirectingDialer{target: srv.Listener.Addr().String()}
		resolver := &recordingResolver{id: sameOwnerIdentity()}
		if _, _, err := BootstrapPeerCert(context.Background(), resolver.resolve, dialer.dial, "100.64.0.9", port, Expected{}); err == nil {
			t.Fatal("BootstrapPeerCert() with no StableID = nil error, want refusal")
		}
		if len(dialer.calls) != 0 || len(resolver.called) != 0 {
			t.Errorf("an unpinned bootstrap reached the resolver/dialer: resolver=%v dialer=%v", resolver.called, dialer.calls)
		}
	})

	t.Run("non-mesh target never dials", func(t *testing.T) {
		dialer := &redirectingDialer{target: srv.Listener.Addr().String()}
		resolver := &recordingResolver{id: sameOwnerIdentity()}
		if _, _, err := BootstrapPeerCert(context.Background(), resolver.resolve, dialer.dial, "8.8.8.8", port, Expected{StableID: "nodekey-stable-abc"}); err == nil {
			t.Fatal("BootstrapPeerCert() for a non-mesh target = nil error, want fail closed")
		}
		if len(dialer.calls) != 0 || len(resolver.called) != 0 {
			t.Errorf("a non-mesh target reached the resolver/dialer: resolver=%v dialer=%v", resolver.called, dialer.calls)
		}
	})
}

// genSelfSignedLeaf mints a throwaway self-signed leaf, distinct from httptest's
// shared built-in certificate, for the pinned-cert-mismatch test.
func genSelfSignedLeaf(t *testing.T) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "unrelated-peer"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	return leaf
}
