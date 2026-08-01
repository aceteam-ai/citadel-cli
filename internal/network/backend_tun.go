// internal/network/backend_tun.go
// Machine-wide Backend: a real kernel TUN device, so every process on the
// host routes 100.64.0.0/10 — not just citadel. Requires root/admin and is
// strictly opt-in (issue #643).
package network

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"path/filepath"
	"sync"

	"tailscale.com/client/local"
	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnlocal"
	"tailscale.com/ipn/ipnserver"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/ipn/store"
	"tailscale.com/net/dns"
	"tailscale.com/net/netmon"
	"tailscale.com/net/netns"
	"tailscale.com/net/tsdial"
	"tailscale.com/net/tstun"
	"tailscale.com/tailcfg"
	"tailscale.com/tsd"
	"tailscale.com/types/logger"
	"tailscale.com/types/logid"
	"tailscale.com/wgengine"
	"tailscale.com/wgengine/netstack"
	"tailscale.com/wgengine/router"
)

// tunBackend builds the same stack tailscaled does, minus the parts citadel
// has no use for (logtail, the web client, outbound proxy, TPM attestation).
//
// It exists because tsnet CANNOT do this. tsnet.Server has a public `Tun
// tun.Device` field that looks like it would suffice, but tsnet constructs
// `wgengine.Config{Tun: s.Tun, ...}` and sets neither Router nor DNS, so
// wgengine substitutes router.NewFake() and a no-op DNS configurator. Packets
// would cross a real interface while the OS routing table was never touched
// and the resolver never configured. tsnet exposes no hook to supply them, so
// the stack has to be assembled here.
type tunBackend struct {
	cfg      ServerConfig
	stateDir string
	authKey  string
	tunName  string

	mu       sync.Mutex
	sys      *tsd.System
	lb       *ipnlocal.LocalBackend
	srv      *ipnserver.Server
	lc       *local.Client
	apiLn    net.Listener
	srvCtx   context.Context
	srvStop  context.CancelFunc
	srvDone  chan struct{}
	devClose func()
}

// DefaultTUNName is the interface citadel asks the OS for. On macOS the name
// must match `utun[0-9]*` and the kernel picks the number, so "utun" lets it
// choose. Linux takes the literal name.
func DefaultTUNName() string {
	if isDarwin() {
		return "utun"
	}
	return "citadel0"
}

func newTUNBackend(cfg ServerConfig, stateDir, authKey string) *tunBackend {
	return &tunBackend{
		cfg:      cfg,
		stateDir: stateDir,
		authKey:  authKey,
		tunName:  DefaultTUNName(),
	}
}

