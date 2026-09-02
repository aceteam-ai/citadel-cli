package socks

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"syscall"
	"testing"
	"time"
)

// recordingDialer implements Dialer against a fixed local echo listener,
// while recording the network/addr it was actually called with — so a test
// can assert a DOMAINNAME request reaches the dialer UNRESOLVED.
type recordingDialer struct {
	mu          sync.Mutex
	echoAddr    string
	calls       []dialCall
	err         error // if set, every Dial call fails with this error
	dialTimeout time.Duration
}

type dialCall struct {
	network string
	addr    string
}

func (d *recordingDialer) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	d.mu.Lock()
	d.calls = append(d.calls, dialCall{network: network, addr: addr})
	failErr := d.err
	d.mu.Unlock()

	if failErr != nil {
		return nil, failErr
	}

	var dialer net.Dialer
	return dialer.DialContext(ctx, "tcp", d.echoAddr)
}

func (d *recordingDialer) lastCall() dialCall {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.calls) == 0 {
		return dialCall{}
	}
	return d.calls[len(d.calls)-1]
}

// startEchoServer starts a bare TCP echo server and returns its address and
// a cleanup func.
func startEchoServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				io.Copy(c, c) //nolint:errcheck
			}(conn)
		}
	}()

	return ln.Addr().String()
}

// startSOCKSServer starts a Server backed by opts on a local listener and
// returns its address plus a cancel func that stops serving.
func startSOCKSServer(t *testing.T, opts Options) (addr string, cancel func()) {
	t.Helper()
	srv, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	ctx, cancelCtx := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.Serve(ctx, ln) //nolint:errcheck
	}()

	return ln.Addr().String(), func() {
		cancelCtx()
		<-done
	}
}

// --- raw protocol helpers (deliberately hand-rolled rather than pulling in
// a SOCKS client library, so the test drives the exact wire bytes) ---

func noAuthGreeting() []byte {
	return []byte{version5, 1, methodNoAuth}
}

func userPassGreeting() []byte {
	return []byte{version5, 1, methodUserPass}
}

func connectRequestIPv4(ip net.IP, port uint16) []byte {
	v4 := ip.To4()
	buf := []byte{version5, cmdConnect, 0x00, atypIPv4}
	buf = append(buf, v4...)
	portBuf := make([]byte, 2)
	binary.BigEndian.PutUint16(portBuf, port)
	return append(buf, portBuf...)
}

func connectRequestDomain(domain string, port uint16) []byte {
	buf := []byte{version5, cmdConnect, 0x00, atypDomain, byte(len(domain))}
	buf = append(buf, []byte(domain)...)
	portBuf := make([]byte, 2)
	binary.BigEndian.PutUint16(portBuf, port)
	return append(buf, portBuf...)
}

func bindRequestIPv4(ip net.IP, port uint16) []byte {
	buf := connectRequestIPv4(ip, port)
	buf[1] = cmdBind
	return buf
}

func udpAssociateRequestIPv4(ip net.IP, port uint16) []byte {
	buf := connectRequestIPv4(ip, port)
	buf[1] = cmdUDPAssociate
	return buf
}

func userPassAuthBytes(user, pass string) []byte {
	buf := []byte{userPassAuthVersion, byte(len(user))}
	buf = append(buf, []byte(user)...)
	buf = append(buf, byte(len(pass)))
	buf = append(buf, []byte(pass)...)
	return buf
}

func readReply(t *testing.T, conn net.Conn) (rep byte, full []byte) {
	t.Helper()
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(conn, hdr); err != nil {
		t.Fatalf("read reply header: %v", err)
	}
	var addrLen int
	switch hdr[3] {
	case atypIPv4:
		addrLen = 4
	case atypIPv6:
		addrLen = 16
	case atypDomain:
		lb := make([]byte, 1)
		io.ReadFull(conn, lb) //nolint:errcheck
		addrLen = int(lb[0])
	}
	rest := make([]byte, addrLen+2)
	if _, err := io.ReadFull(conn, rest); err != nil {
		t.Fatalf("read reply body: %v", err)
	}
	return hdr[1], append(hdr, rest...)
}

func TestNew_RequiresDialer(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("expected error when Dialer is nil")
	}
}

