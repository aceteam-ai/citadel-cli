// Package socks implements a minimal RFC 1928 SOCKS5 server (CONNECT only,
// no BIND/UDP-ASSOCIATE), built for citadel #786 ("citadel socks", a
// dynamic-forward proxy analogous to `ssh -D`).
//
// The server is dialer-agnostic: every CONNECT request is satisfied by
// calling an injected Dialer rather than net.Dial directly. `cmd/socks.go`
// wires network.Dial in, so a CONNECT to a bare mesh hostname resolves via
// MagicDNS for free (network.Dial already does that for citadel proxy /
// citadel connect). The injection point exists specifically so citadel #787
// (a node-side egress relay) can reuse this exact server with a different
// dialer instead of a second implementation.
package socks

import (
	"context"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// SOCKS5 protocol constants (RFC 1928 / RFC 1929).
const (
	version5 = 0x05

	methodNoAuth       = 0x00
	methodUserPass     = 0x02
	methodNoAcceptable = 0xFF

	cmdConnect      = 0x01
	cmdBind         = 0x02
	cmdUDPAssociate = 0x03

	atypIPv4   = 0x01
	atypDomain = 0x03
	atypIPv6   = 0x04

	repSucceeded           = 0x00
	repGeneralFailure      = 0x01
	repNotAllowed          = 0x02
	repNetworkUnreachable  = 0x03
	repHostUnreachable     = 0x04
	repConnectionRefused   = 0x05
	repTTLExpired          = 0x06
	repCommandNotSupported = 0x07
	repAddrNotSupported    = 0x08

	userPassAuthVersion = 0x01
	userPassStatusOK    = 0x00
	userPassStatusFail  = 0x01
)

// Dialer dials network ("tcp") to addr ("host:port") and returns the
// resulting connection. addr's host is passed through EXACTLY as received
// off the wire — including an unresolved DOMAINNAME — so an implementation
// backed by network.Dial gets MagicDNS/mesh-name resolution for free.
type Dialer func(ctx context.Context, network, addr string) (net.Conn, error)

// Options configures a Server.
type Options struct {
	// Dialer is required. It is called once per accepted CONNECT request.
	Dialer Dialer

	// Username/Password, if both non-empty, require SOCKS5 username/password
	// auth (RFC 1929) for every connection. If either is empty, the server
	// offers/accepts only the no-auth method.
	Username string
	Password string

	// MaxConns bounds concurrent connections being served. 0 means
	// unlimited. A connection over the limit is closed immediately.
	MaxConns int

	// DialTimeout bounds each call to Dialer. Defaults to 30s.
	DialTimeout time.Duration

	// Logf receives verbose per-connection diagnostics. Defaults to a no-op.
	Logf func(format string, args ...any)
}

// Server is a minimal SOCKS5 CONNECT-only proxy server.
type Server struct {
	dialer       Dialer
	username     string
	password     string
	authRequired bool
	dialTimeout  time.Duration
	logf         func(format string, args ...any)

	sem chan struct{} // nil when Options.MaxConns == 0 (unlimited)
}

// New constructs a Server. Dialer is required.
func New(opts Options) (*Server, error) {
	if opts.Dialer == nil {
		return nil, errors.New("socks: Dialer is required")
	}
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	dialTimeout := opts.DialTimeout
	if dialTimeout <= 0 {
		dialTimeout = 30 * time.Second
	}
	s := &Server{
		dialer:       opts.Dialer,
		username:     opts.Username,
		password:     opts.Password,
		authRequired: opts.Username != "" && opts.Password != "",
		dialTimeout:  dialTimeout,
		logf:         logf,
	}
	if opts.MaxConns > 0 {
		s.sem = make(chan struct{}, opts.MaxConns)
	}
	return s, nil
}

// Serve accepts connections from ln until ctx is cancelled or Accept fails.
// It blocks until every in-flight connection has been handled. A cancelled
// ctx also closes ln (unblocking Accept) and is treated as a clean shutdown
// (nil return).
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	stopped := make(chan struct{})
	defer close(stopped)
	go func() {
		select {
		case <-ctx.Done():
			ln.Close()
		case <-stopped:
		}
	}()

	var wg sync.WaitGroup
	for {
		conn, err := ln.Accept()
		if err != nil {
			wg.Wait()
			if ctx.Err() != nil {
				return nil
			}
			return err
		}

		if s.sem != nil {
			select {
			case s.sem <- struct{}{}:
			default:
				s.logf("socks: max connections reached, rejecting %s", conn.RemoteAddr())
				conn.Close()
				continue
			}
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.sem != nil {
				defer func() { <-s.sem }()
			}
			defer conn.Close()
			s.ServeConn(ctx, conn)
		}()
	}
}

