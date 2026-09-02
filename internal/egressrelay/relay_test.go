package egressrelay

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/socks"
)

// --- Authorize --------------------------------------------------------

func TestAuthorizeSameOrgAccepted(t *testing.T) {
	resolver := &MockIdentityResolver{Identity: &PeerIdentity{
		NodeName:  "peer-1",
		LoginName: "alice@example.com",
		SameOwner: true,
	}}

	id, err := Authorize(context.Background(), resolver, "100.64.0.5:12345")
	if err != nil {
		t.Fatalf("expected same-org peer to be authorized, got error: %v", err)
	}
	if id == nil || id.LoginName != "alice@example.com" {
		t.Fatalf("unexpected identity: %+v", id)
	}
}

func TestAuthorizeDifferentOrgRejected(t *testing.T) {
	resolver := &MockIdentityResolver{Identity: &PeerIdentity{
		NodeName:  "peer-2",
		LoginName: "eve@other-org.example",
		SameOwner: false,
	}}

	id, err := Authorize(context.Background(), resolver, "100.64.0.9:12345")
	if err == nil {
		t.Fatal("expected different-org peer to be rejected")
	}
	if id != nil {
		t.Fatalf("expected nil identity on rejection, got %+v", id)
	}
}

func TestAuthorizeUnverifiableRejected(t *testing.T) {
	resolver := &MockIdentityResolver{Err: errors.New("not connected to AceTeam Network")}

	id, err := Authorize(context.Background(), resolver, "203.0.113.1:1")
	if err == nil {
		t.Fatal("expected unverifiable peer to be rejected")
	}
	if id != nil {
		t.Fatalf("expected nil identity on rejection, got %+v", id)
	}
}

func TestAuthorizeNilIdentityRejected(t *testing.T) {
	// A resolver that returns (nil, nil) -- no error, but no identity either
	// -- must still fail closed rather than panic or silently authorize.
	resolver := &MockIdentityResolver{Identity: nil}

	id, err := Authorize(context.Background(), resolver, "203.0.113.1:1")
	if err == nil {
		t.Fatal("expected nil identity to be rejected")
	}
	if id != nil {
		t.Fatalf("expected nil identity on rejection, got %+v", id)
	}
}

func TestAuthorizeNilResolverRejected(t *testing.T) {
	id, err := Authorize(context.Background(), nil, "203.0.113.1:1")
	if err == nil {
		t.Fatal("expected nil resolver to be rejected")
	}
	if id != nil {
		t.Fatalf("expected nil identity on rejection, got %+v", id)
	}
}

// --- Server (end-to-end over a loopback listener, injected dialer) ----

// dialedTarget records what the (fake) egress dialer was asked to reach, so
// tests can assert a connection either got all the way to "dial the real
// target" or was refused before ever reaching it.
type fakeDialer struct {
	dialed chan string
}

func newFakeDialer() *fakeDialer {
	return &fakeDialer{dialed: make(chan string, 8)}
}

func (f *fakeDialer) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	f.dialed <- addr
	// Return an in-memory pipe end whose peer is immediately closed, so
	// internal/socks's post-CONNECT bidirectional relay() sees EOF on its
	// very first read from this "remote" side (rather than blocking forever
	// waiting for data that will never come) once the test's own client
	// connection also closes. No real network access happens.
	server, client := net.Pipe()
	client.Close()
	return server, nil
}

// socks5Connect performs a minimal, no-auth SOCKS5 handshake and a CONNECT
// request against conn, returning the reply code byte. It intentionally
// implements just enough of RFC 1928 to drive the server under test; it does
// not need to be a general-purpose client.
func socks5Connect(t *testing.T, conn net.Conn, host string, port uint16) byte {
	t.Helper()
	r := bufio.NewReader(conn)

	// Method negotiation: version 5, 1 method, no-auth.
	if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		t.Fatalf("write method negotiation: %v", err)
	}
	methodReply := make([]byte, 2)
	if _, err := readFull(r, methodReply); err != nil {
		t.Fatalf("read method reply: %v", err)
	}

	// CONNECT request with a domain name address.
	req := []byte{0x05, 0x01, 0x00, 0x03, byte(len(host))}
	req = append(req, []byte(host)...)
	portBuf := make([]byte, 2)
	binary.BigEndian.PutUint16(portBuf, port)
	req = append(req, portBuf...)
	if _, err := conn.Write(req); err != nil {
		t.Fatalf("write connect request: %v", err)
	}

	hdr := make([]byte, 4)
	if _, err := readFull(r, hdr); err != nil {
		t.Fatalf("read connect reply header: %v", err)
	}
	rep := hdr[1]
	atyp := hdr[3]
	var addrLen int
	switch atyp {
	case 0x01:
		addrLen = 4
	case 0x04:
		addrLen = 16
	case 0x03:
		lenByte := make([]byte, 1)
		if _, err := readFull(r, lenByte); err != nil {
			t.Fatalf("read domain len: %v", err)
		}
		addrLen = int(lenByte[0])
	}
	rest := make([]byte, addrLen+2)
	if _, err := readFull(r, rest); err != nil {
		t.Fatalf("read connect reply tail: %v", err)
	}
	return rep
}

