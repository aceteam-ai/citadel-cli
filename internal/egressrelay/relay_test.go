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

	// A literal public IP -- not a hostname -- so this test exercises
	// "authorized peer reaches the dialer" without depending on real DNS
	// resolution (PolicyDialer now resolves hostnames itself; see the
	// dedicated TestPolicyDialer*/TestServerHostnameResolvingToPrivateIP...
	// tests below for hostname-resolution coverage, which stub resolveHost
	// instead of hitting the network).
	rep := socks5Connect(t, conn, "8.8.8.8", 443)
	if rep != 0x00 {
		t.Fatalf("expected SOCKS5 success (0x00), got 0x%02x", rep)
	}

	select {
	case addr := <-fd.dialed:
		if addr != "8.8.8.8:443" {
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

// --- PolicyDialer: hostname resolution, pinning, and the literal
// unspecified-address bypass (citadel #787 security-review follow-up) ------

// stubResolveHost swaps the package-level resolveHost hook for the duration
// of the test, restoring the real resolver on cleanup. Lets these tests
// drive PolicyDialer's hostname path deterministically without any real DNS.
func stubResolveHost(t *testing.T, fn func(ctx context.Context, host string) ([]net.IP, error)) {
	t.Helper()
	orig := resolveHost
	resolveHost = fn
	t.Cleanup(func() { resolveHost = orig })
}

func mustParseIP(t *testing.T, s string) net.IP {
	t.Helper()
	ip := net.ParseIP(s)
	if ip == nil {
		t.Fatalf("invalid test IP literal %q", s)
	}
	return ip
}

func TestPolicyDialerDeniesHostnameResolvingToPrivateIP(t *testing.T) {
	stubResolveHost(t, func(ctx context.Context, host string) ([]net.IP, error) {
		if host != "attacker.example" {
			t.Fatalf("unexpected resolve host %q", host)
		}
		return []net.IP{mustParseIP(t, "192.168.2.201")}, nil
	})

	fd := newFakeDialer()
	dial := PolicyDialer(fd.dial, func() bool { return false })

	_, err := dial(context.Background(), "tcp", "attacker.example:22")
	if err == nil {
		t.Fatal("expected a hostname resolving to a private IP to be denied")
	}

	select {
	case addr := <-fd.dialed:
		t.Fatalf("underlying dialer must never be called for a denied resolved address, was called with %s", addr)
	default:
	}
}

func TestPolicyDialerDeniesHostnameResolvingToUnspecifiedAddress(t *testing.T) {
	// 0.0.0.0/:: as a RESOLVED answer (not just a literal CONNECT target,
	// see TestPolicyDialerLiteral0000AndDoubleColonDeniedByDefault below) --
	// covers a DNS answer, not just what a client typed directly.
	stubResolveHost(t, func(ctx context.Context, host string) ([]net.IP, error) {
		return []net.IP{mustParseIP(t, "0.0.0.0")}, nil
	})

	fd := newFakeDialer()
	dial := PolicyDialer(fd.dial, func() bool { return false })

	if _, err := dial(context.Background(), "tcp", "weird.example:22"); err == nil {
		t.Fatal("expected a hostname resolving to 0.0.0.0 to be denied")
	}
	select {
	case addr := <-fd.dialed:
		t.Fatalf("underlying dialer must never be called, was called with %s", addr)
	default:
	}
}

func TestPolicyDialerPinsResolvedIPRatherThanRedialingHostname(t *testing.T) {
	// Proves the DNS-rebinding fix directly: the underlying dialer must
	// receive the EXACT address PolicyDialer validated, never the original
	// hostname (which a second, independent resolution inside the
	// underlying dialer could answer differently for).
	stubResolveHost(t, func(ctx context.Context, host string) ([]net.IP, error) {
		if host != "public.example" {
			t.Fatalf("unexpected resolve host %q", host)
		}
		return []net.IP{mustParseIP(t, "8.8.8.8")}, nil
	})

	fd := newFakeDialer()
	dial := PolicyDialer(fd.dial, func() bool { return false })

	if _, err := dial(context.Background(), "tcp", "public.example:443"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	select {
	case addr := <-fd.dialed:
		if addr != "8.8.8.8:443" {
			t.Fatalf("underlying dialer received %q, want the pinned resolved IP %q -- PolicyDialer must never hand the hostname back to the dialer", addr, "8.8.8.8:443")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for dialer to be called")
	}
}

func TestPolicyDialerDeniesWhenAnyResolvedAddressIsPrivate(t *testing.T) {
	// A multi-answer hostname (e.g. dual-stack, or round-robin) where only
	// SOME resolved addresses are private must be refused ENTIRELY, not
	// dialed via whichever answer happened to be public -- fail closed on
	// ambiguity rather than silently picking a "safe-looking" address.
	stubResolveHost(t, func(ctx context.Context, host string) ([]net.IP, error) {
		return []net.IP{mustParseIP(t, "8.8.8.8"), mustParseIP(t, "10.0.0.5")}, nil
	})

	fd := newFakeDialer()
	dial := PolicyDialer(fd.dial, func() bool { return false })

	if _, err := dial(context.Background(), "tcp", "mixed.example:443"); err == nil {
		t.Fatal("expected denial when any resolved address is private")
	}
	select {
	case addr := <-fd.dialed:
		t.Fatalf("underlying dialer must never be called, was called with %s", addr)
	default:
	}
}

func TestPolicyDialerResolveErrorRefusesRatherThanFallingThrough(t *testing.T) {
	stubResolveHost(t, func(ctx context.Context, host string) ([]net.IP, error) {
		return nil, errors.New("simulated resolver failure")
	})

	fd := newFakeDialer()
	dial := PolicyDialer(fd.dial, func() bool { return false })

	if _, err := dial(context.Background(), "tcp", "broken.example:443"); err == nil {
		t.Fatal("expected a resolver error to refuse the connection, not fall through to the underlying dialer")
	}
	select {
	case addr := <-fd.dialed:
		t.Fatalf("underlying dialer must never be called on a resolve error, was called with %s", addr)
	default:
	}
}

func TestPolicyDialerAllowLANSkipsResolutionAndPassesTargetThrough(t *testing.T) {
	// With allow_lan on, there is nothing to protect, so resolution/pinning
	// is skipped entirely and the original target reaches the dialer
	// unmodified -- proven here by making the resolver hook panic if it is
	// ever called.
	stubResolveHost(t, func(ctx context.Context, host string) ([]net.IP, error) {
		t.Fatal("resolveHost must not be called when allow_lan is on")
		return nil, nil
	})

	fd := newFakeDialer()
	dial := PolicyDialer(fd.dial, func() bool { return true })

	if _, err := dial(context.Background(), "tcp", "anything.example:443"); err != nil {
		t.Fatalf("unexpected error with allow_lan on: %v", err)
	}
	select {
	case addr := <-fd.dialed:
		if addr != "anything.example:443" {
			t.Fatalf("dialed unexpected target: %s", addr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for dialer to be called")
	}
}

func TestPolicyDialerLiteral0000AndDoubleColonDeniedByDefault(t *testing.T) {
	for _, target := range []string{"0.0.0.0:22", "[::]:22"} {
		t.Run(target, func(t *testing.T) {
			fd := newFakeDialer()
			dial := PolicyDialer(fd.dial, func() bool { return false })

			if _, err := dial(context.Background(), "tcp", target); err == nil {
				t.Fatalf("expected literal unspecified address %s to be denied", target)
			}
			select {
			case addr := <-fd.dialed:
				t.Fatalf("underlying dialer must never be called, was called with %s", addr)
			default:
			}
		})
	}
}

func TestPolicyDialerLiteral0000AllowedWithAllowLAN(t *testing.T) {
	for _, target := range []string{"0.0.0.0:22", "[::]:22"} {
		t.Run(target, func(t *testing.T) {
			fd := newFakeDialer()
			dial := PolicyDialer(fd.dial, func() bool { return true })

			if _, err := dial(context.Background(), "tcp", target); err != nil {
				t.Fatalf("unexpected error with allow_lan on: %v", err)
			}
			select {
			case <-fd.dialed:
			case <-time.After(2 * time.Second):
				t.Fatal("timed out waiting for dialer to be called")
			}
		})
	}
}

// --- Server end-to-end: hostname->private-IP denial through the real
// accept/authorize/PolicyDialer stack, not just the PolicyDialer unit -----

func TestServerHostnameResolvingToPrivateIPIsDeniedEndToEnd(t *testing.T) {
	stubResolveHost(t, func(ctx context.Context, host string) ([]net.IP, error) {
		if host != "internal.attacker.example" {
			t.Fatalf("unexpected resolve host %q", host)
		}
		return []net.IP{mustParseIP(t, "192.168.2.201")}, nil
	})

	resolver := &MockIdentityResolver{Identity: &PeerIdentity{LoginName: "alice", SameOwner: true}}
	fd := newFakeDialer()

	ln, cleanup := startTestServer(t, resolver, fd.dial, false)
	defer cleanup()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial relay: %v", err)
	}
	defer conn.Close()

	rep := socks5Connect(t, conn, "internal.attacker.example", 22)
	if rep == 0x00 {
		t.Fatal("expected a non-success SOCKS5 reply for a hostname resolving to a private IP")
	}

	select {
	case addr := <-fd.dialed:
		t.Fatalf("underlying dialer must never be called for a denied resolved address, was called with %s", addr)
	case <-time.After(200 * time.Millisecond):
		// Expected: no dial happened.
	}
}
