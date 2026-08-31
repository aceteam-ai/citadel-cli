# Design: Meeting/Transcribe VRAM Fail-Fast or Preempt (citadel#891, split from #558)

Status: DESIGN — no production code in this PR. Companion to
[design-resource-isolation.md](design-resource-isolation.md) (#831/#842/#843)
and [design-model-exclusivity.md](design-model-exclusivity.md) (#8248/#8249);
this doc extends the same primitives to two job types that are not
`SERVICE_START`: `MEETING_JOIN` and `TRANSCRIBE_AUDIO`.

## Context

Issue #891 (split from umbrella #558): on a GPU node where a co-resident vLLM
holds most VRAM, a MEETING_JOIN's transcribe passes repeatedly fail to get a
whisper engine ready — each rolling pass times out at the 120s
`transcribeReadyTimeout` (`internal/jobs/transcribe_audio.go:29`) — rather than
failing fast with a clear "insufficient VRAM" error or preempting to make room.
The head-of-line half of #558 was fixed by #489/#895 (MEETING_JOIN's dedicated
async lane); this is the other half.

The #558 incident (node 1084, v2.80.0, 2026-07-17): `nvidia-smi` showed
22.1/24.6 GB used (vLLM/Qwen3-8B holding ~21GB), the `citadel-transcribe`
container was "Up 18 hours" but never became ready, and every rolling pass
logged `Waiting for transcription service to become ready...` then burned its
budget. A `POST /fabric/models/deploy` issued during the window 504'd (the
head-of-line half). The issue's stated mechanism was "the transcribe/whisper
service can't load into GPU."

## 1. Verified problem statement — and where the issue's framing is wrong

Every claim below is checked against the code as of v2.132.0.

### 1a. The shipped transcribe sidecar CANNOT be VRAM-starved the way #558 says

Two facts, both load-bearing for this design:

1. **The shipped whisper sidecar is CPU-only.**
   `services/compose/transcribe.yml` sets `WHISPER_DEVICE=cpu` and
   `WHISPER_COMPUTE_TYPE=int8`, and has since the file was created (verified
   via `git log --follow` — the only commit touching it, #273, added
   `WHISPER_DEVICE=cpu`). The service source
   (`services/whisper-service/app.py:28`) defaults to `cpu` too. faster-whisper
   `base`/int8 on CPU allocates zero VRAM.

2. **The sidecar's `/health` endpoint returns 200 unconditionally** — it does
   not load the model at all (`services/whisper-service/app.py:100-102`
   returns `{"status": "ok"}` with no model touch; the model is lazy-loaded on
   the FIRST `/transcribe` call, `app.py:41-53` `_get_model`). So even a
   hypothetical GPU-configured whisper could not fail *readiness* on VRAM:
   a CUDA OOM would surface as a 500 from `/transcribe`, never as a
   `waitForReady` timeout (`internal/jobs/transcribe_audio.go:269-297`).

**Consequence:** for the configuration citadel actually ships, "did not become
ready within 120s" means the container's HTTP server was not answering at all —
a crashed/restart-looping uvicorn, a wedged process, host-RAM OOM thrash (vLLM
pins substantial host RAM alongside VRAM), or a port/bind failure. It cannot
mean "whisper is waiting for VRAM." The #558 diagnosis conflated the visible
`nvidia-smi` pressure with the readiness failure; the actual node-1084
mechanism was never confirmed (`docker logs citadel-transcribe` from the
incident window was not captured). Issue #891's own verification posture —
REAL-BUT-NEEDS-HARDWARE, "scope before implementing" — anticipated exactly
this.

### 1b. What IS real, verified in code

The **failure shape** the issue describes is real and reproducible regardless
of the underlying resource:

- Each rolling transcribe pass calls
  `MeetingJoinHandler.transcribe` → `TranscribeAudioHandler.Execute` →
  `waitForReady` (`internal/jobs/transcribe_audio.go:221`), which blocks up to
  120s per pass when the sidecar answers connections but never 200s (the
  8s `transcribeUnreachableTimeout` fast-fail at
  `transcribe_audio.go:284-289` only covers connection-refused, i.e. nothing
  listening). Passes are serialized under `transcribeMu`
  (`internal/jobs/meeting_join.go:480-484`), so a starved sidecar turns
  during-call streaming into a silent sequence of 120s stalls producing zero
  segments.
