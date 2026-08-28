# Design: Per-Job Resource Isolation (citadel#831, #842, #843)

## Context

2026-08-25: a video render on node 1297 (50GB RAM, ~0 free, swap full) OOM-killed
— the kernel's victim-selection is by badness/size, not by "which container is
production," so it could have taken down `bonsai` or `unlimited-ocr` instead. The
trigger was CPU-offload of a ~19GB text encoder into already-full RAM. Owner
decision (citadel#831): **isolation is the mandatory floor**, because a
single-node BYO-compute customer has no second node to move work to. A dedicated
node is a preferred *optimization*, not a substitute for isolation on a shared
one.

**Target hardware:** node 1297 is an RTX 3090 — no MIG, limited/fragile MPS. This
doc is scoped around that constraint, not around what would be easy on an
A100/H100 fleet citadel does not yet operate.

## 1. Current state

### 1a. What's already shipped

**RAM/VRAM/disk *visibility* (citadel#833, PR #841, MERGED).** This is the
producer side only — it surfaces numbers, it does not gate anything:
- `status.SystemMetrics.MemoryAvailableGB` (`internal/status/types.go:300`,
  populated `internal/status/collector.go:480` from gopsutil's `mem.Available`)
  — true "available for programs to allocate," not raw free (accounts for
  reclaimable page cache).
- `status.GPUMetrics.MemoryFreeMB` (`internal/status/types.go:330`) — probed
  directly via `nvidia-smi --query-gpu=...,memory.free` (`internal/platform/gpu.go`),
  not derived as `total - used`. This distinction is load-bearing: PR #841's own
  measurement on a real 3090 found `total-used` overstates free VRAM by
  ~450-460MB (driver/ECC reservation counts against neither total-as-reported nor
  used) — the wrong direction of error for a signal meant to prevent an OOM.
- `resmon.Snapshot.Host` (disk) — not consumed by anything in this repo yet.

**VRAM preflight+preemption — but VRAM only, and gated on a payload field the
backend doesn't send (citadel#577).** `internal/status/preempt.go`'s
`PlanPreemption(candidates, requiredVRAM, availableVRAM) PreemptPlan` is the
pure decision: `requiredVRAM==0` → fits, no preemption (fail-safe: never evict
on an absent signal); already fits → no preemption; otherwise stop non-pinned
services idle-first/largest-first until it fits, or reject (`Fits=false`) if a
pinned service would need to be stopped. `ServiceHandler.preemptForVRAM`
(`internal/jobs/service_handler.go:638`) is the executor: it collects live free
VRAM via `freeVRAMBytes` (`service_handler.go:704`, prefers the real
`MemoryFreeMB` over the derived value per #833), builds candidates from running
services' footprints + the `pinned_services` manifest allowlist, and durably
stops (`desired_status: stopped` then compose-down, so manifest-reconcile can't
resurrect the evicted service under the incoming deploy).

**This is real, tested, wired code — but it only fires when `SERVICE_START`'s
payload carries `vram_mb`/`vram_gb`** (`parseRequiredVRAMBytes`,
`service_handler.go:547`), and the aceteam backend does not send either key yet
(`fabric_provision` dispatches only `{service, model}`). So today, on a real
deploy, `preemptForVRAM` no-ops immediately: `requiredVRAMBytes == 0`. It is a
complete, working VRAM-fit-or-refuse mechanism sitting on an inert input. **This
is the closest existing analogue to what #831 needs for VRAM**, and #842's
"preflight-only shipped for #831" almost certainly refers to this producer-signal
work (#833) plus this pre-existing #577 mechanism, not a new #831-specific
preflight — there is no merged or open PR titled against #831 itself; #831 is
unimplemented.

**A parallel, disk-dimension preflight pattern already exists and is a template
worth copying (citadel#828, PR #840, DRAFT, not yet merged).** Built off the
same 2026-08-25 incident class (a `MODEL_CACHE_PULL` grabbing ~161GB and filling
a node's disk to 97%). `internal/jobs/disk_space.go`'s `planDiskPreflight` is a
**pure** fits/doesn't-fit decision (required bytes vs. free bytes + a safety
margin), separated from the I/O that gathers its inputs
(`disk_space_unix.go`/`disk_space_windows.go` via `syscall.Statfs`/
`GetDiskFreeSpaceEx`, `hf_repo_size.go` for required-bytes estimation). Its
stated failure-direction policy is the one #831 should copy verbatim: **fail
OPEN on estimation failure** (network hiccup fetching size metadata, unreadable
disk-free syscall — log a warning, let the job proceed as before this feature
existed), **fail CLOSED only on a confirmed shortfall** (a real
required > available comparison that came back positive). This avoids a flaky
metadata fetch turning a working pipeline into a new failure mode, while still
catching the exact incident class that motivated it.

**Compose-level `mem_limit:` injection already exists in production — for a
different population of services, and explicitly NOT for GPU services
(`internal/catalog/sandbox.go`).** `GenerateHardeningOverride` builds a second
compose file (`<name>.sandbox.yml`, applied as a second `-f`) for untrusted
(Tier 2) modules: `mem_limit`/`cpus`/`pids_limit` from the module manifest's
`sandbox.resources` block, or conservative defaults (`defaultSandboxMemory =
"2g"`, `sandbox.go:19`) when unset. Delivery mechanism: `cmd/service.go`'s
`sandboxOverridePathFor` → `catalog.ExistingSandboxOverride` resolves the
sibling `.sandbox.yml` if present, and `startServiceComposeArgs` appends it as
a second `-f` — a no-op when the file doesn't exist, so every non-sandboxed
service is unaffected. **Critically, this mechanism explicitly EXEMPTS any
service that requests a GPU** (`serviceRequestsGPU` in
`internal/catalog/gpu_compose.go`, checked in `GenerateHardeningOverride`) —
the doc comment is explicit that the 2g/2cpu conservative defaults "break
inference/embedding services (vLLM, TEI)" and that GPU sandboxing was
deliberately deferred. **This is exactly the gap #831 needs to close**: the
`mem_limit:`-via-second-compose-file delivery mechanism is proven and already
in production, but it structurally does not reach the GPU/media-gen services
#831 is about. #831 is not "invent RAM limiting" — it's "extend an existing,
working mechanism to a population it currently, deliberately, skips."

### 1b. What does NOT exist today

- **No RAM limit is applied to any embedded service** (`vllm`, `bonsai`,
  `diffusers`, `llamacpp`, `ollama`, `sglang`, `unlimited-ocr`, `lmstudio`) — none
  of the `services/compose/*.yml` files declare `mem_limit`/`mem_reservation`/
  `deploy.resources.limits`. The only `deploy:`/`resources:` keys present in
  those files are GPU device *reservations* (`deploy.resources.reservations.
  devices[].capabilities: [gpu]`), unrelated to memory.
- **No raw cgroup writes anywhere in the repo.** `internal/resmon` reads
  `/proc/<pid>/cgroup` to identify which container a process belongs to
  (`internal/resmon/parse.go`), but never writes cgroup limits. Every resource
  *control* lever in this codebase is a docker/compose-level flag
  (`mem_limit:`, `cpus:`, `pids_limit:`), never `/sys/fs/cgroup/...` directly —
  consistent with "jobs run as docker containers via compose," below.
- **No RAM preflight.** Nothing today refuses a `SERVICE_START`/model-deploy job
  because it would exceed free RAM. `preemptForVRAM` is VRAM-only; there is no
  RAM analogue.
- **No text-encoder-offload-aware VRAM/RAM estimate.** Nothing in the codebase
  computes "this model's CPU-offloaded components will need N GB of RAM" — the
  incident's actual trigger (a ~19GB text-encoder offload) has no estimator.

### 1c. How jobs actually run (confirms the natural lever)

Every managed service — including the GPU media/inference engines — starts via
`docker compose -f <file> up -d`, never a raw `docker run` and never direct
cgroup manipulation. `startServiceComposeArgs` (`cmd/service.go:270`) is the
canonical `up -d` args builder (used by `citadel run`, `citadel module start`,
`MODULE_SET`); `preemptForVRAM`'s evictions route through the same compose-down
path used by `SERVICE_STOP`. Since Docker's compose `mem_limit:` key already
translates to a cgroup `memory.max` write by the docker daemon itself, **a
compose-level limit is the correct lever here** — not a citadel-owned cgroup
writer. Building a second, parallel cgroup-writing path would duplicate what
`mem_limit:` (via the already-proven `catalog.sandbox.go` mechanism) already
does, for no isolation benefit.

## 2. RAM isolation (the tractable part)

This works identically on any hardware — RTX 3090, A100, CPU-only. No GPU
constraint applies.

**Mechanism: extend the existing `.sandbox.yml` override delivery to GPU/media
services, with a media-gen-appropriate default instead of the untrusted-module
default.** Concretely:

- Add a memory-limit override path for embedded `ServiceMap` services (vllm,
  bonsai, diffusers, etc.) that is independent of `catalog.sandbox.go`'s
  Tier-2/untrusted-module gate — `GenerateHardeningOverride`'s GPU exemption is
  correct for what it protects against (breaking inference by dropping caps /
  making rootfs read-only / capping at 2 CPU); it should stay in place. What's
  missing is a **narrower** override — `mem_limit:` only, no cap-dropping, no
  read-only rootfs — applied to GPU services specifically.
- Default RAM ceiling: per-service, generous (media-gen jobs legitimately use
  10-20GB+ for a large text encoder offload), NOT the 2GB Tier-2 default —
  something like "node RAM minus a reserved floor for pinned production
  services," derived at start time from `SystemMetrics.MemoryAvailableGB`
  (already available, #833) rather than a single hardcoded constant, since node
  RAM sizes vary across the fleet.
- **Production services keep a reserved floor** (#831's acceptance criterion):
  concretely, this likely means pinned services (`pinned_services` in
  `citadel.yaml`, already the isolation-scope primitive #577 established) get
  either no `mem_limit` (trusted to behave) or a limit sized to their own
  measured footprint (`status.ServiceFootprint`, already collected) plus
  headroom — and the RAM budget computation for a NEW job's limit subtracts
  pinned services' current footprint from `MemoryAvailableGB` before deriving
  the new job's ceiling, so a runaway new job's cgroup OOM fires before it can
  starve the reserved floor.
- **Acceptance restated in this mechanism's terms:** "a runaway job is killed by
  its own cgroup limit and cannot terminate a co-located production service" —
  this is a direct, mechanical consequence of `mem_limit:` (Docker's OOM killer
  fires cgroup-scoped, at the container's own `memory.max`, before the host's
  global OOM killer ever needs to pick a victim across containers). This is
  the single biggest reason RAM isolation is tractable and hardware-independent:
  Linux cgroups memory accounting doesn't care what's plugged into the PCIe
  slot.

## 3. VRAM isolation — the hard part

**Preflight (refuse if it won't fit) is doable today and composes with what
already exists.** #577's `PlanPreemption` + `preemptForVRAM` + #833's real
`MemoryFreeMB` signal are already a complete, tested, wired VRAM-fit-or-refuse
pipeline for the ONE thing they were built for: deciding whether an incoming
`SERVICE_START` should preempt other services or be rejected. Extending it to
also cover `MODEL_CACHE_PULL`/media-gen job dispatch (not just `SERVICE_START`)
and adding a corresponding RAM-fit check alongside it (§4) is straightforward,
additive engineering — no new hardware capability required.

**Hard per-process VRAM CAPS are a different claim, and are NOT reliably
enforceable on a consumer GPU today.** Being honest about this is the actual
point of this document:

- **MIG (#843) requires A100/H100-class hardware.** The RTX 3090 (Ampere
  consumer, compute 8.6) has no MIG capability at the silicon level — this
  isn't a driver/config gap, it's not there. Nothing citadel does in software
  changes this. Parked correctly; revisit only if/when the fleet runs
  MIG-capable datacenter GPUs.
- **MPS (#842) technically runs on a 3090, but `CUDA_MPS_PINNED_DEVICE_MEM_LIMIT`
  is a soft, cooperative limit, not a hardware wall.** It requires every
  process sharing the GPU to run under the same MPS control daemon, is known to
  be fragile across mixed frameworks (vLLM + diffusers + llama.cpp variants, the
  exact mix citadel serves) sharing one GPU, and — per #842's own parking
  rationale — does not hard-stop an OOM the way a cgroup `memory.max` does for
  RAM. Standing up an MPS daemon on every GPU node adds real operational
  surface (daemon lifecycle, restart-on-crash, interaction with citadel's own
  container lifecycle) for a soft guarantee.
- **Net effect on a 3090 node: there is no hardware or driver mechanism available
  today that hard-stops a process from allocating more VRAM than its budget once
  it is running.** A CUDA OOM inside a container is not survivable the way a
  RAM cgroup OOM is (Docker's OOM killer terminates the container cleanly; a
  CUDA allocator failure inside vLLM/diffusers is closer to "the process is now
  in an undefined state," and depending on the engine may crash uncleanly or
  hang). This is a real gap, not a solved problem citadel is choosing not to
  ship — it should be stated as such to anyone reading #831's acceptance
  criteria and expecting a VRAM cap with the same enforcement strength as the
  RAM cgroup limit.

**What VRAM preflight + preemption together DO buy, honestly:** they prevent
the *foreseeable* case — a deploy that declares (or that citadel estimates) a
VRAM requirement exceeding free VRAM is refused or triggers preemption *before*
the container starts. They do NOT prevent a process that started within budget
from growing past it mid-run (e.g., an unexpectedly large batch, a KV cache
that grows with context length beyond what was estimated at launch — this is
exactly the shape of the Bonsai VRAM-tuning issue already documented elsewhere
in this repo's CLAUDE.md, where an unbounded context blew VRAM 4x past the
model's own footprint). That residual risk is real on this hardware and is not
closed by anything in scope here.

## 4. Preflight-first strategy

Given §3, the honest posture for a 3090 fleet is: **lean on preflight + fail-fast
refusal + preemption, not on caps the hardware can't reliably provide.**
Concretely, for a `SERVICE_START` or media-gen job dispatch:

1. **Compute required RAM and required VRAM** for the job (backend-declared
   budget preferred, mirroring `vram_mb`/`vram_gb` — currently inert per §1a;
   a catalog-declared estimate as a fallback, mirroring #828's diffusers-shape
   auto-detection for disk).
2. **RAM preflight** (new): compare against `SystemMetrics.MemoryAvailableGB`
   minus the reserved-floor for pinned services (§2). Refuse (job FAILURE with
   a clear reason, not a silent hang into an OOM) if it won't fit — mirroring
   #828's `planDiskPreflight` fail-open-on-estimation-error / fail-closed-on-
   confirmed-shortfall policy exactly.
3. **VRAM preflight + preemption** (extend #577, mostly already built): same
   `PlanPreemption` call, extended to more job types than `SERVICE_START` alone.
4. **RAM cgroup limit is still applied at container start regardless of whether
   preflight said yes** (§2) — preflight reduces how often the limit is hit,
   the limit is what makes "cannot terminate a co-located production service"
   actually true even when an estimate was wrong. Preflight and the RAM limit
   are complementary, not substitutes for each other: preflight is advisory
   (best-effort estimate, can be wrong), the cgroup limit is the enforced floor.
5. **VRAM has no equivalent enforced floor on a 3090** (§3) — so a wrong VRAM
   estimate that preflight let through, or growth beyond what was estimated,
   is NOT contained the way RAM is. Say this explicitly in any operator-facing
   docs/error messages this produces, rather than implying VRAM safety parity
   with RAM.

This is a deliberate rebalancing: where MPS/MIG would give "prevent it from ever
happening," 3090-only preflight gives "refuse the foreseeable case, and fail
loud and fast rather than fail silent" for the unforeseeable one. That's a
materially weaker guarantee than RAM's, and should be represented as such.

## 5. Phased issue breakdown

**Build now (v1 of #831, tractable on any hardware including the 3090):**

1. **RAM preflight** — new `internal/jobs` package/functions mirroring #828's
   `disk_space.go` pattern (pure decision fn + injectable I/O), gated on a
   declared-or-estimated RAM requirement, comparing against
   `SystemMetrics.MemoryAvailableGB` minus reserved floor. Depends on: nothing
   new (consumes #833's already-shipped `MemoryAvailableGB`).
2. **RAM cgroup limit via compose `mem_limit:` for GPU/media services** — new
   override mechanism parallel to (not replacing) `catalog.sandbox.go`'s
   Tier-2 hardening override; same delivery pattern (second `-f` compose file,
   inject-only-where-absent), different default sizing and no cap-drop/
   read-only-rootfs. Depends on: nothing new structurally, though sizing policy
   (reserved floor for pinned services) benefits from (3).
3. **Reserved-floor computation for pinned services** — read `pinned_services`
   (already the #577 primitive) + `ServiceFootprint` (already collected) to
   derive "how much RAM/VRAM is off-limits to a new job's budget." Shared input
   to both (1) and (2).
4. **Extend VRAM preflight/preemption beyond `SERVICE_START`** to cover
   media-gen job dispatch generally, if media-gen jobs don't already funnel
   through `SERVICE_START`. (Likely low/no-op work if they do.)
5. **Text-encoder-offload VRAM/RAM estimate, or at minimum "prefer GPU offload
   over CPU offload when VRAM allows"** (#831's fix item 3) — this is a
   model/engine-specific estimation problem (diffusers pipeline component
   sizing), separate engineering scope from the isolation plumbing above; likely
   its own issue.

**Stays parked, hardware-gated:**

- **#842 (MPS)** — revisit only if preflight-only VRAM enforcement (§3-4) proves
  insufficient in practice (repeated VRAM-growth-past-estimate incidents), AND
  only after weighing the MPS daemon operational cost against the soft
  guarantee it buys.
- **#843 (MIG)** — revisit only if/when the fleet runs MIG-capable datacenter
  GPUs. No action possible on a 3090.

**Dependency shape:** (1)/(2)/(3) are independent of #842/#843 entirely — they
do not become more or less necessary based on whether MPS/MIG ever land. (4) is
additive. (5) is the only item that's genuinely new estimation logic rather
than infrastructure extension, and could ship after (1)-(3) without blocking
them.

## 6. Open questions for Jason

1. **Reserved-floor sizing policy**: should the RAM/VRAM floor for pinned
   services be "their current measured footprint + a fixed headroom
   percentage," a manifest-declared value per pinned service, or something
   else? This decision drives both (2) and (3) above and doesn't have an
   obvious default.
2. **Where does the RAM/VRAM requirement come from for a media-gen job**, given
   the backend doesn't send `vram_mb`/`vram_gb` yet and there's no RAM
   equivalent key at all? Is a citadel-side estimate (per #828's diffusers-shape
   heuristic precedent) acceptable as v1, or does this block on an aceteam-side
   catalog change to declare budgets explicitly (as #577's and #828's PR bodies
   both flag as outstanding cross-repo follow-ups)?
3. **What should happen on a RAM preflight refusal** — job FAILURE (as #828's
   disk preflight does), or should it queue/retry (as #831's issue text
   suggests as an alternative: "refuse or queue with a clear error")? Queueing
   implies new state (a pending-job holding area) that doesn't exist today for
   any job type.
4. **Is the CPU-offload-vs-GPU-offload preference (#831 fix item 3) in scope for
   v1**, or should it be split into its own issue given it's estimation/engine
   logic rather than isolation plumbing? This doc treats it as separable (§5
   item 5) but the original issue bundles it with isolation.
5. **Given VRAM has no enforced cap on a 3090 (§3)**, is preflight-only judged
   an acceptable production posture indefinitely, or does repeated VRAM
   overrun in practice force revisiting MPS (#842) sooner than "if preflight
   proves insufficient" implies? Worth a explicit revisit trigger/threshold
   rather than leaving it open-ended.

## 7. Implementation status (citadel#831 v1, shipped)

§5's "build now" items 1-3 shipped together, gated behind a single opt-in
`CITADEL_RESOURCE_ISOLATION` flag (default OFF — see CLAUDE.md's "Per-job
resource isolation" section for the exact mechanism, file names, and why one
flag rather than two). Resolutions to §6's open questions, either from the
owner's 2026-08-25 design-decision comment on citadel#831 or, where still
left open by that comment, chosen here:

1. **Reserved-floor sizing (Q1, left open by the owner):** a fixed 2GiB OS
   headroom on top of pinned services' own measured RAM footprint. When the
   resulting ceiling would fall below a 2GiB viable minimum, RAMBudgetBytes
   returns 0 ("no safe ceiling can be derived") rather than clamping UP to
   that minimum — a clamped-up value would be fabricated, with no
   relationship to what's actually free, and applying it as a real inference
   engine's `mem_limit` would reproduce the exact failure this doc warns
   about for the Tier-2 2GB default. The caller skips applying any ceiling
   for that start instead (fail open). `status.RAMBudgetBytes` /
   `ramHeadroomBytes` / `minViableRAMCeilingBytes` (`internal/status/ram.go`).
   A chosen default, not a value pinned by an external spec — reconsider if
   operational experience says otherwise.
2. **Where the RAM/VRAM requirement comes from (Q2):** VRAM now has a real
   citadel-side estimate (`status.EngineVRAMEstimateMB`, reused from the
   hotswap planner — see CLAUDE.md). RAM does NOT get an equivalent estimate
   table in v1: `ram_mb`/`ram_gb` mirrors `vram_mb`/`vram_gb`'s payload-only
   contract exactly, and an absent value fails OPEN (never refuses) rather
   than guessing a per-engine RAM number this doc has no vetted source for —
   the safer of the two options this implementation had to pick between when
   the doc left this open for RAM specifically.
3. **RAM preflight refusal behavior (Q3):** resolved by the owner — job
   FAILURE, no on-node queue, matching #828's disk preflight exactly.
   `status.PlanRAMPreflight`.
4. **CPU-offload-vs-GPU-offload preference (Q4):** NOT built here — remains
   its own follow-up issue, as §5 item 5 anticipated.
5. **VRAM preflight-only posture indefinitely (Q5):** unchanged by this
   implementation; still an open revisit trigger, not resolved here.

Explicitly NOT built (still parked, per §5): MIG (#843) and MPS (#842) hard
VRAM caps.
