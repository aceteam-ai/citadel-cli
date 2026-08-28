# Design: the `Engine` adapter interface, and a swapper for the gateway chat route

Context: aceteam-ai/citadel-cli#685, #686. Part of the serving-stack epic
aceteam-ai/aceteam#7243. This is a design doc, not an implementation — no Go
changes ship with it.

A meta-note before the content: the #685 issue body cites the backend routing
switch at `internal/worker/llm_inference.go:138-158`. It has already moved to
`Execute`'s switch around line 195-214 in the few weeks since the issue was
filed. That is the exact failure mode this repo's own CLAUDE.md warns about
("a restated fact the code owns... drifts the moment the code changes"), and
it happened to the *bug report about hand-synced tables drifting*. Everywhere
below, treat line numbers as approximate pointers verified at doc-writing
time (2026-08), and function/map names as the durable citation.

## 1. Current state: the per-engine concern matrix

Nine engines exist in some form today: `vllm`, `ollama`, `llamacpp`, `bonsai`,
`unlimited-ocr`, `sglang`, `tei`, `diffusers`, `lmstudio` (plus a few
self-provisioning non-chat services: `kokoro`, `transcribe`, `extraction`,
`gotenberg`, `nvr`, `claudecode`, `meeting*`, `storage` — out of scope for the
`Engine` interface, which is chat/inference-shaped). At least **19 separate
hand-encoded tables or switches** touch engine identity, spread across 8
files, plus 2 more standalone duplicates of the same mapping found outside
the set #685 names. None of them are generated from a shared source; each is
maintained by hand and can silently omit an engine.

### 1a. The concrete bug the issue leads with, confirmed and sharper than stated

`llamacpp` is in `managedProbeEngines` (`internal/status/engines.go`) and
dispatchable in `llm_inference.go`'s routing switch, but absent from BOTH
`hotswap.go`'s `engineModelEnvVars` (the table that reads a model id off an
installed engine's `.env` file) and `engineDefaultModel` (the fallback for
engines with no env override). Because `resolveInstalledModel("llamacpp")`
always returns `""`, and `collectInstalledEngines` unconditionally skips any
engine whose resolved model is `""`, **an installed-but-stopped llamacpp
service can never be advertised as a swap-in candidate — not intermittently,
structurally.** That code path cannot fire for this engine. `vllm`'s absence
from `engineDefaultModel` is the deliberate, documented case (no stable
default weights); `llamacpp`'s is the same shape with no compensating
mechanism and no comment explaining it.

### 1b. sglang: the strongest single case for one interface

`sglang` has a full dispatch path in `llm_inference.go` (`executeSGLang`,
readiness entry in `engineReadyPath`, a load estimate in
`defaultLoadEstimate`, a VRAM estimate in `engineVRAMEstimateMB`) — but it is
**absent from `managedProbeEngines`** (`internal/status/engines.go`), the
list `status.DiscoverLocalEngines` iterates. Because of that one omission,
sglang is invisible to `EngineTypeFromName`, `DiscoverModels`,
`CheckServiceHealth`, the gateway chat router (`resolveChatModel`), mesh
discovery, and hotswap residency/preemption tracking — five to six
consumers, one root cause, un-discoverable without reading every table by
hand. This is precisely the "missing one produces a silent partial" pattern
#685 describes, on the engine that should be least controversial to fix and
most illustrative of why one interface beats nine tables.

### 1c. Two more duplicate tables the issue didn't name

- `internal/mesh/discovery.go` has its **own copy** of
  `status.EngineTypeFromName`, with a comment admitting it: *"This replicates
  internal/status.EngineTypeFromName locally to keep this layer standalone."*
  Same 5 cases, same missing sglang/lmstudio. Two independently-maintained
  copies of one mapping, with nothing enforcing they stay in sync beyond the
  comment.
- `internal/capabilities/detector.go` has a **fourth** engine-name table (an
  ordered substring→tag list) for GPU capability-tag generation. Omits
  bonsai/unlimited-ocr/tei/diffusers with no comment explaining whether
  that's intentional.

### 1d. Coverage matrix

`Y` = correct. `∅(doc)` = absent, and a comment or CLAUDE.md entry says why.
`∅(undoc)` = absent, unexplained. `N/A` = concern doesn't apply to that
engine's nature (e.g. tei/diffusers aren't chat-completions engines).

