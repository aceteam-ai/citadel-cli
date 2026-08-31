# Design: Expose Custody — EXPOSE_LIST, UNEXPOSE, and the Epoch-Regression Guard

Status: **IMPLEMENTED (2026-08-30).** §9's open questions are resolved below;
the citadel-cli (Go) node-side implementation (EXPOSE_LIST, UNEXPOSE, the
epoch-regression guard, and the funnel mutex) shipped in this PR. §7's
backend/MCP wire contract is unimplemented follow-up in the other repo.
Cross-refs: citadel-cli#944 (this issue), #598 (EXPOSE_SET + visibility ladder),
#647 (durable exposures + `citadel service unexpose`), aceteam#8557
(node-hosted publishing epic; the backend/MCP counterpart is a paired aceteam
issue owned by another agent — §7 states the wire contract they must honor and
nothing more).

## 0. Scope

Three custody gaps on the node side of `expose`, all verified against source
(§1), and a design for closing each:

1. **No inventory read-back** — an `EXPOSE_LIST` job (§3).
2. **No remote teardown** — an `UNEXPOSE` job (§4, including why a job type
   beats an `EXPOSE_SET remove:true` flag).
3. **Epoch-resurrection landmine** — revoked link tokens re-validate after a
   blind re-expose (§5, the security core; recommendation is a refined
   option (b)).

citadel-cli (Go) node side only. The aceteam MCP tools and any server-side
exposure records are the other repo's slice; §7 is the full extent of what this
doc says about them.

## 1. Current state (file:line)

### 1.1 The one remote verb: EXPOSE_SET

- `internal/worker/expose_set.go` is the only expose-lifecycle job handler.
  `ExposeRequest` carries `name, port, visibility, ttl_seconds, creator, epoch`
  (expose_set.go:33-51); `parseExposeRequest` defaults a missing/non-positive
  epoch to **1** (expose_set.go:167-169). Registered in `cmd/nodejobs.go:168`
  with `liveExposeOps{}` as its `ExposeOps`.
- Privilege gate: honored only on the per-node stream
  (`isPerNodeStream(job.SourceQueue)`, expose_set.go:103) — the `:node:`
  segment check in `internal/worker/agent_update.go:334-341`, the same
  fail-closed posture as MODULE_SET / AGENT_UPDATE / WHATSAPP_PROVISION /
  SHOW_PAIRING_CODE.
- Job-type string lives at `internal/worker/job.go:148` and in the known-types
  list at job.go:216.
- Lane routing: EXPOSE_SET is in **neither** `unboundedJobTypes`
  (deadline.go:59) nor `serializedLaneJobTypes` (deadline.go:119) — it executes
  on the inline/default branch with the ordinary watchdog tier. Relevant to
  §5.4's concurrency analysis.

### 1.2 The funnel: liveExposeOps (cmd layer)

- `liveExposeOps.Expose` (`cmd/expose_ops.go:37-85`) is the single choke point
  — its own comment: "Every caller — the CLI, the MCP verb, the EXPOSE_SET job
  — funnels through here." It programs the in-process gateway
  (`gateway.Server.Expose`, exposure.go:249), persists the record
  (`config.SaveExposure`, best-effort: a failed write is logged, not returned —
  expose_ops.go:59-67), and mints the link token bound to `req.Epoch`
  (expose_ops.go:81).
- `liveExposeOps.Unexpose` (`cmd/expose_ops.go:109-128`) is **already written**:
  live route first (`gateway.Server.Unexpose`, exposure.go:222 — order is
  load-bearing, see its comment), then `config.DeleteExposure`. But it is
  reachable only through the local control endpoint: `citadel service unexpose`
  POSTs to `/agent/unexpose` on the running `citadel work` process
  (`cmd/service_unexpose.go:52`). The backend cannot call it.
- `restoreExposures` (`cmd/expose_ops.go:138-173`) re-wires the persisted set
  at gateway start, before `Start` — the #647 restart-survival leg.

### 1.3 The durable record

- `config.ExposureRecord` (`internal/config/exposures.go:37-54`): `name, port,
  visibility, creator, token_epoch`. **No created-at timestamp** — the issue's
  EXPOSE_LIST field list ("name, source, visibility, epoch, created") needs one
  added (§3.2). `TokenEpoch`'s own doc comment says it MUST survive a restart
  so a bump can revoke what was already handed out — persistence exists;
  *enforcement* does not.
