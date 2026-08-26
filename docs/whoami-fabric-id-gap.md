# `citadel whoami` and the missing numeric fabric node ID

Context: aceteam-ai/aceteam#8139. This note records what `citadel whoami`
(`cmd/whoami.go`) found when it went looking for a numeric AceTeam fabric node
ID (the integer/database ID used by `fabric_node_status`, `nexus_get_node`,
and the other MCP tools that take a `node_id`) somewhere on a node's local
filesystem, so the finding survives independent of the PR description.

## The finding

**No numeric fabric node ID is persisted anywhere on a citadel node's local
filesystem.** Confirmed by an exhaustive grep for `NodeID|node_id|FabricID|
fabric_id|DeviceID` across `internal/nexus/`, `internal/network/`,
`internal/config/`, `internal/heartbeat/`, and `internal/status/`, plus manual
reads of every plausible carrier:

- `nexus.TokenResponse` (`internal/nexus/deviceauth.go`) — the device-auth
  `/token` response, the thing `citadel init`/`citadel login` actually
  receives — carries `OrgID`, `OrgName`, `UserEmail`, `UserName`,
  `DeviceAPIToken`, `RedisURL`, `NexusURL`, `APIBaseURL`. No node ID field at
  all.
- `internal/heartbeat/marker.go`'s `Marker` struct (the on-disk heartbeat
  freshness record) has zero identity fields — timestamps and an error string
  only.
- The Headscale/mesh numeric node ID (`network.NetworkStatus.NodeID`,
  Headscale's `StableNodeID`) IS resolvable, but only LIVE, from
  `NetworkServer.Status()` reading `status.Self.ID` off a running tsnet
  backend (`internal/network/server.go`). Nothing writes it to disk;
  `network.GetGlobalNodeID` returns `""` whenever no global server is
  running. This is why `whoami` performs the same saved-state
  `VerifyOrReconnect` + `GetGlobalStatus` probe `citadel status` already does,
  rather than a pure file read.

## The one on-disk slot that was clearly intended for it — and is empty

`SSHSyncConfig.NodeID` (`internal/nexus/sshkeys.go`, doc comment: `"Node ID in
AceTeam platform"`), persisted to `ssh_sync.yaml` via `SaveSSHSyncConfig`. This
is exactly the field this feature would want. But `SaveSSHSyncConfig` has zero
non-test callers anywhere in the codebase — only `LoadSSHSyncConfig` is ever
called (`cmd/run.go`, to read a config that nothing writes). `whoami` still
reads it opportunistically (`NodeIdentity.PlatformNodeID`) so it lights up for
free the moment some backend-side code starts populating it; today it reads
empty on essentially every real node.

## A related-but-distinct identity that is NOT this

`internal/devicemode` (aceteam #5959, `citadel device enroll`) persists a
`NodeUID` in `device.json` — "the stable fabric identity assigned at
enrollment (the leaf's CN / `aceteam:node:` SAN value)". This comes from
`nodeidentity.PairingStatusResponse.NodeUID`
(`internal/nodeidentity/pairing.go`), returned by
`GET /api/fabric/pairing/{code}/status` when the caller sent a CSR.

This looks promising at first glance but is the wrong thing for two reasons:

1. It is a certificate CN/SAN string identity for mTLS re-enrollment, not the
   numeric fabric/dashboard node ID `fabric_node_status`/`nexus_get_node`
   expect.
2. It is populated only by `citadel device enroll` — the lightweight
   "device" profile for non-Citadel mesh devices (laptops: mesh membership
   only, no worker, no job queues). The standard Citadel compute-node path
   (`citadel init` -> `ensureNodeIdentity` in `cmd/init.go`) only generates
   the EC keypair and caches the CA chain; it does NOT go through the
   CSR/pairing-status/`NodeUID` flow. So this field is essentially always
   absent for the nodes `whoami` is meant to describe.

`whoami` does not read `device.json`/`NodeUID` for this reason — wiring it in
would surface a value that means something different (and is populated for a
different, non-overlapping population of hosts) under a label a reader would
reasonably assume is the fabric node ID.

## What this means for the cross-repo follow-up

For `citadel whoami --json` to ever report a real numeric fabric node ID, the
**aceteam backend** needs to echo it back to the node at a point the node
already talks to the backend — the two natural points are the device-auth
`/token` response (`TokenResponse`) or a heartbeat ack — and the node needs a
small persistence change to store it (most naturally: start actually calling
`SaveSSHSyncConfig`, or add a new field next to it). Until then, `whoami`
reports everything it CAN answer locally (mesh/Headscale node ID, node name,
org, user, connection state, citadel version) and is explicit in its output
and warnings about the fabric ID gap rather than guessing.
