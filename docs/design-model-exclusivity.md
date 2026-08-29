# Design: Model Exclusivity — `run --exclusive` + Local MCP Deploy/Evict

Status: **DESIGN ONLY — no Go changes in this PR.**
Cross-refs: aceteam#8248 ("run a model exclusively" CLI/one-command swap), aceteam#8249
(local MCP node-lifecycle tools, v2 — model deploy/evict + run-exclusive).
Builds on citadel-cli#832/#851 (reserve/evict/restore primitive, merged, zero
callers), citadel-cli#846 (`citadel module stop/start/restart`, merged),
aceteam#8249 v1 (local MCP tools: module control + local chat + local files,
merged; see `cmd/mcp_local.go`'s package doc), and citadel-cli#858 (the
`stdoutCaptureInFlight` nesting guard those v1 tools rely on).

## 0. Scope of this document

This is the **remaining** slice of #8248/#8249: everything else in those two
issues already shipped (see §1). What's left is exactly two things that share
one primitive:

1. A caller that actually invokes `Reserve`/`Release` — today they have zero
   callers.
2. Two local MCP tools (`local_model_deploy`/`local_model_evict`, or similar)
   plus one that wires the above into `citadel mcp` with local authority.

Both need the same underlying design decision — **who owns a reservation's
lifetime, and what happens if that owner dies** — so §2 works that out once
and §3 reuses it.

## 1. Current state (file:line)

### 1.1 The reserve/evict/restore primitive (#851, `internal/jobs/reservation.go`)

Three methods on `*ServiceHandler`, all merged, all currently uncalled outside
their own tests:

```go
func (h *ServiceHandler) Reserve(ctx JobContext, jobID string, requiredVRAMBytes uint64) (*Reservation, error)   // reservation.go:96
func (h *ServiceHandler) Release(ctx JobContext, jobID string) ([]string, error)                                  // reservation.go:187
func (h *ServiceHandler) ReconcileOrphanedReservations(ctx JobContext, holdsWorkerLock bool) ([]string, error)    // reservation.go:295
func (h *ServiceHandler) ActiveReservations() ([]ReservationSummary, error)                                       // reservation.go:340
```

- **`Reserve(jobID, requiredVRAMBytes)`** reuses #577 unchanged
  (`buildPreemptCandidates` → `status.PlanPreemption`, `internal/status/preempt.go:65`)
  to decide which non-pinned services to durably stop. The new piece is that
  every stopped service is tagged `evicted_by_job: <jobID>` +
  `evicted_prior_status: <its desired_status before eviction>` directly in
  `citadel.yaml` (`setEvictedMarkersInManifestFile`,
  `internal/jobs/service_handler.go:1586`) — durable-FIRST, then stop
  (`reservation.go:144-162`). `requiredVRAMBytes==0` is a no-op reservation
  (never evicts on an absent signal, mirroring `PlanPreemption`'s own
  contract). Unlike `preemptForVRAM` (#577), an **unknown** free-VRAM signal
  is a hard error here, not a skip — Reserve is an explicit ask for a
  *guaranteed* hold (`reservation.go:80-95`).
- **`Release(jobID)`** restarts every service still tagged `evicted_by_job ==
  jobID`, restores its `evicted_prior_status` (not an unconditional clear —
  respects a service an operator had already marked stopped for an unrelated
  reason), then clears both tags — **only on full success per service**. A
  service whose restart or tag-write fails **keeps its tag**, so a retried
  `Release` (or `ReconcileOrphanedReservations`) picks it up again. Idempotent:
  no tag ⇒ no-op, nil error.
- **`ReconcileOrphanedReservations(holdsWorkerLock bool)`** is the crash-safety
  leg: it restores every service still tagged at the moment it runs. **It
  refuses outright unless `holdsWorkerLock` is true.** The doc comment
  (`reservation.go:252-291`) is explicit that this argument is a *required
  assertion*, not a convenience default, and the *only correct call site is
  `cmd/work.go`'s `runWork`, immediately after a successful
  `worklock.Acquire`, before the job consume loop starts* — because "any tag
  found here is orphaned" is only true when exactly one job-consuming process
  can be live for the node. **It explicitly flags a currently-latent gap**:
  `internal/worklock` guards `citadel work` against a second `citadel work`,
  but NOT against the control-center TUI's own consume loop
  (`cmd/controlcenter.go`), which runs the same handler set without ever
  calling `worklock.Acquire`. The doc names `#8248` by number as the future
  caller that could reopen this hazard if wired carelessly. **This is the
  single most load-bearing piece of prior art for this design** — read §2.3
  before proposing a call site.
- **No separate ledger.** The `evicted_by_job` tag *is* the reservation — a
  deliberate choice (`reservation.go:22-27`) so there is nothing else that can
  drift out of sync with it. `ActiveReservations()` is a pure manifest read,
  already wired onto the heartbeat as `NodeStatus.GPUReservations` via
  `cmd/work.go`'s `reservationsFrom` projection (mirrors #717's
  `swapStatsFrom` pattern, since `internal/status` cannot import
  `internal/jobs`).
