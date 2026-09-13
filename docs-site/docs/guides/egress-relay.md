---
sidebar_position: 4
title: Egress Relay
---

# Egress Relay

Every node you connect to the AceTeam Network already has its own internet
connection. The egress relay lets **another node you own** borrow it: outbound
traffic from a client node is tunneled through the mesh to an "exit" node and
leaves from *that* node's network instead. Your exit, your IP -- useful when
you want a workload's outbound traffic to appear to originate from a specific
network or location you control, or when you'd rather consolidate outbound
traffic through one trusted point instead of every node dialing out
independently.

This is opt-in and off by default. Nothing relays another node's traffic
until you explicitly turn it on.

## How It Works

- **Exit node** -- the node whose internet connection gets shared. It runs a
  SOCKS5 listener on the mesh (network IP, not localhost or LAN) that accepts
  connections from other nodes and dials out to the destination using its own
  network path.
- **Client node** -- the node that wants its traffic to leave through the
  exit node's connection instead of its own.

Authorization is entirely identity-based: the exit node verifies every
connecting peer's network identity and only serves peers in the **same
organization**. There is no token, passcode, or shared secret to configure or
leak -- if a peer isn't a verified same-org node on the mesh, it's refused
before a single byte of the proxy protocol runs.

## Setting Up the Exit Node

There are three ways to run the relay on the node whose internet connection
you want to share. All three are equivalent under the hood -- they start the
exact same listener -- so pick whichever fits how that node already runs.

### 1. Enable it, then start (or restart) the worker

```bash
citadel egress-relay enable
citadel work
```

This persists the setting, so every future `citadel work` on this node starts
the relay automatically. Settings changes only take effect on the *next*
`citadel work` start -- the relay listener is started once at worker startup
and isn't re-evaluated while the worker is already running.

### 2. Force it on for one worker run

```bash
citadel work --egress-relay
```

Starts the relay for this run regardless of the persisted `enable`/`disable`
setting -- handy for a one-off test without touching persisted config.

### 3. Run as a pure relay, no worker at all

```bash
citadel egress-relay serve
```

For a node that should *only* be an egress exit -- no Redis job source, no
worker loop. This matters because `citadel work` requires a reachable Redis
job queue and exits if it can't connect, which would take the relay down with
it. `citadel egress-relay serve` joins the AceTeam Network, starts the relay,
and then blocks in the foreground until you press Ctrl-C -- nothing else.

The node must already be on the AceTeam Network (`citadel login` or `citadel
init`) before running this; it doesn't establish network identity on its own.

Unlike the other two options, `serve` always runs the relay -- it doesn't
consult the persisted enable/disable setting, since running the relay *is*
the point of the command.

### Check the current configuration

```bash
citadel egress-relay status
```

Shows whether the relay is enabled, whether LAN/mesh destinations are
allowed, and the mesh port it listens on.

### Disable it

```bash
citadel egress-relay disable
```

Takes effect on the next `citadel work` start.

## Connecting From a Client Node

By default, network membership is scoped to the Citadel process, not your
whole machine -- so a plain browser or `curl` on the client node can't reach
the exit node's mesh address directly. Bring the relay to a local port with
`citadel proxy`, the same tool used to reach any other peer's service over
the mesh:

```bash
citadel proxy 1080 exit-node-name:7861
```

The first argument is the local port to listen on; the second is
`<exit-node>:7861` -- the exit node's name or network IP, and `7861`, the
relay's fixed mesh port. Point any SOCKS5-aware client at the resulting local
port:

```bash
curl --socks5-hostname 127.0.0.1:1080 https://example.com
```

No SOCKS5 username or password is needed -- the exit node already verified
your node's identity over the mesh before the connection reached the proxy
protocol at all. Leave `citadel proxy` running for as long as you want the
tunnel available; it exits on Ctrl-C.

If the client node itself runs in [Machine-Wide Network
Mode](./machine-wide-mode.md) (`citadel up`), every program on that machine
can already reach the exit node's network address directly, and a
SOCKS5-capable app can point straight at `exit-node-name:7861` with no
`citadel proxy` step in between.

## Security and Default-Deny Posture

- **Off by default.** `citadel work` never binds the relay port unless the
  relay has been explicitly enabled (or force-enabled with `--egress-relay`,
  or run via `citadel egress-relay serve`).
- **Mesh-only.** The relay listens on the node's network address, never on
  localhost or a LAN interface -- there's no plain-TCP path to it that would
  bypass mesh identity checks.
- **Same-org verified peers only.** Every inbound connection is checked
  against the peer's verified network identity before anything else happens.
  A peer that can't be verified, or that belongs to a different organization,
  is refused -- there is no fallback token or passcode.
- **LAN/mesh pivoting is denied by default.** An authorized peer can egress to
  the public internet through the exit node, but by default it *cannot* use
  the relay to reach the exit node's own LAN or other mesh addresses
  (private/RFC1918 ranges, loopback, link-local, and the mesh's own address
  range are all refused). Opt in explicitly if you actually want that:

  ```bash
  citadel egress-relay allow-lan on   # or: off
  ```

  Unlike enabling/disabling the relay itself, this takes effect immediately
  on an already-running relay -- there's no flag to force it off, only on, to
  keep the fail-closed default the harder direction to override by mistake.

## Environment Variables

| Variable | Default | Effect |
|---|---|---|
| `CITADEL_EGRESS_RELAY` | unset | Overrides the persisted enable/disable setting for any `citadel work` started from this shell. |
| `CITADEL_EGRESS_ALLOW_LAN` | unset | Overrides the persisted `allow-lan` setting. |

Set either to `1`, `true`, `yes`, or `on` to enable; any other value disables.
An env var, when set, always wins over the persisted `egress-relay.yaml`
setting.

## Troubleshooting

- **Client can't connect through the relay at all.** Confirm both nodes are
  online and can see each other: `citadel peers` on the client should list
  the exit node. Confirm the exit node's relay is actually running:
  `citadel egress-relay status` on the exit node.
- **Connections to the relay are refused.** The most common cause is that the
  exit and client nodes aren't in the same organization -- the relay has no
  other way to authorize a peer, so this fails closed rather than falling
  back to anything weaker.
- **A destination inside your own LAN or mesh gets refused.** That's the
  default-deny LAN/mesh policy working as intended. Use `citadel egress-relay
  allow-lan on` on the exit node only if you actually want peers to be able to
  reach its LAN/mesh through the relay.
- **Settings change but nothing happens.** `enable`/`disable` only take
  effect on the next `citadel work` start -- restart the worker (or use
  `citadel egress-relay serve` for a relay-only run). `allow-lan` is the one
  setting that applies live to an already-running relay.
