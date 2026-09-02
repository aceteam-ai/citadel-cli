// Package egressrelay implements citadel #787: an on-node SOCKS5 egress
// relay that lets ANOTHER citadel node on the mesh tunnel outbound traffic
// through THIS node's own network egress. It is the server-side counterpart
// to #786's client-side `citadel socks` command, and reuses that issue's
// internal/socks.Server (dialer-agnostic, CONNECT-only SOCKS5) instead of a
// second protocol implementation -- see that package's doc comment, which
// names this issue as the reason its Dialer is injectable.
//
// A plain internal/socks.Server is enough for #786 (a local-only proxy the
// caller already trusts, since it never leaves 127.0.0.1). This package adds
// the two things a MESH-FACING relay needs that a local proxy does not:
//
//   - Authorization: every inbound connection arrives from another mesh peer,
//     so it must be checked against a verified identity BEFORE a single byte
//     of the SOCKS protocol runs. See Authorize / IdentityResolver.
//   - Destination policy: an authorized peer can still ask this relay to
//     CONNECT somewhere on THIS node's own LAN or mesh (RFC1918, loopback,
//     link-local, or the 100.64.0.0/10 CGNAT range citadel's own mesh IPs
//     live in). Left open by default that would turn the relay into a
//     LAN-pivot primitive, so it is denied unless the operator opts in. See
//     IsDestinationAllowed.
//
// Security posture (owner decision, citadel #787): default OFF, same-org
// verified peers only, LAN/mesh destinations denied by default. Both knobs
// are persisted via internal/config.EgressRelay and are editable from three
// surfaces (the citadel CLI, the local MCP tool set, and the platform's
// APPLY_DEVICE_CONFIG push) -- see cmd/egress_relay.go, cmd/mcp_local.go, and
// internal/jobs/config_handler.go respectively.
package egressrelay

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/socks"
)

// PeerIdentity is the verified mesh identity of a connecting peer. It mirrors
// gateway.MeshPeerIdentity / terminal.MeshPeerIdentity's shape exactly (see
// those packages' doc comments for the same "standalone by design" reasoning
// this package follows): this is a local copy, not a shared type, so
// internal/egressrelay does not need to import internal/network.
type PeerIdentity struct {
	// NodeName is the peer's node name, for the audit log.
	NodeName string
	// LoginName is the peer's tailnet user login (e.g. an email).
	LoginName string
	// SameOwner reports whether the peer belongs to the same tailnet
	// owner/org as this node. This is the ENTIRE authorization gate for the
	// relay (citadel #787's "same-org verified peers only" posture) -- a
	// peer that resolves but has SameOwner==false is still refused.
	SameOwner bool
}

// IdentityResolver resolves an inbound connection's remote address to a
// verified mesh peer identity. Injected so this package stays standalone and
// unit-testable without a live mesh -- mirrors gateway.MeshIdentityResolver /
// terminal.MeshIdentityResolver exactly; production wires network.WhoIsPeer
// through the cmd layer (cmd/egress_relay_mesh_auth.go), the same split those
// two packages use and for the same reason (this package must not import
// internal/network).
type IdentityResolver interface {
	// ResolvePeer resolves remoteAddr ("ip:port") to a verified identity, or
	// an error if the peer cannot be verified.
	ResolvePeer(ctx context.Context, remoteAddr string) (*PeerIdentity, error)
}

// MockIdentityResolver is an IdentityResolver for tests: it returns a fixed
// identity (or error) regardless of the remote address.
type MockIdentityResolver struct {
	Identity *PeerIdentity
	Err      error
}

// ResolvePeer implements IdentityResolver for the mock.
func (m *MockIdentityResolver) ResolvePeer(context.Context, string) (*PeerIdentity, error) {
	if m.Err != nil {
		return nil, m.Err
	}
	return m.Identity, nil
}

// Authorize decides whether an inbound connection from remoteAddr may use the
// relay. Fail-closed (citadel #787's owner decision, "option 1"): a resolver
// error, a nil identity, or SameOwner==false are ALL treated as unauthorized
// -- there is no case in which an unverifiable or different-org peer is
// granted access. The returned identity is non-nil only on success, for the
// caller's audit log.
func Authorize(ctx context.Context, resolver IdentityResolver, remoteAddr string) (*PeerIdentity, error) {
	if resolver == nil {
		return nil, errors.New("egress relay: no identity resolver configured")
	}
	id, err := resolver.ResolvePeer(ctx, remoteAddr)
	if err != nil {
		return nil, fmt.Errorf("egress relay: peer %s could not be verified: %w", remoteAddr, err)
	}
	if id == nil {
		return nil, fmt.Errorf("egress relay: peer %s resolved to no identity", remoteAddr)
	}
	if !id.SameOwner {
		return nil, fmt.Errorf("egress relay: peer %s (%s) is not a same-org verified peer", remoteAddr, id.LoginName)
	}
	return id, nil
}

