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

## Windows: citadel needs its own Wintun adapter identity

Verified on the Windows 11 test VM (DESKTOP-6UKHJAN), which has Tailscale
installed and running.

`tailscale.com/net/tstun`'s init pins BOTH the Wintun tunnel type and the
adapter GUID to Tailscale's own values:

```go
// net/tstun/tun_windows.go
tun.WintunTunnelType = "Tailscale"
tun.WintunStaticRequestedGUID = &{37217669-42da-4657-a55b-0d995d328250}
```

Wintun identifies an adapter by GUID, so **any** process using tstun asks for
the same adapter. The interface name passed to `tstun.New` is irrelevant.
First run of `citadel up --check` on that box:

```
Using existing driver 0.14
Creating adapter
Failed to initiate stub device creation: Cannot create a file when that file
already exists. (Code 0x800700B7)
```

`internal/network/tun_windows.go` overrides both to citadel-owned values (the
package's init runs after tstun's, so it overrides rather than races). The GUID
is fixed, not random, so a restart re-attaches to citadel's existing adapter
instead of leaking a new one per run. After the fix, on the same box with
Tailscale still up:

```
Machine-wide network readiness:
  Administrator:   yes
  Network device:  yes ({C17ADE10-9C5B-4B8E-9F0D-7C3A1E5D6B21})
```

The adapter was gone afterwards and Tailscale was undisturbed, still on the
mesh at 100.64.0.110.

**Scope of that result — it is narrower than "citadel and Tailscale coexist".**
`--check` creates and destroys the adapter; it installs no routes. What is
proven is that the two no longer collide on the Wintun *adapter GUID*. Both
products want to route `100.64.0.0/10`, and that conflict lives in the routing
table, which this test never touched. Expect a live `citadel up` on a box
running Tailscale to fight it for routes; treat "runs alongside Tailscale" as
untested until someone has done a real bring-up there.

One thing the test does establish for slice 6: the driver loaded fine from the
binary's own directory, which is exactly where a `go:embed` extraction would
put it.

### The Wintun embed can only extract to two places

`golang.zx2c4.com/wintun`'s loader is not a general DLL search:

```go
// dll.go
windows.LoadLibraryEx(d.Name, 0,
    LOAD_LIBRARY_SEARCH_APPLICATION_DIR|LOAD_LIBRARY_SEARCH_SYSTEM32)
```

Those two flags are the *entire* search path: the directory of the running
executable, and `System32`. `PATH` is not consulted, nor the working
directory, nor any path we could choose. So the plan of "extract the embedded
DLL to an Administrator-only-writable directory" only works if that directory
is one of those two.

That is a happy accident for the security requirement rather than a conflict:
- The **application directory** is where the citadel binary lives — on Windows
  under `%LOCALAPPDATA%\Citadel` or `Program Files`, already
  Administrator-only in the latter case.
- **System32** is Administrator-only by definition, but writing there is
  invasive and would collide with any other Wintun consumer.

So: extract beside the binary, and verify the directory is not
attacker-writable before loading (an unprivileged `%LOCALAPPDATA%` install is
writable by that user — who is also the one running `citadel up` elevated, so
a planted DLL would be a genuine privilege-escalation path). This is the one
remaining design question for slice 6, and it is a permissions question, not a
packaging one.

Verified on a clean Windows machine (VM 109, no Tailscale, no wintun.dll):
with the DLL beside the binary, citadel installed the Wintun driver from
scratch — "Installing driver 0.14 / Extracting driver / Creating adapter" —
and `citadel up` brought up `citadel0`. The license permits this: §3d allows
redistribution "insofar as the Software is distributed alongside other
software that uses the Software only via the Permitted API", which is exactly
citadel's use.

## Two identities on one machine — decided before slice 2

A running `citadel work` (tsnet) and a `citadel up` (TUN) sharing
`~/.citadel-node/network/` would be two live WireGuard endpoints presenting one
node key to the coordination server. Sharing the state dir is therefore not an
option, and issue #643's "sharing the existing state dir so `citadel login`
credentials carry over" is wrong as written.

It also reintroduces #176: `citadel up` runs elevated, so root-owned state files
reappear under a directory the non-root worker must read. `FixStatePermissions()`
exists precisely because of that.