- `SaveExposure` (exposures.go:78-101) blindly replaces by name — including
  `TokenEpoch`. This is the write that makes the landmine real.
- `DeleteExposure` (exposures.go:105-120) removes the record entirely —
  including the only durable memory of the name's epoch. This is the hole in
  the issue's option (a) that §5.2 works through.
- All reads/writes are unlocked load-modify-writes of `exposures.json`
  (temp-file + rename for crash atomicity, but nothing serializes two
  concurrent writers — §5.4).

### 1.4 The verifier

- `verifyLinkToken` (`internal/gateway/exposure.go:365-406`) checks the HMAC,
  then requires an **exact epoch match**: `gotEpoch != epoch → false`
  (exposure.go:397-399). `exposureMiddleware` passes the live policy's
  `TokenEpoch` (exposure.go:440, 451). Exact match is the correct primitive —
  the bug is not here, it is that nothing stops the live policy's epoch from
  being *rewound* by a later EXPOSE_SET.

### 1.5 The caller contract today (aceteam side, read-only grounding)

`aceteam/python-backend/routes/aceteam_mcp_expose.py`:
`dispatch_expose_set(..., epoch: int = 1)` — the caller supplies the epoch, the
console "holds the current value in local UI state," and the module docstring
itself flags "durable server-side exposure records + a monotonic cross-session
epoch are a documented follow-up." Its result dict already echoes an `epoch`
field back to the tool caller. Both facts matter for §5.3: the backend has no
durable epoch state *by its own admission*, and the tool surface already has a
slot for an echoed epoch.

### 1.6 The landmine, end to end

1. Expose `frigate` at epoch 1, visibility `link`; share tokens.
2. A token leaks. Operator revokes: re-expose with epoch 2. `SaveExposure`
   records `token_epoch: 2`; every epoch-1 token now fails the exact-match
   check. Correct.
3. Weeks later, a different session / operator / agent — holding no state,
   because *nothing* holds this state durably (§1.5) — re-exposes `frigate`.
   The MCP default is `epoch=1`. `parseExposeRequest` accepts it,
   `liveExposeOps.Expose` programs the gateway with `TokenEpoch: 1`,
   `SaveExposure` overwrites the record. **Every token revoked in step 2
   verifies again.** Nothing logs, nothing refuses, nothing even notices.

The signing key is durable (`expose_key.go`) precisely so tokens survive
restarts — which is what makes a resurrected epoch *fully* weaponizable rather
than merely theoretical.

## 2. Design principles carried over

- **Fail-closed per-node gating for all three verbs**, including the read-only
  one. EXPOSE_LIST discloses the node's ingress inventory (names, ports,
  visibility, whether a revocable-token surface exists) — recon value — and a
  uniform gate is also simpler to reason about than "writes are gated, reads
  are not."