- The end-of-call batch pass fails the same way, and `Execute` deliberately
  converts that into a SUCCESS result with
  `status: "recorded_transcription_failed"`
  (`internal/jobs/meeting_join.go:626-643`) — correct for preserving the
  recording, but it means the platform sees a *completed* meeting job whose
  transcript silently never arrives. That is the "silently starves" of the
  issue title.
- A standalone `TRANSCRIBE_AUDIO` job in the same state fails after 120s with
  the bare `transcription service did not become ready within 2m0s`
  (`transcribe_audio.go:296`) — no diagnosis of WHY, even though the node
  knows its own VRAM/RAM state.

And the **VRAM co-residency class** is real *prospectively*: the moment the
transcribe stack is GPU-configured, the #558 scenario becomes mechanically
possible at the `/transcribe` call (CUDA OOM / driver-level thrash). Concrete
near-term paths to a GPU transcribe stack:

- An operator override of the materialized `transcribe.yml`
  (`WHISPER_DEVICE=cuda` — the app honors it, `app.py:28,49-50`).
- Full diarization (citadel#522, a reviewed draft): pyannote/whisperx is
  GPU-hungry in every practical deployment.
- A future `transcribe-gpu` variant for long-meeting batch speed (the 43-min
  meeting in `transcribe_audio.go:44-49`'s comment ran >30min on CPU whisper;
  GPU whisper is ~10-30x faster).

So the design below builds the general mechanism — but keyed so that it
**no-ops by construction for the shipped CPU configuration** (a `0` VRAM need
always fits, the same `requiredVRAM == 0` contract `PlanPreemption` already
pins at `internal/status/preempt.go:66-68`), and it treats "diagnose the
readiness failure honestly" as a deliverable independent of VRAM, because that
half fixes the incident class we actually observed.

### 1c. What already exists to build on (file:line)

| Primitive | Where | What it gives us |
|---|---|---|
| `PlanPreemption(candidates, required, available)` | `internal/status/preempt.go:65` | Pure decision: `required==0` ⇒ fits; pinned NEVER stopped; idle-first then largest-VRAM ordering; `Fits=false` + `Blocked` + human `Reason` on refusal. |
| `preemptForVRAM` executor | `internal/jobs/service_handler.go:966` | SERVICE_START-path executor: collect status once, `freeVRAMBytes` (`service_handler.go:1073`), `buildPreemptCandidates` (`service_handler.go:1099`), durable-stop each `plan.Stop` entry. Fail-open when free VRAM unknown. |
| `Reserve` / `Release` / `ReconcileOrphanedReservations` (#832) | `internal/jobs/reservation.go:117,208` | Job-scoped VRAM hold with durable `evicted_by_job` tags and **auto-restore** — including across a crash (reconciled at the next `citadel work` boot, `cmd/work.go` after `worklock.Acquire`). Takes no service-name exclusion (`reservation.go:144-148`): a reservation "is not necessarily a manifest service," i.e. it was DESIGNED for exactly a caller like a meeting job. Fail-closed on unknown free VRAM (`reservation.go:134-136`) — the opposite of `preemptForVRAM`, deliberately. **No production caller exists yet** (CLAUDE.md's #832 scope note); this design would be its first job-type caller alongside #8248's CLI/MCP one. |
| `resolveRequiredVRAMBytes` precedence | `internal/jobs/service_handler.go:702` | Payload `vram_mb`/`vram_gb` wins unconditionally; a citadel-side estimate applies ONLY under `CITADEL_RESOURCE_ISOLATION` (`resourceIsolationEnabled`, `service_handler.go:739`). The precedence pattern to mirror, not the table to reuse blindly — see §3. |
| `EngineVRAMEstimateMB` table | `internal/status/hotswap.go:100-107` | Per-engine provisioning budgets (vllm 22000, ollama 8000, ...). **No `transcribe` entry** — correct today (CPU), and §3 argues a flat entry would stay wrong even for GPU whisper. |
| `PlanRAMPreflight` refusal contract | `internal/status/ram.go:102` | "0 always fits; a declared requirement exceeding budget is the ONLY refusing case — a confirmed shortfall, never a guess, with a precise needs-X-has-Y message." The owner's #831 decision ("refuse fast, clear error") that §4's fail-fast arm inherits verbatim. |
| `planDiskPreflight` | `internal/jobs/disk_space.go:58` | Same fail-open/fail-closed shape, one more precedent. |
| `pinned_services` | `cmd/manifest.go` + `internal/jobs/service_handler.go` (`manifest.pinnedSet()`) | The node operator's existing "never evict this" declaration — already how a paid vLLM is protected from #577/#832 eviction. §4 leans on it instead of inventing a meeting-specific protection. |
| `SwapRateLimitedError` → structured job failure | `internal/worker/swap.go` (reason `swap_rate_limited`) | Precedent for a machine-readable refusal `reason` the backend can branch on. §5 mirrors it with `insufficient_vram`. |
| Handler construction seam | `internal/worker/handler_adapter.go:323` (transcribe), `:334-336` (meeting, behind `MeetingEnabled`), `:343` (`NewServiceHandlerWithWorkspace`) | All three handlers are built in ONE function from `LegacyHandlerOpts`, so threading a shared `*jobs.ServiceHandler` (or a narrow reserve/preflight interface) into the meeting/transcribe handlers is construction-site plumbing, not architecture. |
| Backend cloud-transcription fallback | `transcribe_audio.go:36-39` (comment) | The backend already falls back to cloud transcription when node transcription fails fast — the reason fail-fast is USEFUL, not just honest. |

## 2. Decision 1 — fail-fast is the default; preemption is a separately-gated opt-in

**Recommendation: refuse fast with a precise error (the `PlanRAMPreflight`
posture); never evict a co-resident engine for a meeting unless the operator
has explicitly opted in AND the engine is not pinned.**

Rationale, in order of weight:

1. **The co-resident vLLM is plausibly a paid inference service; the meeting
   bot is a convenience.** Evicting ~21GB of serving engine — and its warm
   model — to run a whisper pass is the wrong default trade on any node we
   didn't explicitly configure otherwise. The codebase already encodes this
   value judgment: `PlanPreemption` protects `pinned_services` absolutely, and
   every eviction-shaped behavior shipped to date (#416 auto-stop, #831
   isolation, #577's estimate fallback) is default-OFF.
2. **Fail-fast is actionable; silent starvation is not.** A job FAILURE with
   `reason: insufficient_vram` and "transcribe needs 4.0GB, node has 2.4GB
   free; vllm holds 21.2GB" lets the backend do what it already knows how to
   do: fall back to cloud transcription (§1c last row) or dispatch the meeting
   to a different node. Today it instead gets a *successful* meeting job with
   `recorded_transcription_failed` buried in the result 4 hours later.
3. **Preemption for a bounded session should restore afterward, and we have a
   primitive for exactly that** — `Reserve`/`Release` (#832), unused by any
   job type so far. A meeting is the ideal first caller: reserve at meeting
   start, release (auto-restarting vLLM) at meeting end, crash-reconciled by
   the next `citadel work` boot. Using bare `preemptForVRAM`-style durable
   stops instead would leave vLLM down after every meeting — strictly worse.
4. **Timing: check BEFORE joining the call.** A meeting that joins, records,
   and then can't transcribe still produced value (the recording + Opus
   backup, `meeting_join.go:600-605`); but if the node *knows at dispatch
   time* the transcribe stack can't fit, refusing before the bot ever enters
   the meeting gives the platform time to re-route while the meeting is still
   joinable by another node. So the preflight/reserve hook is at the top of
   `MeetingJoinHandler.Execute`, not per-pass. (Per-pass behavior — log and
   continue — is unchanged; a mid-meeting VRAM change should degrade exactly
   as today, not abort a half-recorded call.)

Failure-direction summary (mirrors #831's tables):

| Signal state | Fail-fast arm (gated, §4) | Preempt arm (gated, §4) |
|---|---|---|
| VRAM need resolves to 0 (shipped CPU whisper; no payload, no estimate) | proceed (no check at all) | proceed (nothing reserved) — `Reserve`'s own `requiredVRAMBytes==0` short-circuit, `reservation.go:124-127` |
| Need declared, free VRAM UNKNOWN | proceed + log (fail open, `preemptForVRAM`'s posture — a meeting is not an explicit "guaranteed hold" ask) | refuse the reservation (fail closed, `Reserve`'s posture `reservation.go:134-136`) and **fall back to the fail-fast arm's proceed+log**, since the operator asked for best-effort preemption, not a guarantee |
| Need declared, confirmed shortfall, preempt OFF | **job FAILURE, `insufficient_vram`, needs-X-has-Y + holder names** | n/a |
| Need declared, shortfall, preempt ON, fits after evicting non-pinned | proceed | `Reserve(meetingJobID, need)` evicts idle-first, restores on `Release` at meeting end |
| Need declared, shortfall coverable only by evicting PINNED services | job FAILURE naming the pinned holders (`PreemptPlan.Blocked`) | same — pinned is absolute, unchanged from #577 |

## 3. Decision 2 — where the VRAM need comes from

**Recommendation: payload wins; otherwise a *configuration-derived* estimate
that is 0 for the shipped CPU sidecar. Never a flat per-engine constant, and
never a probe.**

Three candidate sources, evaluated:

1. **Payload field (wire contract, backend-forwarded).** `MEETING_JOIN` and
   `TRANSCRIBE_AUDIO` payloads gain the SAME optional keys `SERVICE_START`
   already parses — `vram_mb` (preferred) / `vram_gb` — read by the existing
   `parseRequiredVRAMBytes` (`service_handler.go:668`; it takes a
   `map[string]string`, which is exactly `nexus.Job.Payload`'s shape, so it is
   reusable verbatim). Semantics for these job types: "the VRAM this job's
   transcription stack needs on this node." Like #577's, this is inert until
   the backend sends it; unlike #577's it triggers *refusal*, not preemption,
   when unmet (§2). **This is the authoritative source when present** — the
   backend knows whether diarization was requested, which whisper size the
   org's plan uses, etc.
2. **Citadel-side estimate — but keyed on the transcribe stack's own
   configuration, not the engine name.** A flat `"transcribe": N` entry in
   `engineVRAMEstimateMB` (`hotswap.go:100`) would be WRONG in both
   directions: nonzero for the shipped CPU config (fabricating a need that
   refuses meetings on healthy nodes — the exact "clamped-up fabricated
   number" failure `RAMBudgetBytes`'s doc warns about, `ram.go:57-62`), and a
   single constant can't span whisper `base`/int8 (~1GB) to `large-v3`/fp16 +
   pyannote (~10GB+). Instead, a new resolver:

   ```go
   // internal/jobs/transcribe_vram.go (sketch)
   // TranscribeVRAMEstimateBytes returns the VRAM the node's OWN transcribe
   // stack would need, derived from the materialized transcribe compose/env
   // (WHISPER_DEVICE, WHISPER_MODEL, WHISPER_COMPUTE_TYPE — the same values
   // app.py will read). WHISPER_DEVICE=cpu (the shipped default) => 0,
   // which every downstream contract treats as "always fits".
   func TranscribeVRAMEstimateBytes(configDir string) uint64
   ```

   with a small model-size×compute-type table (pinned by a test the way
   `TestEngineCacheDirsMatchComposeMounts` pins `EngineCacheDirs`). Gated the
   same way `resolveRequiredVRAMBytes` gates its estimate: applied ONLY under
   the opt-in flag (§4), so a node that never opted in sees zero new behavior
   even after a future GPU-whisper compose lands.
3. **Probe (attempt the load, catch CUDA OOM).** Rejected: probing IS the
   silent starvation we have — it costs a model load against a contended GPU,
   its failure mode (driver-level thrash on a nearly-full card) is exactly the
   degradation being designed away, and `/health`'s no-model-touch design
   (§1a) exists specifically so readiness stays cheap.

Precedence mirrors `resolveRequiredVRAMBytes` (`service_handler.go:702`)
exactly: payload > (flag-gated) config-derived estimate > 0.

## 4. Decision 3 — gating: which flag, what default

**Recommendation:**

| Variable | Default | Gates |
|---|---|---|
| *(none — always on)* | — | **Readiness-failure diagnosis enrichment** (§4a): when `waitForReady` times out on a GPU node, the returned error is annotated with free VRAM + the top VRAM holders (and, where cheap, `docker inspect` state via the existing `internal/servicediag` primitives). Changes only an error string on an already-failing path. |
| `CITADEL_RESOURCE_ISOLATION` | unset (OFF) | **The fail-fast preflight's estimate source** (§3 option 2). Reused, not a new flag: this IS #831's "preflight-only, refuse fast" posture extended to two more job types, and the flag's documented contract ("enables refuse-on-confirmed-shortfall behavior an operator has reviewed") covers it. Payload-declared `vram_mb` refuses even without the flag, mirroring how #577's payload budget already acts ungated on `SERVICE_START`. |
| `CITADEL_MEETING_VRAM_PREEMPT` | unset (OFF) | **The preempt arm** (§2): `Reserve` at meeting start / `Release` at meeting end. Requires the need to resolve nonzero (so it is doubly inert today). Truthy: `1`/`true`/`yes`/`on`, per house convention. |

**Why default-OFF when "silent starvation" is itself the bug:** the honest
answer is that the *silence* is fixed by the always-on tier, which is safe to
ship ungated because it changes no success/failure outcome — a node that
today fails with a bare "did not become ready within 2m0s" will fail with the
same error plus the VRAM/RAM/container facts an operator needs. The
*behavior-changing* tiers (refusing a meeting earlier than it would otherwise
fail; evicting a serving engine) follow the codebase-wide rule that anything
which can newly refuse or stop a service on a node that has never seen that
happen ships OFF (`SERVICE_AUTO_STOP_WHEN_IDLE`, `CITADEL_RESOURCE_ISOLATION`,
`CITADEL_GROUNDING_GUARDRAIL`, `CITADEL_SIGN_AEP_RECEIPTS` — every precedent
agrees). Additionally, both gated tiers are **inert for the shipped CPU
sidecar regardless of the flags** (need resolves to 0), so default-ON would
buy nothing today while breaking the convention.

**Why reuse `CITADEL_RESOURCE_ISOLATION` rather than a new
`CITADEL_MEETING_VRAM_GUARD`:** same posture, same failure direction, same
operator decision ("this node may refuse work on a confirmed resource
shortfall"), and flag proliferation has its own cost — #831 already
deliberately chose ONE flag over two for exactly this reason (its CLAUDE.md
section: "one opt-in flag, default OFF... both mechanisms"). The counter-case
(an operator wants RAM cgroups but not meeting refusals, or vice versa) is
listed as an open question (§7 Q3) rather than silently decided.

### 4a. The always-on diagnosis tier, concretely

`waitForReady`'s timeout return (`transcribe_audio.go:296`) becomes:

```
transcription service did not become ready within 2m0s
  (gpu: 2.4GB free of 24.6GB; top VRAM holders: vllm 21.2GB;
   citadel-transcribe container: running, restarts=0, device=cpu)
```

Implementation shape: a `diagnoseFn func() string` field on
`TranscribeAudioHandler` (nil ⇒ no annotation — keeps the handler's tests
hermetic and the package free of a status-collection import), wired at the
construction seam (`handler_adapter.go:323`) to a closure over the same
`freeVRAMBytes`+footprint collection `preemptForVRAM` uses. One collection,
only on the already-2-minutes-slow failure path — no new steady-state
`docker stats`/`nvidia-smi` sweeps (the #416/#612 rule about riding existing
collections applies; here we don't even ride one, we pay only on failure).

This tier is the part that would have turned the node-1084 incident from a
4-hour silent wedge into a first-pass log line naming vLLM's 21GB and the
container's actual state — *whatever* the true mechanism was, VRAM or not.

## 5. Wire contract (backend side — other repo, described not designed)

1. **Request:** optional `vram_mb`/`vram_gb` on `MEETING_JOIN` and
   `TRANSCRIBE_AUDIO` payloads (string values, like every `nexus.Job.Payload`
   field), meaning "VRAM this job's transcription stack needs." Absent ⇒ node
   decides per §3. The backend owns knowing when a job implies a GPU stack
   (diarization tier, org plan).
2. **Refusal:** a job FAILURE whose output carries
   `reason: "insufficient_vram"` plus the `PlanPreemption`/`PlanRAMPreflight`
   style message (needs X, has Y free, holders/blocked list) — the
   `swap_rate_limited` precedent (`internal/worker/swap.go`): a structured
   reason the backend can branch on. Expected backend behaviors (their
   choice): fall back to cloud transcription (already exists per
   `transcribe_audio.go:36-39`), re-dispatch to another meeting-capable node,
   or surface "this node can't host the notetaker while <engine> is serving"
   to the user.
3. **No new heartbeat field is required** for v1. If the platform later wants
   to route meetings away from VRAM-full nodes *before* dispatch, the
   heartbeat already carries per-service `Footprint.VRAMBytes` and
   `GPUMetrics` free VRAM; that is a scheduler concern, out of scope here
   (same boundary #832 drew: node primitive here, fabric placement is the
   platform's).

## 6. Reuse map — wired-up-existing vs genuinely new

**Reused unchanged:**
- `status.PlanPreemption` + `PreemptCandidate`/`PreemptPlan`
  (`internal/status/preempt.go`) — the entire decision, including pinned
  protection and ordering.
- `ServiceHandler.Reserve`/`Release`/`ReconcileOrphanedReservations` +
  `ActiveReservations` heartbeat surfacing (`internal/jobs/reservation.go`,
  `cmd/work.go`) — the whole preempt-and-restore lifecycle, crash-safety
  included. MEETING_JOIN runs inside `citadel work`'s consume loop, which
  holds `internal/worklock`, so the meeting caller does NOT reopen the
  documented CC/worklock reconcile race — with one caveat, §7 Q5.
- `parseRequiredVRAMBytes` (`service_handler.go:668`) — payload parsing,
  verbatim.
- `freeVRAMBytes`, `buildPreemptCandidates`, `collectNodeStatus`
  (`service_handler.go:1073,1099,1021`) — via `Reserve`, untouched.
- `resourceIsolationEnabled` (`service_handler.go:739`) — the gate check.
- `pinned_services` (`manifest.pinnedSet()`) — the protection an operator
  already uses for vLLM; no meeting-specific pin concept.
- `internal/servicediag` inspect/log-tail primitives — optional input to §4a's
  enrichment.

**Mirrored (same shape, new instance):**
- The `resolveRequiredVRAMBytes` precedence (payload > gated estimate > 0)
  re-instantiated for the transcribe stack.
- The `PlanRAMPreflight` refusal message contract ("needs X, node has Y",
  refuse only on confirmed shortfall).
- The `swap_rate_limited` structured-failure-reason pattern, as
  `insufficient_vram`.

**Genuinely new:**
1. `TranscribeVRAMEstimateBytes` — the configuration-derived (device × model ×
   compute-type) estimator, plus its pinning test. Small, pure, table-driven.
2. The meeting-lifecycle reservation wiring: a narrow interface (e.g.
   `vramGuard` with `Preflight(need) error` / `Reserve(jobID, need)` /
   `Release(jobID)`) threaded from `LegacyHandlerOpts` construction
   (`handler_adapter.go:334-343`, where the `ServiceHandler` is already built)
   into `MeetingJoinHandler` and `TranscribeAudioHandler`; nil ⇒ everything
   off (hermetic tests, `--node-dir`-refused contexts, GPU-less nodes).
   Deterministic reservation id `"meeting:" + job.ID`, following
   `ExclusiveReservationJobID`'s pattern (`model_exclusivity.go:59`).
3. `waitForReady`'s failure-path diagnosis hook (§4a).
4. The `vram_mb` payload contract on two more job types (node side of a
   cross-repo contract, inert until the backend sends it — same posture as
   #577's original landing).

## 7. Open questions for Jason

1. **Was node-1084's starvation actually VRAM?** The shipped sidecar is
   CPU-only and `/health` never touches the model (§1a), so the #558
   mechanism as written cannot reproduce on today's config — the readiness
   failure was something else (host-RAM thrash beside a 21GB-resident vLLM is
   the leading suspect; a RAM-sibling of this design would then matter more
   than the VRAM arm). Before building the preempt arm (P2), is it worth a
   live re-check on node 1297 (vLLM serving + a meeting join, capture
   `docker logs citadel-transcribe` + `free -m` this time)? P0/P1 are
   justified either way; P2's priority depends on the answer.
2. **Is evicting a co-resident vLLM for a meeting EVER the right call on our
   fleet, or should the preempt arm be dropped entirely** (fail-fast only,
   backend re-routes/falls back)? §2 recommends keeping it as a double-gated
   opt-in because `Reserve`/`Release` makes it *restorative* rather than
   destructive — but if the answer is "never," P2 disappears and the design
   is purely preflight + diagnosis.
3. **Flag identity:** reuse `CITADEL_RESOURCE_ISOLATION` for the fail-fast
   estimate tier (§4's recommendation), or a dedicated
   `CITADEL_MEETING_VRAM_GUARD` so an operator can opt into RAM cgroups
   without meeting refusals? Reuse keeps one flag family; a split is more
   granular. This mirrors #831's own Q-and-decision, so your call there
   ("one flag") suggests reuse — confirm.
4. **Is GPU whisper a supported configuration we intend to ship** (a
   `transcribe-gpu` compose variant or `WHISPER_DEVICE=cuda` interpolation,
   plus #522 diarization), or is CPU whisper the permanent posture? If
   permanent, §3's estimator only ever returns 0 from our own composes and
   the payload field is the sole live source — still worth building (the
   backend can declare diarization needs), but the estimator's table shrinks
   to "whatever #522 ships."
5. **Control-center caveat for P2:** the CC TUI's own consume loop
   (`cmd/controlcenter.go`, `workerHeld == false` path) runs MEETING_JOIN
   without holding `worklock` — the exact door
   `ReconcileOrphanedReservations`'s doc comment flags. A meeting reserved
   from the CC path, concurrent with a fresh `citadel work` boot, could have
   its reservation "reconciled" (vLLM restored) mid-meeting. Options: gate
   the preempt arm to the worklock-holding path only (recommended for v1),
   make the CC path acquire `worklock`, or accept-and-document. Same
   deferred-follow-up territory as #851's identical note — but P2 should not
   ship pretending the door isn't there.

## 8. Phased plan

- **P0 — diagnosis (always-on, no flag, no behavior change):**
  `waitForReady` timeout error enrichment (§4a) + the `diagnoseFn` seam +
  tests (hermetic: injected closure, no real docker/nvidia-smi). Fixes the
  observed incident class regardless of mechanism. Smallest reviewable unit.
- **P1 — fail-fast preflight (gated):** `TranscribeVRAMEstimateBytes` +
  payload `vram_mb` parsing on `TRANSCRIBE_AUDIO`/`MEETING_JOIN` + the
  pre-join preflight refusal with `reason: insufficient_vram` + the
  `vramGuard` interface threading. Inert on every current node (need resolves
  0) until a payload arrives or an operator opts in on a GPU-whisper node.
- **P2 — preempt-and-restore (double-gated, pending Q1/Q2/Q5):**
  `CITADEL_MEETING_VRAM_PREEMPT` wiring `Reserve` at meeting start /
  `Release` after `backupAndPrune`, worklock-holding path only. First
  production job-type caller of #832.
- **P3 — cross-repo (aceteam, other agent):** backend forwards `vram_mb` when
  a job implies a GPU transcribe stack; backend branches on
  `insufficient_vram` (cloud fallback / re-dispatch). Contract in §5.
- **Explicitly out of scope:** host-RAM starvation of the CPU sidecar (the
  likely true node-1084 mechanism — belongs with #831's RAM machinery if Q1
  confirms it), fabric-level meeting placement/scheduling, per-pass
  mid-meeting re-preflight, and any change to the whisper sidecar's Python
  (`/health` stays model-free by design).