**DECIDED (Jason, 2026-07-31): option 3 below.** Implemented — see
`backend_attached.go` and `SelectBackend`. The three options considered:

1. **Separate state dir.** `citadel up` gets its own identity and shows up as a
   second node in the coordination server. Simple, honest, but clutters the
   node list and doubles the key-renewal surface.
2. **Mutual exclusion.** One identity; `citadel up` refuses while a worker holds
   the tsnet, and vice versa. Clean mesh view, but a node running the worker can
   never have machine-wide routing — which is most of the fleet.
3. **`citadel up` becomes the only backend** *(chosen)*. With a TUN up,
   `citadel work` does not need its own tsnet at all: it dials and listens on
   `100.x` through the OS stack. The worker attaches to the running backend
   instead of starting a second one. One identity, one node, and the worker's
   network code does not change — it already goes through `network.Dial` /
   `network.Listen`, which the TUN backend implements as stdlib calls. Needs a
   third `attached` backend and a way to detect a live `citadel up`.

## Verified end to end (2026-08-01, Windows VM 109 — clean, no Tailscale)

```
headscale:  tun-testvm109 | 100.64.0.46 | online
interface:  citadel0 Up, 100.64.0.46
routes:     full peer table + 100.100.100.100/32 (MagicDNS)
firewall:   citadel0 = Private

ping 100.64.0.78                     ->  3/3 replies, avg 3ms
Resolve-DnsName ubuntu-gpu.internal  ->  100.64.0.78
```

`ping.exe` and `Resolve-DnsName` know nothing about citadel — that is machine-wide
routing working, and it is what `tsnet` structurally cannot do.

**One machine, one node**, the core claim: org node count went 12 → 13 on
`citadel up` (+1), and stayed 13 when `citadel status` ran alongside it (+0).
`citadel status` reported the same node and IP, i.e. it attached rather than
starting a second endpoint.

**Teardown** after `Stop-Process -Force` (the crash path, no graceful
shutdown): adapter removed, zero leftover `100.x` routes, DNS restored.

Two bugs surfaced only here, both invisible to `--check`:
- `lb.Start` does not begin login for a node that has never joined — it calls
  `cc.Login` only when a node key exists or a config file set WantRunning. A
  fresh machine parked in `NeedsLogin` with `goal=nil` and failed with the
  generic "timeout waiting for network connection". tsnet handles this by
  calling `StartLoginInteractive` when the state lands on `NeedsLogin`
  (tsnet.go:960); the TUN backend now does the same.
- `citadel up` never called `network.SetLogf`, so every engine diagnostic was
  discarded. The bug above was one opaque line; with logging on it took one
  run to find. Wire `SetLogf` in any new command that brings up the network.

## Slices

1. **Backend interface + `userspace` implementation** — DONE. No behavior
   change; `citadel login` verified identical against the live mesh.
2. **`tun` backend + `attached` backend** — DONE. Shares one state dir, and
   therefore one node identity, with `citadel login`.
3. **`citadel up` / `citadel down` / `citadel up --check`** — DONE. Elevation
   errors rather than downgrading; `--check` creates and removes the interface
   without starting the engine, so it is safe on a box running other VPN
   software.
4. **MagicDNS `*.internal`** — DONE, verified resolving on Windows.
5. **Linux** (`/dev/net/tun`, or `CAP_NET_ADMIN` on the binary) — code is
   platform-agnostic and `osrouter` covers it; needs a live bring-up on a
   disposable host.
6. **Windows** — adapter identity, router registration and COM init all DONE
   and verified on a clean machine; the remaining piece is shipping the driver
   (see the loader constraint above). `golang.zx2c4.com/wintun` is pure Go but is a *loader*
   (`newLazyDLL("wintun.dll")`); the driver DLL is a separate wireguard.com
   artifact. Decision (Jason, 2026-07-30): embed via `go:embed` and extract on
   first run **to an Administrator-only-writable directory, never
   `os.TempDir()`** — `citadel up` runs elevated, so an attacker-writable
   extraction path loads a planted DLL into an elevated process. The DLL's
   redistribution terms need their own check.