// PolicyDialer wraps an underlying egress dialer with the destination policy
// (IsDestinationAllowed): every CONNECT target is checked before the
// underlying dialer is ever called, so a denied destination never reaches
// net.Dial. allowLAN is read at call time (not captured once), so a
// long-running Server always applies its CURRENT configured policy rather
// than whatever was in effect when the Server was constructed.
func PolicyDialer(underlying socks.Dialer, allowLAN func() bool) socks.Dialer {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			host = addr
		}
		if ok, reason := IsDestinationAllowed(host, allowLAN()); !ok {
			return nil, fmt.Errorf("egress relay: destination %s refused: %s", addr, reason)
		}
		return underlying(ctx, network, addr)
	}
}

// Options configures a Server.
type Options struct {
	// Dialer is the underlying egress dialer, called for each AUTHORIZED
	// CONNECT request that also passes the destination policy. Production
	// wires plain (&net.Dialer{}).DialContext -- deliberately NOT
	// network.Dial: this relay's whole purpose is reaching the PUBLIC
	// internet from this node's own network path, not looping back through
	// the mesh (see cmd/egress_relay.go).
	Dialer socks.Dialer

	// Resolver authorizes each inbound connection. Required.
	Resolver IdentityResolver

	// AllowLAN reports whether the destination deny-list is disabled. Read
	// per-connection (a func, not a bool) so a long-running Server reflects a
	// config change without needing to be reconstructed.
	AllowLAN func() bool

	// MaxConns bounds concurrent connections. 0 means unlimited.
	MaxConns int

	// DialTimeout bounds each call to Dialer. Defaults to 30s (matches
	// internal/socks's own default).
	DialTimeout time.Duration

	// Logf receives verbose diagnostics, including every authorization
	// decision (accepted peer, or the reason a connection was refused).
	// Defaults to a no-op.
	Logf func(format string, args ...any)
}

// Server runs the egress relay's own accept loop on top of #786's
// socks.Server. Unlike a plain SOCKS5 proxy, every accepted connection is
// mesh-facing, so it is authorized BEFORE socks.Server.ServeConn ever runs --
// an unauthorized peer never reaches SOCKS5 method negotiation, and the
// connection is simply closed rather than answered with a decodable SOCKS5
// error (closing silently is deliberate: it does not confirm to a probing,
// unauthorized peer that something SOCKS5-shaped is listening here).
type Server struct {
	socksSrv *socks.Server
	resolver IdentityResolver
	logf     func(format string, args ...any)

	sem chan struct{} // nil when Options.MaxConns == 0 (unlimited)
}

// New constructs a Server. Resolver and Dialer are required.
func New(opts Options) (*Server, error) {
	if opts.Resolver == nil {
		return nil, errors.New("egressrelay: Resolver is required")
	}
	if opts.Dialer == nil {
		return nil, errors.New("egressrelay: Dialer is required")
	}
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	allowLAN := opts.AllowLAN
	if allowLAN == nil {
		allowLAN = func() bool { return false }
	}

	socksSrv, err := socks.New(socks.Options{
		Dialer:      PolicyDialer(opts.Dialer, allowLAN),
		MaxConns:    0, // this Server's own sem (below) gates admission instead
		DialTimeout: opts.DialTimeout,
		Logf:        logf,
	})
	if err != nil {
		return nil, err
	}

	s := &Server{
		socksSrv: socksSrv,
		resolver: opts.Resolver,
		logf:     logf,
	}
	if opts.MaxConns > 0 {
		s.sem = make(chan struct{}, opts.MaxConns)
	}
	return s, nil
}

// Serve accepts connections from ln until ctx is cancelled or Accept fails,
// authorizing each one before handing it to the underlying SOCKS5 server.
// Mirrors socks.Server.Serve's shutdown contract exactly: a cancelled ctx
// closes ln (unblocking Accept) and is a clean shutdown (nil return); Serve
// blocks until every in-flight connection has finished.
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
				s.logf("egress relay: max connections reached, rejecting %s", conn.RemoteAddr())
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
			s.serveConn(ctx, conn)
		}()
	}
}

func (s *Server) serveConn(ctx context.Context, conn net.Conn) {
	remoteAddr := conn.RemoteAddr().String()
	id, err := Authorize(ctx, s.resolver, remoteAddr)
	if err != nil {
		s.logf("egress relay: rejecting %s: %v", remoteAddr, err)
		return
	}
	s.logf("egress relay: authorized %s (%s)", remoteAddr, id.LoginName)
	s.socksSrv.ServeConn(ctx, conn)
}