| Concern (owning symbol) | vllm | ollama | llamacpp | bonsai | unlimited-ocr | sglang | tei | diffusers | lmstudio |
|---|---|---|---|---|---|---|---|---|---|
| `llm_inference.go`: `baseURLs` (in `NewLLMInferenceHandler`) | Y | Y | Y | Y | Y | Y | N/A | N/A | N/A |
| `llm_inference.go`: `Execute`'s backend switch | Y | Y | Y | Y (aliases llamacpp) | Y | Y (completions only, no chat dialect) | N/A | N/A | N/A |
| `llm_readiness.go`: `engineReadyPath` | Y | Y | Y | Y | Y | Y | N/A | N/A | N/A |
| `swap.go`: `defaultLoadEstimate` | Y | Y(default) | Y(default) | Y | Y | Y | N/A | N/A | N/A |
| `ports.go`: `ServiceHostPorts` | Y | ∅(doc, native 11434) | Y | Y | Y | ∅(doc, native 30000, own const) | ∅(doc, fixed 8102) | Y | N/A |
| `ports.go`: `InferenceMetricsPorts` | Y | ∅(doc) | ∅(doc) | ∅(undoc) | ∅(undoc) | Y | N/A | N/A | ∅(doc) |
| `status/engines.go`: `managedProbeEngines` | Y | Y | Y | Y | Y | **∅(undoc)** | N/A | N/A | N/A |
| `status/engines.go`: `idleCapableEngines` | Y | ∅(doc, "extend as others grow") | ∅(doc) | ∅(doc) | ∅(doc) | ∅(doc) | N/A | N/A | N/A |
| `status/models.go`: `EngineTypeFromName` | Y | Y | Y | Y | Y | **∅(undoc)** | N/A | N/A | ∅(undoc) |
| `status/models.go`: `DiscoverModels` switch | Y | Y | Y | Y | Y | **∅(undoc)** — errors if reached | N/A | N/A | N/A |
| `status/models.go`: `CheckServiceHealth` | Y | Y | Y | Y | Y | **∅(undoc)** — returns Unknown | N/A | N/A | N/A |
| `hotswap.go`: `engineModelEnvVars` | Y | ∅(doc) | **∅(undoc)** | Y | Y | ∅(doc) | N/A | N/A | N/A |
| `hotswap.go`: `engineDefaultModel` | ∅(doc) | ∅(undoc) | **∅(undoc, dead code path — §1a)** | Y | Y | ∅(undoc) | N/A | N/A | N/A |
| `hotswap.go`: `engineVRAMEstimateMB` | Y | Y | Y | Y | Y | Y | N/A | N/A | N/A |
| `service_handler.go`: `serviceModelEnvVar` (write side) | Y | ∅(doc) | ∅(doc) | **∅(undoc — contradicts read side above)** | **∅(undoc, same)** | ∅(doc) | N/A | N/A | N/A |
| `model_cache_pull.go` switch | Y | Y | Y | Y | ∅(doc, self-provisioning) | **∅(undoc)** — errors | ∅(doc) | ∅(doc) | ∅(undoc) |
| `model_cache_evict.go` switch | Y | Y | Y | **∅(doc in CLAUDE.md, no code comment)** | ∅(undoc) | ∅(undoc) | N/A | N/A | N/A |
| `mesh/discovery.go`: 2nd `EngineTypeFromName` copy | Y | Y | Y | Y | Y | **∅(undoc)** | N/A | N/A | ∅(undoc) |
| `capabilities/detector.go` engine tags | Y | Y | Y | **∅(undoc)** | **∅(undoc)** | Y | N/A | N/A | ∅(undoc) |

Two more asymmetries worth naming even though they aren't per-engine tables:

- **Write/read mismatch on model persistence.** `hotswap.go`'s
  `engineModelEnvVars` (read side) knows bonsai's `BONSAI_MODEL` and
  unlimited-ocr's `OCR_MODEL`/`OCR_SERVED_NAME`, and both compose files
  genuinely support a serve-time override. But `service_handler.go`'s
  `serviceModelEnvVar` (write side, used to persist a `SERVICE_START` job's
  chosen model into `<name>.env`) only knows `vllm`. A `SERVICE_START` job
  targeting bonsai with a model override has no path to persist it.
