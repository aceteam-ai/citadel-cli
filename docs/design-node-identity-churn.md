# Stable node identity across reconnect (citadel-cli #1235)

## Problem

A Citadel node can mint a **new** Headscale node on reconnect/reboot instead of
reattaching to its existing registration. Every capability keyed to the old
numeric node id orphans in one shot: WhatsApp/WeChat bridge provisions, Files
access, `node:exec` grants, and the per-node Redis job stream. The operator must
re-provision and re-grant by hand, and the superseded offline registrations
accumulate as ghosts.

Observed on `ubuntu-gpu`: it registered as 1063, then 1084, 1297, now 1795, each
with a fresh node id, a de-duplicated given-name (`ubuntu-gpu`, `-1`, `-2`, ...),
and often a new mesh IP.

## Root cause (verified, not inferred)

Three independent sources (a read of the live reconnect/tsnet code, a read of
Headscale's registration source at the pinned tag, and the forensic transcript of
the 2026-10-07 `1297 -> 1795` incident) converge:

1. **This is NOT a Headscale bug, and a Headscale upgrade does not fix it.** nexus
   pins `headscale v0.29.1`. Its `state.HandleNodeFromPreAuthKey` /
   `findExistingNodeForPAK` match an incoming registration on **machine key +
   PAK user** and, on a match, **update the existing node in place** (reattach),
   even when the prior node key is expired. A new node (with the `-N` given-name
   suffix) is created only when no node exists for that (machine key, org user).
   The `0.29.2`/`0.29.3` registration fixes live in the interactive-followup and
   tagged-node paths, not the synchronous authkey path citadel uses. Reattach
   already works at v0.29.1.

2. **tsnet already preserves the machine key on the normal path.** `tsnet.Server.
   start()` (v1.100.0) calls `lb.Start(ipn.Options{UpdatePrefs: prefs, AuthKey})`
   with `UpdatePrefs` (not `Prefs`), so the persisted `Persist` (machine key) in
   `<stateDir>/tailscaled.state` is kept; when valid state exists it logs
   "Authkey is set; but state is ...; Ignoring authkey" and reuses the existing
   identity. So presenting the same machine key => Headscale reattaches,
   regardless of the given-name.

3. **The churn is in citadel's recovery logic, which presents a FRESH machine
   key.** Two citadel paths do this:
   - `cmd/reconnect.go:recoverStaleVPN` is a two-attempt recovery. Attempt 1
     (`network.ReconnectWithAuthKey`) reuses `GetStateDir()` and preserves the
     machine key. **Attempt 2 is labeled "IDENTITY CHURN": it calls
     `network.ClearState()` (wipes `tailscaled.state` -> new machine key) + a
     best-effort `reclaimStaleNodeByHostname` + a fresh `network.Connect`.** On
     any Attempt-1 error it falls straight through to Attempt 2 with no refusal
     path.
   - The `#1131` empty-state-dir branch (still open) can short-circuit certain
     re-registration paths to a fresh key.

4. **The trigger in the observed incident was a transient timeout, not an expiry.**
   At churn time `nexus_get_node 1297` showed `Expiry: None` (valid) and the live
   status was `DISCONNECTED (context deadline exceeded)`. `isStaleStateError`
   (`internal/network/singleton.go`) maps `context.DeadlineExceeded` and the
   generic `"timeout waiting for network connection"` string to **stale**, so a
   transient control-plane-unreachable condition escalated into the fresh-authkey
   reauth that re-registered. On the embedded-tsnet path a genuinely rejected key
   and an unreachable control plane are **indistinguishable from the returned
   error** today: both exit `(*NetworkServer).waitForConnection` as that same bare
   string. The `BackendState` discriminator (`NeedsLogin`/`NeedsMachineAuth` vs
   `Starting`) and `ipnstate.Status.Health[]` are observed in the poll loop but
   discarded before the caller can see them.

5. **The `IP preserved` banner is derived from "Attempt 1 did not error", not from
   validating the assigned identity.** `recoverStaleVPN` hardcodes
   `IPPreserved:true` on an Attempt-1 success; it never compares the resulting
   Headscale node id / IP against the prior one. So a run that re-registered as a
   new node still reported "VPN connection recovered successfully / IP address was
   preserved" (confirmed in the incident stdout).

6. **The given-name climb is against a permanent collision, not just the ghost.**
   The box has three `ubuntu-gpu*` registrations: `ubuntu-gpu` (1297, org-owned,
   the offline citadel ghost), `ubuntu-gpu-1` (the **host's own system tailscaled**,
   owner `jason@`, kernel `tailscale0`, where host SSH rides), and `ubuntu-gpu-2`
   (1795, org-owned, the live citadel tsnet). The host system-tailscale node never
   goes away, so deleting ghosts alone cannot stop the `-N` climb on the fresh-key
   path.

## Scope boundary (which repo owns which half)

Node-side (this PR) can deliver: no destructive churn on any unattended path; a
transient timeout no longer escalates to a re-register; honest detection and
reporting of a churn when one is genuinely unavoidable; a distinct given-name so a
fresh registration cannot collide with the host system-tailscale node; and a
durable identity emitted for the backend to rebind on.

Node-side CANNOT, alone, guarantee "a reconnect never creates a new Headscale
node" in the genuine-fresh-key case (fresh install, wiped state), nor GC the
accumulated ghosts. Those are:

- **aceteam (part 2, held):** `app/api/fabric/authkey/generate` does zero node
  reconciliation. Port the delete-prior-node / GC that `device-auth/token`
  (`MachineMapping` cleanup) and `nexus/reenroll` (step 7, "folds #294") already
  perform, onto the reconnect mint. This is the control-plane GC #1235 asks for.
- **nexus (optional hygiene, held):** bump `headscale/Dockerfile`
  `v0.29.1-debug -> v0.29.3-debug` for `#3383` and general hygiene. NOT the fix.

## Node-side plan (this PR)

1. **One shared recovery policy; churn only under explicit `--force`.**
   `recoverStaleVPN` gains an explicit `allowChurn bool`. Attempt 2 (reclaim +
   `ClearState` + fresh `Connect`) runs only when `allowChurn` is true. Only
   `citadel reconnect --force` passes true; every unattended caller
   (`cmd/work.go`, `cmd/egress_relay_serve.go`, `cmd/ingress.go`) passes false and
   **refuses** with an actionable error instead of churning. The hand-rolled
   duplicate churn block in `cmd/controlcenter.go` is routed through the same
   function so the two can never diverge. Invariant to pin by test: `ClearState`
   is reachable only from `Logout` (explicit) and `reconnect --force`.

2. **A transient timeout no longer escalates to a re-register.** Give the
   IP-preserving Attempt 1 (`ReconnectWithAuthKey`) a real budget via a dedicated
   constant rather than the 10s `reconnectTimeout` probe cap (leave
   `reconnectTimeout` and `TestReconnectTimeoutConstant` untouched). Distinguish a
   transient-unreachable condition from a genuine key rejection by returning a
   typed error from `waitForConnection` carrying the last `BackendState` and
   `Status.Health[]`, and branch on it before any churn: `NeedsLogin`/
   `NeedsMachineAuth` is a real auth problem; `Starting` / no control response is
   transient and must retry/refuse, never churn.

3. **Honest churn detection, persisted identity.** Persist the node's Headscale
   identity to `<network.GetNodeConfigDir()>/node-identity.json` (a sibling of
   `network/`, so `ClearState` does NOT remove it), refreshed whenever the node is
   cleanly connected: `{ headscale_node_id, stable_id, mesh_ip, hostname,
   recorded_at }`. Drive the recovery result's `IPPreserved` from an actual
   comparison of the post-recovery `status.Self` against this record. On mismatch,
   report a churn honestly (`was <old> -> now <new>`) rather than a false
   "preserved". First connect (no record) records a baseline and reports "first
   registration", never a churn. The long-lived `citadel work` is the natural
   writer. Persist via `nodeidentity`'s machine-convergent store accessor that
   **loads, never mints** (`Store.PublicKey()`), per the K-A "no side-effect key
   minting" rule.

4. **Attempt 2 fetches its own fresh authkey.** `recoverStaleVPN` currently fetches
   one single-use key and reuses it for Attempt 2 after Attempt 1 may already have
   spent it at Headscale -- a spent single-use key on an expired node is the
   headscale `#2434` panic/401 condition. The churn path (now `--force` only) must
   `FetchFreshAuthkey` again for its own `Connect`.

5. **Distinct given-name belt-and-suspenders.** When a fresh registration is
   genuinely unavoidable (`--force`, or a first-ever enrollment on a host that
   already runs its own tailscaled), register the citadel node under a given-name
   that cannot collide with the host system-tailscale node or a prior citadel
   ghost (e.g. a `-citadel` suffix on the resolved node name). This is secondary:
   on the happy path the preserved machine key makes the given-name irrelevant.

6. **Part 3: emit the durable identity.** Add the `nodeidentity` SPKI fingerprint
   (loaded, never minted) to the heartbeat as a stable key the backend can map to
   the current node id and rebind capabilities at use time. Inert until the
   backend consumes it, matching the `FabricNodeID` precedent. Additive/omitempty.

## Test strategy

This repo's CI `Build + Test` is the merge authority. Several `internal/network`
tests (`EnsureStateDir`, `ClearState`-adjacent) and `recoverStaleVPN` reach the
**real** `GetStateDir()`, which on a live node resolves to that node's live
`tailscaled.state`; running them unguarded on such a box would wipe the node's
identity. Therefore:

- `cmd/reconnect.go` gains package-level func-var seams (mirroring
  `cmd/init.go`'s `initGetStateDirFn = network.GetStateDir` convention):
  `clearStateFn`, `reconnectWithAuthKeyFn`, `connectFn`, `fetchFreshAuthkeyFn`,
  `reclaimStaleNodeFn`, `getStateDirFn`. The refusal/no-churn tests swap these for
  stubs and assert `clearStateFn` is **never** called on the no-`--force` path.
- New tests must never call the real `ClearState`/`Connect`/`EnsureStateDir`, and
  must assert their sandbox took effect before proceeding. CI validates the full
  suite on a clean runner.

## References

- citadel-cli `#1235` (this), `#1131` (re-registration mints a duplicate identity;
  same class, still open), `#246` (`reclaimStaleNodeByHostname`), `#383`/`#845`
  (machine-convergent state/config dir).
- headscale `v0.29.1` `hscontrol/state/state.go` (`findExistingNodeForPAK`,
  `HandleNodeFromPreAuthKey`, update-in-place vs create-new branches),
  `#2434`/`#2435` (spent-key panic), `#3383` (v0.29.3 ephemeral lingering).
- aceteam `app/api/fabric/authkey/generate` (reconnect mint, no GC),
  `app/api/fabric/device-auth/token` (`MachineMapping` cleanup), nexus
  `reenroll/app.py` step 7 (delete-before-register).
