// internal/network/backend_attached.go
// Backend for a citadel process running on a host where `citadel up` already
// holds the mesh. It starts nothing: it reads status from the running backend
// and routes through the kernel (issue #643).
package network

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"

	"tailscale.com/client/local"
	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
)

// attachedBackend is what makes "one machine, one node" work.
//
// Without it, a host running `citadel up` (TUN) and `citadel work` (tsnet)
// would have two live WireGuard endpoints presenting one node key to the
// coordination server — or, if given separate state dirs, would appear as two
// nodes for one machine. Instead the worker detects the running backend and
// attaches: Dial/Listen go through the kernel (100.64.0.0/10 is routable, so
// plain stdlib reaches peers) and everything that needs control-plane data
// asks the running backend over its local API socket.
//
// Nothing in the rest of the codebase changes. Callers already go through
// network.Dial / network.Listen / network.GetGlobalPeers, so they get
// machine-wide behaviour for free.
type attachedBackend struct {
	lc         *local.Client
	socketPath string
}

func newAttachedBackend(socketPath string) *attachedBackend {
	return &attachedBackend{
		socketPath: socketPath,
		lc: &local.Client{
			Socket:        socketPath,
			UseSocketOnly: true,
		},
	}
}

// Up verifies the backend we are attaching to is actually there and running.
// It never starts anything: if the socket is dead this must fail loudly
// rather than silently falling back, or a node would come up with no mesh
// while reporting success.
func (b *attachedBackend) Up(ctx context.Context) error {
	st, err := b.lc.Status(ctx)
	if err != nil {
		return fmt.Errorf("attach to running citadel backend at %s: %w", b.socketPath, err)
	}
	if st == nil {
		return fmt.Errorf("attach to running citadel backend at %s: empty status", b.socketPath)
	}
	return nil
}

// Close detaches. It deliberately does NOT stop the backend it attached to —
// this process does not own the machine's mesh, and tearing down the routes
// out from under every other program on the host would be a surprising thing
// for `citadel work` to do on exit.
func (b *attachedBackend) Close() error { return nil }

func (b *attachedBackend) Status(ctx context.Context) (*ipnstate.Status, error) {
	return b.lc.Status(ctx)
}

func (b *attachedBackend) TailscaleIPs() (netip.Addr, netip.Addr) {
	st, err := b.lc.Status(context.Background())
	if err != nil || st == nil || st.Self == nil {
		return netip.Addr{}, netip.Addr{}
	}
	return firstV4V6(st.Self.TailscaleIPs)
}

// Kernel-routed, exactly as in tunBackend — the interface is up machine-wide,
// so the host stack already reaches peers.
func (b *attachedBackend) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, network, addr)
}

func (b *attachedBackend) Listen(network, addr string) (net.Listener, error) {
	return net.Listen(network, addr)
}

func (b *attachedBackend) ListenTLS(network, addr string) (net.Listener, error) {
	return nil, errors.New("ListenTLS is not supported when attached to machine-wide mode; terminate TLS in-process instead")
}

func (b *attachedBackend) Ping(ctx context.Context, ip netip.Addr, pingType tailcfg.PingType) (*ipnstate.PingResult, error) {
	return b.lc.Ping(ctx, ip, pingType)
}

func (b *attachedBackend) WhoIs(ctx context.Context, remoteAddr string) (*apitype.WhoIsResponse, error) {
	return b.lc.WhoIs(ctx, remoteAddr)
}

// Reauth re-authorizes the node key of the backend we are attached to. That
// is the correct target: there is one node identity for the machine, and this
// process is a consumer of it, so a key renewal here must renew that one.
func (b *attachedBackend) Reauth(ctx context.Context, authKey string) error {
	return b.lc.Start(ctx, ipn.Options{AuthKey: authKey})
}
