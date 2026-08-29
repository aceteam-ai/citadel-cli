# Design: Node Execution Model — Claim/Execute Decoupling + Inference Slot Queue (citadel#908 / aceteam#8254)

## Context

Two symptoms, one root cause: **a citadel node's job-fetch loop and its job-execution
capacity are the same thing.** `Runner.Run` (`internal/worker/runner.go`) calls
`source.Next()` exactly once per iteration, and for any job type not already on a
dedicated async lane, `r.processJob(ctx, job)` runs *inline*, in the same goroutine
that would otherwise call `source.Next()` again. Two different symptoms fall out of
that single fact, depending on which axis is missing:

- **citadel-cli#908**: a `SERVICE_START` / model pull / `MODULE_SET` / build / etc.
  (`unboundedJobTypes`, `internal/worker/deadline.go:59`) runs inline and can take
  minutes to hours. While it runs, `source.Next()` is not called, so the node stops
  claiming *anything else* — including a `FILE_READ_BYTES` in the same
  `file_parse` pipeline. The backend's short claim-ack window (~12s,
  aceteam#7318) elapses and it fast-fails the dispatch as if the node were
  unreachable, even though the node is healthy and simply busy.
- **aceteam#8254**: `llm_inference` jobs (`gpuBoundJobTypes`,
  `internal/worker/gpu_tracker.go:94`) already run on an async lane (#903 Stage
  1) and don't block the fetch loop. But admission past the GPU-slot count
  (`internal/worker/gpu_tracker.go`) is a single `Acquire()`/`Nack` decision with
  no queue — see §1c below for why that Nack, under concurrent load, degrades
  into real job loss (not just latency), which is the "lost, not queued" half of
  the bug report.