func readFull(r *bufio.Reader, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		m, err := r.Read(buf[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func startTestServer(t *testing.T, resolver IdentityResolver, dialer socks.Dialer, allowLAN bool) (net.Listener, func()) {
	t.Helper()
	srv, err := New(Options{
		Dialer:      dialer,
		Resolver:    resolver,
		AllowLAN:    func() bool { return allowLAN },
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve(ctx, ln)
	}()

	cleanup := func() {
		cancel()
		<-done
	}
	return ln, cleanup
}

func TestServerAuthorizedPeerReachesDialer(t *testing.T) {
	resolver := &MockIdentityResolver{Identity: &PeerIdentity{LoginName: "alice", SameOwner: true}}
	fd := newFakeDialer()

	ln, cleanup := startTestServer(t, resolver, fd.dial, false)
	defer cleanup()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial relay: %v", err)
	}
	defer conn.Close()

	rep := socks5Connect(t, conn, "example.com", 443)
	if rep != 0x00 {
		t.Fatalf("expected SOCKS5 success (0x00), got 0x%02x", rep)
	}

	select {
	case addr := <-fd.dialed:
		if addr != "example.com:443" {
			t.Fatalf("dialed unexpected target: %s", addr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for dialer to be called")
	}
}

func TestServerUnauthorizedPeerNeverReachesDialer(t *testing.T) {
	resolver := &MockIdentityResolver{Err: errors.New("not a verified peer")}
	fd := newFakeDialer()

	ln, cleanup := startTestServer(t, resolver, fd.dial, false)
	defer cleanup()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial relay: %v", err)
	}
	defer conn.Close()

	// An unauthorized connection is closed before any SOCKS5 bytes are
	// answered: writing the method-negotiation frame and then reading should
	// see the connection closed (EOF), not a SOCKS5 method-selection reply.
	if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		// A write racing the server's close is also an acceptable signal
		// that nothing was served.
		return
	}
	buf := make([]byte, 2)
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := conn.Read(buf)
	if err == nil && n > 0 {
		t.Fatalf("expected unauthorized connection to be closed with no SOCKS5 reply, got %d bytes: %v", n, buf[:n])
	}

	select {
	case addr := <-fd.dialed:
		t.Fatalf("dialer must never be called for an unauthorized peer, was called with %s", addr)
	case <-time.After(200 * time.Millisecond):
		// Expected: no dial happened.
	}
}

func TestServerDeniedDestinationRefusedWithoutDialing(t *testing.T) {
	resolver := &MockIdentityResolver{Identity: &PeerIdentity{LoginName: "alice", SameOwner: true}}
	fd := newFakeDialer()

	// allow_lan is false, so a private destination must be refused by the
	// policy dialer wrapper before the underlying fake dialer is ever
	// called.
	ln, cleanup := startTestServer(t, resolver, fd.dial, false)
	defer cleanup()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial relay: %v", err)
	}
	defer conn.Close()

	rep := socks5Connect(t, conn, "192.168.1.50", 22)
	if rep == 0x00 {
		t.Fatal("expected a non-success SOCKS5 reply for a denied LAN destination")
	}

	select {
	case addr := <-fd.dialed:
		t.Fatalf("underlying dialer must never be called for a denied destination, was called with %s", addr)
	case <-time.After(200 * time.Millisecond):
		// Expected: no dial happened.
	}
}

func TestServerAllowLANPermitsPrivateDestination(t *testing.T) {
	resolver := &MockIdentityResolver{Identity: &PeerIdentity{LoginName: "alice", SameOwner: true}}
	fd := newFakeDialer()

	ln, cleanup := startTestServer(t, resolver, fd.dial, true)
	defer cleanup()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial relay: %v", err)
	}
	defer conn.Close()

	rep := socks5Connect(t, conn, "192.168.1.50", 22)
	if rep != 0x00 {
		t.Fatalf("expected SOCKS5 success with allow_lan on, got 0x%02x", rep)
	}

	select {
	case addr := <-fd.dialed:
		if addr != "192.168.1.50:22" {
			t.Fatalf("dialed unexpected target: %s", addr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for dialer to be called")
	}
}