func TestNoAuthHandshakeAndConnect_IPv4(t *testing.T) {
	echoAddr := startEchoServer(t)
	dialer := &recordingDialer{echoAddr: echoAddr}
	addr, stop := startSOCKSServer(t, Options{Dialer: dialer.Dial})
	defer stop()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial socks server: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write(noAuthGreeting()); err != nil {
		t.Fatalf("write greeting: %v", err)
	}
	method := make([]byte, 2)
	if _, err := io.ReadFull(conn, method); err != nil {
		t.Fatalf("read method selection: %v", err)
	}
	if method[0] != version5 || method[1] != methodNoAuth {
		t.Fatalf("unexpected method selection: %v", method)
	}

	target := net.ParseIP("93.184.216.34") // arbitrary IPv4, dialer ignores it
	if _, err := conn.Write(connectRequestIPv4(target, 80)); err != nil {
		t.Fatalf("write connect request: %v", err)
	}

	rep, _ := readReply(t, conn)
	if rep != repSucceeded {
		t.Fatalf("expected repSucceeded, got 0x%02x", rep)
	}

	// Data round-trips bidirectionally through the relay.
	payload := []byte("hello over socks5")
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("echo mismatch: got %q want %q", got, payload)
	}

	call := dialer.lastCall()
	if call.network != "tcp" {
		t.Fatalf("dialer network = %q, want tcp", call.network)
	}
	if call.addr != "93.184.216.34:80" {
		t.Fatalf("dialer addr = %q, want 93.184.216.34:80", call.addr)
	}
}

func TestConnectDomain_PassesDomainUnresolvedToDialer(t *testing.T) {
	echoAddr := startEchoServer(t)
	dialer := &recordingDialer{echoAddr: echoAddr}
	addr, stop := startSOCKSServer(t, Options{Dialer: dialer.Dial})
	defer stop()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial socks server: %v", err)
	}
	defer conn.Close()

	handshake(t, conn)

	const domain = "some-mesh-node.internal.example"
	if _, err := conn.Write(connectRequestDomain(domain, 11434)); err != nil {
		t.Fatalf("write connect request: %v", err)
	}

	rep, _ := readReply(t, conn)
	if rep != repSucceeded {
		t.Fatalf("expected repSucceeded, got 0x%02x", rep)
	}

	call := dialer.lastCall()
	want := domain + ":11434"
	if call.addr != want {
		t.Fatalf("dialer addr = %q, want %q (domain must NOT be resolved by the server)", call.addr, want)
	}

	// Round-trip still works over the domain-addressed connection.
	payload := []byte("ping")
	conn.Write(payload) //nolint:errcheck
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("echo mismatch: got %q want %q", got, payload)
	}
}

func TestBindRejected(t *testing.T) {
	echoAddr := startEchoServer(t)
	dialer := &recordingDialer{echoAddr: echoAddr}
	addr, stop := startSOCKSServer(t, Options{Dialer: dialer.Dial})
	defer stop()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial socks server: %v", err)
	}
	defer conn.Close()

	handshake(t, conn)

	if _, err := conn.Write(bindRequestIPv4(net.ParseIP("127.0.0.1"), 9999)); err != nil {
		t.Fatalf("write bind request: %v", err)
	}

	rep, _ := readReply(t, conn)
	if rep != repCommandNotSupported {
		t.Fatalf("expected repCommandNotSupported for BIND, got 0x%02x", rep)
	}

	if len(dialer.calls) != 0 {
		t.Fatalf("dialer should never be called for a rejected BIND, got %d calls", len(dialer.calls))
	}
}

func TestUDPAssociateRejected(t *testing.T) {
	echoAddr := startEchoServer(t)
	dialer := &recordingDialer{echoAddr: echoAddr}
	addr, stop := startSOCKSServer(t, Options{Dialer: dialer.Dial})
	defer stop()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial socks server: %v", err)
	}
	defer conn.Close()

	handshake(t, conn)

	if _, err := conn.Write(udpAssociateRequestIPv4(net.ParseIP("127.0.0.1"), 9999)); err != nil {
		t.Fatalf("write udp associate request: %v", err)
	}

	rep, _ := readReply(t, conn)
	if rep != repCommandNotSupported {
		t.Fatalf("expected repCommandNotSupported for UDP ASSOCIATE, got 0x%02x", rep)
	}
}

func TestUsernamePasswordAuth_Accept(t *testing.T) {
	echoAddr := startEchoServer(t)
	dialer := &recordingDialer{echoAddr: echoAddr}
	addr, stop := startSOCKSServer(t, Options{
		Dialer:   dialer.Dial,
		Username: "alice",
		Password: "s3cret",
	})
	defer stop()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial socks server: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write(userPassGreeting()); err != nil {
		t.Fatalf("write greeting: %v", err)
	}
	method := make([]byte, 2)
	if _, err := io.ReadFull(conn, method); err != nil {
		t.Fatalf("read method selection: %v", err)
	}
	if method[1] != methodUserPass {
		t.Fatalf("expected server to select username/password method, got 0x%02x", method[1])
	}

	if _, err := conn.Write(userPassAuthBytes("alice", "s3cret")); err != nil {
		t.Fatalf("write auth: %v", err)
	}
	authReply := make([]byte, 2)
	if _, err := io.ReadFull(conn, authReply); err != nil {
		t.Fatalf("read auth reply: %v", err)
	}
	if authReply[1] != userPassStatusOK {
		t.Fatalf("expected auth success, got status 0x%02x", authReply[1])
	}

	if _, err := conn.Write(connectRequestIPv4(net.ParseIP("1.2.3.4"), 80)); err != nil {
		t.Fatalf("write connect: %v", err)
	}
	rep, _ := readReply(t, conn)
	if rep != repSucceeded {
		t.Fatalf("expected repSucceeded after valid auth, got 0x%02x", rep)
	}
}