// ServeConn handles exactly one SOCKS5 connection: method negotiation,
// optional auth, the CONNECT request, and the bidirectional relay once
// established. It does not close conn; the caller owns that (Serve's accept
// loop does it via defer).
func (s *Server) ServeConn(ctx context.Context, conn net.Conn) {
	if err := s.serveConn(ctx, conn); err != nil {
		s.logf("socks: %s: %v", conn.RemoteAddr(), err)
	}
}

func (s *Server) serveConn(ctx context.Context, conn net.Conn) error {
	if err := s.negotiateAuth(conn); err != nil {
		return fmt.Errorf("auth negotiation: %w", err)
	}

	req, err := readRequest(conn)
	if err != nil {
		return fmt.Errorf("read request: %w", err)
	}

	if req.cmd != cmdConnect {
		writeReply(conn, repCommandNotSupported, nil) //nolint:errcheck
		return fmt.Errorf("unsupported command 0x%02x (only CONNECT is supported)", req.cmd)
	}

	dialCtx := ctx
	if s.dialTimeout > 0 {
		var cancel context.CancelFunc
		dialCtx, cancel = context.WithTimeout(ctx, s.dialTimeout)
		defer cancel()
	}

	target := net.JoinHostPort(req.host, strconv.Itoa(int(req.port)))
	remote, err := s.dialer(dialCtx, "tcp", target)
	if err != nil {
		writeReply(conn, repForDialError(err), nil) //nolint:errcheck
		return fmt.Errorf("dial %s: %w", target, err)
	}
	defer remote.Close()

	var bindAddr *net.TCPAddr
	if la, ok := remote.LocalAddr().(*net.TCPAddr); ok {
		bindAddr = la
	}
	if err := writeReply(conn, repSucceeded, bindAddr); err != nil {
		return fmt.Errorf("write reply: %w", err)
	}

	relay(conn, remote)
	return nil
}

// negotiateAuth performs the RFC 1928 method-selection exchange and, if
// username/password auth is required, the RFC 1929 sub-negotiation.
func (s *Server) negotiateAuth(conn net.Conn) error {
	var hdr [2]byte
	if _, err := io.ReadFull(conn, hdr[:]); err != nil {
		return err
	}
	if hdr[0] != version5 {
		return fmt.Errorf("unsupported SOCKS version 0x%02x", hdr[0])
	}

	nmethods := int(hdr[1])
	methods := make([]byte, nmethods)
	if nmethods > 0 {
		if _, err := io.ReadFull(conn, methods); err != nil {
			return err
		}
	}

	hasNoAuth, hasUserPass := false, false
	for _, m := range methods {
		switch m {
		case methodNoAuth:
			hasNoAuth = true
		case methodUserPass:
			hasUserPass = true
		}
	}

	selected := byte(methodNoAcceptable)
	switch {
	case s.authRequired && hasUserPass:
		selected = methodUserPass
	case !s.authRequired && hasNoAuth:
		selected = methodNoAuth
	}

	if _, err := conn.Write([]byte{version5, selected}); err != nil {
		return err
	}
	if selected == methodNoAcceptable {
		return errors.New("no acceptable authentication method offered")
	}
	if selected == methodUserPass {
		return s.authenticateUserPass(conn)
	}
	return nil
}

func (s *Server) authenticateUserPass(conn net.Conn) error {
	var verLen [2]byte
	if _, err := io.ReadFull(conn, verLen[:]); err != nil {
		return err
	}
	if verLen[0] != userPassAuthVersion {
		return fmt.Errorf("unsupported username/password auth version 0x%02x", verLen[0])
	}

	uname := make([]byte, verLen[1])
	if len(uname) > 0 {
		if _, err := io.ReadFull(conn, uname); err != nil {
			return err
		}
	}

	var plen [1]byte
	if _, err := io.ReadFull(conn, plen[:]); err != nil {
		return err
	}
	passwd := make([]byte, plen[0])
	if len(passwd) > 0 {
		if _, err := io.ReadFull(conn, passwd); err != nil {
			return err
		}
	}

	ok := constantTimeEqual(string(uname), s.username) && constantTimeEqual(string(passwd), s.password)
	status := byte(userPassStatusOK)
	if !ok {
		status = userPassStatusFail
	}
	if _, err := conn.Write([]byte{userPassAuthVersion, status}); err != nil {
		return err
	}
	if !ok {
		return errors.New("invalid username or password")
	}
	return nil
}

func constantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		// Still run the comparison so failure timing doesn't trivially leak
		// length, then report unequal.
		subtle.ConstantTimeCompare([]byte(a), []byte(a))
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// request is a parsed SOCKS5 request (RFC 1928 §4).
type request struct {
	cmd  byte
	host string
	port uint16
}

// readRequest fully parses the request line, including address+port, even
// for a command this server will go on to reject — SOCKS5 framing requires
// consuming exactly what the client sent regardless of whether the server
// intends to honor it.
func readRequest(conn net.Conn) (*request, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(conn, hdr[:]); err != nil {
		return nil, err
	}
	if hdr[0] != version5 {
		return nil, fmt.Errorf("unsupported SOCKS version 0x%02x in request", hdr[0])
	}

	cmd := hdr[1]
	atyp := hdr[3]

	var host string
	switch atyp {
	case atypIPv4:
		var b [4]byte
		if _, err := io.ReadFull(conn, b[:]); err != nil {
			return nil, err
		}
		host = net.IP(b[:]).String()
	case atypIPv6:
		var b [16]byte
		if _, err := io.ReadFull(conn, b[:]); err != nil {
			return nil, err
		}
		host = net.IP(b[:]).String()
	case atypDomain:
		var l [1]byte
		if _, err := io.ReadFull(conn, l[:]); err != nil {
			return nil, err
		}
		domain := make([]byte, l[0])
		if l[0] > 0 {
			if _, err := io.ReadFull(conn, domain); err != nil {
				return nil, err
			}
		}
		host = string(domain)
	default:
		writeReply(conn, repAddrNotSupported, nil) //nolint:errcheck
		return nil, fmt.Errorf("unsupported address type 0x%02x", atyp)
	}

	var portBuf [2]byte
	if _, err := io.ReadFull(conn, portBuf[:]); err != nil {
		return nil, err
	}

	return &request{cmd: cmd, host: host, port: binary.BigEndian.Uint16(portBuf[:])}, nil
}

// writeReply writes a SOCKS5 reply. bindAddr may be nil, in which case
// 0.0.0.0:0 is reported (the common convention for a CONNECT reply, since
// most clients ignore BND.ADDR/BND.PORT for CONNECT).
func writeReply(conn net.Conn, rep byte, bindAddr *net.TCPAddr) error {
	atyp := byte(atypIPv4)
	ip := net.IPv4zero
	var port uint16

	if bindAddr != nil {
		if v4 := bindAddr.IP.To4(); v4 != nil {
			ip = v4
			atyp = atypIPv4
		} else if v6 := bindAddr.IP.To16(); v6 != nil {
			ip = v6
			atyp = atypIPv6
		}
		port = uint16(bindAddr.Port)
	}

	buf := make([]byte, 0, 6+len(ip))
	buf = append(buf, version5, rep, 0x00, atyp)
	buf = append(buf, ip...)
	portBuf := make([]byte, 2)
	binary.BigEndian.PutUint16(portBuf, port)
	buf = append(buf, portBuf...)

	_, err := conn.Write(buf)
	return err
}

// repForDialError maps a Dialer error to the closest SOCKS5 reply code.
func repForDialError(err error) byte {
	if errors.Is(err, syscall.ECONNREFUSED) {
		return repConnectionRefused
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return repHostUnreachable
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return repTTLExpired
	}
	return repGeneralFailure
}

// relay copies bidirectionally between a and b until both directions have
// finished (mirroring the splice loop in cmd/connect.go and cmd/proxy.go).
func relay(a, b net.Conn) {
	done := make(chan struct{}, 2)

	go func() {
		io.Copy(a, b) //nolint:errcheck
		if cw, ok := a.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite() //nolint:errcheck
		}
		done <- struct{}{}
	}()

	go func() {
		io.Copy(b, a) //nolint:errcheck
		if cw, ok := b.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite() //nolint:errcheck
		}
		done <- struct{}{}
	}()

	<-done
	<-done
}