- **Pull vs. evict normalize differently.** `model_cache_pull.go` calls
  `normalizeEngineToken` to fold `"llama.cpp"/"llama-cpp"` → `"llamacpp"`
  before its switch; `model_cache_evict.go` lowercases but never normalizes.
  A backend-emitted `engine: "llama.cpp"` pulls fine and fails evict with
  "unsupported engine" — a second-order bug from having two switches instead
  of one adapter method.

### 1e. Request dialects: fewer than they look

Six "dialect" functions exist in `llm_inference.go`
(`executeVLLM`, `executeSGLang`, `executeCompletions`, `executeOllama`,
`executeOllamaChat`, `executeLlamaCppAt`, `executeChatCompletionsAt`), but
there are really only **3 wire dialects**:

| Dialect | Engines |
|---|---|
| OpenAI `/v1/completions` (prompt) | vllm (prompt path), sglang |
| OpenAI `/v1/chat/completions` (messages, multimodal-safe via `ContentJSON()`) | vllm (chat path), llamacpp, bonsai, unlimited-ocr |
| Ollama native (`/api/generate`, `/api/chat`) | ollama only |

Bonsai has **zero engine-specific request code** — it is 100% an alias of
llamacpp's dialect pointed at bonsai's own base URL. This is the cleanest
existing precedent for "one adapter, parameterized by spec, not by engine
name" and should be the first migration target (§3).

## 2. Proposed `Engine` interface

Adopting #685's proposed shape, refined against what the inventory above
actually needs to carry:

```go
type Engine interface {
    Name() string
    Kind() EngineKind          // ComposeService | NativeProcess | Remote
    Spec() EngineSpec

    Start(ctx context.Context) error
    Stop(ctx context.Context) error
    Probe(ctx context.Context) Readiness
    Serve(ctx context.Context, req Request) (Response, error)
}

type EngineSpec struct {
    HostPort            int           // ServiceHostPorts
    HostPortEnvVar       string        // serviceHostPortEnv
    ComposeFile          string        // services.ServiceMap entry
    AuxFiles              []AuxFile     // services.ServiceAuxFiles (bonsai's Dockerfile)
    Dialect              RequestDialect // OpenAICompletions | OpenAIChat | OllamaNative
    ReadyPath            string        // engineReadyPath
    ReadyBudget          time.Duration // engineReadyBudget
    LoadEstimate          time.Duration // defaultLoadEstimate
    VRAMEstimateMB        int           // engineVRAMEstimateMB
    ModelEnvVar           []string      // engineModelEnvVars (read) == serviceModelEnvVar (write), unified
    DefaultModel          string        // engineDefaultModel; "" is valid (vllm) and must stay distinguishable from "unset by omission" (llamacpp's actual bug)
    IdleCapable            bool          // idleCapableEngines — has scraped Prometheus metrics
    EmbeddingCapable       bool          // embeddingProbeServices
    MetricsPath            string        // InferenceMetricsPorts, "" if none
    SelfProvisioning       bool          // model_cache_pull.go's selfProvisioningEngines
    PullCommand            PullFunc      // model_cache_pull.go switch
    EvictCommand           EvictFunc     // model_cache_evict.go switch — nil is a valid, explicit "not supported yet"
    VRAMMeasurableOnReady   bool          // swap.go's vramMeasurableOnReady (false only for ollama)
    CapabilityTag           string        // capabilities/detector.go's tag table
    HealthCheck             HealthCheckFn // CheckServiceHealth
}
```

Typed errors per #685: `EngineDown`, `EngineWarming`, `ModelNotLoaded`,
`VramExhausted(required, free, evictable)`, `EngineUnhealthy`,
`UpstreamTimeout` — replacing ad hoc string-matched transport errors
scattered through `llm_inference.go` and `chat_route.go`.

### How each existing engine implements it