// Up assembles the engine and brings the interface up. The caller must
// already have checked for elevation — Up does not, so that the privilege
// error can be phrased by the command rather than surfacing as a bare EPERM
// from the tun device.
func (b *tunBackend) Up(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	logf := logger.Logf(func(format string, args ...any) { logf(format, args...) })

	// Restore any system state a previous run left behind. tailscaled does
	// this unconditionally on every start, not just on an explicit --cleanup,
	// because a crash or a reboot-without-shutdown leaves stale routes and a
	// rewritten resolver that no amount of restarting would otherwise undo.
	// Must run BEFORE the device is created.
	CleanUpSystemState()

	sys := tsd.NewSystem()
	b.sys = sys

	netMon, err := netmon.New(sys.Bus.Get(), logf)
	if err != nil {
		return fmt.Errorf("network monitor: %w", err)
	}
	sys.Set(netMon)

	dialer := &tsdial.Dialer{Logf: logf}
	dialer.SetBus(sys.Bus.Get())
	sys.Set(dialer)

	// Outbound WireGuard packets must not re-enter the tunnel. netns binds
	// them to the physical interface. tsnet never needs this (it is always
	// netstack, so there is no tunnel to loop through); a TUN backend that
	// skips it routes itself in a circle the moment a route overlaps.
	netns.SetEnabled(true)

	dev, devName, err := tstun.New(logf, b.tunName)
	if err != nil {
		tstun.Diagnose(logf, b.tunName, err)
		return fmt.Errorf("create %s: %w", b.tunName, err)
	}
	b.devClose = func() { dev.Close() }
	sys.Set(dev)

	rtr, err := router.New(logf, dev, netMon, sys.HealthTracker.Get(), sys.Bus.Get())
	if err != nil {
		dev.Close()
		return fmt.Errorf("create router: %w", err)
	}
	sys.Set(rtr)

	dnsCfg, err := dns.NewOSConfigurator(logf, sys.HealthTracker.Get(), sys.Bus.Get(),
		sys.PolicyClientOrDefault(), sys.ControlKnobs(), devName)
	if err != nil {
		dev.Close()
		rtr.Close()
		return fmt.Errorf("dns configurator: %w", err)
	}

	eng, err := wgengine.NewUserspaceEngine(logf, wgengine.Config{
		Tun:           dev,
		Router:        rtr,
		DNS:           dnsCfg,
		NetMon:        netMon,
		Dialer:        dialer,
		SetSubsystem:  sys.Set,
		ControlKnobs:  sys.ControlKnobs(),
		HealthTracker: sys.HealthTracker.Get(),
		Metrics:       sys.UserMetricsRegistry(),
		EventBus:      sys.Bus.Get(),
	})
	if err != nil {
		dev.Close()
		rtr.Close()
		return fmt.Errorf("wireguard engine: %w", err)
	}
	sys.Set(eng)
	sys.HealthTracker.Get().SetMetricsRegistry(sys.UserMetricsRegistry())

	ns, err := netstack.Create(logf, sys.Tun.Get(), eng, sys.MagicSock.Get(),
		dialer, sys.DNSManager.Get(), sys.ProxyMapper())
	if err != nil {
		eng.Close()
		return fmt.Errorf("netstack: %w", err)
	}
	// With a real TUN the kernel handles local IPs and subnet routes; netstack
	// is here only for the traffic it must still terminate in-process.
	ns.ProcessLocalIPs = false
	ns.ProcessSubnets = false
	sys.Set(ns)

	// Deliberately the SAME state file tsnet uses, so `citadel up` and
	// `citadel login` are ONE node identity rather than two. The state-dir
	// lock is what keeps a second backend from opening it concurrently.
	st, err := store.New(logf, filepath.Join(b.stateDir, "tailscaled.state"))
	if err != nil {
		eng.Close()
		return fmt.Errorf("state store: %w", err)
	}
	sys.Set(st)

	sys.Tun.Get().Start()

	lb, err := ipnlocal.NewLocalBackend(logf, logid.PublicID{}, sys, 0)
	if err != nil {
		eng.Close()
		return fmt.Errorf("local backend: %w", err)
	}
	lb.SetVarRoot(b.stateDir)
	b.lb = lb

	if err := ns.Start(lb); err != nil {
		eng.Close()
		return fmt.Errorf("start netstack: %w", err)
	}

	if err := b.serveLocalAPI(lb); err != nil {
		eng.Close()
		return fmt.Errorf("local api: %w", err)
	}

	// WantRunning drives the backend to Running; CorpDNS is MagicDNS, which
	// is the whole point of machine-wide mode (`peer.internal` resolving in
	// any program, not just citadel). RouteAll accepts subnet routes peers
	// advertise.
	wantTrue := true
	prefs := ipn.NewPrefs()
	prefs.ControlURL = b.cfg.ControlURL
	prefs.WantRunning = true
	prefs.CorpDNS = true
	prefs.RouteAll = true
	prefs.Hostname = b.cfg.Hostname

	if err := lb.Start(ipn.Options{
		AuthKey: b.authKey,
		UpdatePrefs: &ipn.Prefs{
			ControlURL:  b.cfg.ControlURL,
			WantRunning: wantTrue,
			CorpDNS:     true,
			RouteAll:    true,
			Hostname:    b.cfg.Hostname,
		},
	}); err != nil {
		eng.Close()
		return fmt.Errorf("start backend: %w", err)
	}

	return nil
}

// serveLocalAPI exposes the backend on a citadel-owned socket so other
// citadel processes (`citadel work`, `citadel status`) can attach to this
// node's mesh instead of starting a second one. See attachedBackend.
func (b *tunBackend) serveLocalAPI(lb *ipnlocal.LocalBackend) error {
	ln, err := listenLocalAPI(LocalAPISocketPath(b.stateDir))
	if err != nil {
		return err
	}
	b.apiLn = ln

	srv := ipnserver.New(logger.Logf(func(format string, args ...any) { logf(format, args...) }),
		logid.PublicID{}, b.sys.Bus.Get(), b.sys.NetMon.Get())
	srv.SetLocalBackend(lb)
	b.srv = srv

	ctx, cancel := context.WithCancel(context.Background())
	b.srvCtx, b.srvStop = ctx, cancel
	b.srvDone = make(chan struct{})
	go func() {
		defer close(b.srvDone)
		if err := srv.Run(ctx, ln); err != nil && !errors.Is(err, context.Canceled) {
			logf("tun: local api server stopped: %v", err)
		}
	}()

	b.lc = &local.Client{
		Socket:        LocalAPISocketPath(b.stateDir),
		UseSocketOnly: true,
	}
	return nil
}