func TestUsernamePasswordAuth_Reject(t *testing.T) {
	echoAddr := startEchoServer(t)
	dialer := &recordingDialer{echoAddr: echoAddr}
	addr, stop := startSOCKSServer(t, Options{
		Dialer:   dialer.Dial,
		Username: "alice",
		Password: "s3cret",
	})
	defer stop()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial socks server: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write(userPassGreeting()); err != nil {
		t.Fatalf("write greeting: %v", err)
	}
	method := make([]byte, 2)
	if _, err := io.ReadFull(conn, method); err != nil {
		t.Fatalf("read method selection: %v", err)
	}
	if method[1] != methodUserPass {
		t.Fatalf("expected server to select username/password method, got 0x%02x", method[1])
	}

	if _, err := conn.Write(userPassAuthBytes("alice", "wrong-password")); err != nil {
		t.Fatalf("write auth: %v", err)
	}
	authReply := make([]byte, 2)
	if _, err := io.ReadFull(conn, authReply); err != nil {
		t.Fatalf("read auth reply: %v", err)
	}
	if authReply[1] != userPassStatusFail {
		t.Fatalf("expected auth failure status, got 0x%02x", authReply[1])
	}

	if len(dialer.calls) != 0 {
		t.Fatalf("dialer should never be called after failed auth, got %d calls", len(dialer.calls))
	}
}

func TestNoAuthClientRejectedWhenAuthRequired(t *testing.T) {
	echoAddr := startEchoServer(t)
	dialer := &recordingDialer{echoAddr: echoAddr}
	addr, stop := startSOCKSServer(t, Options{
		Dialer:   dialer.Dial,
		Username: "alice",
		Password: "s3cret",
	})
	defer stop()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial socks server: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write(noAuthGreeting()); err != nil {
		t.Fatalf("write greeting: %v", err)
	}
	method := make([]byte, 2)
	if _, err := io.ReadFull(conn, method); err != nil {
		t.Fatalf("read method selection: %v", err)
	}
	if method[1] != methodNoAcceptable {
		t.Fatalf("expected methodNoAcceptable when client offers no auth but server requires it, got 0x%02x", method[1])
	}
}

func TestMaxConnsRejectsOverLimit(t *testing.T) {
	echoAddr := startEchoServer(t)
	dialer := &recordingDialer{echoAddr: echoAddr}
	addr, stop := startSOCKSServer(t, Options{Dialer: dialer.Dial, MaxConns: 1})
	defer stop()

	// Hold one connection open without completing the handshake, so it
	// occupies the single slot.
	held, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial socks server: %v", err)
	}
	defer held.Close()

	// Give the accept-loop goroutine a moment to claim the semaphore slot.
	time.Sleep(50 * time.Millisecond)

	second, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial socks server (second): %v", err)
	}
	defer second.Close()

	second.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	n, err := second.Read(buf)
	if n != 0 || err != io.EOF {
		t.Fatalf("expected second connection to be closed immediately (EOF), got n=%d err=%v", n, err)
	}
}

func TestDialFailure_ConnectionRefused(t *testing.T) {
	dialer := &recordingDialer{err: &net.OpError{Op: "dial", Err: errConnRefusedStub{}}}
	addr, stop := startSOCKSServer(t, Options{Dialer: dialer.Dial})
	defer stop()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial socks server: %v", err)
	}
	defer conn.Close()

	handshake(t, conn)

	if _, err := conn.Write(connectRequestIPv4(net.ParseIP("127.0.0.1"), 1)); err != nil {
		t.Fatalf("write connect: %v", err)
	}
	rep, _ := readReply(t, conn)
	if rep != repConnectionRefused {
		t.Fatalf("expected repConnectionRefused, got 0x%02x", rep)
	}
}

// errConnRefusedStub implements the minimal surface errors.Is(err,
// syscall.ECONNREFUSED) needs without depending on a real closed port
// (flaky under sandboxing / firewalls in CI).
type errConnRefusedStub struct{}

func (errConnRefusedStub) Error() string { return "connection refused (stub)" }
func (errConnRefusedStub) Is(target error) bool {
	return target == syscall.ECONNREFUSED
}

// handshake performs a no-auth negotiation and asserts it is accepted.
func handshake(t *testing.T, conn net.Conn) {
	t.Helper()
	if _, err := conn.Write(noAuthGreeting()); err != nil {
		t.Fatalf("write greeting: %v", err)
	}
	method := make([]byte, 2)
	if _, err := io.ReadFull(conn, method); err != nil {
		t.Fatalf("read method selection: %v", err)
	}
	if method[0] != version5 || method[1] != methodNoAuth {
		t.Fatalf("unexpected method selection: %v", method)
	}
}