| Engine | Kind | Dialect | Notable spec values |
|---|---|---|---|
| vllm | ComposeService | OpenAIChat + OpenAICompletions | `DefaultModel=""` (deliberate), has metrics |
| sglang | ComposeService | OpenAICompletions only | no `ModelEnvVar`, has metrics — this is where the interface should force a decision: give it a chat dialect for real, or encode "completions-only" as a first-class `Dialect` value instead of a silent gap |
| ollama | NativeProcess (fixed port) | OllamaNative | `VRAMMeasurableOnReady=false`, `SelfProvisioning=true` for pull (pull-based, not env-based) |
| llamacpp | ComposeService | OpenAIChat | `DefaultModel` and `ModelEnvVar` must both be filled in during migration — this is the forcing function that fixes §1a for real, because the struct has no way to express "readable but has no default" by omission the way two independent maps did |
| bonsai | ComposeService, build-based | OpenAIChat (delegates to llamacpp's adapter with a different `HostPort`/`ComposeFile`/`AuxFiles`) | first and only engine needing `AuxFiles` |
| unlimited-ocr | ComposeService | OpenAIChat | `SelfProvisioning=true` (pull), has explicit `ModelEnvVar` |
| tei | ComposeService, fixed port | N/A (embedding only) | `EmbeddingCapable=true`, everything else zero-value |
| diffusers | ComposeService | N/A (not chat) | outside the `Engine` interface's Serve() concern; may still want a lighter adapter for lifecycle only |
| lmstudio | ComposeService | unclear/likely OpenAIChat | currently has almost no entries anywhere — a genuine "is this actually maintained" question, see open questions |

Collapsing effect: adding an engine (or fixing sglang/llamacpp) becomes
"write one `EngineSpec` literal and one small Serve() implementation (or
reuse an existing one, as bonsai does today)," and a missing field is a
zero-value in one struct literal, not a silently-absent key in one of eight
files. The acceptance test from #685 — "a half-registered engine like
llamacpp is not expressible" — is satisfied by the struct requiring
`DefaultModel`/`ModelEnvVar` to be considered together (e.g. a linter/test
that flags `ModelEnvVar` set with `DefaultModel` unset and no compose-time
default, or vice versa) rather than living in two unrelated files that
happen to agree today.

## 3. Migration strategy: incremental, not big-bang

Per #685's own ordering note ("best landed after the readiness and warming
contracts are settled") and the instruction not to design a rewrite, the
adapter should **wrap existing behavior first**, then callers migrate one at
a time:

**Phase A — adapter shell, zero behavior change.**
Define `Engine`/`EngineSpec`/`EngineKind`/`RequestDialect` types and one
`Registry map[string]Engine`. Populate it by literally reading the existing
nine-plus tables at init time (a translation layer, not new logic) so
`Registry["llamacpp"].Spec()` returns exactly what today's tables return
today — bugs and all, including llamacpp's empty `DefaultModel`. This phase
is invisible: nothing calls `Registry` yet. Its only purpose is to prove the
struct shape is sufficient by successfully expressing all nine engines,
including the asymmetric ones (bonsai's build files, sglang's
completions-only dialect, ollama's native protocol).

**Phase B — one adapter body per unique implementation, not per engine.**
Because bonsai already aliases llamacpp's `executeLlamaCppAt`, and vllm/sglang
share `executeCompletions`, the actual number of `Serve()` implementations
needed is 3 (OpenAI-completions, OpenAI-chat, Ollama-native), matching §1e.
Write these three, each taking `EngineSpec` as a parameter instead of a
hardcoded base URL, and unit-test them against the existing dialect
functions' behavior (golden requests/responses) before touching any caller.

**Phase C — migrate callers one at a time, cheapest/lowest-risk first:**
1. `internal/status/models.go`'s `EngineTypeFromName`/`DiscoverModels`/
   `CheckServiceHealth` — pure functions, easy to test in isolation,
   immediately fixes sglang's coverage gap (§1b) as a side effect.
2. `internal/mesh/discovery.go`'s duplicate `EngineTypeFromName` — delete it,
   call the shared one. Removes one of the two "silently drift" duplicates
   outright.
3. `internal/status/engines.go`'s `managedProbeEngines`/`idleCapableEngines`/
   `embeddingProbeServices` — becomes `Registry` filters
   (`IdleCapable`/`EmbeddingCapable` fields), fixing §1b's root cause for
   real once `DiscoverLocalEngines` reads from `Registry` instead of the
   hand-written slice.
4. `services/ports.go` — `ServiceHostPorts`/`serviceHostPortEnv` become
   generated from `Registry`'s `HostPort`/`HostPortEnvVar`, keeping the
   existing port-collision test as the safety net (per CLAUDE.md: "a test...
   pins them").
5. `internal/worker/llm_inference.go` — swap `baseURLs` and the routing
   switch for `Registry[backend].Serve(ctx, req)`. This is the highest-risk
   step (touches the live inference path) and should land after 1-4 are
   proven, with the three dialect implementations from Phase B already
   independently tested.
6. `internal/worker/llm_readiness.go`, `swap.go`'s `defaultLoadEstimate`, and
   `internal/jobs/model_cache_pull.go`/`model_cache_evict.go` — last, because
   these touch the readiness/warming contract #685 explicitly says should be
   settled first; migrating them is also where the pull/evict normalization
   asymmetry (§1d) and the write/read model-env mismatch get fixed as a
   consequence of having one spec instead of two.
7. `internal/capabilities/detector.go` — lowest priority; it's a different
   concern (GPU capability tags, not per-request dialect) and can migrate
   independently or stay hand-written if the tag set is meant to diverge on
   purpose (open question below).

**Do not delete the old tables until each migrated caller has a passing
regression test against the adapter's output equaling the old table's
output for all nine engines.** This is what makes it incremental rather than
a rewrite: at every commit, the binary behaves identically; only the source
of truth moves.

## 4. #686: giving the gateway chat route a swapper

The research confirms the issue's claim and sharpens it into two distinct
gaps, not one:

- **`citadel work --gateway`**: a `*worker.SwapManager` is constructed in the
  same process (inside `buildNodeJobHandlers`, attached only to the
  `llm_inference` job handler), but `internal/gateway.Server` has no field
  referencing it anywhere, and construction order makes even a quick fix
  non-trivial: the gateway's chat router is wired (`SetChatRouter`) before
  the swapper is constructed and stored, in `cmd/work.go`. This is "exists,
  not wired, and wired late."
- **`citadel serve`**: no `SwapManager`, no `buildNodeJobHandlers` call, no
  `jobs.ServiceHandler` construction anywhere in that file. This is "does
  not exist in this process" — a strictly bigger gap needing new
  construction, not rewiring.

### Reuse `SwapManager` vs. a new shared abstraction

**Recommendation: reuse `worker.SwapManager` via its existing narrow public
surface (`EnsureResident`), not a new abstraction**, for three reasons:

1. `SwapManager`'s public surface is already narrow and side-effect-free from
   a caller's perspective: `EnsureResident(ctx, backend, model) (SwapOutcome,
   error)` is the *only* entry point `llm_inference.go` uses. The gateway
   needs exactly this same call, at exactly the same trigger (a request for
   a model that isn't currently resident). Building a second abstraction
   would either (a) re-implement `EnsureResident`'s single-flight lock,
   LRU/residency-protection, and rate-limit ledger, reopening the exact bugs
   #688/#717 already fixed once, or (b) wrap `SwapManager` anyway, at which
   point it isn't really a new abstraction.
2. The swap-rate limit and residency ledger are meant to be **per-node**
   state, not per-caller. A separate SwapManager instance for the gateway
   path would double-count VRAM and let the two paths independently swap the
   same engine out from under each other — worse than the current bug,
   which is at least consistent (job path is swap-aware, gateway path is
   simply blind).
3. `internal/gateway` currently has zero dependency on `internal/worker`
   (confirmed by the research: `gateway.Server`'s struct has no worker
   import). Reusing `SwapManager` means accepting that import direction
   change; a new shared abstraction would have to live somewhere both
   packages already import (there is no such package today) — likely a new
   leaf package (e.g. `internal/residency`) that both `worker.SwapManager`
   and the gateway depend on. That is a bigger, riskier move than threading
   an existing interface reference through server construction, and #685's
   own ordering note (adapter interface should land first, "since that is
   where the shared entry point naturally lives") suggests the RIGHT
   long-term home for this shared entry point is the `Engine` interface's
   `Serve()`, once `Serve()` itself is swap-aware. In other words: don't
   build a second residency abstraction now; let #685's `Engine.Serve()`
   become that shared entry point later, and in the interim thread the
   existing `SwapManager` reference through minimally.

### Proposed wiring (post-#685, since the ordering note recommends landing
adapter work first)

- Define a minimal interface in `internal/gateway` (so it stays
  import-direction-clean — gateway defines what it needs, worker satisfies
  it, not the reverse):
  ```go
  type ModelSwapper interface {
      EnsureResident(ctx context.Context, backend, model string) (worker.SwapOutcome, error)
  }
  ```
  (or, once `Engine.Serve()` exists, gateway's chat route calls
  `Registry[backend].Serve(ctx, req)` directly and swap-awareness is inside
  `Serve()` itself — no separate swapper interface needed at the gateway
  layer at all. This is the cleaner end state; the `ModelSwapper` interface
  above is the incremental stepping stone if #686 needs to land before
  `Engine.Serve()` is ready.)
- `gateway.Server` gets a `SetModelSwapper(ModelSwapper)` setter, mirroring
  the existing `SetChatRouter`/`SetMetering` pattern already used for
  optional, late-bound capabilities.
- `resolveChatModel` (`chat_route.go`) gains a fallback: when no *running*
  engine matches the model (today's only check), consult the same
  installed-but-stopped advertising `hotswap.go` already computes for the
  heartbeat, and if a match exists there, call `EnsureResident` before
  proxying instead of returning `model_not_found`. The response contract
  needs a `model_warming`-shaped response (matching the job path's existing
  retryable-warming contract from aceteam#6866) rather than either a bare
  404 or a request that blocks for the full load time — SSE/streaming
  callers need this distinction made explicit in the design, not inferred.
- Fix the construction-order problem in `cmd/work.go`: either move
  `buildNodeJobHandlers` (or just swapper construction) before
  `SetChatRouter`, or make `SetModelSwapper` safely callable after the
  gateway is already serving (an atomic pointer swap, matching the existing
  `nodeSwapManager atomic.Pointer[worker.SwapManager]` pattern already used
  for heartbeat reporting — reuse that same pointer for the gateway instead
  of adding a third one).
- `cmd/serve.go` is the harder case: it has no job-handler machinery at all
  today. Options: (a) port in enough of `buildNodeJobHandlers`/
  `newModelSwapManager` to construct a `SwapManager` standalone (likely
  requires a `jobs.ServiceHandler` too, since `SwapController` wraps it) —
  real new wiring, not a rewire; (b) scope `citadel serve`'s swap support
  out of the first #686 PR and land it as a fast-follow once the `work
  --gateway` path is validated in production. Recommend (b): `citadel serve`
  is the standalone-gateway entry point and is lower-traffic than `work
  --gateway`; shipping the fix where it matters most first, with the gap
  documented, matches this repo's stated preference for honest partial
  progress over scope creep in one PR.

## 5. Phased issue breakdown

Ordered, PR-sized, with dependencies. Each should carry its own
regression test per the "don't delete old tables until proven equivalent"
rule in §3.

1. **Adapter shell + registry (Phase A).** New types
   (`Engine`/`EngineSpec`/`EngineKind`/`RequestDialect`), `Registry`
   populated from existing tables (translation only). No caller changes. Add
   a test asserting `Registry`'s output equals every existing table's output
   for all nine engines today (this test is the safety net for every later
   PR). *Depends on: nothing.*
2. **Three dialect implementations (Phase B).** `Serve()` for
   OpenAI-completions, OpenAI-chat, Ollama-native, parameterized by
   `EngineSpec`, tested against golden requests from the existing
   `execute*` functions. Not yet wired to any live caller. *Depends on: 1.*
3. **Migrate `internal/status` read paths** (`EngineTypeFromName`,
   `DiscoverModels`, `CheckServiceHealth`, `managedProbeEngines`,
   `idleCapableEngines`, `embeddingProbeServices`) to `Registry`. Fixes
   sglang's cross-cutting invisibility (§1b) as a direct consequence.
   *Depends on: 1.*
4. **Delete the `internal/mesh/discovery.go` duplicate**
   `EngineTypeFromName`; call the shared one. *Depends on: 3.*
5. **Generate `services/ports.go`'s port tables from `Registry`.** Keep the
   existing port-collision test. *Depends on: 1.*
6. **Migrate `llm_inference.go`'s routing + `baseURLs` to `Registry` +
   `Serve()`.** Highest-risk step; land only after 1-5 are in production for
   at least one release cycle. Explicitly fixes llamacpp's dead
   `engineDefaultModel` path (§1a) as a required part of populating its
   `EngineSpec` correctly, and fixes the pull/evict normalization asymmetry
   and the write/read model-env mismatch (§1d) as the same spec now serves
   both directions. *Depends on: 2, 3, 5.*
7. **Migrate readiness/warming (`llm_readiness.go`, `swap.go`'s
   `defaultLoadEstimate`, `model_cache_pull.go`/`model_cache_evict.go`).**
   Explicitly sequenced last per #685's own note that these should be
   "settled" before the adapter encodes their final shape — if the warming
   contract changes shape during this work, better to change it once here
   than twice (once ad hoc, once in the adapter). *Depends on: 6.*
8. **`capabilities/detector.go` engine tags → `Registry.CapabilityTag`,
   or explicitly leave hand-written with a comment explaining why.**
   Lowest priority, independent of 1-7. *Depends on: 1 (optional).*
9. **#686, gateway swapper for `work --gateway`.** `ModelSwapper` interface
   in `internal/gateway`, `SetModelSwapper` setter, `resolveChatModel`
   fallback to installed-but-stopped + `EnsureResident`, construction-order
   fix in `cmd/work.go` (reuse the existing `atomic.Pointer` pattern), a
   `model_warming` response contract for streaming callers. *Depends on: 6
   (issue's own ordering note — the shared entry point should be
   `Engine.Serve()`, which requires 6 to exist first; if #686 needs to land
   before 6, it can target `SwapManager.EnsureResident` directly per the
   interim design in §4, dropping this dependency to just "1".)*
10. **#686 follow-up: `citadel serve` swap support.** Port in
    `SwapManager`/`jobs.ServiceHandler` construction for the standalone
    gateway binary. Explicitly scoped out of 9; separate PR once 9 is
    validated in production. *Depends on: 9.*

## 6. Open questions for the human

1. **sglang's chat dialect**: it has no `/v1/chat/completions` path today —
   is that intentional (sglang deployments are completions-only in
   practice), or a genuine gap that should get an `executeChatCompletionsAt`
   equivalent as part of the migration? This affects whether `RequestDialect`
   needs a `CompletionsOnly` variant or whether sglang should just gain the
   chat dialect.
2. **lmstudio's status**: it appears in `services.ServiceMap` but has almost
   no entries anywhere else in the nine tables (not in `managedProbeEngines`,
   not in `EngineTypeFromName`, not in any dialect). Is lmstudio still
   actively supported, or should the migration treat it as legacy/deprecated
   and simply not populate an `EngineSpec` for it (making the gap explicit
   rather than continuing to carry it silently)?
3. **`ModelSwapper` interim interface vs. waiting for `Engine.Serve()`**: §4
   proposes a narrow interface as a stepping stone so #686 doesn't have to
   block entirely on #685 landing. Is that the right call, or should #686
   simply wait for `Engine.Serve()` to exist and skip the interim step
   entirely, accepting the longer timeline?
4. **`citadel serve`'s swap support scope**: §4/§5 recommend explicitly
   deferring it to a follow-up PR after `work --gateway`'s fix ships. Confirm
   that's acceptable, or whether `citadel serve` is used in a context (e.g.
   a specific deployment topology) where the gap is higher-priority than
   assumed here.
5. **Typed-error boundary**: #685 proposes `EngineDown`/`EngineWarming`/
   `ModelNotLoaded`/`VramExhausted`/`EngineUnhealthy`/`UpstreamTimeout`. Do
   these need to be stable, versioned wire-contract types (since the gateway
   chat route returns OpenAI-shaped errors to external callers, and the job
   path returns retryable-warming signals to the backend), or are they
   purely internal to `internal/worker`/`internal/gateway` with each
   caller translating at its own boundary? This affects whether the error
   types belong in a new shared leaf package or can live inside whichever
   package owns `Engine`.
6. **Fixing vs. preserving today's silent gaps during migration**: several
   gaps found (tei/diffusers missing from `capabilities/detector.go`,
   lmstudio's near-total absence, `model_cache_evict.go`'s missing bonsai/
   unlimited-ocr/sglang cases) are pre-existing and out of #685/#686's
   explicit scope. Should the migration PRs fix these opportunistically as
   they touch the relevant table (cheap, since the `EngineSpec` literal has
   to be written anyway), or file them as separate follow-up issues to keep
   each migration PR reviewable as "moved, not changed" per §3's own rule?