// Close tears the interface down and restores system state. Routes and the
// resolver are the OS-visible side effects, so failing to run this leaves the
// machine misconfigured — which is why Up also runs CleanUpSystemState.
func (b *tunBackend) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.srvStop != nil {
		b.srvStop()
		<-b.srvDone
	}
	if b.apiLn != nil {
		b.apiLn.Close()
	}
	if b.lb != nil {
		b.lb.Shutdown()
	}
	if eng, ok := b.sys.Engine.GetOK(); ok {
		eng.Close()
	}
	if b.devClose != nil {
		b.devClose()
	}
	removeLocalAPISocket(LocalAPISocketPath(b.stateDir))

	// Belt and braces: Shutdown should have restored routes and DNS via the
	// router/OSConfigurator Close paths, but a partial bring-up may have left
	// some behind.
	CleanUpSystemState()
	return nil
}

func (b *tunBackend) Status(ctx context.Context) (*ipnstate.Status, error) {
	b.mu.Lock()
	lb := b.lb
	b.mu.Unlock()
	if lb == nil {
		return nil, fmt.Errorf("not connected to network")
	}
	return lb.Status(), nil
}

func (b *tunBackend) TailscaleIPs() (netip.Addr, netip.Addr) {
	st, err := b.Status(context.Background())
	if err != nil || st == nil || st.Self == nil {
		return netip.Addr{}, netip.Addr{}
	}
	return firstV4V6(st.Self.TailscaleIPs)
}

// Dial and Listen are plain stdlib in machine-wide mode: 100.64.0.0/10 is in
// the kernel routing table, so the host stack reaches peers with no
// citadel-specific plumbing. This is the entire point of TUN mode, and it is
// why `citadel proxy` becomes unnecessary here.
func (b *tunBackend) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, network, addr)
}

func (b *tunBackend) Listen(network, addr string) (net.Listener, error) {
	return net.Listen(network, addr)
}

// ListenTLS has no automatic-certificate equivalent here: tsnet's ListenTLS
// is backed by its own cert store. Callers that need TLS on a TUN node should
// terminate it themselves (internal/tlscert). Returning an error is better
// than handing back a plaintext listener a caller believes is encrypted.
func (b *tunBackend) ListenTLS(network, addr string) (net.Listener, error) {
	return nil, errors.New("ListenTLS is not supported in machine-wide (TUN) mode; terminate TLS in-process instead")
}

func (b *tunBackend) Ping(ctx context.Context, ip netip.Addr, pingType tailcfg.PingType) (*ipnstate.PingResult, error) {
	b.mu.Lock()
	lb := b.lb
	b.mu.Unlock()
	if lb == nil {
		return nil, fmt.Errorf("not connected to network")
	}
	return lb.Ping(ctx, ip, pingType, 0)
}

func (b *tunBackend) WhoIs(ctx context.Context, remoteAddr string) (*apitype.WhoIsResponse, error) {
	b.mu.Lock()
	lc := b.lc
	b.mu.Unlock()
	if lc == nil {
		return nil, fmt.Errorf("not connected to network")
	}
	return lc.WhoIs(ctx, remoteAddr)
}

func (b *tunBackend) Reauth(ctx context.Context, authKey string) error {
	b.mu.Lock()
	lb := b.lb
	b.mu.Unlock()
	if lb == nil {
		return fmt.Errorf("not connected to network")
	}
	return lb.Start(ipn.Options{AuthKey: authKey})
}

// CleanUpSystemState restores routes and resolver configuration left behind
// by a previous machine-wide run. Safe to call when nothing is up, and safe
// to call as a non-root user (it simply fails to change anything).
func CleanUpSystemState() {
	l := logger.Logf(func(format string, args ...any) { logf(format, args...) })
	netMon, err := netmon.New(nil, l)
	if err != nil {
		return
	}
	defer netMon.Close()
	name := DefaultTUNName()
	dns.CleanUp(l, netMon, nil, nil, name)
	router.CleanUp(l, netMon, name)
}

func firstV4V6(addrs []netip.Addr) (v4, v6 netip.Addr) {
	for _, a := range addrs {
		if a.Is4() && !v4.IsValid() {
			v4 = a
		}
		if a.Is6() && !v6.IsValid() {
			v6 = a
		}
	}
	return v4, v6
}
