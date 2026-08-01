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

	"github.com/tailscale/wireguard-go/tun"
	"tailscale.com/client/local"

	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/control/controlclient"
	"tailscale.com/health"
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
	"tailscale.com/util/eventbus"
	"tailscale.com/wgengine"
	"tailscale.com/wgengine/netstack"
	"tailscale.com/wgengine/router"

	// Registers the per-OS Router implementation. router.New dispatches
	// through a feature hook (router.HookNewUserspaceRouter) that is ONLY
	// populated by importing this package.
	//
	// Without it everything compiles and the TUN device is created — then
	// router.New returns `unsupported OS "windows"` at runtime. Verified on
	// the Windows 11 test VM: the adapter came up and the bring-up died
	// there. It is a runtime-only failure on EVERY platform, which is why the
	// type checker never caught it.
	//
	// tailscaled gets this via `_ "tailscale.com/feature/condregister"`, but
	// that umbrella also registers Taildrive, TPM, and the Kubernetes and AWS
	// state stores — measured: it links the AWS SSM client and 76 aws/smithy
	// packages into citadel. Importing the one implementation package we
	// actually need avoids all of it.
	_ "tailscale.com/wgengine/router/osrouter"
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

	dev, devName, err := tstunNew(b.tunName)
	if err != nil {
		return fmt.Errorf("create %s: %w", b.tunName, err)
	}
	b.devClose = func() { dev.Close() }
	// NB: do NOT sys.Set(dev) here. tsd.System.Set's type switch accepts
	// *tstun.Wrapper, not a raw tun.Device, and its default case panics.
	// wgengine registers the wrapped device via SetSubsystem below, which is
	// what makes sys.Tun.Get() (and its .Start()) valid. tailscaled does the
	// same: it only ever puts the device in wgengine.Config.

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

	// LocalBackendStartKeyOSNeutral is REQUIRED, not optional: it is what
	// tsnet passes (tsnet.go:930). Without it NewLocalBackend derives an
	// OS-dependent StateStore start key, so `citadel up` would open the same
	// tailscaled.state that `citadel login` wrote, find no profile under the
	// key it looked for, and mint a fresh machine key — appearing as a SECOND
	// node in the coordination server. That is silent: `citadel up` succeeds
	// and prints an IP. It is exactly the split identity this design exists
	// to prevent. See tailscale/tailscale#6973.
	lb, err := ipnlocal.NewLocalBackend(logf, logid.PublicID{}, sys,
		controlclient.LocalBackendStartKeyOSNeutral)
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

	// WantRunning drives the backend to Running. CorpDNS is MagicDNS, which is
	// much of the point of machine-wide mode (`peer.internal` resolving in any
	// program, not just citadel). RouteAll accepts subnet routes peers
	// advertise.
	if err := lb.Start(ipn.Options{
		AuthKey: b.authKey,
		UpdatePrefs: &ipn.Prefs{
			ControlURL:  b.cfg.ControlURL,
			WantRunning: true,
			CorpDNS:     true,
			RouteAll:    true,
			Hostname:    b.cfg.Hostname,
		},
	}); err != nil {
		eng.Close()
		return fmt.Errorf("start backend: %w", err)
	}

	// lb.Start alone does NOT begin registration for a node that has never
	// joined. It only calls cc.Login when it already has a node key or a
	// config-file WantRunning (local.go: `if !loggedOut && (b.hasNodeKeyLocked()
	// || confWantRunning)`), so a fresh machine parks in NeedsLogin with
	// `authRoutine: loggedIn=false; goal=nil` — the authkey is held by the
	// control client but nothing ever asks it to log in, and Up() then fails
	// with the generic "timeout waiting for network connection".
	//
	// tsnet hits the same gate and handles it the same way (tsnet.go:960).
	// StartLoginInteractive is a misleading name here: with an authkey already
	// on the control client it completes non-interactively and never prompts.
	if st := lb.State(); st == ipn.NeedsLogin {
		logf("tun: backend needs login (state=%v); starting registration", st)
		if err := lb.StartLoginInteractive(ctx); err != nil {
			eng.Close()
			return fmt.Errorf("start login: %w", err)
		}
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

	// Real subsystems, not nils: netmon.New calls bus.Client() and
	// dns.CleanUp threads the bus into a Manager, so a nil bus panics — and
	// this function is both the first statement of tunBackend.Up and the
	// entire body of `citadel down`, so a panic here would take out the
	// recovery path along with the start path.
	bus := eventbus.New()
	defer bus.Close()
	health := new(health.Tracker)

	netMon, err := netmon.New(bus, l)
	if err != nil {
		logf("tun: cleanup: network monitor: %v", err)
		return
	}
	defer netMon.Close()

	name := DefaultTUNName()
	dns.CleanUp(l, netMon, bus, health, name)
	router.CleanUp(l, netMon, name)
}

// tstunNew creates the OS network interface, annotating the failure with
// tstun's own platform diagnosis (which explains, for example, a missing
// wintun.dll or an absent /dev/net/tun far better than the bare errno).
func tstunNew(name string) (tun.Device, string, error) {
	l := logger.Logf(func(format string, args ...any) { logf(format, args...) })
	dev, devName, err := tstun.New(l, name)
	if err != nil {
		tstun.Diagnose(l, name, err)
		return nil, "", err
	}
	return dev, devName, nil
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
