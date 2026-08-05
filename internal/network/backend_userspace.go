// internal/network/backend_userspace.go
// Userspace (tsnet) Backend — the unprivileged default behind `citadel login`.
package network

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"runtime"
	"strings"

	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
	"tailscale.com/tsnet"
)

// userspaceBackend runs the mesh in-process via tsnet's userspace netstack.
// It gives the citadel process a mesh identity, not the machine: peers are
// reachable through Dial/Listen but not from the host's own `ssh`/`curl`.
// That limitation is why `citadel proxy` exists and why the TUN backend
// (issue #643) is being added alongside it.
type userspaceBackend struct {
	srv *tsnet.Server
}

// newUserspaceBackend builds the tsnet server. Nothing starts until Up.
func newUserspaceBackend(cfg ServerConfig, stateDir, authKey string) *userspaceBackend {
	return &userspaceBackend{
		srv: &tsnet.Server{
			Hostname:   cfg.Hostname,
			ControlURL: cfg.ControlURL,
			Dir:        stateDir,
			AuthKey:    authKey,
			Ephemeral:  false,                               // We want persistent nodes
			Logf:       func(format string, args ...any) {}, // Suppress verbose tsnet logs
		},
	}
}

func (b *userspaceBackend) Up(ctx context.Context) error {
	// On Windows, restrict Group Policy locks to avoid ERROR_ACCESS_DENIED
	// in non-interactive sessions (WinRM, services). No-op on other platforms.
	removeRestriction := restrictPolicyLocks()
	defer removeRestriction()

	if err := b.srv.Start(); err != nil {
		// On Windows, wrap cryptic syspolicy errors with actionable guidance.
		if runtime.GOOS == "windows" && strings.Contains(err.Error(), "syspolicy") {
			return fmt.Errorf("Windows requires SYSTEM privileges for network access.\n"+
				"   Run via a scheduled task with /ru SYSTEM, or install as a service.\n"+
				"   Original error: %w", err)
		}
		return fmt.Errorf("failed to start network: %w", err)
	}
	return nil
}

func (b *userspaceBackend) Close() error {
	return b.srv.Close()
}

func (b *userspaceBackend) Status(ctx context.Context) (*ipnstate.Status, error) {
	lc, err := b.srv.LocalClient()
	if err != nil {
		return nil, fmt.Errorf("failed to get local client: %w", err)
	}
	return lc.Status(ctx)
}

func (b *userspaceBackend) TailscaleIPs() (netip.Addr, netip.Addr) {
	return b.srv.TailscaleIPs()
}

func (b *userspaceBackend) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	return b.srv.Dial(ctx, network, addr)
}

func (b *userspaceBackend) Listen(network, addr string) (net.Listener, error) {
	return b.srv.Listen(network, addr)
}

func (b *userspaceBackend) ListenTLS(network, addr string) (net.Listener, error) {
	return b.srv.ListenTLS(network, addr)
}

func (b *userspaceBackend) Ping(ctx context.Context, ip netip.Addr, pingType tailcfg.PingType) (*ipnstate.PingResult, error) {
	lc, err := b.srv.LocalClient()
	if err != nil {
		return nil, err
	}
	return lc.Ping(ctx, ip, pingType)
}

func (b *userspaceBackend) WhoIs(ctx context.Context, remoteAddr string) (*apitype.WhoIsResponse, error) {
	lc, err := b.srv.LocalClient()
	if err != nil {
		return nil, fmt.Errorf("failed to get local client: %w", err)
	}
	return lc.WhoIs(ctx, remoteAddr)
}

func (b *userspaceBackend) Reauth(ctx context.Context, authKey string) error {
	lc, err := b.srv.LocalClient()
	if err != nil {
		return fmt.Errorf("local client: %w", err)
	}
	// Start applies the new AuthKey to the already-running state machine,
	// re-authorizing the node key in place. UpdatePrefs is left nil so existing
	// prefs (and Persist, i.e. the machine key) are preserved.
	return lc.Start(ctx, ipn.Options{AuthKey: authKey})
}