- **Tag-scoped restore is job-path-only.** Only `Execute()`'s `SERVICE_START`/
  `SERVICE_STOP` branches clear a reservation tag as a side effect of an
  unrelated explicit action. The *local* start paths (`citadel run --service
  X`, boot-time `startManagedServices`) bypass `Execute()` and do NOT clear it
  — documented as latent/low-severity today because nothing reserves yet
  (`reservation.go`, "Tag-clearing is a job-path-only guarantee" section: an
  operator manually running a reservation-held service back up this way
  leaves it still tagged, so a later `Release` still finds it and re-applies
  a harmless, idempotent start — not a second, disruptive one). This stops
  being purely latent the moment something reserves — see §2.2, which covers
  the operator-facing consequences directly.

### 1.2 `citadel module stop/start/restart` (#846, `cmd/module_control.go`)

`runModuleControl(ctx, name, action)` (`module_control.go:192`) is the shared
entry point. It validates the name against the manifest, applies
`--expect-node`/`--dry-run` (#853), then dispatches to `liveModuleOps.Start`/
`Stop` (`cmd/module_ops.go`) — the **same** primitive `MODULE_SET` drives, but
scoped to one module with no worklock, no reconcile engine, no worker
restart. This is the pattern §2's design should match for the "local,
immediate action" half: **module control does not go through worklock at
all** — it acts directly on the manifest + compose from whatever process
invokes it, trusting that a scoped, idempotent, durable-marker action is safe
to run concurrently with anything else. Contrast this with reservation.go's
`ReconcileOrphanedReservations`, which explicitly is NOT safe to call this
way. Both patterns exist in the codebase today; §2.3 has to pick the right one
for exclusivity, and they are not the same pattern for good reason (see
below).

### 1.3 Local MCP tools (#8249 v1, `cmd/mcp_local.go`)

`citadel mcp` merges a local, `local_`-prefixed tool set into the normal
backend-proxied `tools/list`/`tools/call` (`cmd/mcp.go`'s `mcpBridge.run`).
Shipped in v1: `local_module_stop/start/restart` (thin wrapper over
`runModuleControl` via `runModuleControlCaptured`), `local_list_models`/
`local_chat` (reuse `status.DiscoverLocalEngines` + `gateway.ResolveChatModel`,
`internal/gateway/chat_route.go:283`), `local_read_file`/`local_list_files`
(reuse the `FILE_READ`/`FILE_LIST` handlers' own sandbox). The v1 package doc
comment (`mcp_local.go:12-16`) explicitly deferred model deploy/evict and
`run --exclusive` pending this primitive — that deferral is what this design
closes.

Two mechanisms every new mutating local tool must reuse, not reinvent:

- **`captureStdout`** (`mcp_local.go:639`): swaps `os.Stdout` for a pipe for
  the duration of `fn`, because under `citadel mcp`, `os.Stdout` *is* the
  JSON-RPC transport, and both `runModuleControl`'s own `fmt.Printf` progress
  lines AND the `docker compose up|down` subprocess it shells out to
  (`cmd.Stdout = os.Stdout`, hardwired) write there. A writer parameter alone
  would not be enough — the subprocess's own stdout is hardwired, not
  injected.
- **`stdoutCaptureInFlight`** (`mcp_local.go:637`, an `atomic.Bool`, not a
  mutex): refuses to nest a second `captureStdout` call while one is still in
  flight (possible since #858 gave local tool calls a real 5-minute
  timeout — a timed-out caller's goroutine can still be running, still holding
  the redirect, when the JSON-RPC loop moves on to dispatch a new call). It
  fails fast with a clear "no action was taken" error rather than blocking or
  racing. **This guard is process-global** — it already protects any new tool
  that reuses `captureStdout`, with no extra wiring needed, because it's one
  package-level `atomic.Bool` shared by every caller in the process.

### 1.4 `preemptForVRAM` / `serviceStart` (#577, `internal/jobs/service_handler.go`)

```go
func (h *ServiceHandler) serviceStart(ctx JobContext, svc manifestService, model string, requiredVRAMBytes, requiredRAMBytes uint64, trustRemoteCode trustRemoteCodeIntent) ([]byte, error)  // :313
func (h *ServiceHandler) preemptForVRAM(ctx JobContext, svc manifestService, requiredVRAMBytes uint64) error                                                                                  // :965
func parseRequiredVRAMBytes(payload map[string]string) uint64                                                                                                                                  // :667 — reads vram_mb (preferred) or vram_gb
```

`preemptForVRAM` runs inside `serviceStart`'s docker branch, gated on the
target not already running. It is the **non-restoring** sibling of `Reserve`:
same decision (`PlanPreemption`), same durable-stop discipline, but no tag, no
restore-on-release — an evicted service stays down until an explicit
`SERVICE_START` clears its marker. `vram_mb`/`vram_gb` on the `SERVICE_START`
payload is a real, tested, wired contract that the aceteam backend **does not
send yet** (`fabric_provision` dispatches only `{service, model}`) — so
preemption is inert on a live deploy today except via the #831 fallback
(`resolveRequiredVRAMBytes`, opt-in via `CITADEL_RESOURCE_ISOLATION`, estimates
from `status.EngineVRAMEstimateMB` when the payload is silent).

### 1.5 `MODULE_SET` (`internal/worker/module_set.go`)

Interim, imperative, single-module-scoped desired-state setter
(`running`/`stopped`/`absent`), consumed only on the per-node stream. It is
NOT how `SERVICE_START` reaches a node (that's a separate job type,
`JobTypeServiceStart = "SERVICE_START"`, `internal/worker/job.go:124`) — the
platform's model-deploy path (`fabric_deploy_model`/`provision_model` MCP
tools) dispatches `SERVICE_START` directly, not `MODULE_SET`. This matters for
§4: the backend-side hook for "make this deploy exclusive" is a `SERVICE_START`
payload field, not a `MODULE_SET` field.

## 2. `run --exclusive` design (#8248)

### 2.1 What "exclusive" means, precisely

The issue's ask is "free the node, load the model with real context, restore
on exit/undeploy." The wrinkle: nobody knows the model's true VRAM need ahead
of time — that's the entire point (today, hand-editing the manifest to evict
*everything* was the only way to find out).

**The naive version of this doesn't work, and the reason matters for the
implementation.** A first instinct is `requiredVRAMBytes := gpuTotalVRAMBytes
- margin`, fed straight into the existing `Reserve(jobID, requiredVRAMBytes)`.
Trace it through `PlanPreemption` (`internal/status/preempt.go:65`): it
declares `Fits` only when `availableVRAM + Σ(non-pinned candidate
VRAMBytes) >= requiredVRAM`. Asking for "total minus a small margin" fits only
when free VRAM plus every non-pinned candidate's *attributed* footprint adds up
to nearly the whole card — which fails whenever VRAM is held by something
`PlanPreemption` never sees as a candidate at all: an unmanaged process, a
non-manifest container (the codebase already tracks this distinct class —
`RESOURCE_SNAPSHOT`, `JobTypeResourceSnapshot`, reports "managed and unmanaged"
GPU consumers separately), or plain CUDA context/driver overhead that no
per-service `Footprint.VRAMBytes` attributes to any container. Net: "reserve
the whole card" as a literal budget is **unsatisfiable by construction** in
exactly the scenario #8248 describes — a node with some unaccounted-for VRAM
draw — and `Reserve` would refuse with an insufficient-VRAM error even though
evicting everything non-pinned is precisely what the user asked for and would
actually free real VRAM.

Two ways to fix this — **`Reserve`/`PlanPreemption` as they exist today
cannot express "exclusive," so one of these is a real, in-scope code change**,
not a zero-diff reuse:

- **(i) Budget = what's actually reclaimable.** Compute
  `requiredVRAMBytes := freeVRAM + Σ(non-pinned candidate VRAMBytes) - margin`
  before calling `Reserve`. This is satisfiable by construction (you're asking
  for exactly what evicting everything would produce), so no primitive change
  is needed — only a helper that sums candidates the same way
  `buildPreemptCandidates` already does. The tradeoff: it's tautological
  (`Reserve`'s own fit-check becomes a no-op once you've pre-computed the
  answer it would give), and it silently under-reserves by exactly the
  unmanaged/overhead VRAM `PlanPreemption` can't see — so "exclusive" ends up
  meaning "everything this primitive is aware of," with no way to know how
  much headroom that actually leaves.
- **(ii) An explicit exclusive mode.** Add an `Exclusive bool` to `Reserve`'s
  signature (or a `ReserveExclusive(ctx, jobID)` sibling) that skips the
  fit-check arithmetic entirely, evicts every non-pinned candidate
  unconditionally, and reports the *resulting* free VRAM in the returned
  `Reservation` rather than asking the caller to predict it up front. This is
  a real, small change to `internal/jobs/reservation.go` and (if the skip
  needs to happen inside `PlanPreemption` itself rather than just around it)
  possibly `internal/status/preempt.go` — in scope for Phase 2 (§6), not a
  reuse of the existing signature.

**Recommendation: (ii)** — it says what it means ("evict everything
non-pinned," not "evict enough to hit a number I precomputed by evicting
everything non-pinned"), and it reports ground truth (actual resulting free
VRAM) instead of a caller-side estimate. This is a §5 open question, not a
settled decision — flagging because it changes the primitive, which is the
one thing the issue framing (#832 "primitive already exists, just wire it
up") suggests might not be needed.

Either way, **pinned services are never touched — "exclusive" is
exclusive-of-non-pinned, not literally alone-on-the-card.** Under (i), if
pinned VRAM plus required headroom means the pre-computed budget still can't
be satisfied without a pinned service, `Reserve` refuses with `Blocked` naming
the holder(s) (existing #577 behavior). Under (ii), the exclusive mode should
apply the identical rule explicitly (never touch `Pinned` candidates) and
should still refuse-and-report rather than silently proceed with less than
the caller expects.

An optional `--vram <GB>` / `--context <N>` override should still be accepted
(a caller-supplied ceiling below "everything non-pinned") for a user who wants
headroom for something else running alongside — but the *default* should
match the issue's literal ask: free as much as this node can give a
non-pinned deploy.

Reporting max context window (the acceptance criterion "reports the resulting
max context window") is a **separate, harder, engine-specific problem** not
solved by either option above — freeing VRAM says nothing about how a given
engine trades VRAM for context length. Flagged as an open question (§5) and
likely its own follow-up issue, not solved in this design.

### 2.2 Relation to `citadel run`, `MODULE_SET`, and the durable stopped marker

- **`citadel run <service>` (`cmd/run.go`, `runSingleService`)** adds a
  service to the manifest (if absent) and starts it — it has no concept of
  eviction or VRAM budget at all. An exclusive-run command is not a variant of
  `citadel run`'s add-and-start behavior; it's a variant of `SERVICE_START`'s
  eviction behavior (§1.4) layered on top of a normal start. `citadel run`
  remains the right tool for "start this service, non-exclusively"; exclusive
  is additive, not a `run.go` flag that changes what starting means.
- **`MODULE_SET` is not the hook** (already established in §1.5): it sets a
  single module's desired state (`running`/`stopped`/`absent`) via the
  reconcile-engine-reuse trick in `internal/worker/module_set.go`, and has no
  VRAM-budget or eviction concept either — the platform's actual model-deploy
  path is `SERVICE_START` (`fabric_deploy_model`/`provision_model`), not
  `MODULE_SET`. §2.4's `exclusive` flag belongs on `SERVICE_START`'s payload,
  not on a `ModuleAssignment`.
- **Durable stopped markers interact exactly as `reservation.go` already
  documents for `Release`, worth restating here because it's the failure mode
  an operator will actually hit**: if a service an operator had *already*
  durably stopped (`desired_status: stopped`, e.g. via a prior `SERVICE_STOP`)
  gets swept up as an exclusive-run eviction candidate, `Reserve` tags it with
  `evicted_prior_status: stopped`. On `Release`, it is restarted only to
  immediately be re-marked `stopped` again — its `desired_status` is restored
  to what it was, not unconditionally cleared — so the net effect after
  release is correctly "still stopped," but the reservation's `Evicted` list
  (and the heartbeat's `GPUReservations`) will show it as evicted during the
  exclusive window even though nothing about its *desired* state actually
  changed. Worth surfacing in the CLI/MCP result text so an operator doesn't
  read "evicted: bonsai" as "bonsai was running and I stopped it" when it
  wasn't running at all.
- **Restore can be silently defeated in both directions by an unrelated
  platform action, and this needs to be stated plainly, not just implied by
  §1.1's citation.** `Execute()`'s `SERVICE_START`/`SERVICE_STOP` branches
  clear a service's reservation tag as a side effect of handling that
  explicit action (`reservation.go`: "operator intent is a stronger signal
  than a pending reservation"). So a routine platform `SERVICE_START` for a
  currently-evicted service — e.g. the backend redeploying it for an unrelated
  reason mid-exclusive-window — silently drops it out of the reservation:
  `Release` will no longer restore it, because its tag is gone. Independently,
  `module_control.go`'s own documented scope caveat means the reverse can also
  happen: if a node also runs the desired-state pull reconcile loop and the
  control plane's assignment for an evicted service is still "running," that
  loop can restart it *during* the exclusive window, undoing part of the
  eviction the reservation was supposed to hold. Net: exclusivity is durable
  against a reboot/crash of the reserving process (that's what `Reserve`'s
  manifest tagging buys), but **not** durable against a disagreeing control
  plane in either direction. This should be stated in the CLI/MCP tool
  documentation, not just this design doc.

### 2.3 Who calls `Reserve`/`Release`, and the crash-safety tension this creates

This is the central design decision, and it's harder than it looks because
the primitive's crash-safety story (§1.1) was built around a *single
long-running worker process* owning reservations, while "run a model right
now from my terminal" is naturally a **short-lived foreground CLI
invocation** — exactly the shape `citadel up` already uses (foreground,
blocks, SIGINT/SIGTERM-driven teardown via `defer`).

Three call-site shapes, and why each one does or doesn't inherit the
crash-safety guarantee `ReconcileOrphanedReservations` was built for:

**(a) Standalone foreground CLI process calls `Reserve`/`Release` directly
(mirrors `citadel module stop/start`'s pattern, §1.2).**
`citadel model run <model> --exclusive` reserves, starts the engine, blocks in
foreground reporting status, and on SIGINT/SIGTERM calls `Release` before
exiting — clean, and matches `citadel up`'s existing UX idiom exactly. The
gap: if this process is SIGKILLed (or the machine loses power) instead of
cleanly interrupted, the `evicted_by_job` tags are left in `citadel.yaml`
forever until *something* calls `ReconcileOrphanedReservations` — and that
function refuses to run unless `holdsWorkerLock` is true, which a bare CLI
invocation is not. On a node that also runs a long-uptime `citadel work`, the
next reconcile opportunity is **that worker's next restart**, which on a
healthy node could be weeks away. This is a real, node-going-dark-on-GPU risk,
not a theoretical one — it's exactly the class of incident CLAUDE.md's
"CC/worklock gap" paragraph in `reservation.go` was written to warn a future
`#8248` caller about, just via a third door (a standalone CLI process) instead
of the control-center's consume loop the doc explicitly names.

**(b) `Reserve`/`Release` invoked as job-dispatch actions inside the already-
running `citadel work` worker** (a new job type, or a flag on the existing
`SERVICE_START`/`SERVICE_STOP` job types — see §2.4). This is the shape the
primitive's own doc comments assume: it is created and torn down inside the
process that already calls `ReconcileOrphanedReservations` at boot, under the
lock the reconcile function requires. A SIGKILL of `citadel work` itself is
already the exact scenario `ReconcileOrphanedReservations` exists to clean up
on the worker's *next* start — no new gap, because it's the same worker, same
lock, same reconcile pass that already exists. The cost: "run exclusively"
becomes something you *ask the worker to do* rather than a directly-blocking
foreground command — a `citadel model run --exclusive` CLI invocation becomes
a thin client that dispatches a local job and polls/streams status, not a
process that itself holds the reservation.

**(c) A hybrid: the CLI process acquires `worklock` itself for its own
foreground lifetime** when no `citadel work` is currently running (refusing to
proceed, or falling back to shape (b), if one already holds the lock) —
mirroring how `citadel up --check` claims and releases a TUN interface for a
bounded lifetime. This gets (a)'s ergonomics with (b)'s safety story on a
dev/single-purpose node where nothing else is consuming jobs, at the cost of
`worklock`'s contract needing to support a second, CLI-scoped kind of holder
(today it's written for exactly one long-running worker) and a `--dry-run`/
`--expect-node`-style refuse-loudly UX when a worker already owns the lock.

**Leaning toward (b) as the primary, always-correct path, but this rests on a
premise that is NOT yet verified and should not be read as settled.**
Conceptually: `Reserve`/`Release` invoked from inside `ServiceHandler.
Execute()`'s existing `SERVICE_START`/`SERVICE_STOP` handling (see §2.4) would
run wherever the node's job dispatch runs (worker or, per `reservation.go`'s
documented latent gap, control-center — see §2.5 for why that gap needs
closing *before*, not after, this ships), inheriting the existing boot-time
reconcile for free. But this requires a way to hand the running worker a
*local* job to execute — and a repo-wide check while writing this doc found
**no existing local job-submission path into `internal/worker.Runner`**:
`citadel work` serves `GET /status`, `GET /worker` (read-only snapshots) and
`/agent/worker-restart` (kills the process), but nothing that enqueues a new
job from outside the worker's own `JobSource` (Redis/API). Building that (a
new local endpoint, or an in-process short-circuit that bypasses `JobSource`
entirely for CLI-originated jobs) is real, non-trivial plumbing, not a thin
wrapper — and it sits awkwardly against #8249's own acceptance criterion
("an agent on the node connects to local `citadel mcp` and can... run
qwen3.8 exclusively"), which implies this should work with *only* `citadel
mcp` running, not a separate `citadel work` process too. So (b) should be
read as "the safest shape if the plumbing gap turns out to be small," not a
recommendation to build on top of — §5 Q1 asks Jason to weigh that gap
against (a)'s weaker crash-safety story and (c)'s `worklock` contract change
before committing to one. If (b) is chosen and the plumbing turns out to be
more than "small," that alone may be reason to prefer (a) or (c) instead.

### 2.4 Wiring shape: new job type, or a flag on `SERVICE_START`/`SERVICE_STOP`?

Two options, both compatible with §2.3(b):

- **Option A — payload flag.** `SERVICE_START` gains `exclusive: "true"`
  (parsed alongside `vram_mb`/`vram_gb`). When set, `serviceStart` calls
  `h.Reserve(ctx, jobID, requiredVRAMBytes)` instead of the non-restoring
  `preemptForVRAM`, where `requiredVRAMBytes` is computed per §2.1 (total −
  margin, or the payload's explicit `vram_mb` if the caller wants a smaller
  budget). `jobID` is **not** the dispatched job's own `nexus.Job.ID`
  (opaque, not reproducible later) but a **deterministic function of the
  target service**, e.g. `"exclusive:" + serviceName` — so a later,
  independently-dispatched `SERVICE_STOP` (or a new restore action) can
  compute the *same* `jobID` string to call `Release` without needing to
  persist or pass around an opaque reservation handle. `SERVICE_STOP` (or a
  paired `release: "true"` flag on it) then calls `h.Release(ctx,
  "exclusive:"+serviceName)`.
- **Option B — new job type**, e.g. `MODEL_RUN_EXCLUSIVE` /
  `MODEL_RUN_RESTORE`. Cleaner separation (no overloaded meaning on
  `SERVICE_START`'s existing, already-complex payload — see
  `internal/jobs/service_payload.go`'s header comment on that payload's
  existing surface), but duplicates most of `serviceStart`'s model-resolution
  and engine-start logic, or has to call into it as a subroutine anyway.

**Recommendation: Option A**, because `serviceStart` already owns "resolve a
model, ensure disk cache, decide VRAM budget, start the compose stack" —
`Reserve` vs. `preemptForVRAM` is a one-line branch inside logic that already
exists, not a parallel implementation. The deterministic `jobID` convention
(`"exclusive:" + serviceName`) is the piece that makes Option A work cleanly
and should be treated as a stable, documented contract regardless of which
option is chosen — the local MCP tools (§3) and the CLI (§2.3) both need to
agree on it independently, without a shared in-memory handle.

### 2.5 Closing `reservation.go`'s documented latent gap is a prerequisite, not a follow-up

`reservation.go`'s own doc comment (`:269-291`) already names `#8248` as the
future caller that reopens the control-center/worklock hazard if wired
carelessly: a control-center-held reservation plus a later `citadel work`
startup (which legitimately `Acquire`s, since nobody holds the lock) would see
the tag, conclude "orphaned," and destructively restart a service the
still-live control-center job is using. **This design should not ship §2.3/
§2.4 without also closing that gap** — either make the control-center's own
consume loop `Acquire` the worklock too (simplest, and consistent with
`runWork`'s existing precondition), or extend the marker with owner identity
(pid + start time, mirroring `worklock`'s own stale-lock classification) so
`ReconcileOrphanedReservations` can distinguish "orphaned" from "owned by a
still-live sibling process." Scope this as an explicit, named phase (§6),
not an implicit assumption.

### 2.6 Operator escape hatch (recommended, not in either issue verbatim)

Regardless of §2.3's outcome, a reservation that is legitimately stuck (the
worker holding it is gone, or an explicit `Release` call itself keeps failing
per-service) needs a manual out that doesn't require editing `citadel.yaml` by
hand — the whole point of #8248 is "no manual manifest hacking." Two small,
low-risk additions:

- `citadel module reservations list` — thin wrapper over the already-shipped
  `ActiveReservations()`, printing job id → evicted service list (same data
  already on the heartbeat as `GPUReservations`, just surfaced locally too).
- `citadel module reservations release <jobID>` — thin wrapper over
  `Release(ctx, jobID)`, with the same `--dry-run`/`--expect-node` posture
  `module stop/start/restart` already has. Since `jobID` is the deterministic
  `"exclusive:<service>"` string from §2.4, an operator can reconstruct it
  from `citadel services`/`citadel.yaml` even without running `reservations
  list` first.

Both are pure additions to already-tested primitives (`ActiveReservations`,
`Release`) — no new decision logic, just a CLI surface. Cheap enough that they
should ship in the same phase as §2.4, not deferred.

## 3. Local MCP tools (#8249)

Three new tools, following `mcp_local.go`'s established pattern exactly
(struct-based `localMCPTool`, deps injected via `localMCPDeps`, tests stub
every side-effecting dependency):

```go
local_model_deploy   // pull (if needed) + start a model's engine, optionally with a VRAM budget
local_model_evict    // stop the engine currently serving a given model (module-stop, addressed by model name)
local_run_exclusive  // deploy + Reserve (§2.1's "whole card minus margin" semantics)
```

### 3.1 `local_model_deploy`

Thin wrapper over the existing `MODEL_CACHE_PULL` + `SERVICE_START` machinery,
run **in-process** (not by dispatching a job to a running worker — this tool's
whole premise, like the rest of #8249, is that the caller IS on the node and
should not need one running). Arguments: `model` (required), `engine`
(optional — inferred from the model catalog/name pattern the same way
`SERVICE_START`'s existing payload resolution does today), `vram_mb`
(optional). **Must** go through `captureStdout` (§1.3) — `serviceStart`'s
compose-up call hardwires `cmd.Stdout = os.Stdout` exactly like
`runModuleControl`'s does, and the model-pull subprocess
(`huggingface-cli`/`docker compose build` for a build-based engine like
bonsai) can emit megabytes of progress, so reuse `tailTruncate` too. The
`stdoutCaptureInFlight` guard (§1.3) already protects this automatically
since it is process-global — no new wiring needed there, just call
`captureStdout` like `runModuleControlCaptured` does.

### 3.2 `local_model_evict`

**Naming collision risk, flag for §5**: `MODEL_CACHE_EVICT` (`internal/worker/
job.go`) is an existing, differently-scoped job type that deletes weights
from the on-disk cache. `local_model_evict` as proposed here means "stop the
engine serving this model" (a model-name-addressed `local_module_stop`), not
"free disk space." An agent reading both tool names side-by-side (this one,
and a future `local_model_cache_evict` if one is ever added) would reasonably
guess wrong. Resolve the model name to a service via the same lookup
`gateway.ResolveChatModel`/`status.DiscoverLocalEngines` already do (so
`local_list_models`'s output is directly usable as this tool's input), then
delegate to the exact same `moduleControlFn` (`runModuleControlCaptured`)
`local_module_stop` already uses — this is a thin, model-name-addressed alias,
not a new stop mechanism.

### 3.3 `local_run_exclusive`

The MCP-shaped version of §2's `run --exclusive`, with one structural
difference that matters: **an MCP `tools/call` is a single request/response,
not a blocking session.** A CLI `run --exclusive` can hold the foreground and
release on Ctrl-C; an MCP tool call cannot — the JSON-RPC connection may
outlive or be shorter than the caller's intended "exclusive" window, and there
is no signal to catch. So `local_run_exclusive` must be **decoupled into two
independently-callable, paired actions**, matching §2.4's deterministic
`jobID` design exactly:

- `local_run_exclusive(model, vram_mb?)` — acquire + start. Internally: same
  §2.1 VRAM math, same `"exclusive:" + serviceName` jobID, calls `Reserve`
  then starts the engine (both via §2.3(b)'s job-dispatch path if a local
  worker is running — see the recommendation below — or the primitive
  directly if not). Returns the reservation's `Evicted` list and engine
  connection info in the result, so the calling agent can see the blast
  radius (what got stopped) without a separate dry-run step.
- `local_model_evict(model)` (§3.2, reused, not a new tool) — releases it:
  since the target service is now tagged `evicted_by_job`, resolving the
  model to that service and calling the same stop primitive should route
  through `Release("exclusive:"+serviceName)` rather than a bare
  `SERVICE_STOP`-style compose-down, restoring exactly what was evicted. This
  needs a small branch in the shared stop helper — "is this service currently
  tagged as an active exclusive reservation? If so, `Release` its jobID
  instead of a plain stop" — rather than a fourth tool.

**This reopens §2.3's crash-safety question in its sharpest form.** An agent
that calls `local_run_exclusive` and then never calls the release half (client
crash, connection drop, agent forgets) leaves the reservation held with **no
foreground process to catch a signal at all** — strictly worse than the CLI
case in §2.3(a). This is the strongest argument for §2.3's recommendation
(b): if `local_run_exclusive` dispatches into the same worker-owned job path
`SERVICE_START` uses, the *existing* boot-time `ReconcileOrphanedReservations`
and (once §2.5 is closed) the control-center path both already recover from
this without any new code specific to the MCP surface. If `local_run_exclusive`
instead calls `Reserve` directly, in-process, from the short-lived `citadel
mcp` process (mirroring how `local_module_stop` calls `runModuleControl`
directly today with no worklock) — an MCP client disconnecting mid-session
leaves an orphaned reservation with **no owning process left to reconcile it
at all**, since `citadel mcp` itself never calls `ReconcileOrphanedReservations`
and was never meant to be a job-consuming worker. §2.6's escape hatch becomes
load-bearing here, not optional, if this direct-call shape is chosen.

**Safety posture**: unlike the v1 tools (stop/start of an already-installed
service, read-only chat/files), these three tools can evict *every non-pinned
running service on the node* and write to disk (a model pull). #8249's whole
premise is that local authority = no confirmation gate (the caller is already
on the box) — this design doesn't propose adding one — but the tool
descriptions should say exactly what "exclusive" evicts (mirroring
`local_module_stop`'s existing precise wording about durability/no-op
semantics) and `local_run_exclusive`'s result should always include the
`Evicted` list, never just an opaque success. A `dry_run` argument (reusing
`PlanPreemption`'s existing purity — it's a pure decision function with no
I/O, so previewing the stop-list costs nothing) is a natural, cheap addition
mirroring the CLI's `--dry-run`.

## 4. Cross-repo contract (aceteam-side, NOT done here)

Everything below is a follow-up in the `aceteam` repo, listed so the citadel-
side work has a clear contract to build against:

- **`SERVICE_START` payload gains `exclusive: "true"`** (Option A, §2.4),
  alongside the already-defined-but-unsent `vram_mb`/`vram_gb` — the backend's
  `fabric_deploy_model`/`provision_model` MCP tool handlers need a new
  `exclusive: bool` parameter threaded through to this payload. Today those
  handlers dispatch `SERVICE_START` with just `{service, model}` (§1.4) — this
  is the same "not yet forwarded" gap #577's `vram_mb` already has, extended
  by one more field.
- **A restore/evict dispatch** for the "undeploy" half — either a
  `SERVICE_STOP` with a `release_reservation: "true"` flag, or a distinct
  action the `/fabric` UI's evict button sends. Must compute (or receive) the
  same `"exclusive:" + serviceName` jobID convention (§2.4) — this needs to be
  a **documented, versioned string contract** between the two repos, not an
  implementation detail either side can silently rename.
- **`node_module_set`/`fabric_node_module_set`** (existing REMOTE MCP tools,
  gated on the `node:modules` scope, aceteam#8246) are a separate surface from
  local MCP — #8249's whole point is that a caller *on* the node should not
  need `node:modules` at all. No change needed there for this design, but the
  backend MCP bridge should not conflate the two: a remote `node_module_set`
  call and a local `local_run_exclusive` call should converge on the *same*
  on-node primitive (§2.3's recommendation makes this automatic if both funnel
  through `SERVICE_START`/`SERVICE_STOP` job dispatch) without needing two
  independent backend code paths to stay in sync.
- **`/fabric` model evict/disable/manage UI (#8247)** — surfacing "run
  exclusively" as a toggle and "evicted by reservation X" as a visible node
  state (the heartbeat already carries `GPUReservations`, §1.1, so this is a
  read-only UI addition once the citadel-side heartbeat field exists — it
  already does).
- **aceteam#8246** (`node:modules` grant-request flow) is orthogonal to this
  design — it gates the *remote* MCP path, not the local one. No dependency
  either direction, but worth sequencing awareness: an agent with an
  `node:modules` grant and an agent using local MCP on-box should both end up
  driving the same `SERVICE_START`/`Reserve` primitive, per the point above.

## 5. Open questions for Jason

1. **§2.3 — is `run --exclusive` primarily a CLI command backed by a running
   worker (job-dispatch, recommendation (b)), a standalone CLI process that
   owns `worklock` for its own lifetime (recommendation (c)), or does it need
   to support both** (worker-backed when one's running, self-contained
   fallback when not)? This is the single decision everything else in this
   doc hangs off of.
2. **Is §2.5 (closing the control-center/worklock reservation gap) a hard
   prerequisite before shipping `run --exclusive` at all**, or is
   control-center's consume loop rare/dev-only enough to accept the
   documented risk and fix it as a fast-follow? (The existing doc comment
   treats it as latent-but-real; this design would make it live.)
3. **CLI surface naming**: `citadel model run <model> --exclusive` (matches
   the issue's literal acceptance criteria, introduces a new `model`
   subcommand namespace) vs. extending the existing `citadel run <service>
   --exclusive` (`cmd/run.go`, no new namespace, but "service" and "model"
   aren't quite the same axis — `run.go` targets a service by name, while
   exclusivity is really about a *model* the service should load). Does this
   want a new `citadel model` command group (which could also host §2.6's
   `reservations list/release`, or should those live under `citadel module`
   instead, alongside `module stop/start/restart`)?
4. **§2.1's default VRAM budget** ("whole card minus a fixed margin") —
   confirm the margin (1 GiB was used as a placeholder above, matching
   informal precedent elsewhere) and whether a caller-supplied `--vram`/
   `--context` override is in scope for v1 or a fast-follow.
5. **Reported max context window** (#8248's acceptance criterion) has no
   existing engine-agnostic formula anywhere in this codebase today — is this
   in scope for the same PR as the reserve/deploy wiring, or is it explicitly
   a separate follow-up issue (recommended, given it likely needs
   per-engine-family logic, not a `Reserve`-level change)?
6. **§3.2 naming collision** — is `local_model_evict` acceptable given the
   existing, differently-scoped `MODEL_CACHE_EVICT` job type, or should it be
   named `local_model_stop`/`local_run_restore` to avoid an agent confusing
   "stop serving" with "delete from disk cache"?
7. **§3.3's decoupled two-call MCP design** (`local_run_exclusive` +
   `local_model_evict` as the release half, rather than a single tool with
   session semantics) — confirm this is the right shape, or whether a
   time-boxed auto-release (a TTL on the reservation, expired and swept by a
   periodic check) is preferred despite adding new state `Reserve`/`Release`
   don't have today.

## 6. Phased breakdown

| Phase | Repo | Scope | Depends on |
|---|---|---|---|
| 0 (done) | citadel | #851 primitive, #846 module commands, #8249 v1 local MCP (module control + chat + files) | — |
| 1 | citadel | Close reservation.go's control-center/worklock gap (§2.5) | Phase 0 |
| 2 | citadel | Wire `Reserve`/`Release` into `SERVICE_START`/`SERVICE_STOP` behind `exclusive`/deterministic jobID (§2.4); local worker HTTP endpoint for job submission if §5 Q1 picks (b) | Phase 1, §5 Q1 |
| 3 | citadel | `citadel model run <model> --exclusive` CLI (or chosen surface per §5 Q3) + `citadel module reservations list/release` escape hatch (§2.6) | Phase 2 |
| 4 | citadel | `local_model_deploy`/`local_model_evict`/`local_run_exclusive` MCP tools (§3), reusing Phase 2's primitive | Phase 2 |
| 5 | aceteam | `exclusive`/restore payload fields on `fabric_deploy_model`/`provision_model` MCP dispatch; `/fabric` UI toggle + reservation state (#8247) | Phase 2 (contract), independent of Phases 3-4 |
| 6 (stretch) | citadel | Reported max context window (§5 Q5) — likely its own issue | Phase 2 |

Phases 3 and 4 can run in parallel once Phase 2 lands (CLI and MCP are both
thin clients over the same primitive per §2.3(b)'s recommendation). Phase 5 is
independent aceteam-side work gated only on Phase 2's payload contract being
documented (§4), not on Phases 3/4 shipping.