Both are the same underlying gap: **there is a fetch/claim step and an execute step,
and the node conflates them.** This doc designs one coherent execution model —
"claim always advances the fetch loop; execution is separately admitted onto a
bounded, observable lane" — with two lane instances: a general unbounded-job lane
(#908) and an inference admission queue (#8254). §5 gives the concrete reason these
can ship independently and in what order.

## 1. Current state

### 1a. Dispatch paths today (`internal/worker/runner.go` `Run`, lines ~254-397)

Per job fetched from `source.Next()`, in order of first match:

1. **Long-session lane** (`longSessionJobTypes`, `deadline.go:49` —
   `MEETING_JOIN`, `COBROWSE`): always dispatched via `go func(){ r.processJob(...) }()`,
   unconditionally, never touching the concurrency semaphore. Shipped in #489.
2. **GPU-bound async lane** (`gpuBoundAsync := needsGPUSlot(job.Type) && r.gpuTracker != nil`,
   `runner.go:377`): same unconditional-goroutine treatment, but **only when a
   `GPUTracker` exists** (i.e. the node has a discrete GPU — `cmd/work.go:2383-2386`
   constructs one only when `platform.GetGPUCountSimple() > 0`). Shipped in #903
   Stage 1 (PR #911). On a GPU-less node serving GPU-typed inference (native
   ollama, Apple Silicon), this falls through to case 4 — deliberately, since the
   sequential loop is the *only* thing throttling concurrent dispatch to that
   node's single serving engine (`runner.go:352-368`).
3. **Concurrency-pool lane** (`concurrency > 1`): semaphore-gated
   (`sem <- struct{}{}` then goroutine), sized by `--max-concurrency`
   (`cmd/work.go:2381`, default = GPU count, else 1).
4. **Inline** (everything else, including every `unboundedJobTypes` member on the
   default `maxConcurrency=1` node): `r.processJob(ctx, job)` called directly in
   the `select` body. This is case 4 that #908 is about.

`processJob` (`runner.go:416-661`) is one function covering: target-node filter +
Ack-and-skip, `stream.WriteClaimed()` (the aceteam#6000 claim-ack the backend's
short window waits on), cancellation check, handler lookup, GPU-slot
acquire/Nack (#825, gated by `needsGPUSlot`), `resolveJobTimeout` +
`executeWithDeadline` (or synchronous `handler.Execute` when
`unboundedJobTypes` returns `ok=false`, `deadline.go:83`), and terminal
Ack/Nack/Fail + stream publish. **`WriteClaimed` and `handler.Execute` are the
same function call stack, with nothing between them but a target-node check and
a cancellation check** — there is no existing seam that lets "claim" happen
without also blocking on "execute."

### 1b. Manifest and lockfile write paths have no lock

`ServiceHandler` (`internal/jobs/service_handler.go`) is the SERVICE_START/STOP
handler. Every mutating operation on `citadel.yaml` is a **read-modify-write of
the whole file**, not a per-service patch:

- `addServiceToManifestFile` (line 1405): `os.ReadFile` → `yaml.Unmarshal` into a
  `yaml.Node` tree → mutate → `yaml.Marshal` the *entire document* → write.
- `setDesiredStatusInManifestFile` (line 1488): identical shape.
- `setEvictedMarkersInManifestFile` (line 1586): identical shape.
- All three funnel their final write through `writeManifestBytes` (line 1052),
  which is a plain `os.WriteFile(path, data, 0600)` unless a test override is
  injected. **No mutex, no file lock, no CAS.**

`internal/catalog/lockfile.go` mirrors this exactly for `modules.lock`:
`UpsertLockEntry` (line 94) and `DeleteLockEntry` (line 129) are both
`LoadLockfile()` → mutate slice → `yaml.Marshal` → `os.WriteFile`, called from
`citadel module install/update`, `citadel catalog install`, and the reconcile
engine's own `Install`/`Uninstall` (`cmd/module_ops.go`) — which `MODULE_SET`
(`internal/worker/module_set.go`) drives.

**Why this has never mattered:** the sequential consume loop (case 4 above) is
today's *implicit* lock — only one `unboundedJobTypes` job (the population that
writes these files) executes at a time, process-wide, because nothing else can
run concurrently with it. Any change that lets two such jobs execute
concurrently reopens a classic read-modify-write race: two goroutines both
`ReadFile` the pre-change content, each computes its own mutated tree, and
whichever `WriteFile` lands second silently discards the first goroutine's
change. This is a **whole-file** race, not a per-service one — two goroutines
targeting *different* service names still read and write the *same*
`citadel.yaml`, so the race exists even when they never touch the same service
entry. §2b returns to this.

**What is *not* the right lock for this:** `internal/worklock`
(`internal/worklock/worklock.go`) is a **cross-process** OS advisory `flock`
guarding against a second `citadel work` for the same node (#443/#435) — it says
nothing about two goroutines inside one already-running worker process. It is
the wrong layer for this problem entirely; do not reach for it here.

### 1c. GPU-slot admission is a bare Nack, and Nack is not free

`processJob`'s GPU-slot acquire (`runner.go:502-545`) is a single non-blocking
`r.gpuTracker.Acquire()`. On failure it Nacks
(`r.source.Nack(ctx, job, err)`, `runner.go:536`) with **no terminal stream
event** (deliberate — see the #559 note inline at `runner.go:528-535`: this Nack
is meant to be a transparent, invisible retry).

`RedisSource.Nack` / `WSSource.Nack` (`internal/worker/redis_source.go:338`,
`internal/worker/ws_source.go:252`) do **not** ack the message — it stays
unacknowledged in the Redis consumer group's Pending Entries List (PEL) and is
redelivered by Redis's own reclaim mechanism after the group's idle timeout.
Each redelivery increments the message's Redis-tracked delivery count, read via
`XPendingExt` (`internal/redis/client.go:235`,`:346`). **`NewClient`
(`internal/redis/client.go:75-90`) defaults `MaxAttempts` to 3.** `willRetry`
(`runner.go:685-690`) documents that `RedisSource` uses this exact delivery
count to decide, on the *next* read, whether to hand the message to
`processJob` again or move it straight to the DLQ
(`MoveToDLQ`/`MoveToDLQFromQueue`, `internal/redis/client.go:256`,`:367`).

**What this establishes, and what it does not.** On `RedisSource`, with the
default `MaxAttempts=3` (`internal/redis/client.go:83`), sustained contention
(a given job Nacked across 3 redeliveries before a slot ever frees for it)
**does** drive it to the DLQ per `willRetry`'s documented logic
(`runner.go:685-690`): a real, permanent drop.

But `RedisSource` is the direct-Redis debug path, not what a production node
runs. Per this repo's own CLAUDE.md ("Worker Mode" section) the default
production path is the AceTeam Redis **API proxy** (`WSSource`/`APISource`),
and `willRetry`'s comment is explicit that `MaxAttempts == 0` there — **"the
AceTeam Redis API proxy does not expose a per-message delivery count to the
node."** So on the path a real node (e.g. node 1297, where aceteam#8254 was
observed) actually runs, whether repeated Nack drives a job to the DLQ is
decided **server-side, by the API proxy, and is not established by anything in
this repo** — do not carry the RedisSource-specific DLQ mechanism forward as
"the" explanation for aceteam#8254's reported drops without confirming the
proxy's equivalent behavior.

**What *is* established, and holds identically on every source:** `Nack` never
publishes a terminal stream event (`runner.go:528-535`, the #559 note, applies
regardless of source). Under the contention this issue reports (~10 concurrent
`llm_inference` jobs against a single-GPU node's one `gpuTracker` slot), most
of those jobs are repeatedly Nacked and **never produce a terminal event on
`stream:v1:{jobId}`** — so the caller waiting on that stream (the aceteam
backend / `inference_chat` MCP tool) times out and reports failure (504), by
the exact mechanism #559 already documents, just reached via GPU contention
instead of a publish failure. This is sufficient, on its own, to justify §3's
queue-on-full redesign without depending on the unverified DLQ claim above —
whether the underlying job is *also* silently dropped by the API proxy is a
real open question (see §6) but not load-bearing for the fix.

### 1d. WorkerState conflates "claimed" and "executing" (`internal/worker/state.go`)

`RecordJobReceived()` / `RecordJobDone()` bracket the *entire* `processJob` call
— claim, cancellation check, GPU-slot wait, and handler execution are all one
undifferentiated `inFlight` count. `oldestInFlightUnixNano`
(`state.go:53`, surfaced as `WorkerSnapshot.OldestInFlightAt`) is the timestamp
the self-heal STUCK check (`selfheal.go:194`) measures against — "how long has
*something* been continuously in flight," already correctly decoupled from
`LastJobAt` by the #489 review fix (a stream of short jobs finishing beside a
long one must not reset it). But it has no notion of *why* a job has been
in-flight a long time: "waiting its turn" and "a handler is actually hung" are
indistinguishable in the current model. This is exactly the ambiguity
prerequisite (c) in #908 flags.

### 1e. What already composes correctly and must not regress

- The per-job watchdog (`executeWithDeadline`, `deadline.go:203`) already starts
  its `context.WithTimeout` at the moment it is called — which is already
  *inside* `processJob`, after claim/cancellation/GPU-slot-acquire. Any refactor
  that moves lane-admission *before* the call to `processJob`/its successor
  preserves "deadline starts at execution" for free; moving it *after* would
  break it. This is prerequisite (d) — already true structurally, and the
  design below is explicit about preserving the ordering that makes it true.
- `needsGPUSlot`/`gpuBoundJobTypes` (#825) and `unboundedJobTypes`/
  `longSessionJobTypes` (#548/#489) are disjoint, already-tested job-type
  partitions (`TestGPUBoundJobTypes`, `gpu_tracker_test.go`). The design below
  adds a third partition (general unbounded-lane membership = `unboundedJobTypes`
  itself) rather than inventing a new classification scheme.

## 2. Claim/execute decoupling (#908)

### 2a. Split `processJob` into `claimJob` + `executeJob`

Refactor (mechanical, not a new concurrency primitive) `processJob` into two
functions:

- **`claimJob(ctx, job) (proceed bool, stream StreamWriter)`** — runs
  **synchronously, in the fetch-loop goroutine**, immediately after
  `source.Next()` returns and before any queueing decision. Contains exactly
  what happens today before the handler lookup: the target-node filter
  (Ack-and-return if not ours), `stream.WriteClaimed()`, and the
  `IsJobCancelled` check (WriteCancelled-and-return if so). Nothing in this
  function blocks on a lane, a semaphore, or a handler.
- **`executeJob(ctx, job, stream)`** — everything after that in today's
  `processJob`: handler lookup, GPU-slot acquire (unchanged, #825), timeout
  resolution + `executeWithDeadline`/`handler.Execute`, terminal
  Ack/Nack/Fail + stream publish. This is what a lane admits and runs in a
  goroutine.

**Why this alone fixes the reported #908 symptom, before any lane/queue exists
at all:** `WriteClaimed` — the event the backend's ~12s window is actually
waiting on — now fires the instant a job is fetched, regardless of how long
`executeJob` ends up taking or how long it waits for a lane slot. The fetch
loop's only remaining job is to call `claimJob` (fast, no I/O beyond one
best-effort pub/sub publish) and then hand off to a lane. It is free to call
`source.Next()` again immediately.

### 2b. Lane abstraction: bounded admission, never a blocking fetch loop

Generalize the pattern already used for the long-session and GPU-bound lanes
into a named primitive:

```go
type lane struct {
    admit chan struct{} // buffered, capacity = maxQueued (admission bound)
    exec  chan struct{} // buffered, capacity = execConcurrency (execution bound)
}
```

For a claimed job whose type belongs to this lane, the fetch loop does:

```go
select {
case l.admit <- struct{}{}:
    r.state.RecordJobClaimed() // queued++ (see §2d for the full counter lifecycle)
    go func() {
        defer func() { <-l.admit }()
        l.exec <- struct{}{}             // may block *this goroutine*, never the fetch loop
        defer func() { <-l.exec }()
        r.executeJob(ctx, job, stream)   // transitions queued -> executing -> done; see §2d
    }()
default:
    // lane saturated at the admission bound: Nack now (transparent retry),
    // same shape as today's GPU-slot-full Nack (runner.go:528-535) --
    // non-terminal, no stream publish. Because claimJob already ran (§2a),
    // `queued` was never incremented for this job -- there is nothing to
    // undo here. See the accepted-tradeoff note at the end of §2c: this IS a
    // new shape (claimed, then Nacked) for job types that could not
    // previously reach it.
    r.source.Nack(ctx, job, errLaneSaturated)
}
```

**The fetch loop never blocks.** It either succeeds a non-blocking channel send
(bounded by `admit`'s capacity) and immediately loops back to `source.Next()`,
or it Nacks immediately and loops back. All waiting — for a free `exec` slot —
happens inside the spawned goroutine, off the fetch-loop's critical path. This
is what makes claim-ack latency independent of execution backlog, on every lane,
uniformly.

**Every exit path and its `WorkerState` counter effect, enumerated exhaustively
(this is the fix for the leak a partial accounting would otherwise create —
see §2d):**

| Path | Counters touched | Terminal event |
|---|---|---|
| (i) target-node filter says "not mine" (`claimJob`) | none | Ack, no stream publish (unchanged from today) |
| (ii) cancelled before processing (`claimJob`) | none | WriteCancelled, Ack |
| (iii) `admit` full (this `select`'s `default`) | none — `RecordJobClaimed` has not yet run for this job | Nack, no stream publish |
| (iv) admitted, then `executeJob` runs | `RecordJobClaimed` (queued++) at admission, `RecordJobExecuting` (queued--, executing++) once the `exec` slot is actually acquired, `RecordJobDone` (executing--) on every `executeJob` return | per `executeJob`'s existing terminal logic (unchanged) |

Path (iii) is why `RecordJobClaimed` must be called **inside** the `case
l.admit <- struct{}{}:` branch, not in `claimJob` itself (§2a) or anywhere
else that runs unconditionally on every claimed job — a job rejected at the
admission bound was claimed (`WriteClaimed` published) but must never
increment `queued`, or nothing would ever decrement it and `InFlight` would
never return to 0 (permanently disarming self-heal's STALL check, and
permanently drifting `Processed`/`Failed`). This is the precise misfire
prerequisite (c) warns about, and the reason the counter lifecycle is spelled
out exhaustively here rather than left implicit.

**This `l.exec <- struct{}{}` acquire is unbounded-wait** (no timeout) — the
right shape for the general unbounded-job lane (§2c), which has no notion of
"give up and answer busy." The inference lane (§3) needs the opposite —
a *bounded* wait that, on expiry, produces a real terminal response — and for
that reason moves the `exec`-acquire out of this wrapper and into
`executeJob` itself, so the timeout path can reuse `executeJob`'s own terminal
Ack/Nack/Fail logic instead of a second implementation here. See §3a for the
exact placement and why.

This directly answers prerequisite **(b)**: fetch-ahead depth is bounded by
`admit`'s capacity (a small constant per lane, e.g. 4-8 — tunable, not
load-bearing to get exactly right at v1), and the failure mode at the bound is
an *immediate* Nack (no PEL growth beyond `admit`'s capacity, since a rejected
job was never handed to a goroutine and the Nack is exactly today's established,
tested pattern for "temporarily can't admit this"). This is a smaller, more
predictable PEL footprint than today's inline model produces in the aceteam#8254
GPU-contention case, not a new risk.

### 2c. The general unbounded-job lane (#908's actual fix)

Instantiate one `lane` for `unboundedJobTypes` membership, with:

- `admit` capacity: small (e.g. 8) — bounds how many `SERVICE_START`/pull/build/
  `MODULE_SET` jobs can be claimed-but-not-yet-running at once.
- `exec` capacity: **1**, at v1, with no manifest/lockfile locking required.

This is the load-bearing design choice: **execution concurrency for this lane
starts at exactly 1, which is the same "at most one unbounded job runs at a
time" guarantee the inline dispatch model already provided.** Nothing about the
write-path race in §1b changes — the implicit lock (mutual exclusion via
"only one goroutine ever runs this code") is preserved exactly, just relocated
from "the fetch loop's own call stack" to "this lane's `exec` semaphore." The
*only* thing that changes is that the fetch loop is no longer blocked while
that one job runs, because claim (§2a) already happened and `source.Next()` is
free to keep going.

**This closes #908's reported symptom with zero new locking risk.** Raising
`exec` capacity above 1 for this lane — letting two unrelated `SERVICE_START`s
run truly concurrently — is a distinct, optional improvement gated on §2e's
locking work, not a requirement to fix the issue as reported.

**Accepted tradeoff, stated explicitly rather than left for a reviewer to
find:** under this design, an unbounded-type job can now reach the "claimed,
then Nacked with no terminal event" shape (§2b's path (iii)) — today that
shape is only reachable for `gpuBoundJobTypes` on a tracker node (the existing
#825/#559 GPU-slot-full Nack). This is a deliberate consequence of never
blocking the fetch loop (§2b), not an oversight: the backend already has to
tolerate this shape for GPU-bound jobs, and `admit`'s capacity (§2b, e.g. 8)
should be sized so hitting it is rare in practice — it only triggers when 8+
unbounded jobs are claimed while the single `exec` slot is still busy with an
earlier one, a much higher bar than #908's reported single-long-job case.

### 2d. `WorkerState`: queued vs executing (prerequisite c)

Replace the single conflated in-flight bracket with two explicit phases, kept
strictly additive to the existing `/worker` JSON contract (per this codebase's
own convention for every prior heartbeat extension — see e.g. `SwapActivity`,
`GPUReservations` in CLAUDE.md — new fields are added, existing ones are never
removed or repurposed):

- `RecordJobClaimed()` — called exactly once per claimed-and-admitted job,
  from lane admission (§2b's `case l.admit <- struct{}{}:` branch — **not**
  from `claimJob` itself, since a job rejected at the admission bound, §2b
  path (iii), must never increment this; see §2b's exit-path table).
  Increments a new `queued int64` counter; stamps `lastJobUnixNano` (unchanged
  semantics from today).
- `RecordJobExecuting()` — called at the top of `executeJob`, **after** the
  lane's `exec` slot is actually acquired (i.e. exactly where `processJob`
  begins today). Decrements `queued`, increments a new `executing int64`
  counter, and on the 0→1 transition of `executing` stamps a new
  `oldestExecutingUnixNano` — a NEW, second timestamp alongside the existing
  `oldestInFlightUnixNano`, not a replacement for it (see below).
- `RecordJobDone(ok)` — unchanged signature and unchanged call site
  (`executeJob`'s existing terminal paths), decrements `executing`. A job
  rejected at the admission bound (§2b path (iii)) never called
  `RecordJobClaimed` and so needs no corresponding decrement here — see §2b's
  exit-path table for the complete accounting.

`WorkerSnapshot.InFlight` stays defined as `queued + executing` — numerically
identical to today's combined count for any job that reaches execution, so
every existing consumer of the `/worker`/`/status` JSON (the platform,
`citadel services`) is unaffected. New fields `Queued`, `Executing`, and
`OldestExecutingAt` are purely additive; **`OldestInFlightAt` stays on the
wire, computed exactly as it is today** (the 0→1 transition of `queued +
executing`) — nothing removes it, only self-heal's own internal read target
changes (below).

**Self-heal update** (`selfheal.go`):
- **STALL** (`check`, line 168-178): keep reading the combined
  `InFlight == 0` gate (`queued + executing == 0`), unchanged. Under this
  design the fetch loop polls continuously regardless of lane saturation (that
  is the entire point of §2a), so this check becomes less load-bearing than
  before but stays correct as a backstop against a genuinely dead loop (a bug
  unrelated to lane saturation).
- **STUCK** (line 194-197): change to read the NEW `OldestExecutingAt` instead
  of `OldestInFlightAt`. This is the actual fix for the misfire prerequisite
  (c) warns about: a job that has been legitimately **queued** for hours
  behind a large model pull (lane `exec` capacity 1, per §2c) must not look
  like a wedged handler — only a job that has been **executing** (inside
  `handler.Execute`) past the ceiling should. `TestLivenessMonitorCheck_
  StuckUsesOldestInFlightNotLastJob` already pins the "measure the right
  timestamp" pattern (#489) against the existing field; add a companion test
  asserting a long *queued* wait does not trip STUCK (reading the new field)
  while a long *executing* wait still does.

### 2e. Locking, for when unbounded-lane `exec` capacity needs to exceed 1

Not required to close #908 (see §2c), but designed now since the issue asks for
it as a prerequisite for the *general* case, and because §5 treats it as an
explicit, separately-shippable phase.

**Two write surfaces, each needing its own lock, at file granularity —
not per-service:**

1. `citadel.yaml` (all four `internal/jobs/service_handler.go` yaml.Node-surgery
   functions: `addServiceToManifestFile`, `setDesiredStatusInManifestFile`,
   `setEvictedMarkersInManifestFile`, plus any future one following the same
   pattern).
2. `modules.lock` (`internal/catalog/lockfile.go`'s `UpsertLockEntry`,
   `DeleteLockEntry`).

**Tradeoff analysis, as the issue asks for explicitly:**

- **Per-manifest-service mutex (keyed by service name) — analyzed and
  rejected as the primary mechanism.** It looks natural (`SERVICE_START vllm`
  and `SERVICE_START ollama` "shouldn't" conflict), but every writer here does
  a **whole-document** read-modify-write: it reads all of `citadel.yaml`,
  mutates one `yaml.Node` inside the tree, and re-marshals and writes the
  *entire* document. Two goroutines targeting *different* service names still
  race on the same underlying file — goroutine A's `ReadFile` doesn't see
  goroutine B's not-yet-written change, so A's write silently clobbers B's.
  A per-service mutex does not serialize this at all; it would give false
  confidence. The actual conflict granularity is the **file**, not the
  service entry.
- **A single in-process `sync.Mutex` per file (recommended).** One
  `manifestMu sync.Mutex` in `internal/jobs` guarding the full
  read-modify-write sequence (not just the final `writeManifestBytes` call —
  the race window starts at `ReadFile`) in all `citadel.yaml` writers; one
  `lockfileMu sync.Mutex` in `internal/catalog` guarding `modules.lock`'s
  read-modify-write in `UpsertLockEntry`/`DeleteLockEntry`. Simple, easy to
  retrofit onto the existing functions without changing their signatures,
  matches how every other shared-mutable-state guard in this codebase is done
  (`GPUTracker.mu`, `WorkerState.mu`/`inFlightMu`). Risk: a future write path
  added to either file that forgets to take the lock reopens the race
  silently — mitigate by keeping all read-modify-write logic behind the two
  existing narrow entry points (`writeManifestBytes` already centralizes the
  *write*; extend it, or add a sibling, to also own the *lock* around
  load+mutate+write, so a new caller can't easily bypass it).
- **Alternative considered: a single-writer actor goroutine** (all manifest
  mutations funneled through a channel to one dedicated goroutine) instead of
  a mutex. Marginally safer against "forgot to lock" (there's only one place
  that can write at all), at the cost of every caller becoming
  request/response over a channel instead of a direct function call — a much
  larger refactor for marginal benefit given the mutex's blast radius here is
  already small (four functions in one file, two in another). **Not
  recommended for v1**; worth reconsidering only if the mutex approach proves
  to leak call sites in practice.
- **Lock ordering:** if an operation ever needs both locks (e.g. an install
  path that writes the manifest and the lockfile in one job), always acquire
  `manifestMu` before `lockfileMu` and document it at both declarations, to
  rule out a deadlock from a hypothetical future caller that acquires them in
  the opposite order.
- **Not in scope for this fix, worth flagging separately:** neither write is
  currently atomic against a crash mid-write (`os.WriteFile` can leave a
  truncated file if the process dies between `open` and `close`). This has
  been latent since before this design and is orthogonal to the concurrency
  fix — write-to-temp-then-rename would close it, but raising unbounded-lane
  concurrency does increase how often these writes happen, which raises the
  exposure. Worth a follow-up issue, not bundled into this one.
- **Service-scoped sidecar files need no new locking.** `persistServiceModel`
  and `persistServiceTrustRemoteCode` write a *per-service* `<name>.env`
  file — a genuinely different file per service, so two `SERVICE_START`s for
  *different* services never race on the same file today. Two concurrent
  `SERVICE_START`s for the *same* service racing on the *same* `<name>.env`
  is a real but low-severity race (worst case: last-write-wins on a config
  about to be re-read momentarily, not corruption spanning unrelated
  services) — out of scope for this design; note it as a candidate for a
  lightweight per-service-name mutex if it ever proves to matter in practice.

## 3. Inference node-slot queue (#8254)

### 3a. What changes: Nack-on-full becomes queue-on-full

Give `gpuBoundJobTypes` their own `lane` (§2b's primitive), sized:

- `exec` capacity = `gpuTracker.Total()` (unchanged from today's GPU-slot
  count — this design does not claim the GPU can serve more concurrent
  requests than it can; that is a model/engine-level batching question,
  explicitly out of scope here).
- `admit` capacity = a small multiple of `exec` capacity (e.g. 2-3x), plus a
  **bounded queue wait**, `inferenceQueueWait` (default a few minutes,
  configurable) — the maximum time an admitted-but-not-yet-executing job may
  sit waiting for an `exec` slot before the lane gives up on it.

**Concretely, the bounded wait lives INSIDE `executeJob`, as its very first
step, before handler lookup — not in a separate branch in the lane-admission
wrapper.** This is deliberate: `executeJob`'s existing tail (terminal
Ack/Nack/Fail + stream publish, unchanged from today's `processJob`) is the
ONLY place that implements the terminal-publish-then-Ack sequence, and it must
stay the only place — duplicating it in a second location is exactly how a
"queued" job would end up Acked with no terminal event (reintroducing the
#559 bug this design exists to avoid). So for `gpuBoundJobTypes` specifically,
`executeJob` gains a pre-step:

```go
func (r *Runner) executeJob(ctx context.Context, job *Job, stream StreamWriter) {
    if lane := laneFor(job.Type); lane != nil && lane.hasExecWait {
        select {
        case lane.exec <- struct{}{}:
            defer func() { <-lane.exec }()
        case <-time.After(inferenceQueueWait):
            // Queue-wait exceeded: synthesize the SAME structured JobResult
            // handler.Execute would return for "still warming" (§3b) and fall
            // straight into the EXISTING success-path terminal logic below —
            // no separate Ack/publish implementation.
            result := h.warming(job.Payload["model"], 0, inferenceQueueWaitHint, "queue")
            r.finishSuccess(ctx, job, stream, result) // == today's success tail, factored out
            return
        case <-ctx.Done():
            // job cancelled or worker shutting down while queued: fall through
            // to the existing cancellation/shutdown handling.
        }
    }
    // ... existing handler lookup, GPU-slot acquire, deadline execution,
    // terminal Ack/Nack/Fail (unchanged from today's processJob tail).
}
```

`r.finishSuccess` is a small, mechanical extraction of `processJob`'s existing
success tail (`runner.go:642-660`: `jobOK = true`, usage recording,
`stream.WriteEnd(output)`, `source.Ack`) into a helper both the normal
handler-success path and this queue-wait-exceeded path call — not new
terminal-publish logic, just naming the ONE existing implementation so it has
two callers instead of duplicating it.

**This is the fix for the risk §1c establishes (Nack producing no terminal
event under contention).** On queue-wait exceeded, the job is **not**
Nacked — it goes through the exact same success-shaped terminal publish +
`Ack` that a normal completed job does, carrying the `model_warming`-style
output (§3b) instead of a real answer. It is removed from the PEL and (on
`RedisSource`) consumes zero delivery attempts, so it cannot be driven into
DLQ by contention alone on that source; on the production WS/API path, this
also means the backend always receives an explicit terminal event with a
"still busy" payload instead of silence, regardless of how the API proxy's
own retry/DLQ policy behaves server-side (§1c's open question).

### 3b. Backpressure signal: reuse the existing `model_warming` contract

`internal/worker/llm_inference.go`'s `warming()` (line 1060) already returns a
**success**-status `JobResult` with `{status: "model_warming", model,
eta_seconds, retry_after, warming_for}`, and the platform already has a
retry loop that branches on `output.status == "model_warming"` and honors
`retry_after` (see `swap.go`'s doc comments, `internal/worker/swap.go:9`,
`:54`). This is exactly the shape #8254 asks for ("return a queued/warming
signal, not a 504").

Two options, not mutually exclusive as a rollout sequence:

- **v1 (zero backend change): reuse `model_warming` verbatim.** When the
  inference lane's queue-wait is exceeded, call
  `h.warming(payload.Model, 0, inferenceQueueWaitHint, "queue")` (or similar) —
  the SAME structured output the platform's retry logic already understands.
  Ships without any aceteam-side PR. Con: conflates "the model is still
  loading" with "the model is ready but the node is saturated" in any
  operator-facing UI that renders `warming_for`.
- **v2 (more precise, needs a small aceteam-side change): add a distinct
  `status: "queued"`/`"busy"` discriminator**, same shape
  (`eta_seconds`/`retry_after`, plus optionally `queue_position`), and have
  the backend treat it identically to `model_warming` (retry after the hint).
  This is the target contract; v1 is the safe way to ship the *mechanism*
  (the local queue) before the *vocabulary* is finalized cross-repo.

Either way, the queue-wait's `eta_seconds`/`retry_after` should be informed by
real data where available: the swap-ETA machinery
(`internal/worker/swap.go`'s `MeasuredLoad`/`defaultLoadEstimate`) already
tracks *load* time per (engine, model); a symmetrical, simpler estimate for
*queue* wait is just "average recent execution duration for this lane × current
queue depth ahead of this job" — cheap to compute, no new measurement
infrastructure required for v1 (a fixed conservative default is an acceptable
v1 fallback, mirroring how `defaultLoadEstimate` itself is a fallback table).

### 3c. Model-sized timeout and latency metrics

- **Per-job execution deadline is unaffected by queue wait.** Per §1e/§2a, the
  watchdog deadline (`executeWithDeadline`) is only invoked once the job
  actually acquires an `exec` slot — queue-wait time never counts against it.
  This directly satisfies prerequisite (d) for this lane too.
- `llm_inference` is not in `unboundedJobTypes` or `longSessionJobTypes`, so it
  gets the generic 60-minute fallback deadline (`defaultJobTimeoutSeconds`,
  `deadline.go:31`) absent an explicit `timeout_ms`. That ceiling is not what
  produces the reported 504s (the reported failures happen at the scale of
  seconds to low minutes, driven by admission/DLQ churn, not by hitting a
  60-minute wall) — so no citadel-side timeout-sizing change is needed to fix
  the *reported* symptom. A genuinely **model-size-aware** generation timeout
  (distinguishing a fast 8B model from a slow 27B one) is real but is
  primarily about the *caller's* wait budget, which is aceteam-side — see §4.
- **Latency metrics** (#8254 ask #3): add wall-clock timing around
  `handler.Execute` inside `executeJob`/the lane goroutine, and surface
  `total_ms`, `queue_wait_ms`, and (reusing the existing
  `_usage_prompt_tokens`/`_usage_completion_tokens` keys already populated in
  `JobResult.Output` — see `buildUsageRecord`, `runner.go:769`)
  `tokens_per_second` in the SAME `JobResult.Output` map the handler already
  returns via its `success()` helper (`llm_inference.go:958`). Purely additive
  to the output map; no contract change, no backend coordination needed. This
  satisfies #8254's ask #3 entirely on the citadel side.
- **Batch inference** (#8254 ask #2, `inference_batch`) is an aceteam-side MCP
  tool, not a citadel change — but it is exactly the workload this design
  makes safe: N prompts fired concurrently now land in the bounded local
  queue instead of Nack-storming the node, so a batch tool built against
  today's (pre-#8254) citadel would still misbehave at N > GPU slot count.

## 4. Backpressure contract with the backend (cross-repo)

Two distinct signals, serving different purposes, both citadel-emitted:

1. **Per-request** (already designed in §3b): the `model_warming`/`queued`
   `JobResult.Output`, delivered on the SAME job the caller dispatched, via the
   existing `stream:v1:{jobId}` terminal-event mechanism. **Reactive** — it
   answers "what happened to the job I just sent."
2. **Per-heartbeat** (new): a coordinator-aware "how loaded is this node right
   now" signal, so a dispatcher can avoid *targeting* an already-saturated node
   in the first place, rather than only reacting after a fast-fail or a
   queued response. This is the "busy since T" signal #908's prerequisite (5)
   asks for, and it generalizes to cover #8254's saturation case too — one
   field serves both.

**Design (citadel-Go):** extend the heartbeat with a small struct mirroring the
established `SwapActivity`/`GPUReservations` pattern (`internal/status/types.go`
— hand-maintained mirror, since `internal/status` cannot import
`internal/worker`; projected in `cmd/work.go`, same split as
`swapStatsFrom`/`reservationsFrom`):

```go
// internal/status/types.go
type LaneActivity struct {
    Lane          string     `json:"lane"`           // "unbounded" | "inference"
    Queued        int        `json:"queued"`
    Executing     int        `json:"executing"`
    ExecCapacity  int        `json:"exec_capacity"`
    BusySince     *time.Time `json:"busy_since,omitempty"` // set when Executing==ExecCapacity, cleared when it drops below
}
```

`BusySince` is the literal "busy since T" primitive: stamped the moment a lane's
`executing` count first reaches its `exec` capacity (fully saturated), cleared
the moment it drops below. Additive on the heartbeat (`omitempty`-friendly,
absent entirely on a node with no lanes constructed, e.g. hotswap/queue features
off) — a heartbeat consumer that doesn't know this field yet is unaffected.

**What citadel emits (this repo, this design):**
- The per-request `model_warming`/`queued` `JobResult.Output` (§3b).
- The `LaneActivity[]` heartbeat field above, for both the unbounded lane and
  the inference lane.
- Latency metrics in the inference job output (§3c).

**What aceteam must consume (different repo, NOT built here — flagging for
cross-repo coordination):**
- The dispatcher/coordinator reading `LaneActivity`/`BusySince` before a
  *targeted* dispatch, to prefer a node that isn't already saturated, or to
  extend its own claim-ack wait window for a node reporting `BusySince` recently
  (a legitimately busy-but-healthy node, not an unreachable one) — this is the
  aceteam#7318 fast-fail complaint's actual fix on the backend side; citadel can
  only emit the signal, not act on the backend's dispatch policy.
  (aceteam#7318 tracks the fast-fail cause; a new aceteam-side follow-up should
  track *consuming* this field once it ships.)
- If v2 of §3b's contract is adopted: recognizing `status: "queued"`/`"busy"`
  in the SAME retry branch that already handles `model_warming`.
  (aceteam#8254 is the tracking issue on that side already.)
- `inference_batch` MCP tool, and surfacing the new latency metrics to the
  caller (aceteam#8254 asks #2 and #3's user-facing half).
- Model-size-aware **caller-side** wait budget (aceteam#8254 ask #4's backend
  half) — sizing how long the backend itself waits before giving up on a job,
  informed by citadel's `eta_seconds` hints (§3b) and/or a per-model latency
  history the backend could build from the new `total_ms`/`tokens_per_second`
  metrics (§3c) over time.
- Forwarding `vram_mb`/`ram_mb`/`timeout_ms` remains a separate, pre-existing,
  already-documented gap (§Service Preemption / §Per-job resource isolation in
  `CLAUDE.md`) — unrelated to this design, noted only so it isn't confused with
  the new fields above.

## 5. Phased breakdown

**Phase 0 — already shipped, this design composes on top of it, not instead of
it:**
- #489 (long-session always-async lane).
- #903 Stage 1 / PR #911 (GPU-bound always-async lane, gated on `gpuTracker != nil`).
- #825 (GPU-slot gate scoped to `needsGPUSlot` job types only).
- #548 (per-job watchdog + self-heal + `WorkerLiveness`).

**Phase 1 — claim/execute split + general unbounded lane (closes #908, no
locking, no backend change required):**
- Split `processJob` into `claimJob` + `executeJob` (§2a).
- Introduce the `lane` primitive (§2b) and instantiate it once, for
  `unboundedJobTypes`, with `exec` capacity **1** (§2c) — preserves today's
  implicit single-writer guarantee exactly, just decoupled from the fetch loop.
- `WorkerState` queued/executing split + self-heal STUCK reading
  `OldestExecutingAt` instead of `OldestInFlightAt` (§2d). New regression test
  alongside the existing #489-pattern tests.
- Confirm (via a test analogous to `TestRunnerLongSessionJobDoesNotBlockOtherJobs`)
  that an in-flight `SERVICE_START` no longer blocks a queued `FILE_READ_BYTES`
  from being claimed and executed on a `maxConcurrency=1` node — this is the
  direct regression test for #908's reported incident shape.
- This phase alone is a complete, shippable fix for #908 as filed.

**Phase 2 — inference slot queue (closes aceteam#8254's citadel-side scope; no
dependency on Phase 3, and no dependency on Phase 1 either — could ship
independently, though sharing Phase 1's `lane` primitive is the reason to
sequence it after):**
- Instantiate a second `lane` for `gpuBoundJobTypes`, replacing the bare
  `gpuTracker.Acquire()`/Nack-on-full with queue-on-full (§3a).
- Ship v1 of the backpressure signal (reuse `model_warming` verbatim, §3b) —
  zero backend coordination required to ship.
- Add latency metrics to `JobResult.Output` (§3c).
- Cross-repo follow-up issue (aceteam side) for v2's `status: "queued"`
  discriminator and for the dispatcher consuming `LaneActivity` (§4) — can
  land later without a citadel-side change once §4's heartbeat field exists.

**Phase 3 — manifest/lockfile locking (§2e), optional, only needed to raise the
unbounded lane's `exec` capacity above 1:**
- One `sync.Mutex` guarding `citadel.yaml` read-modify-write in
  `internal/jobs`, one guarding `modules.lock` read-modify-write in
  `internal/catalog`. Lock ordering documented (manifest before lockfile).
- Raise `unboundedJobTypes` lane `exec` capacity above 1 (e.g. to 2-4) once
  landed and soaked.
- Explicitly a stretch goal: closing #908 as filed does not require this
  phase (Phase 1 already does, at `exec` capacity 1). Sequence it whenever
  there's appetite for true concurrent `SERVICE_START`/`MODULE_SET` execution,
  not before.

**Dependency summary:** Phase 1 and Phase 2 are independent of each other and
of Phase 3. Phase 3 depends on nothing here except a decision to pursue it (§6
Q1) and is the only phase gated behind new locking.

## 6. Open questions for Jason

1. **Is Phase 3 (real concurrent unbounded-job execution) worth building at
   all, or does `exec` capacity 1 (Phase 1) suffice indefinitely?** The #908
   symptom is fully fixed without it; Phase 3 only helps if two unrelated
   long-running deploys/pulls need to overlap in practice.
2. **`inferenceQueueWait` (§3a) and the unbounded lane's `admit` capacity
   (§2c)** are proposed as small tunable constants (env-var-overridable,
   following this codebase's `WORKER_*`/`SERVICE_*` convention) rather than
   pinned here — want a specific starting value, or is "ship a conservative
   default, tune from real node telemetry" fine?
3. **§3b's v1-vs-v2 sequencing**: ship v1 (reuse `model_warming` verbatim, zero
   backend change) now and file the v2 (`status: "queued"`) cross-repo issue
   for later, or hold citadel's queue mechanism until the v2 contract is
   agreed so the vocabulary only changes once?
4. **§4's `LaneActivity`/`BusySince` heartbeat field**: worth building now
   speculatively (citadel-side is cheap and additive), or defer until the
   aceteam-side dispatcher work that would actually consume it is scoped, so
   the shape isn't guessed twice?
