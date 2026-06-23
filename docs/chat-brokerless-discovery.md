# Chat: connection status & broker-less peer discovery

This document covers the Control Center **Chat** pane (Alt+3): how it connects,
the connection-status indicator, and the design for broker-less peer discovery
over the Headscale/nexus mesh.

## Background: the "connecting…" bug (issue #293)

The Chat pane connects lazily on first activation. The original lifecycle
optimistically set the message view to `Connected` **before** the blocking
`chat.Client.Connect()` call, and then discarded that call's error
(`_ = client.Connect(ctx)`). The peers sidebar was seeded with a static
`connecting…` placeholder that was only ever cleared by a presence callback.

The result: if the real-time handshake failed (endpoint unreachable, auth
rejected, subscribe error) no presence ever arrived, so the sidebar stayed on
`connecting…` forever while the main view falsely claimed `Connected`. The user
saw a permanent, silent half-connected state.

### Fix

The chat client now exposes a real connected signal:

- `chat.Client.OnConnect(fn)` — fired exactly once, **after** the WebSocket
  handshake and channel subscribe both succeed (`internal/chat/client.go`).
- `chat.Client.IsConnected()` — read-only passthrough to the transport.

The Chat pane (`internal/tui/controlcenter/chat_page.go`) drives a three-state
machine — `connecting → connected | error`:

- It stays in **connecting** until `OnConnect` fires.
- On `OnConnect` it flips to **connected**, clears the sidebar placeholder, and
  marks the page connected.
- If `Connect()` returns an error before a successful connect, it flips to
  **error**, shows the real error message in the message view and a red
  `offline` sidebar, instead of an eternal `connecting…`.

## Connection status indicator

A one-line status bar sits between the message view and the input box. It shows
the **WSS endpoint** the chat is using and its health:

```
● connecting…  wss://aceteam.ai
● connected    wss://aceteam.ai
● error        wss://aceteam.ai   <real error>
```

The endpoint label is produced by `chat.SanitizeEndpoint`, which converts the
API base URL to `scheme://host` (https→wss, http→ws) and **deliberately drops
the path**. The underlying transport (a Redis pub/sub proxy reached at an
internal path) is never surfaced to the user — the status line shows only the
public host, not how messages are carried. This is enforced by a unit test that
asserts the sanitized output never contains the transport path.

## Broker-less peer discovery (design + stub)

Today chat is **broker-mediated**: every node subscribes to an org-scoped
channel on a central WSS endpoint, and that broker fans messages out. This is
simple but couples chat liveness to a single hosted service — the same coupling
that produced the stuck `connecting…` state when the endpoint was unreachable.

**Broker-less discovery** removes the central directory. Every node in an org is
already a member of the same Headscale/nexus mesh, so a node can:

1. **Enumerate** its org peers directly from the local tailnet — no central
   directory. Headscale scopes peers by Headscale user, and each org maps to
   user `org_<organization_id>`, so `network.GetGlobalPeers` already returns
   only same-org nodes.
2. **Probe** each peer over the VPN mesh.
3. (future) Exchange messages **peer-to-peer** over the mesh.

`internal/chat/discovery.go` implements the enumerate + probe half:

```go
results, err := chat.DiscoverPeers(ctx)
// each ProbeResult: NodeName, IP, Online, Reachable, LatencyMs, SpeaksChat
```

### What is real vs. stubbed

| Step              | Status        | Notes                                                                 |
| ----------------- | ------------- | --------------------------------------------------------------------- |
| Enumerate         | Implemented   | `network.GetGlobalPeers` (org-scoped via Headscale user).             |
| Reachability probe| Implemented   | `network.PingPeer` over the mesh; proves routable, **not** chat-able. |
| Protocol probe    | Not yet       | No per-node chat listener exists to probe against.                    |

`ProbeResult.SpeaksChat` is therefore **always false** today and is asserted as
such in tests. Reachability is not proof a peer speaks chat.

### Completing it

To make chat genuinely broker-less, a follow-up needs to:

1. Add a small per-node chat listener (placeholder port `chatProbePort` in
   `discovery.go`).
2. Replace `probeReachable` with a real protocol handshake against that
   listener, setting `SpeaksChat` truthfully.
3. Route outbound messages to peers returned by `DiscoverPeers` (direct mesh
   dial via `network.Dial`) instead of the central channel, falling back to the
   broker when a peer has no listener.

`DiscoverPeers` is currently informational; the chat client still uses the
central WSS transport. The discovery primitive ships now so the enumerate/probe
plumbing is in place for that follow-up.