- **Worker handlers stay standalone** (expose_set.go's "Standalone by design"):
  handlers validate + route, an injected ops interface does side effects, the
  cmd layer wires the live gateway/config-dir/mesh edges. All new logic that
  touches `exposures.json` or the gateway lives in `cmd/expose_ops.go`, the
  existing funnel.
- **The verifier does not change.** `verifyLinkToken`'s exact-match rule is
  right; custody is enforced where epochs are *chosen*, not where tokens are
  *checked* (§5.3).

## 3. EXPOSE_LIST

### 3.1 Job type, gating, registration

- `JobTypeExposeList = "EXPOSE_LIST"` in `internal/worker/job.go` (const block
  next to JobTypeExposeSet at :148, plus the known-types list at :216).
- New handler `internal/worker/expose_list.go` (`ExposeListHandler`), same
  shape as ExposeSetHandler: `isPerNodeStream` gate first, nil-ops check,
  no payload fields required (an empty payload lists everything; the sets are
  a handful of records, so no filter/pagination). Failure on the gate, never
  retry — a misrouted read is terminal.
- Registered in `cmd/nodejobs.go` alongside EXPOSE_SET, sharing the same ops
  value (§6.1).
- Lane/watchdog: default tier, inline branch — it is a file read plus a map
  lookup, no compose/docker/network calls.

### 3.2 What it returns: durable set as authority, live bit as honesty

The durable set (`config.LoadExposures`) is the source of truth — it is what
survives restarts and what restore re-wires. But the two sets can diverge in
both directions today: `SaveExposure` is best-effort (live-but-not-persisted,
expose_ops.go:59-67), and a persisted record can point at a gateway that
rejected it on restore (restoreExposures skips bad records loudly). So each
row carries a `live` bool read from the gateway's in-memory policy table
(a `getExposure(name) != nil` equivalent surfaced via a small exported
accessor, e.g. `gateway.Server.HasExposure(name) bool`), and the result also
reports any live-only exposures the durable set is missing:

```json
{
  "exposures": [
    {
      "name": "frigate",
      "port": 5000,
      "visibility": "link",
      "creator": "",
      "epoch": 2,
      "created_at": "2026-08-30T01:02:03Z",
      "live": true
    }
  ],
  "count": 1,
  "live_only": ["scratch-dash"]
}
```

- `epoch` is `ExposureRecord.TokenEpoch` verbatim. Deliberately included: it is
  not a secret (it rides inside every minted token's payload,
  `MintLinkToken`, exposure.go:344-352), and returning it is what lets an
  option-(a)-style read-before-write caller exist at all — and lets the
  backend rebuild its documented-follow-up server-side records from truth.
- `created_at` requires adding `CreatedAt string` (RFC3339, `omitempty`) to
  `ExposureRecord`. Set on first save; **preserved on replace-by-name** (the
  record's identity continuity across re-exposes), refreshed only when a
  record is created after not existing. Additive JSON field: old files decode
  with it empty, old binaries ignore it — no migration.
- The token itself is never returned, matching exposures.go:34-36's rule
  (never persist or re-derive a credential outside the mint path).
- Deliberately NOT included: a `localPortListening` probe per record. It costs
  up to 300ms per row (expose_ops.go:179) and answers a different question
  ("is the app up") than custody ("what did this node agree to serve"). Open
  question §9.2 if the backend wants it anyway.

### 3.3 Ops seam

```go
// internal/worker (expose_list.go)
type ExposureInfo struct {
    Name       string `json:"name"`
    Port       int    `json:"port"`
    Visibility string `json:"visibility"`
    Creator    string `json:"creator,omitempty"`
    Epoch      int    `json:"epoch"`
    CreatedAt  string `json:"created_at,omitempty"`
    Live       bool   `json:"live"`
}

type ExposeListResult struct {
    Exposures []ExposureInfo `json:"exposures"`
    LiveOnly  []string       `json:"live_only,omitempty"`
}
```

The cmd adapter merges `config.LoadExposures(platform.ConfigDir())` with the
gateway's policy names. A corrupt `exposures.json` is a job FAILURE with the
parse error in the output (the caller must know the durable set is unreadable),
not an empty success — the opposite lenience direction from `SaveExposure`'s
"start fresh" recovery, because a *reader* reporting an empty set as truth is
exactly the blindness this job exists to end.

## 4. UNEXPOSE

### 4.1 Job type, not an EXPOSE_SET flag

Recommendation: a dedicated `JobTypeUnexpose = "UNEXPOSE"`. Reasons, in order:

1. **Payload shape.** `parseExposeRequest` *requires* a valid port and
   visibility (expose_set.go:161-166). A `remove: true` flag means those
   requirements become conditional, the parser grows a mode switch, and every
   existing validation test needs a removal-mode twin. An UNEXPOSE payload is
   `{name}` — two-line parser, fails on nothing else.
2. **Precedent.** EXPOSE_SET is imperative, not declarative. The codebase's
   declarative verb (MODULE_SET) folds removal into `desired_status: absent`
   because the whole job IS a desired-state assertion; the imperative pairs get
   distinct types — SERVICE_START / SERVICE_STOP is the direct analogue, and
   exposures are service-shaped.
3. **Audit legibility.** Revocation hidden inside a "set" verb is invisible in
   job-history/dispatch logs at exactly the moment someone is reconstructing
   who tore a route down. The aceteam dispatcher already AUDIT-logs per job
   type (aceteam_mcp_expose.py's `AUDIT expose dispatch` line); a distinct type
   gets that for free.

Cost acknowledged: one more entry in job.go's two lists and one more small
handler file. Cheap.

### 4.2 Behavior

- `isPerNodeStream` gate, identical to EXPOSE_SET — teardown of a node's
  ingress is exactly as privileged as its creation.
- Calls the already-written `liveExposeOps.Unexpose` (§1.2) — live route down
  first, then durable delete, its ordering rationale intact. The handler adds
  nothing to that logic.
- Result mirrors `cmd.UnexposeResult` (expose_ops.go:89-97):

```json
{ "name": "frigate", "was_exposed": true }
```

  `was_exposed:false` is still SUCCESS (revoke is idempotent — same contract
  the CLI already presents, service_unexpose.go:81-88).
- Retry-vs-failure split mirrors EXPOSE_SET's: "no in-process gateway" is a
  retry (transient, expose_ops.go:115); a durable-delete failure is a FAILURE
  carrying Unexpose's own "no longer served, but its saved record could not be
  removed" message (expose_ops.go:124) — the route is already down, so the
  runner must not retry a teardown that half-succeeded into re-running the
  gateway half forever.
- **Epoch interaction (matters for §5):** UNEXPOSE does not bump anything. Old
  tokens die because the policy is gone (`exposureMiddleware` 404s an
  unregistered name, exposure.go:420-424). What keeps them dead *after a later
  re-expose* is §5.3's high-water rule, not anything UNEXPOSE does.

## 5. Epoch custody — the security core

### 5.1 The invariant to enforce

> For any exposure name, the live policy's `TokenEpoch` never takes a value
> less than or equal to any epoch under which tokens were previously revoked —
> across re-exposes, unexposes, restarts, and caller amnesia.

Equivalently: the per-name epoch sequence is non-decreasing over the name's
entire lifetime, and every revocation event strictly increases it. Given
`verifyLinkToken`'s exact match, that makes "a revoked token re-validates"
unreachable.

### 5.2 Option (a): reject regression — necessary idea, insufficient alone

Rule: fail an EXPOSE_SET whose `req.Epoch <` the persisted record's
`TokenEpoch` for that name.

- **Correctness hole the issue doesn't state:** `DeleteExposure` removes the
  record — the comparison target. Sequence: expose@1 → rotate@2 (revoking
  leaked epoch-1 tokens) → UNEXPOSE (which §4 just made remotely reachable!)
  → re-expose, default epoch 1. No record exists, the guard has nothing to
  compare against, epoch 1 is accepted — and the revoked epoch-1 tokens, still
  signed by the durable key, verify again. Option (a) as literally specified
  closes the re-expose door and opens the unexpose→re-expose door *in the same
  PR that ships UNEXPOSE*. Any correct design needs epoch memory that
  **outlives the exposure record** (§5.3's high-water store) — at which point
  most of (a)'s "simple" advantage is spent.
- **Blind-caller breakage is real and immediate:** the MCP default is
  `epoch=1` (§1.5). Post-(a), every innocent re-expose of a once-rotated name
  fails until the backend learns to read-before-write via EXPOSE_LIST. The
  issue itself flags this sequencing trap. A hard failure is at least honest
  (fail closed, clear error naming the minimum acceptable epoch) — but it
  converts every stateless caller into a two-round-trip caller forever.

### 5.3 Option (b), refined: node-owned epoch with explicit rotation — RECOMMENDED

Pure (b) ("auto-increment on each re-expose") has its own regression: a plain
re-expose — the backend re-asserting desired state, or a second link being
minted for the same dashboard to share with a second person — would silently
revoke every outstanding link. Rotation and re-expose are different intents;
pure (b) collapses them. The refinement separates them while keeping
everything that makes (b) right:

**The node owns the epoch. The caller expresses intent, not state.**

New `ExposeRequest` field: `Rotate bool` (`rotate` in JSON). The caller's
`Epoch` field is kept for wire back-compat but demoted to a fast-forward hint
(never a rewind). The exact rule, applied inside `liveExposeOps.Expose` under
§5.4's mutex, before the gateway is programmed:

```go
// base: the highest epoch this name has ever lived at.
//   - live record exists      -> record.TokenEpoch
//   - no record (fresh/post-unexpose) -> highWater(name) + 1
//     (never re-enter a dead epoch: pre-unexpose tokens stay dead)
//   - name never seen         -> highWater=0, so base = 1
effective := max(base, req.Epoch) // caller may fast-forward, never rewind
if req.Rotate {
    effective = max(base, req.Epoch) + 1 // explicit revoke-all verb
}
// persist: record.TokenEpoch = effective; highWater(name) = effective
```

Properties, checked against the four criteria in the issue:

- **Can a revoked token ever re-validate? No.** Revocation is any event that
  moved the name's epoch past the token's (rotation, legacy fast-forward, or
  unexpose followed by any re-expose). `effective` is non-decreasing by
  construction, `max(base, ...)` absorbs a stale caller value, and the
  post-unexpose `highWater+1` closes §5.2's hole. The invariant in §5.1 holds
  across every path.
- **Blind re-expose caller: fully safe, zero round trips.** Default payload
  (`epoch=1`, no rotate) against a name living at epoch 5 →
  `max(5,1)=5` → outstanding links keep working, no revocation, no failure.
  This is strictly better than (a) for the exact caller (a) breaks.
- **Migration/back-compat: monotone in both directions.** Today's console
  rotation flow ("send current+1") still works unchanged — a caller epoch
  above base is honored as a fast-forward, so the legacy revoke gesture
  revokes. An old backend that never sends `rotate` loses nothing; a new
  backend can drop epoch bookkeeping entirely and send `rotate:true` to
  revoke. No flag day, no version gate.
- **MCP tool contract: additive, and the slot already exists.** The tool
  result already echoes an `epoch` field (§1.5); the node now returns the
  *effective* epoch in EXPOSE_SET's output (`"epoch": effective` added to the
  output map at expose_set.go:126-130 — additive key, same pattern as
  `token`/`expires_at`), and the tool echoes truth instead of its own input.
  The tool grows an optional `rotate` param. Its docstring's "monotonic
  cross-session epoch is a documented follow-up" is discharged *on the node*,
  which is where aceteam's own module doc says the source of truth lives.

**Where the pieces go:**

- The rule runs in `cmd/expose_ops.go`'s `Expose`, not the worker handler —
  it needs `LoadExposures` + the high-water store + the mutex, all cmd-layer
  edges, and putting it at the funnel covers the CLI `/agent/expose` path and
  the EXPOSE_SET path with one implementation (the same reason #647 put
  persistence there). `parseExposeRequest` keeps its epoch default (a
  harmless floor once the node owns the value).
- `verifyLinkToken` and `exposureMiddleware` are untouched (§2).
- The worker handler's only change: carry `Rotate` through and surface the
  returned effective epoch in the job output.

**Epoch high-water store:** a new small map file next to `exposures.json` —
`expose_epochs.json`, `{"<name>": <int>}`, 0600, temp+rename, same
crash-atomicity pattern as writeExposures (exposures.go:125-146). Written on
every effective-epoch computation; entries are never deleted (that is the
point — it is the memory that survives `DeleteExposure`). Growth is bounded by
the number of distinct operator-chosen slugs a node has ever exposed;
negligible. Alternative considered and rejected: tombstone records inside
`exposures.json` (a `revoked: true` flag) — it would put non-exposures into
every existing `LoadExposures` consumer (`restoreExposures` would have to
learn to skip them; EXPOSE_LIST would have to filter them) to save one small
file. Lenient-read posture matches the store's role: a missing/corrupt
high-water file degrades to `highWater=0` — for a *live* record `base` still
comes from `record.TokenEpoch`, so the common case loses nothing; the loss is
the post-unexpose memory, and failing every future expose of every name
because one file is corrupt is the wrong trade (mirrors `SaveExposure`'s own
corrupt-store recovery reasoning, exposures.go:83-87).

### 5.4 Serializing the funnel (required by §5.3, latent before it)

`SaveExposure`/`DeleteExposure` are unlocked load-modify-writes (§1.3), and
`liveExposeOps` has **two concurrent entry points inside one process**: the
worker lane (EXPOSE_SET, inline branch — §1.1) and the `/agent/expose` +
`/agent/unexpose` HTTP control handlers serving the local CLI. Today that race
loses at worst a record. Once epochs are computed read-modify-write, two
concurrent exposes could both read `base=N` and both settle on the same
`effective` — with `rotate`, a revocation that silently didn't.

Fix: a package-level `sync.Mutex` in `cmd/expose_ops.go`, held across each
whole op (epoch resolution + gateway programming + persistence) in `Expose`
and `Unexpose` (and the EXPOSE_LIST read, cheaply, for a consistent snapshot).
Deliberately NOT `serializedLaneJobTypes` membership: the lane serializes
*jobs* against each other only (deadline.go:72-91's own framing) — it cannot
see the HTTP control endpoints, so lane membership alone leaves the CLI-vs-job
race open, and the mutex alone makes lane membership redundant. These ops are
milliseconds (map writes + one small file), so holding a mutex across them is
not a lane-blocking concern.

## 6. Implementation shape (sketch, not code)

### 6.1 One ops interface, three verbs

Extend `worker.ExposeOps` rather than growing sibling interfaces per verb —
one live adapter, one wiring site, one test fake:

```go
type ExposeOps interface {
    Expose(ctx context.Context, req ExposeRequest) (*ExposeResult, error)
    Unexpose(ctx context.Context, name string) (*UnexposeResult, error) // worker-side mirror of cmd.UnexposeResult
    List(ctx context.Context) (*ExposeListResult, error)
}
```

`cmd.liveExposeOps` already has `Unexpose` with a cmd-typed result; the
adapter converts (cmd imports worker, never the reverse — the existing
direction). Compile-time breakage of the interface is confined to
`liveExposeOps` and the handler tests' fakes. `ExposeResult` gains
`Epoch int` so the effective epoch flows back (§5.3).

### 6.2 Files touched (when implemented)

| Change | Where |
|---|---|
| `JobTypeExposeList`, `JobTypeUnexpose` consts + known-types list | `internal/worker/job.go` |
| `ExposeListHandler`, `UnexposeHandler` (gate → ops → result) | `internal/worker/expose_list.go`, `unexpose.go` (new) |
| `Rotate` field; effective epoch in output | `internal/worker/expose_set.go` |
| Epoch rule + mutex + list/unexpose adapters | `cmd/expose_ops.go` |
| `CreatedAt` on `ExposureRecord`; high-water store | `internal/config/exposures.go` (+ `expose_epochs.go` or same file) |
| `HasExposure(name)` accessor (or equivalent) | `internal/gateway/exposure.go` |
| Register both handlers | `cmd/nodejobs.go` |

### 6.3 Tests that pin the contract

- Handler-level (fake ops): per-node-stream refusal for both new types;
  UNEXPOSE idempotency (`was_exposed:false` is success); EXPOSE_LIST failure
  on a corrupt store.
- Epoch rule (pure function — extract `resolveEffectiveEpoch(base, reqEpoch,
  rotate)` so it tests without a gateway): the §5.3 table, including the
  §5.2 unexpose→re-expose sequence asserting the pre-unexpose token can never
  find a matching live epoch again. This test is the design's reason to exist;
  it should read like §1.6's sequence.
- `CreatedAt` preserved across replace-by-name; absent on old files.
- Concurrency: two goroutines through the mutexed funnel never produce
  duplicate effective epochs for one name.

## 7. Paired backend contract (wire only — the other repo designs its own internals)

What the node will honor / return; the aceteam agent owns everything behind it:

- **EXPOSE_LIST**: dispatch `{type: "EXPOSE_LIST", payload: {target_node}}` on
  the per-node stream; Output is §3.2's shape. Anything not on the per-node
  stream fails closed, same as EXPOSE_SET today.
- **UNEXPOSE**: `{type: "UNEXPOSE", payload: {name, target_node}}`; Output
  `{name, was_exposed}`; idempotent success.
- **EXPOSE_SET (amended)**: payload may add `rotate: true` (revoke-all);
  `epoch` remains accepted but is a fast-forward hint only — the node's
  Output now carries the authoritative `epoch`, and the backend/console should
  display and (if it keeps any state at all) store *that*, not its input. A
  backend that wants (a)-style read-before-write UX can build it on
  EXPOSE_LIST, but nothing requires it: rotation is `rotate:true`, and plain
  re-expose is safe blind.
- Sequencing: none required. §5.3 is deliberately not dependent on the backend
  changing first (unlike option (a), which is only safe to land after
  EXPOSE_LIST + a backend read-before-write change ships).

## 8. Security posture summary

- All three verbs per-node-stream gated, fail closed, including the read
  (§2). No new always-allowed surface; `exposureMiddleware` remains the sole
  request-path gate and is unchanged.
- Tokens: never persisted, never returned by EXPOSE_LIST; minting stays the
  only path (§3.2).
- The §5.1 invariant becomes enforced rather than aspirational, with its two
  durability legs named: `TokenEpoch` survives restarts (#647, exists) and the
  high-water mark survives `DeleteExposure` (new).
- Residual risks, stated plainly: (1) the high-water file is node-local — a
  wiped config dir wipes revocation memory along with the signing key it sits
  beside (a wiped key kills all old tokens anyway, so these fail together, in
  the safe direction); (2) `exposures.json` custody uses invoker-scoped
  `platform.ConfigDir()` (expose_ops.go:59,123,139) rather than the
  machine-convergent `network.GetNodeConfigDir()` — today every reader/writer
  lives inside the one `citadel work` process so it is convergent in practice,
  but it is the same divergence class CLAUDE.md documents for #845; migrating
  exposure state (key + records + high-water together) is a deliberate
  non-goal here and flagged as follow-up material.

## 9. Open questions for Jason — RESOLVED (2026-08-30)

1. **Re-expose semantics under node-owned epoch — RESOLVED: refined option
   (b), preserve-links + explicit rotate.** A plain re-expose PRESERVES the
   current epoch (outstanding links and blind/stateless callers keep
   working); only an explicit `rotate:true` (or a legacy caller sending a
   LOWER epoch than the name's current one — a fast-forward is still honored,
   but a value at or below the live epoch never regresses it) increments the
   epoch — the deliberate revocation act. Implemented exactly as §5.3
   specifies: `resolveEffectiveEpoch(base, reqEpoch, rotate)` in
   `cmd/expose_ops.go`, pinned by `TestResolveEffectiveEpoch`
   (`cmd/expose_ops_test.go`).
2. **EXPOSE_LIST upstream probe — RESOLVED: no probe (the doc's default).**
   `ExposureInfo` carries the durable set's own fields plus the gateway's
   `live` bit; no `localPortListening` call was added. Revisit only if the
   console asks for it later.
3. **High-water store location — RESOLVED: separate `expose_epochs.json`
   (§5.3's recommendation).** Implemented in
   `internal/config/expose_epochs.go`: a `{"<name>": <highest-epoch-ever>}`
   map, 0600, temp+rename, entries never deleted. This is the piece of
   durable memory that OUTLIVES `DeleteExposure` — the property that closes
   the unexpose→re-expose resurrection hole (§5.2): when Expose finds no
   durable record for a name (fresh, or the record was just deleted by
   UNEXPOSE), the new epoch's floor is `ExposeEpochHighWater(name) + 1`, not
   1. `TestExposeOps_UnexposeThenReExposeDoesNotResurrectRevokedEpoch`
   (`cmd/expose_ops_test.go`) is the acceptance test for this property
   end-to-end.
4. **`citadel service list-exposures` — RESOLVED: out of scope for this
   PR.** EXPOSE_LIST (the remote job) shipped; a local CLI read-back over the
   same `liveExposeOps.List` is cheap to add later but was not requested and
   is not implemented here.

Also decided as part of the same pass: **UNEXPOSE is a dedicated job type**
(`internal/worker/unexpose.go`), not an `EXPOSE_SET remove:true` flag —
§4.1's three reasons (payload shape, imperative-verb precedent, audit
legibility) stand as written. And the §5.4 concurrency mutex was implemented
exactly as specified: a package-level `sync.Mutex` (`exposeOpsMu`) in
`cmd/expose_ops.go`, held across the whole of `Expose`, `Unexpose`, and
`List`, closing the CLI-vs-job race lane membership alone cannot reach.
`TestExposeOps_ConcurrencyMutexSerializesRMW` pins it: 20 concurrent
`rotate:true` calls against the same name from a base of 1 land on exactly
epoch 21, never fewer (a lost update would mean the mutex regressed).

## 10. Phased plan

- **Phase 1 — EXPOSE_LIST + groundwork** (safe, additive, unblocks the blind
  backend): job type + handler + ops `List`; `CreatedAt` on ExposureRecord;
  the funnel mutex (§5.4 — landed first so later phases inherit it);
  `HasExposure` accessor.
- **Phase 2 — UNEXPOSE**: job type + handler over the existing
  `liveExposeOps.Unexpose`. Ship with or after Phase 1; must NOT ship before
  Phase 3 is at least scheduled, since remote unexpose widens §5.2's hole
  from "operator ran a local CLI" to "any backend caller."
- **Phase 3 — epoch custody**: high-water store, effective-epoch rule in
  `Expose`, `rotate` flag, `epoch` in EXPOSE_SET output. The §6.3 epoch test
  table is the acceptance gate.
- **Phase 4 — aceteam side (other agent, separate repo)**: `expose_list` /
  `unexpose` MCP tools, `rotate` param on `expose`, tool result echoes the
  node's effective epoch. No node-side dependency in either direction beyond
  the §7 wire shapes.
