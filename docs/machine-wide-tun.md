# Machine-wide TUN mode (`citadel up`) — design

Tracking issue: [#643](https://github.com/aceteam-ai/citadel-cli/issues/643).
Companion: [#642](https://github.com/aceteam-ai/citadel-cli/issues/642) (stop
telling users to install Tailscale — docs only, ships independently).

## The gap

Citadel already has no dependency on Tailscale-the-product. `internal/network`
embeds `tsnet`, Tailscale's Go library — there is no `tailscaled` daemon and no
Tailscale.app.

What `tsnet` gives is a mesh identity for the **citadel process**, not for the
**machine**. `srv.Dial` and `srv.Listen` reach peers; the user's own shell does
not. `ssh 100.64.0.5` from a terminal fails. That single fact is the reason for:

- `citadel proxy <local> <peer:port>`, a per-port workaround,
- the platform being relay-first (Next.js is not on the mesh),
- the `mesh chat` reachability caveat in CLAUDE.md, where an engine bound to
  `0.0.0.0:8210` is unreachable over the mesh because nothing called
  `srv.Listen()` for that port.

Machine-wide TUN closes all three at once. A real kernel interface means the OS
routes `100.64.0.0/10`, so *every* program on the box — `ssh`, `curl`, a
browser, `psql` — reaches peers with no citadel-specific plumbing.

## Constraints (from Jason, 2026-07-30)

1. **Pure Go dependencies only.** No bundled C libraries, no Tailscale.app, no
   driver we ask the user to install separately.
2. **Root/admin is opt-in.** `citadel login` (userspace `tsnet`) stays the
   unprivileged default and must keep working byte-for-byte as it does today.
   `citadel up` is what a user *chooses* when they want machine-wide routing.
3. **Never silently downgrade.** An unelevated `citadel up` must error. A user
   who asked for machine-wide routing must not be left believing they got it.

## Verified facts (do not re-derive)

**Pure Go is achievable, including the OS-integration packages.** The earlier
check in #643 only covered `tstun` + `wgengine`; the full set a TUN backend
needs also builds CGO-free on every target:

```
CGO_ENABLED=0 go build \
  tailscale.com/net/tstun tailscale.com/wgengine tailscale.com/wgengine/router \
  tailscale.com/net/dns tailscale.com/ipn/ipnlocal tailscale.com/wgengine/netstack

darwin/arm64    OK
linux/amd64     OK
windows/amd64   OK
```

`tailscale.com v1.100.0` is already a **direct** dependency, so none of this
adds to the module graph.

**`tsnet.Server.Tun` is NOT the answer — this is the load-bearing finding.**
`tsnet.Server` does expose a public `Tun tun.Device` field, and it is tempting
to read that as "hand tsnet a real `utun` and you are done". You are not.
`tsnet.go` builds its engine like this:

```go
eng, err := wgengine.NewUserspaceEngine(tsLogf, wgengine.Config{
    Tun:      s.Tun,
    EventBus: sys.Bus.Get(),
    // ... no Router, no DNS
})
```

`wgengine.Config` has `Router` and `DNS` fields; tsnet sets neither, so
`wgengine` substitutes `router.NewFake()` and a no-op DNS configurator.
Packets would flow through a real interface while **the OS routing table is
never touched and the resolver is never configured** — no `100.64/10` route, no
MagicDNS. tsnet offers no hook to supply them, so the TUN backend must build the
stack itself, the way `cmd/tailscaled` does:

```
tsd.NewSystem() → netmon.New → tstun.New(name) → router.New(dev)
                → dns.NewOSConfigurator(devName) → wgengine.NewUserspaceEngine(conf)
                → netstack.Create → ipnlocal.NewLocalBackend → lb.Start(ipn.Options{...})
```

That is construction code (~200 lines), not a research problem: once `Router`
and `DNS` are in `wgengine.Config`, `LocalBackend` drives route and resolver
reconfiguration off netmap updates on its own.

**The caller surface is small.** Only seven call sites outside this package
receive a `*NetworkServer` (`network.Connect`), and every one uses nothing but
`GetIPv4()`. Everything else goes through package-level helpers —
`network.Dial`, `network.Listen`, `network.ListenVPN`, `network.GetGlobalPeers`.
Making those backend-agnostic makes the whole codebase backend-agnostic.

**`LocalClient` never needed to be public.** `NetworkServer.LocalClient()` had
zero callers outside `internal/network`. That matters because a TUN backend
drives an in-process `ipnlocal.LocalBackend` and has no local API socket to hand
out. Both paths can produce an `*ipnstate.Status` (`LocalBackend.Status()` and
`LocalClient.Status()` return the same type), so the interface is expressed in
`ipnstate` terms and the accessor is gone.

## Architecture

```go
type Backend interface {
    Up(ctx) error
    Close() error
    Status(ctx) (*ipnstate.Status, error)
    TailscaleIPs() (ip4, ip6 netip.Addr)
    Dial(ctx, network, addr) (net.Conn, error)
    Listen(network, addr) (net.Listener, error)
    ListenTLS(network, addr) (net.Listener, error)
    Ping(ctx, ip, pingType) (*ipnstate.PingResult, error)
    WhoIs(ctx, remoteAddr) (*apitype.WhoIsResponse, error)
    Reauth(ctx, authKey) error
}
```

| | `userspace` (today) | `tun` (#643) |
|---|---|---|
| Device | none (gVisor netstack) | real `utun` / `/dev/net/tun` / Wintun |
| Privilege | none | root / admin |
| Scope | the citadel process | the whole machine |
| `Dial`/`Listen` | tsnet netstack | plain stdlib on the tailnet IP |
| Used by | `citadel login`, `citadel work` | `citadel up` |

In TUN mode `Dial` and `Listen` become ordinary `net.Dial` / `net.Listen`,
because `100.64/10` is routable by the kernel. `ListenVPN` — and all the
listener-matching subtlety behind #286 — collapses to a plain `net.Listen` on
the assigned IP. The TUN backend is likely *smaller* than the userspace one.

## Two identities on one machine — decided before slice 2

A running `citadel work` (tsnet) and a `citadel up` (TUN) sharing
`~/.citadel-node/network/` would be two live WireGuard endpoints presenting one
node key to the coordination server. Sharing the state dir is therefore not an
option, and issue #643's "sharing the existing state dir so `citadel login`
credentials carry over" is wrong as written.

It also reintroduces #176: `citadel up` runs elevated, so root-owned state files
reappear under a directory the non-root worker must read. `FixStatePermissions()`
exists precisely because of that.

Three options, to settle with Jason before the TUN backend lands:

1. **Separate state dir.** `citadel up` gets its own identity and shows up as a
   second node in the coordination server. Simple, honest, but clutters the
   node list and doubles the key-renewal surface.
2. **Mutual exclusion.** One identity; `citadel up` refuses while a worker holds
   the tsnet, and vice versa. Clean mesh view, but a node running the worker can
   never have machine-wide routing — which is most of the fleet.
3. **`citadel up` becomes the only backend** *(preferred)*. With a TUN up,
   `citadel work` does not need its own tsnet at all: it dials and listens on
   `100.x` through the OS stack. The worker attaches to the running backend
   instead of starting a second one. One identity, one node, and the worker's
   network code does not change — it already goes through `network.Dial` /
   `network.Listen`, which the TUN backend implements as stdlib calls. Needs a
   third `attached` backend and a way to detect a live `citadel up`.

## Slices

1. **Backend interface + `userspace` implementation** — this PR. No behavior
   change; `citadel login` must be provably identical.
2. **`tun` backend, macOS first** — `tstun.New("utun")` + `router.New` +
   `dns.NewOSConfigurator`, own state dir pending the decision above.
3. **`citadel up` / `citadel down`** — elevation check that errors rather than
   downgrading; teardown that leaves no orphaned interface or stale route on
   crash.
4. **MagicDNS `*.internal`** — without name resolution this is half a feature.
5. **Linux** (`/dev/net/tun`, or `CAP_NET_ADMIN` on the binary).
6. **Windows** — Wintun. `golang.zx2c4.com/wintun` is pure Go but is a *loader*
   (`newLazyDLL("wintun.dll")`); the driver DLL is a separate wireguard.com
   artifact. Decision (Jason, 2026-07-30): embed via `go:embed` and extract on
   first run **to an Administrator-only-writable directory, never
   `os.TempDir()`** — `citadel up` runs elevated, so an attacker-writable
   extraction path loads a planted DLL into an elevated process. The DLL's
   redistribution terms need their own check.
