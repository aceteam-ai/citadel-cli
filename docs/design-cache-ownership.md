# Design: Model-Cache Ownership + GC (citadel #682 / #683)

Status: DRAFT for review. No code in this doc; it defines the shape of the fix so
the phased issues below can be filed and implemented independently.

> **Update 2026-08-30:** P0 and P1 (plus P4's `hfCacheBaseDir()` sync and P6's
> llamacpp half) have SHIPPED — see §7 for what landed and where. §8 is the
> implementation design for P2 (the durable cache index), appended after the
> original draft; §§1–6 are left as written and describe the pre-P0 state.
>
> **Update 2026-08-30 (later):** P2a has also SHIPPED (`internal/cacheindex`,
> #936 + the #940 follow-ups) — §7 updated. §9 (P3, reporting) and §10 (P5,
> GC) are the implementation designs for the two remaining value-unlock
> phases, appended in the same convention as §8; §11 collects their open
> questions.

## 1. Current state

### 1.1 Where weights live today

There is no single engine→cache-path table. The mapping exists only as 13
independently-authored compose mounts plus three unrelated Go download paths:

| Path | Engine(s) | Container mount | Source |
|---|---|---|---|
| `~/citadel-cache/huggingface` | vllm, sglang, diffusers, extraction, kokoro (HF half), transcribe, unlimited-ocr | `/root/.cache/huggingface` | `services/compose/vllm.yml:14`, `sglang.yml:11`, `diffusers.yml:26`, `extraction.yml:12`, `kokoro.yml:56`, `transcribe.yml:9`, `unlimited-ocr.yml:34` |
| `~/citadel-cache/bonsai` | bonsai | `/models` | `services/compose/bonsai.yml:36`, `internal/jobs/model_cache_pull.go:159-168` (`bonsaiCacheDir`) |
| `~/citadel-cache/llamacpp` | llamacpp | `/models` (raw GGUF dir, not an HF cache) | `services/compose/llamacpp.yml:16` |
| `~/citadel-cache/ollama` | ollama | `/root/.ollama` | `services/compose/ollama.yml:11` |
| `~/citadel-cache/lmstudio` | lmstudio | `/root/.cache/lm-studio` | `services/compose/lmstudio.yml:11` |
| `~/citadel-cache/kokoro` | kokoro (voices/data) | `/data` | `services/compose/kokoro.yml:58` |
| `~/citadel-cache/tei` | tei | `/data` (+`HUGGINGFACE_HUB_CACHE=/data`) | `services/compose/tei.yml:41,43` |
| `~/citadel-cache/claudecode` | claudecode-service | `/home/claude/.claude` | `services/compose/claudecode.yml:36` |
| `~/citadel-cache/instances/<id>` | BYOC agent-host launches | arbitrary, allowlisted | `internal/jobs/service_payload.go:18,349` |
| `~/citadel-cache/<model_type>/<file>` | legacy `DOWNLOAD_MODEL` job | n/a, host-only | `internal/jobs/download_model.go:29-31` |

Most inference engines share one bucket (`citadel-cache/huggingface`); GGUF and
ollama are structurally separate stores.

### 1.2 Root cause of the duplicated HF cache (the 40G/13G split in #682)

This is fully traceable, not a mystery, and it's the load-bearing finding for
the whole design:

- **Container-side** (`~/citadel-cache/huggingface`): every self-provisioning
  container (vllm, sglang, diffusers, tei, transcribe, unlimited-ocr,
  extraction — `selfProvisioningEngines`, `internal/jobs/model_cache_pull.go:70-77`)
  writes here because that's the compose mount target and it's the container's
  own default HF cache path.
- **Host-side** (`~/.cache/huggingface`): `MODEL_CACHE_PULL`'s `pullHuggingFace`
  handler (for `vllm`/`llamacpp`, `internal/jobs/model_cache_pull.go:363-394`)
  runs `hf download <repo>` as a **host** subprocess with no `HF_HOME` /
  `HUGGINGFACE_HUB_CACHE` set on `cmd.Env` — confirmed by grep, the only
  reference is a *read* of `os.Getenv("HF_HOME")` in `hfCacheDir` (`:415-435`),
  never a write. `resolveHFDownloader`/`BuildHuggingFaceDownloadCommand`
  (`:259-273`, `:446-448`) never sets a custom env either. So the pull lands
  at the host's plain default (`~/.cache/huggingface`), a directory the vLLM
  **container** never sees.

Consequence: a `MODEL_CACHE_PULL` pre-fetch and the engine's own first-start
download are two independent, unreconciled copies of the same weights. This is
not a rare edge case — it is the expected outcome of the current code path for
every vllm/llamacpp pre-fetch.

**#828/#840 does not fix this, and its `hfCacheBaseDir()` currently disagrees
with #682's answer.** `fix/828-model-pull-disk-guard` (unmerged, separate
worktree) adds a disk-preflight to `pullHuggingFace` — `planDiskPreflight` in
`internal/jobs/disk_space.go`, size estimation via `internal/jobs/hf_repo_size.go`,
pattern filtering via `internal/jobs/model_cache_pull_patterns.go` — genuinely
useful prior art for the "don't lie about disk headroom" half of #683. But its
`hfCacheBaseDir()` (mirroring `huggingface_hub`'s real precedence chain:
`HF_HUB_CACHE` > `HUGGINGFACE_HUB_CACHE` > `$HF_HOME/hub` >
`$XDG_CACHE_HOME/huggingface/hub` > `~/.cache/huggingface/hub`) **still
defaults to `~/.cache/huggingface/hub`** — i.e. it measures free space on the
wrong volume once #682 redirects the actual download. This is a direct
ordering dependency, not a "composes with," and needs a named owner: whichever
issue lands second must update `hfCacheBaseDir()`'s default. (Cite #828's
pieces by function/file, not line number — that branch is still moving.)

Separately, `internal/resmon` (merged, #833) already reports disk headroom on
`~/citadel-cache` falling back to `$HOME` (`hostDiskPath`,
`internal/resmon/resmon.go:268-318`) — general read-only telemetry on
`/resources`, gating nothing. It already agrees with #682's canonical
location; `hfCacheBaseDir()`'s default is the outlier.

### 1.3 `MODEL_CACHE_PULL` / `MODEL_CACHE_EVICT` today

`ModelCachePullHandler.Execute` (`internal/jobs/model_cache_pull.go:105-131`)
dispatches by engine: `pullOllama` (`:293-311`), `pullHuggingFace` (`:363-394`,
the divergent-cache path above), `pullBonsai` (`:174-205`, single-file GGUF via
`--local-dir bonsaiCacheDir()`), or a no-op success for the self-provisioning
group (`skipSelfProvisioned`, `:94-103`). No disk check exists on this branch
pre-#828. No size budget, no GC, no LRU, anywhere.

`ModelCacheEvictHandler.Execute` (`internal/jobs/model_cache_evict.go:25-45`)
only handles `ollama` (`ollama rm`) and `vllm`/`llamacpp`
(`os.RemoveAll(hfCacheDir(modelName))`); everything else — **including
bonsai** — hits `default:` and errors (`:42-44`), confirming the CLAUDE.md
note. No size accounting, no LRU, no threshold.

**No cache index or manifest exists anywhere.** `citadel status`'s cache
section (`printCacheInfo`, `cmd/status.go:571-609`) is a display-only `du -sh`
over `~/citadel-cache` at render time — not persisted, not consulted by any
pull/evict decision, and it can't see `~/.cache/huggingface` at all, so it
can't even show the duplication that motivated #682.

A third, independent download path exists: `DOWNLOAD_MODEL`
(`internal/jobs/download_model.go:16-45`) does a raw `curl` into
`~/citadel-cache/<model_type>/<file>`, unrelated to `MODEL_CACHE_PULL`. Worth
reconciling, not fixing here.

### 1.4 `warm_on_demand` today (#683)

`collectInstalledEngines` (`internal/status/hotswap.go:158-204`) gates
"installed" on `engineComposeMaterialized` (`:206-212`):

```go
func (c *Collector) engineComposeMaterialized(name string) bool {
	path := filepath.Join(c.configDir, "services", name+".yml")
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
```

This confirms the issue's claim exactly: no image check, no weights check, no
disk check. `resolveInstalledModel` (`:214-226`) additionally resolves a
model id from the `.env`/compose default, and `:176-179` (#690) excludes a
model already claimed by a running service — both are metadata/dedup checks,
not availability checks.

The ETA table is `defaultLoadEstimate` (`internal/worker/swap.go:255-264`, not
`loadEstimate:150-157` as the issue text says — correct the citation when
filing the follow-up):

```go
func defaultLoadEstimate(backend string) time.Duration {
	switch backend {
	case "bonsai":
		return 3 * time.Minute
	case "vllm", "sglang", "unlimited-ocr":
		return 90 * time.Second
	default:
		return 60 * time.Second
	}
}
```

`swapBackgroundMaxDur = 15 * time.Minute` (`swap.go:52`) is the existing
ceiling before a background swap's single-flight lock force-releases. No
six-clause predicate exists yet; only compose-materialized + model-resolves +
not-already-claimed are implemented today. VRAM-fit is checked later, at
swap-execution time (`PlanPreemption`), not at advertisement time — a
different decision point than what #683 is about.

### 1.5 Adjacent, non-overlapping systems

- `internal/worker/swap_persist.go`'s `lastUsed` (→ `swap-lru.json` at
  `network.GetNodeConfigDir()`) is **VRAM residency LRU**, keyed by engine
  name, driving preemption ordering. It has no relationship to on-disk weight
  files — it answers "which resident engine to evict from VRAM," not "which
  cached weights to delete from disk." It is, however, the precedent to copy
  for *where* a new on-disk index should live (see §2.2).
- `internal/status/footprint.go` has zero disk reporting — CPU/RAM/VRAM +
  idle only.
- `services/ports.go` is a port table only; there is no
  `ServiceCacheDirs`-equivalent today.

## 2. Proposed ownership model

### 2.1 Canonical path per engine family (fixes §1.1/§1.2)

One source of truth, `services/caches.go`, mirroring `services/ports.go`'s
shape:

```go
// CacheDir returns the canonical host cache directory for an engine family.
var EngineCacheDirs = map[string]string{
    "vllm":         "huggingface",
    "sglang":       "huggingface",
    "diffusers":    "huggingface",
    "extraction":   "huggingface",
    "transcribe":   "huggingface",
    "unlimited-ocr": "huggingface",
    "bonsai":       "bonsai",
    "llamacpp":     "llamacpp",
    "ollama":       "ollama",
    ...
}
```

A test (mirroring the port-collision test) asserts every entry agrees with its
compose file's actual bind mount — string-matched against the embedded YAML,
not hand-copied — so this table cannot silently drift from the compose files
the way the current 13-file-plus-3-code-paths arrangement already has.

**Fix the divergence directly:** `pullHuggingFace` sets `HF_HOME`/
`HUGGINGFACE_HUB_CACHE` on the subprocess `cmd.Env` to
`~/citadel-cache/huggingface` (the container's mount), not the host default.
One-line-conceptually, but it's the fix that reclaims the observed 13G and
stops the doubling for every future pull. `hfCacheBaseDir()` (from #828, once
merged) must be updated in the same change to default to the same path, or the
disk preflight goes back to measuring the wrong volume.

### 2.2 Cache index (fixes "no manifest" in §1.3)

A durable index at `<GetNodeConfigDir()>/cache-index.json`, deliberately
**not** `platform.ConfigDir()`. This is the same invoker-scoped-vs-convergent
bug class documented at length in this repo's own CLAUDE.md (#726, #845): the
pull handler runs inside `citadel work` (frequently root/systemd-scoped) while
`citadel status`/`citadel services` run interactively as the invoking user —
`ConfigDir()` resolves to different directories for those two, so a reader
would see nothing and report "unknown" forever. `swap-lru.json` already sets
this precedent for exactly this reason; the cache index follows it.

Shape (one entry per cached artifact — a model or a bonsai GGUF file, not per
engine):

```json
{
  "engine": "vllm",
  "model": "meta-llama/Llama-3.1-8B-Instruct",
  "path": "~/citadel-cache/huggingface/hub/models--meta-llama--Llama-3.1-8B-Instruct",
  "size_bytes": 16800000000,
  "pulled_at": "2026-08-20T10:00:00Z",
  "last_used": "2026-08-27T14:32:00Z",
  "pinned": false
}
```

Written by `MODEL_CACHE_PULL` on success, updated by every path that resolves
a model to actually serve it: `resolveInstalledModel`, the swap-in path
(`SwapManager.EnsureResident`), and a direct request through the gateway chat
route. Read (never blocking a decision on its absence) by `citadel status`,
the heartbeat cache-size report, and the GC policy in §3.

Parse leniently: a missing or corrupt index degrades to "no recency data" for
every entry, never fails startup — same reasoning already applied to
`TokenHashEntry.UnmarshalJSON` (#815). Writes are best-effort and logged, not
fatal, matching the LRU-persist precedent (`persistLogf`).

**`last_used` signal — explicit choice, and the rejected alternative.**
Filesystem atime was considered and rejected: most citadel nodes' cache mounts
are ext4/xfs with `relatime` (or `noatime` on some cloud images), so atime
either barely updates or never does — unusable as a real recency signal. The
index's `last_used` field, touched at the call sites above, is the only
durable source. This is a new signal, not a repurposing of `swap-lru.json`'s
`lastUsed` (that field is VRAM-residency-by-engine-name; this is
disk-weights-by-model-path — different axis, different lifecycle, deliberately
not reused).

**Scope note (narrower than "dedup across engines" might imply).** The
duplication in #682 is one engine's host pull vs. its own container mount —
same HF layout, same engine, two directories. That's what §2.1 fixes, fully.
Cross-*family* dedup (sharing a GGUF between bonsai and llamacpp, or unifying
ollama's internal blob store with the HF layout) would require weight
reformatting or symlink schemes and is explicitly out of scope — #682 already
excludes "mirroring, or a registry." A shared GGUF pool for bonsai/llamacpp is
a plausible future increment (their model sets barely overlap today given the
PrismML fork requirement) but is not part of this design.

## 3. GC policy

**Default OFF**, behind `CITADEL_CACHE_GC` (truthy `1`/`true`/`yes`/`on`),
matching every other destructive/advisory toggle in this codebase
(`SERVICE_AUTO_STOP_WHEN_IDLE`, `CITADEL_GROUNDING_GUARDRAIL`,
`CITADEL_ENERGY_SAMPLING`). Deleting multi-GB weight files must never be an
implicit side effect of upgrading the binary.

**Trigger:** disk pressure, read from the same signal `resmon` (#833) already
computes — reuse `hostDiskPath`'s disk-percent, do not add a second probe.
Threshold `CITADEL_CACHE_GC_DISK_PERCENT` (default e.g. 90%, tunable).

**Ordering when triggered:** idle-first, then LRU by the index's `last_used`
(§2.2), largest-size-first as the tie-break — deliberately mirroring
`PlanPreemption`'s ordering (idle-first, then largest-VRAM-first,
name-ascending tie-break) so the two GC-shaped decisions in this codebase
(#577 VRAM preemption, this disk GC) read the same way to an operator.

**Never evict:**
- A pinned entry (§3.1).
- **Weights for a currently-resident engine — a hard rule, not a policy
  choice.** vLLM/llama.cpp mmap or hold weight files open for the container's
  lifetime; `os.RemoveAll` under a live process is a leaked inode at best and
  a mid-inference crash at worst. This is a stronger guarantee than idle
  ordering alone: an engine can be idle-but-resident (e.g. behind the #416
  idle-detection threshold but not yet auto-stopped), and its weights must
  stay untouched either way. Practically: cross-reference the index against
  `status.DiscoverLocalEngines`'s currently-running set before evicting
  anything for that engine/model.

### 3.1 Pinning — a new axis, not a reuse of `pinned_services`

`pinned_services` (#577) pins a **service** against VRAM preemption; it says
nothing about that service's on-disk weights, and a service being
VRAM-preemptible does not imply its weights are disk-evictable. Cache pinning
needs its own field: a `pinned_models` (or per-index-entry `pinned: true`,
settable via a CLI flag or the same manifest section) list, checked
independently. Interaction rule, stated explicitly: **a service preempted from
VRAM by #577 keeps its weights on disk** — it is expected to come back, and
disk GC is a separate, coarser-grained decision that should not be a side
effect of a VRAM eviction.

### 3.2 Interaction with #688 (LRU-persist) and hotswap

No conflict, different layers: #688's `swap-lru.json` orders VRAM candidates
for `SwapManager.preempt`; this design's index orders disk-GC candidates. Both
read `network.GetNodeConfigDir()`, and both are per-process advisory state that
degrades gracefully on absence. A model evicted from VRAM (via hotswap or
preemption) is untouched by this GC unless it *also* crosses the disk-pressure
threshold and is *also* not the resident engine at the moment GC runs.

## 4. `warm_on_demand` fix (#683)

Six clauses, in order, each a distinct failure reason (per the issue's own
table) — this design only speaks to the two clauses it unblocks:

- **Image present**: `docker image inspect <image>`, replacing the
  `os.Stat`-on-YAML proxy. Needs a short TTL cache in front of it (same
  pattern as `cmd/gateway_chat.go`'s model-resolution cache) since this would
  otherwise run per engine per ~30s heartbeat tick.
- **Weights present**: query the cache index from §2.2 for
  `(engine, resolved_model)`. This is the clause #682 exists to unblock — it
  cannot be answered honestly without a place weights are known to live.
  Absent an index entry, fall back to a live existence check against the
  canonical path from §2.1 (covers a node upgraded mid-flight, before the
  index has backfilled).
- **Disk headroom**: reuse #828's preflight machinery
  (`planDiskPreflight`/size estimation) evaluated for "if we had to pull this
  model's weights," using the corrected `hfCacheBaseDir()` default from §2.1.

**Honest ETA, including the "not actually warm_on_demand" case.** When image
or weights are absent, `estimated_load_seconds` must include pull time (via
#828's size-estimation path), and the payload must flag that it does — the
issue's own point that a 90s ETA and a 12-minute ETA are different products.
Beyond that: if the estimated pull+load time exceeds `swapBackgroundMaxDur`
(15m, `swap.go:52`), the honest answer is not `warm_on_demand` with a large
number — it should be reported as a **different, more conservative state**
(e.g. `installed_not_running` with a `requires_pull` flag, not
`warm_on_demand`), because a number nobody should act on synchronously is
worse than an honest "not now." This ceiling check is new; the issue gestures
at it but doesn't name a resolution — this design proposes one.

## 5. Phased issue breakdown

Ordered; each row is a PR-sized issue.

| # | Scope | Depends on |
|---|---|---|
| P0 | Redirect `pullHuggingFace`'s subprocess env to `~/citadel-cache/huggingface`; report the pre-existing duplicate cache (`~/.cache/huggingface`) once, informationally. Reclaims the observed 13G immediately. | none |
| P1 | `services/caches.go`: single engine→cache-dir table + test pinning it against every compose file's mount. | none (can land parallel to P0) |
| P2 | Cache index at `GetNodeConfigDir()/cache-index.json`: schema, lenient read, best-effort write, wired into `MODEL_CACHE_PULL` success + the three "resolves a model to serve it" call sites (§2.2). | P1 |
| P3 | `citadel status`/heartbeat: report cache size + location per engine (including `~/.cache/huggingface`, which `printCacheInfo` currently can't see) + flag detected duplicates. Closes #682 items 3–4. | P1, P2 |
| P4 | Sync `hfCacheBaseDir()` (from #828, once merged) to the P0 canonical path; resolve which issue "owns" this rename — flag now so it isn't dropped between two branches. | P0, #828 |
| P5 | GC policy: disk-pressure trigger (reusing `resmon`'s disk-percent), idle+LRU ordering from P2's index, pinning (§3.1), never-evict-resident hard rule. Default OFF behind `CITADEL_CACHE_GC`. | P2, P4 |
| P6 | `MODEL_CACHE_EVICT` completeness: add `bonsai` (currently hard-errors) and the self-provisioning engine group; wire eviction to update the P2 index. | P1, P2 |
| P7 (#683) | Six-clause `warm_on_demand` predicate: image-present (`docker image inspect` + TTL cache), weights-present (P2 index), disk-headroom (P4/#828), honest pull-inclusive ETA, `swapBackgroundMaxDur` ceiling → downgrade to `installed_not_running`. | P2, P4, #828, resmon (#833, already merged) |

P0 and P1 can start immediately and in parallel; P3 is the first externally
visible win (closes the "silently carrying both" half of #682) and should ship
before P5 so the duplicate is *visible* before anything starts deleting files
based on it. P7 is deliberately last — it's the whole reason #682 was filed
as a prerequisite, and nothing about it can be honest before P2 and P4 exist.

## 6. Open questions for Jason

1. **Pinning UX**: should `pinned_models` live in `citadel.yaml` (declarative,
   like `pinned_services`) or be a runtime-only CLI/API setting? The manifest
   route is consistent with #577 but couples cache policy to node
   provisioning; a runtime setting is more flexible but not durable across a
   full reprovision.
2. **GC default threshold**: is 90% disk-percent the right trigger, or should
   it track `resmon`'s existing threshold conventions if one already exists
   for other subsystems?
3. **Backfilling the index on upgrade**: a node with pre-existing caches but
   no index entries — do we want a one-time background scan on first boot
   after upgrade (populates `size_bytes`/`path` but `last_used` unknown), or
   let entries populate lazily as models are actually served? Lazy is simpler
   but means P7's weights-present fallback (live existence check) stays load-
   bearing longer than intended.
4. **`hfCacheBaseDir()` ownership (P4)**: should the rename land as part of
   whichever of #682/#828 merges second, or as its own small coordinating PR
   touching both? Given #828 is actively in flight in a separate worktree,
   this needs explicit sequencing to avoid two branches disagreeing about the
   canonical HF path at merge time.
5. **`DOWNLOAD_MODEL` (legacy curl path)**: fold into `MODEL_CACHE_PULL`/the
   P2 index now, or leave it as a documented, unindexed exception? It writes
   into `~/citadel-cache/<model_type>/<file>` today, outside this design's
   index unless explicitly included.

## 7. Status update (2026-08-30): what shipped since the draft

Verified against the code on `main` (not restated from memory — the function
names below are the authorities; read them, not this list, for exact behavior):

- **P0 shipped (#904).** `internal/jobs.canonicalHFCacheDir()` is the one
  place the canonical container-mounted HF path is computed;
  `hfDownloadEnv()` sets `HF_HOME` on the pull subprocess (respecting any
  operator override of the four HF env vars), and
  `warnIfLegacyHFCacheExists()` reports the pre-fix duplicate once,
  informationally. **P4 was folded into P0** rather than staying a separate
  phase: `hfCacheBaseDir()`'s final fallback resolves through
  `canonicalHFCacheDir()`, so the disk preflight, the already-cached netting
  (#840), and the actual download can no longer disagree.
- **P1 shipped (#912/#906).** `services/caches.go` (`EngineCacheDirs`,
  `CacheFamily`, the `HFHubCacheDirName`/`LlamaCppCacheDirName`/
  `BonsaiCacheDirName` constants), pinned against every embedded compose
  mount by `TestEngineCacheDirsMatchComposeMounts`. llamacpp is now routed
  through `pullLlamaCppGGUF` (raw `--local-dir` GGUF layout into
  `llamaCppCacheDir()`, with `runGGUFDiskPreflight` netting against
  `alreadyCachedGGUFBytes`) instead of `pullHuggingFace`.
- **P6's llamacpp half shipped with P1.** `evictLlamaCppGGUF`
  (`internal/jobs/model_cache_evict.go`) evicts an exact cached filename
  from the raw GGUF dir. The rest of P6 (bonsai eviction, resolving a repo
  id to "whichever files belong to it") is exactly what the P2 index
  unlocks — see §8.4.
- **P2a shipped (#936, hardened by #940).** `internal/cacheindex`
  (`cacheindex.go` + `backfill.go`): the versioned-JSON durable index at
  `<network.GetNodeConfigDir()>/cache-index.json`, keyed `(cache_dir, model)`,
  lenient load, atomic version-guarded writes, `ReconcileScan` backfill at
  `citadel work` startup (`cmd/work.go:690`), all §8.3 pull/evict sites wired
  through `internal/jobs/cache_index.go`'s nil-safe singleton,
  `MODEL_CACHE_EVICT` moved onto the serialized lane
  (`internal/worker/deadline.go:124`), and the resident-implies-used
  `MarkUsed` reconciler on the heartbeat's OnStatus fan-out
  (`cacheIndexMarkUsedReconciler`, `cmd/work.go:3011`).
- **Not shipped:** P3 (reporting — §9 below is its implementation design),
  P5 (GC — §10), P7 (`warm_on_demand`).

## 8. P2 implementation design: the durable cache index (#682 P2 / #683 prerequisite)

P2 gives the node a durable answer to "which files on disk belong to which
pulled model" — the record that §1.3 established does not exist anywhere
today, and without which P3's reporting is a `du -sh` guess, P5's GC cannot
safely delete anything, P6 cannot resolve a repo id to its files, and P7's
weights-present clause cannot be answered honestly.

### 8.1 What the index records

One entry per cached artifact. The **primary key is `(cache_dir, model)` —
deliberately NOT `(engine, model)`**: seven engines share the
`huggingface` bucket (`services.EngineCacheDirs`), so an entry keyed by
engine would either duplicate a shared repo seven times or force a fake
"owning engine" onto a directory the layout says is shared. `engine` is
recorded as provenance (who pulled it), not identity.

```json
{
  "version": 1,
  "entries": [
    {
      "cache_dir": "huggingface",
      "family": "hf-hub",
      "model": "meta-llama/Llama-3.1-8B-Instruct",
      "engine": "vllm",
      "files": ["hub/models--meta-llama--Llama-3.1-8B-Instruct"],
      "size_bytes": 16800000000,
      "pulled_at": "2026-08-20T10:00:00Z",
      "last_used": "2026-08-27T14:32:00Z",
      "source": "pull"
    },
    {
      "cache_dir": "llamacpp",
      "family": "gguf-dir",
      "model": "TheBloke/Llama-2-7B-GGUF",
      "engine": "llamacpp",
      "files": ["llama-2-7b.Q4_K_M.gguf", "config.json"],
      "size_bytes": 4080000000,
      "pulled_at": "2026-08-29T08:00:00Z",
      "source": "pull"
    }
  ]
}
```

**`files` semantics differ per `CacheFamily`, matching how each family's
provenance actually works** (this is the §2.2 draft made concrete against
the shipped P1 layout table):

- **`hf-hub`**: ONE directory path, the `models--<org>--<repo>` dir relative
  to the resolved hub dir — the same dir `hfCacheDir(modelName)` resolves
  and `evictHuggingFace` already `os.RemoveAll`s. Directory-level provenance
  is exact in the hub layout (the naming convention IS the provenance, as
  #840 established); recording per-blob paths would fight the
  content-addressed blob store, where blobs dedup across repos and snapshot
  hashes churn. Stored relative to the hub dir, not absolute, so an
  operator's `HF_HUB_CACHE` override doesn't strand entries.
- **`gguf-dir`**: the explicit repo-relative file paths this pull's own
  files landed at. These are FREE at pull time: `hfRepoTreeFn`
  (`internal/jobs/hf_repo_size.go`) already fetches the repo tree with
  per-entry `Path`s for the disk preflight, and `alreadyCachedGGUFBytes`
  already matches those same paths on disk — the index write records the
  post-pull intersection (tree entries passing `patternsInclude` that exist
  under `llamaCppCacheDir()`), i.e. exactly the provenance rule #906 already
  trusts, now persisted. If the tree fetch failed (the preflight's
  fail-open path), fall back to the before/after file-list diff of the
  directory walk `dirTotalSize` already performs — coarser (a concurrent
  pull could cross-attribute) but honest for the single-writer lane (§8.2).
  Bonsai records its one fixed file (`bonsaiGGUFFile`).
- **`native` (ollama)**: `files` empty. Ollama's store is its own
  provenance; `ollama rm <model>` is the evictor and `ollamaModelSize` the
  sizer. The entry still carries model/size/timestamps so reporting and GC
  ordering see ollama models without parsing `ollama list` on every read.
- **`native` (lmstudio, tei)**: one aggregate per-store entry
  (`model: "_store"`, size only, `files` empty, never GC-evictable via the
  index). These stores have no Go-side pull path at all; the entry exists so
  P3's reporting has a size row, nothing more.

**`last_used` — separate from `swap-lru.json`, as §2.2 already decided**
(VRAM-residency-by-engine vs disk-weights-by-model; different axis,
different lifecycle — do not unify them, and do not seed one from the
other: swap-lru's key is an engine name, which for the shared hf-hub bucket
does not identify a model). P2a's signal, in order of precision:

1. A pull sets `last_used = pulled_at`.
2. A heartbeat-tick reconciler ("resident-implies-used"): on the existing
   `OnStatus` fan-out (`SetOnStatus`, the #612/#416 precedent — no new
   sweeps), mark the models each RUNNING engine is currently serving (the
   `services[].models` already on the assembled status) as used, debounced
   (§8.2). Coarse — residency ≠ requests — but strictly truthful in the
   direction GC needs: a model being actively served is never allowed to
   look cold. Per-request precision (touching from
   `status.RecordEngineRequest`'s call sites, which know the model on the
   gateway/worker dispatch paths) is a P5-adjacent refinement, deferred
   until GC actually needs to discriminate among resident models.

Filesystem atime stays rejected (relatime/noatime, §2.2). `pinned` from
§3.1 is RESERVED in the schema but not written by P2a — pinning lands with
GC (P5), where it has a consumer.

### 8.2 Location, ownership, format, concurrency

**Path:** `<network.GetNodeConfigDir()>/cache-index.json`. NOT
`platform.ConfigDir()` — the writer is the pull handler inside a
(frequently systemd-root) `citadel work`; the readers include an
interactive non-root `citadel status`. `ConfigDir()` resolves those to
different directories and the reader sees nothing forever (the
#726/#845/CLAUDE.md bug class). `swap-lru.json` (`cmd/hotswap.go` wiring)
and `aepSigningStoreDir` (`internal/worker/llm_inference.go`) are the two
shipped precedents; this follows them.

**Package:** NEW `internal/cacheindex`, a LEAF that takes an explicit path
(mirroring `nodeidentity.Store`) — it imports `services` (for
`EngineCacheDirs`/`CacheFamily`) and stdlib only. Callers resolve
`network.GetNodeConfigDir()` themselves; `internal/jobs` already imports
`internal/network` (`cobrowse_session.go`), so no hook indirection is
needed, but the handlers reach the store through an injectable package var
(`cacheIndexFn`, mirroring `llamaCppCacheDirFn`/`hfCacheModelSizeFn`'s
existing test seams in the same file) so tests never touch the real node
config dir.

**Format:** versioned JSON (`{"version":1,"entries":[...]}`). Reads are
LENIENT per entry — a malformed entry degrades to "absent", never fails the
load, and a missing/corrupt file degrades to an empty index (the #815
`TokenHashEntry` reasoning, already reapplied by `loadLastUsedFile` in
`swap_persist.go`). Writes are atomic (temp file + rename in the same dir).
Pull/evict mutations flush immediately (they're rare and each one matters);
`MarkUsed` from the heartbeat reconciler is debounced (`persistMinGap`
analogue, `swap_persist.go:166`'s `persistIfDue` pattern) since it fires
every ~30s tick.

**Concurrency — verified against the #908 lane membership, not assumed:**

- `MODEL_CACHE_PULL` is in `unboundedJobTypes` and therefore in
  `serializedLaneJobTypes` (`internal/worker/deadline.go:59-115`, pinned by
  `TestSerializedLaneJobTypes`) — it executes on the exec-concurrency-1
  serialized lane. `SERVICE_START` (whose `ensureOllamaModel` is a pull
  site, §8.3) is too. So all but one index-writing job type already have
  the single-writer guarantee the manifest writers rely on.
- **`MODEL_CACHE_EVICT` is NOT a member** — today it executes on the inline
  branch and can run CONCURRENTLY with a lane pull. That is already a live
  (pre-index) hazard: `llamaCppPullSucceeded`'s clamp comment
  (`model_cache_pull.go:325-329`) documents a concurrent eviction mid-pull
  corrupting the before/after accounting, and an `os.RemoveAll` racing an
  in-progress `hf download` into the same hub dir is worse than an
  accounting bug. **P2a adds `JobTypeModelCacheEvict` to
  `serializedLaneJobTypes`** — exactly what that set's own doc comment
  instructs for a new shared-state writer ("extend THIS set, not the
  routing check") — and updates `TestSerializedLaneJobTypes`. Cost: an
  evict queues behind a multi-GB pull on the exec-1 lane; acceptable, since
  evicting mid-pull was never safe anyway (flagged as an open question,
  §8.7).
- Cross-process: index WRITES are worker-owned (pull/evict handlers, the
  heartbeat reconciler, the startup backfill — all inside `citadel work`).
  Interactive commands (`citadel status`, P2b) are read-only and never
  self-heal-write (they log staleness instead) — same reasoning as
  `citadel whoami --node-dir` skipping the identity cache write. The
  control-center TUI's own consume loop is the same second-door the #832
  reservation reconcile documents (`ReconcileOrphanedReservations`' doc
  comment); a CC-driven pull writing the index concurrently with a
  `citadel work` writer is bounded by read-merge-write-rename to a lost
  update, which the staleness self-heal (§8.5) repairs. Documented, not
  fixed here — same posture as #832.

**Best-effort, and which direction of error is safe (the #739 discipline,
stated up front):** a failed index write is logged, never fatal — the
pull/evict still succeeds, mirroring `catalog.UpsertLockEntry`'s posture and
`persistLogf`. The resulting false NEGATIVE (a genuinely-cached model
missing from the index) degrades to: preflights/`warm_on_demand` treat it as
not-cached (a redundant, idempotent re-pull at worst — `hf download`
re-fetches nothing), and GC skips it (a missed reclaim, never a wrong
delete). The dangerous direction — a STALE entry claiming files that are
gone or attributing files that belong to something else — is guarded not by
write reliability but by **verify-before-act** (§8.4/§8.5): any consumer
about to delete or report re-stats the entry's recorded paths first.
Recording exact paths at pull time (§8.1) is what makes cross-attribution
structurally impossible for gguf-dir, and the hub layout's own naming makes
it impossible for hf-hub.

### 8.3 Every write site (enumerated, per the #739 lesson: any pull path not wired is invisible)

| Site | File | Index op |
|---|---|---|
| `pullHuggingFace` (vllm) | `internal/jobs/model_cache_pull.go` | Upsert hf-hub entry on the post-`hfCacheModelSize` success path |
| `pullLlamaCppGGUF` | `internal/jobs/model_cache_pull.go` | Upsert gguf-dir entry (tree-verified file list, §8.1) on success |
| `pullBonsai` | `internal/jobs/model_cache_pull.go` | Upsert gguf-dir entry (the one `bonsaiGGUFFile`) on the post-`verifyDownloadedFile` success path |
| `pullOllama` | `internal/jobs/model_cache_pull.go` | Upsert native entry (size via `ollamaModelSize`) |
| `ensureOllamaModel` (#543, the SERVICE_START native-ollama pull) | `internal/jobs/service_handler.go:1252` | Upsert native entry when its pull actually ran — **the one pull site NOT inside `MODEL_CACHE_PULL`**; missing it reopens #739's gap under a new name |
| `evictOllama` | `internal/jobs/model_cache_evict.go` | Remove entry on success |
| `evictHuggingFace` | `internal/jobs/model_cache_evict.go` | Remove entry on success |
| `evictLlamaCppGGUF` | `internal/jobs/model_cache_evict.go` | Remove the file from its entry's `files`; drop the entry when empty |

**Known NON-sites, named so nobody hunts for missing wiring later:**

- `selfProvisioningEngines` (tei/diffusers/kokoro/transcribe/unlimited-ocr/
  extraction): the CONTAINER downloads weights inside `docker compose up` —
  invisible to Go by construction (the same observability wall #717's
  "whether a swap pulled" gap documents). No pull-time write is possible;
  the backfill/staleness scan (§8.5) is how these enter the index, tagged
  `source: "backfill"`.
- Engine first-start downloads for vllm etc. when no `MODEL_CACHE_PULL`
  preceded them: same wall, same answer (backfill).
- `DOWNLOAD_MODEL` (`internal/jobs/download_model.go`): stays unindexed per
  §6 Q5 unless Jason says otherwise (§8.7).

### 8.4 Read API — designed so P3/P5/P6/P7 are thin consumers

```go
package cacheindex

func Load(path string) (*Index, error)            // lenient; empty on missing/corrupt
func Open(path string) *Store                     // the write handle (worker-only)

// Reads (on *Index):
func (ix *Index) Lookup(cacheDir, model string) (Entry, bool)
func (ix *Index) LookupForEngine(engine, model string) (Entry, bool)
    // resolves engine -> cacheDir via services.EngineCacheDirs, then Lookup
func (ix *Index) FilesFor(cacheDir, model string) []string   // exact-provenance eviction (P6)
func (ix *Index) EntriesByDir() map[string][]Entry           // reporting (P3)
func (ix *Index) LeastRecentlyUsed() []Entry                 // GC ordering (P5)
func (ix *Index) Verify(e Entry) (Entry, bool)               // re-stat recorded paths; false = stale

// Writes (on *Store, mutex-guarded, atomic temp+rename):
func (s *Store) Upsert(e Entry) / Remove(cacheDir, model string) / RemoveFile(...)
func (s *Store) MarkUsed(cacheDir, model string, at time.Time)   // debounced flush
func (s *Store) ReconcileScan(cacheRoot string) error            // backfill, §8.5
```

What each follow-on consumes (hooks designed here; features NOT built here):

- **P6, exact-provenance eviction:** `MODEL_CACHE_EVICT` for a gguf repo id
  (which `evictLlamaCppGGUF` today honestly refuses) becomes
  `FilesFor("llamacpp", repoID)` → remove each verified file → `RemoveFile`.
  Bonsai eviction is the same three lines. The existing exact-filename path
  stays as the index-miss fallback.
- **P3, honest reporting:** `printCacheInfo` (`cmd/status.go:565`, the
  display-only `du -sh`) gains per-model rows from `EntriesByDir()` with the
  `du` totals kept as the "unindexed remainder" cross-check (index total vs
  du total diverging IS the duplicate/orphan signal #682 items 3–4 want).
  Heartbeat: a `NodeStatus.Cache` field fed the same way, additive/omitempty
  (the `SwapActivity` wiring pattern, including the atomic-pointer lesson).
- **P5, GC:** `LeastRecentlyUsed()` + `Verify` + the §3 never-evict-resident
  cross-check against `status.DiscoverLocalEngines`. GC never trusts an
  entry without `Verify`, per §8.2's direction-of-error rule.
- **P7, weights-present:** `LookupForEngine(engine, resolvedModel)` +
  `Verify`, falling back to the live canonical-path existence check for a
  node whose index hasn't backfilled — the fallback §4 already specified.

### 8.5 Migration/backfill and staleness

**Backfill:** `Store.ReconcileScan(cacheRoot)` runs at every `citadel work`
startup (in `runWork`, after `worklock.Acquire`, beside
`ReconcileOrphanedReservations` — same single-live-worker precondition,
same call-site reasoning). Idempotent: it never overwrites a richer
`source:"pull"` entry with a backfill one. Per family:

- hf-hub: enumerate `models--*` dirs under the resolved hub dir
  (`hfCacheBaseDir()`); the model id is recoverable by reversing the
  `models--org--repo` sanitization. Size via the same walk
  `hfCacheModelSize` does.
- gguf-dir (llamacpp, bonsai): one entry per file; `model` = filename (the
  repo id is NOT recoverable from a bare file — accepted; eviction by
  exact filename already works for these).
- ollama: `ollama list` (reusing `ollamaModelSize`'s parse).
- other native: the aggregate `_store` row (§8.1).

Backfilled entries set `pulled_at` from file **mtime** — a real signal for
downloaded files (unlike atime) — and leave `last_used` EMPTY, meaning
**unknown, not "never"**. This matters for P5 in both directions: unknown
must not read as coldest (or every pre-index model gets evicted first — the
inverse of #632's "no record must not read as recently loaded" rule, same
class), so GC orders unknowns by `pulled_at` mtime, after any entry with a
real `last_used` older than it, never as an automatic front-of-queue.

**Staleness / out-of-band changes:**

- Files REMOVED out-of-band (operator `rm`, a container's own cache
  management): `Verify` catches it at the point of use; the worker-side
  store additionally drops verified-stale entries at each startup scan
  (logged). Read-only CLI readers report the entry as stale rather than
  writing (§8.2).
- Files ADDED out-of-band (container first-start downloads, operator
  copies): picked up by the next startup `ReconcileScan`. There is
  deliberately NO periodic mid-run rescan — a du-scale walk on a tick is
  exactly the extra sweep the #416 reconciler design avoided; the startup
  scan plus pull-time writes keep the index honest to within one worker
  restart, and every consumer that ACTS re-verifies anyway.

### 8.6 Phased plan (P2a is one Sonnet-sized PR)

**P2a — the index (one PR):**

| Change | File(s) |
|---|---|
| `Entry`/`Index`/`Store`, lenient load, atomic+debounced write, `Verify`, `ReconcileScan` | NEW `internal/cacheindex/cacheindex.go`, `backfill.go`, `cacheindex_test.go` |
| Injectable store seam + wire the 4 pull sites and 3 evict sites (§8.3 table) | `internal/jobs/model_cache_pull.go`, `model_cache_evict.go` (a `cacheIndexFn` package var beside `llamaCppCacheDirFn`) |
| Wire `ensureOllamaModel` | `internal/jobs/service_handler.go` |
| `JobTypeModelCacheEvict` → `serializedLaneJobTypes` | `internal/worker/deadline.go` + `TestSerializedLaneJobTypes` |
| Startup: `Open(GetNodeConfigDir()/cache-index.json)` + `ReconcileScan`; register the `OnStatus` MarkUsed reconciler | `cmd/work.go` |
| Tests: lenient parse, atomic write, verify-self-heal, backfill against a fixture tree, handler-write-on-success (via the seam), lane membership | the `_test.go` files above |

**P2b (next PR):** `printCacheInfo` reads the index + du-remainder
cross-check; heartbeat `NodeStatus.Cache` (this is P3, split so P2a stays
node-internal with zero heartbeat-schema change). *Superseded by §9, the
full P3 implementation design.*

**Later, unchanged from §5:** P5 GC (now buildable on
`LeastRecentlyUsed`/`Verify` + pinning — implementation design in §10), P6
completeness (`FilesFor`-driven repo-id eviction, bonsai evict), P7.

### 8.7 Open questions for Jason (P2 additions to §6)

1. **Index format/unification:** single versioned JSON file at
   `GetNodeConfigDir()/cache-index.json` (proposed, mirroring
   swap-lru.json), vs JSONL, vs folding into one combined node-state file
   with swap-lru. Proposal: separate file, same dir — different lifecycle,
   and swap-lru's schema is deliberately not model-keyed.
2. **`MODEL_CACHE_EVICT` onto the serialized lane:** accept that an evict
   can queue behind a multi-GB pull on the exec-1 lane? (The alternative —
   leaving it inline — keeps the existing evict-races-pull hazard AND makes
   the index a multi-writer file.)
3. **`last_used` v1 fidelity:** is resident-implies-used at heartbeat
   granularity (§8.1) sufficient for P5's LRU, or should per-request
   touches (via the `RecordEngineRequest` call sites) be a P2a requirement
   before GC is allowed to ship?
4. **Backfilled-entry GC eligibility:** mtime-as-`pulled_at` ordering
   (§8.5) OK, or should `source:"backfill"` entries be exempt from GC v1
   entirely (safer, but the pre-index weights are precisely the ones most
   likely to be forgotten disk hogs)?
5. **GC default budget (carries §6 Q2 forward):** disk-percent trigger at
   90% via resmon's existing signal, or an absolute size budget per cache
   dir?
6. **`DOWNLOAD_MODEL` (carries §6 Q5 forward):** still leave unindexed?

## 9. P3 implementation design: reporting off the index (#682 items 3–4)

P3 replaces the live `du` sweeps with reads off the P2a index and puts cache
attribution on the heartbeat for the first time. It is the first externally
visible win (§5's ordering note stands: it must ship before P5 so the
duplication is *visible* before anything deletes files based on it).

### 9.1 What P3 replaces, precisely

- **`printCacheInfo` (`cmd/status.go:565-610`, rendered from `:116-117`)** is
  the only cache reporting that exists: a `du -sh` subprocess on
  `~/citadel-cache` plus one more per glob'd subdirectory, at render time,
  every invocation. It has no model attribution, no freshness, no persistence,
  cannot see `~/.cache/huggingface` (the #682 duplicate), and its per-subdir
  loop is O(subdirs) subprocess execs walking the full tree — seconds on a
  40G cache.
- **The heartbeat carries NOTHING cache-shaped.** `NodeStatus`
  (`internal/status/types.go:20`) has no cache field; the closest signal is
  `internal/resmon`'s `HostResources.DiskAvailableBytes` (`resmon.go:268-286`,
  #833) — *headroom* on the cache volume, not *attribution* of what's
  consuming it. #682 item 3 ("report cache size and location in the heartbeat
  so the duplication is visible before it costs 50G") is entirely unmet.

### 9.2 Two readers, two access paths (the single-writer rule applied)

The index has exactly one legal writer — the `citadel work` process's
singleton `Store` (`internal/jobs/cache_index.go`'s package doc). P3's two
readers respect that differently:

- **Heartbeat (inside `citadel work`):** reads the LIVE store —
  `jobs.CacheIndexStore().Snapshot()` — so it sees pull/evict/MarkUsed
  mutations immediately, not just what's flushed to disk. Wiring is safe as a
  plain closure, NOT an atomic pointer: `InitCacheIndexStore` runs at
  `cmd/work.go:363`, well before the status-publisher goroutines start
  (`:1918`/`:1985`) — this is the #717 plain-var-vs-atomic test applied and
  passed (the assignment falls on the safe side of goroutine startup, unlike
  `nodeSwapManager`). The closure is still nil-safe (`CacheIndexStore()`
  returns nil in any process that never initialized it).
- **Interactive `citadel status` (separate process, any user):** read-only
  `cacheindex.Load(filepath.Join(network.GetNodeConfigDir(), cacheindex.FileName))`
  — never `Open`, never a second `Store` (the same-process clobber hazard the
  package doc names is a cross-process lost-update hazard here). It sees the
  last flushed state, which is at most `markUsedFlushMinGap` behind the live
  store for recency and exactly current for pull/evict (those flush
  immediately). **Readers never write**: no backfill trigger, no self-heal —
  a stale/absent index is *reported* as such (§9.5), same posture as `citadel
  whoami --node-dir` skipping the identity-cache write. The remedy for a
  missing index is running `citadel work`, and the doc says so in the output.

### 9.3 Index API gap: the file has no scan metadata (the one place the thin-consumer claim fails)

Verified against `internal/cacheindex` as shipped: `EntriesByDir()` (:371),
`All()` (:386), `Snapshot()` (:548) and `Load()` (:249) are exactly the read
surface §8.4 promised P3 — per-dir grouping and totals are trivial sums over
them, no new query shape needed. But three things P3 must report are not in
the file format (`fileFormat`, :211 — version + entries, nothing else):

1. **Freshness.** Nothing records when `ReconcileScan` last ran. Without it,
   "unindexed remainder" (below) is uninterpretable — is the delta drift
   since this morning or since three worker restarts ago?
2. **Per-dir measured totals.** The index's per-entry `SizeBytes` sums to
   *indexed* bytes only. The du-replacement needs the *measured* per-dir
   total from the same walk, so `measured - indexed = unindexed remainder` —
   which is precisely the orphan/duplicate signal #682 item 4 wants, computed
   at scan time instead of per render/tick.
3. **The legacy duplicate.** `warnIfLegacyHFCacheExists`
   (`internal/jobs/model_cache_pull.go:1082`) is a per-pull log line, P0's
   deliberate scope; the durable "this node carries a second HF cache at
   `~/.cache/huggingface`, N bytes" record P3 surfaces does not exist.

**Fix: additive scan metadata, written by `ReconcileScan`, version stays 1.**
Extend `fileFormat` (and `Index`) with a top-level block, leniently absent on
old files (zero values → readers render "unknown", exactly the
`TokenHashEntry`/#815 degradation the format already practices per-field):

```go
// fileFormat gains (all omitempty; FormatVersion stays 1 — additive):
ScannedAt time.Time      `json:"scanned_at,omitempty"`
Dirs      []DirScan      `json:"dirs,omitempty"`
LegacyHF  *LegacyHFCache `json:"legacy_hf_cache,omitempty"`

type DirScan struct {          // one per services.EngineCacheDirs dir scanned
    Dir           string `json:"dir"`
    Family        string `json:"family"`
    MeasuredBytes int64  `json:"measured_bytes"`
}
type LegacyHFCache struct {    // present only when the models-- gate passes
    Path      string `json:"path"`
    SizeBytes int64  `json:"size_bytes"`
}

// New read API (thin, on *Index):
func (ix *Index) Meta() Meta                  // ScannedAt + Dirs + LegacyHF
func (ix *Index) IndexedBytesByDir() map[string]int64  // sum of entries
```

Costs and mechanics, stated:

- The per-dir measured walk is `dirSize`-shaped (`backfill.go:420`) work
  `ReconcileScan` already mostly performs (it sizes every discovered
  artifact); the delta is walking a dir's *non-artifact* remainder once per
  scan. That is du-equivalent cost **at worker startup only** — never per
  heartbeat tick, never per `citadel status` render. This is the entire
  point of P3.
- A dir that was NOT scanned this pass (unreadable, or the operator's
  HF_HUB_CACHE override case `scanResult.scannedDirs` already guards) gets
  no `DirScan` row — absence means "not measured", never "zero bytes",
  the same #632-class rule the scanner already applies to pruning.
- **The legacy-HF probe needs the path resolution `legacyHFCacheDir` +
  `warnIfLegacyHFCacheExists` own — including the `models--` gate** (the
  bare `~/.cache/huggingface` holding only an `hf auth login` token must not
  be reported as a reclaimable duplicate; that function's doc comment
  explains the harm). `internal/cacheindex` is a leaf and must not import
  `internal/jobs`, so `ReconcileScan` grows an options param the caller
  fills: `ReconcileScan(cacheRoot string, opts ScanOptions)` with
  `opts.LegacyHFHubDir string` (empty = skip the probe). `cmd/work.go`
  computes it via a small exported `jobs.LegacyHFHubDirForScan()` that
  reuses the existing resolution + gate — one implementation, not a
  duplicated 5-liner that drifts. Existing `ReconcileScan(root)` call sites
  are updated in the same PR (it has one production caller).

### 9.4 Heartbeat shape: `NodeStatus.Cache`

House pattern throughout — the `SwapActivity` precedent (#717): a
hand-maintained mirror in `internal/status/types.go` (additive, `omitempty`;
`internal/status` *could* import the leaf `cacheindex` without a cycle, but
the mirror keeps the heartbeat schema owned in one file next to
Swap/Lanes/Reservations, and the projection lives beside its siblings), a
`CollectorConfig.CacheReport func() *CacheReport` field (the
`SwapStats`/`Reservations` shape, `internal/status/collector.go:54-90`),
projected by a `cacheReportFrom` in `cmd/work.go` and wired at the two
heartbeat collector construction sites (`:1486`/`:1779`). The TUI
control-center collector does NOT get it, consistent with the documented
WorkerLiveness/Swap/Reservations/Lanes gaps there.

```go
// internal/status/types.go — on NodeStatus, additive:
Cache *CacheReport `json:"cache,omitempty"`

type CacheReport struct {
    // ScannedAt is when ReconcileScan last reconciled the index against
    // disk (zero/omitted = never on this node — pre-P3 index file).
    ScannedAt time.Time        `json:"scanned_at,omitempty"`
    TotalIndexedBytes int64    `json:"total_indexed_bytes"`
    Dirs []CacheDirReport      `json:"dirs,omitempty"`
    // LegacyHFCache reports the #682 duplicate when present (§9.3).
    LegacyHFCache *LegacyCacheReport `json:"legacy_hf_cache,omitempty"`
}
type CacheDirReport struct {
    Dir            string `json:"dir"`             // "huggingface", ...
    Family         string `json:"family"`          // "hf-hub", ...
    IndexedBytes   int64  `json:"indexed_bytes"`   // sum of live entries
    MeasuredBytes  int64  `json:"measured_bytes,omitempty"`  // last scan; 0 = not measured
    UnindexedBytes int64  `json:"unindexed_bytes,omitempty"` // max(measured-indexed, 0)
    EntryCount     int    `json:"entry_count"`
    // Models is the per-model attribution, size-descending, capped at
    // maxHeartbeatModelRows (32) so one node's heartbeat cannot balloon on a
    // hoarder cache; EntryCount tells the consumer when it's truncated.
    Models []CacheModelReport `json:"models,omitempty"`
}
type CacheModelReport struct {
    Model     string    `json:"model"`
    Engine    string    `json:"engine,omitempty"`  // provenance, not identity
    SizeBytes int64     `json:"size_bytes"`
    PulledAt  time.Time `json:"pulled_at,omitempty"`
    LastUsed  time.Time `json:"last_used,omitempty"`
    Source    string    `json:"source,omitempty"`  // "pull" | "backfill"
}
```

Decisions:

- **Per-model rows ARE in the heartbeat, capped.** #682 item 3 is satisfiable
  with per-dir aggregates alone, but the dashboard's actionable rendering
  ("this node holds 16.8G of Llama-3.1-8B it hasn't used in 3 weeks") needs
  model rows, and 32 rows × ~150 bytes is noise next to the existing
  services/GPU payload. Flagged as §11 Q1 in case Jason wants aggregates-only.
- **Per-tick cost is map-copy only.** `Snapshot()` copies the entries map
  (tens of entries); `IndexedBytesByDir`/sorting are in-memory. Zero
  subprocesses, zero filesystem walks on the tick — `MeasuredBytes`/
  `UnindexedBytes`/`LegacyHFCache` are scan-time values carried from `Meta()`.
  (The `_store` native rows from `scanNativeDir` are what keep lmstudio/tei
  represented in `IndexedBytes` despite having no per-model entries.)
- **Index-miss / nil store ⇒ field omitted** (non-work processes, legacy
  builds) — indistinguishable from a pre-P3 node, which is correct: the
  consumer treats absence as "unknown", never "empty cache". An initialized
  but empty index reports `Cache` with zeros + `ScannedAt`, distinguishing
  "scanned, nothing there" from "never scanned".
- The `/status` HTTP endpoint and `citadel services`-adjacent surfaces get
  the field for free via the collector.

### 9.5 `printCacheInfo` rewrite

Same data, third access path (§9.2's read-only `Load`):

- **Primary rendering from the index:** per-dir rows (indexed bytes, entry
  count, measured total + unindexed remainder when the scan metadata exists),
  then per-model rows (size-descending, top ~10 for terminal sanity, with
  age/last-used), then the legacy-duplicate line when recorded, then a
  freshness line ("index last reconciled 2026-08-30 04:12 — at `citadel
  work` startup").
- **One `du -sh` total is KEPT, per-subdir `du` dropped.** The live total vs
  the index's `ScannedAt`-stamped measured total diverging is the
  out-of-band-drift cross-check §8.4 already promised ("index total vs du
  total diverging IS the duplicate/orphan signal") — and interactive render
  time is where one `du` is affordable. The per-subdir loop (the expensive
  part) is replaced by index rows.
- **Fallback when no index file exists** (node never ran a post-P2a
  `citadel work`): the current du-based rendering, verbatim, plus one hint
  line ("no cache index yet — run `citadel work` to build it"). Back-compat
  is behavioral, not schema-bound: this output is human-facing tabwriter
  text with no machine consumer to preserve, so keeping the `Total Size` /
  `Breakdown` labels is courtesy, not contract.

### 9.6 Deliberately NOT in P3

- No reader-side writes of any kind (§9.2) — including "trigger a backfill
  scan on stale". §8.5's no-mid-run-rescan decision stands for P3; whether
  the WORKER should rescan on a slow timer for freshness is §11 Q2, a
  worker-side question, not a reader-side one.
- No catalog-module cache coverage (the `EngineCacheDirs` boundary,
  unchanged).
- No `~/.cache/huggingface` *contents* enumeration — the legacy dup is
  reported as one path + one size, not indexed per-model (indexing it would
  bless it as a real cache; it is a to-be-deleted artifact).

## 10. P5 implementation design: GC (size-pressure eviction of stale weights)

P5 is the phase that actually reclaims the #682 disk. Everything below builds
on shipped primitives: `LeastRecentlyUsed()`/`Verify()` (§8.4),
`SourceBackfill` provenance, the per-family evict implementations
(`internal/jobs/model_cache_evict.go`), and `resmon`'s cache-volume disk
signal. Default OFF behind `CITADEL_CACHE_GC`, per §3 — unchanged.

### 10.1 Trigger and budget: disk-pressure hysteresis on the cache volume, not per-dir byte budgets

**Decision: the trigger is disk-percent on the cache volume, with a
high-water/low-water pair.** Rationale: #682's incident chain was
*volume-full* → Docker GC'd engine images → node lied about serveability. A
per-dir byte budget cannot see the volume filling from anything outside its
dir (other caches, Docker images, logs) and adds N knobs where one matches
the actual failure mode. The volume signal already exists and already points
at the right path: `resmon`'s `hostDiskPath()` resolves `~/citadel-cache`
when present (`internal/resmon/resmon.go:294-303`) — reuse that resolution
(exported or mirrored via its pure `resolveDiskPath` core), do not add a
second probe with a second opinion of "the cache volume".

- Trigger: usage ≥ `CITADEL_CACHE_GC_HIGH_PERCENT` (default **90**, §6 Q2's
  number, now concrete).
- Target: evict until usage ≤ `CITADEL_CACHE_GC_LOW_PERCENT` (default
  **80**) or no eligible candidates remain. Hysteresis is what stops GC from
  firing every tick while hovering at the threshold.
- **Evict-one-then-remeasure**, never plan-the-full-set-upfront: entry
  `SizeBytes` are scan-time estimates (a `SourcePull` entry's size is never
  refreshed after pull; a container-side re-download can grow a dir
  invisibly), so the loop's ground truth is a fresh statfs after each
  eviction, and summed `SizeBytes` are only used for candidate ORDERING
  (tie-break), never for deciding "we've freed enough".
- An absolute per-dir/global byte budget is deliberately not in v1 (§11 Q4
  offers it as the alternative if Jason wants proactive trimming below the
  pressure threshold on big-disk nodes).

### 10.2 Ordering: `LeastRecentlyUsed()` as shipped — and why the v1 recency signal is sufficient

§3 specified "idle-first, then LRU, largest-size tie-break". With P5's
exemptions, **idle-first collapses**: resident/serving entries are
categorically exempt (§10.3), so every candidate is non-resident and the
ordering is simply `effectiveRecency` ascending (`LeastRecentlyUsed()`,
`cacheindex.go:417` — including its unknown-sorts-last defensive rule),
with size-descending as the explicit tie-break added at plan time.

**The resident-implies-used heartbeat signal (§8.1) is sufficient for P5's
LRU, resolving §8.7 Q3 in the "ship GC on v1 fidelity" direction.** The
argument: GC only ever *ranks* non-resident entries, and for a non-resident
entry, `last_used` = the last heartbeat tick at which it was resident — i.e.
"when did this model last stop being served", which is exactly the recency a
disk-LRU wants. Per-request precision only discriminates among
currently-resident models, all of which are exempt anyway. Known
imperfection, accepted: llamacpp's repo-keyed entries never match the
reconciler's served-name `MarkUsed` (`cacheIndexMarkUsedReconciler`'s
documented no-op, `cmd/work.go:2995-3010`) and fall back to `PulledAt`
ordering — degraded ordering, not wrong deletion, and §10.3's dir-level
llamacpp exemption is what carries the safety.

`swap-lru.json` stays out, per §2.2/§3.2: engine-keyed VRAM residency cannot
identify a model in the shared hf-hub bucket. Not re-litigated.

### 10.3 Exemptions — the fail-safe set, per family

Evaluated at plan time AND re-checked immediately before each individual
deletion (verify-before-act, §8.2's direction-of-error rule — the plan
snapshot is advisory, the pre-delete check is the guarantee):

1. **Resident/serving weights — the hard rule (§3), per-family because model
   matching has per-family fidelity.** Residency source:
   `status.DiscoverLocalEngines` (`internal/status/local_engines.go:42`) —
   the same authority the gateway chat route and exclusivity tooling use.
   - **gguf-dir (llamacpp, bonsai):** these dirs are single-engine by
     construction (`CacheFamilyGGUFDir`'s doc comment). If the owning engine
     is RUNNING, the **entire dir is exempt** this cycle. Per-entry matching
     is untrustworthy in the dangerous direction here (served name vs
     repo-keyed entry, the §10.2 gap), and a whole-dir exemption is
     right-sized precisely because the dir is single-engine.
   - **hf-hub:** exact case-insensitive model-id match between an entry's
     `Model` and any running HF-family engine's served models — the same
     match that makes the `MarkUsed` reconciler exact for this family. PLUS
     a conservative catch: if an HF-family engine is running but its served
     model list is empty/unknown (probe failure), the whole hf dir is exempt
     this cycle — "we couldn't ask" must not read as "not serving".
   - **native (ollama):** deletions go through `ollama rm` only (the
     engine's own store manager; never a raw file delete), and models in a
     running ollama's served list are exempt anyway. `_store` aggregate rows
     (lmstudio/tei) are never evictable via the index, as already documented
     at `scanNativeDir`.
2. **Pinned.** v1 reads a `pinned_models` list from `citadel.yaml` at plan
   time (the §3.1 axis; recommendation and alternative in §11 Q5). The
   reserved `Entry.Pinned` field stays reserved — a manifest list needs no
   index write, and a `citadel cache pin` CLI writing the index would
   violate the single-writer rule from a second process (§9.2).
3. **Min-age.** `effectiveRecency` younger than `CITADEL_CACHE_GC_MIN_AGE_HOURS`
   (default **24**) is exempt — protects a just-pulled model whose
   `SERVICE_START` hasn't landed yet (pull and deploy are separate jobs; GC
   between them would evict the deploy's own weights).
4. **Backfill entries: RECOMMENDED EVICTABLE — reversing the presumption in
   `SourceBackfill`'s shipped doc comment.** That comment ("Exempt from GC
   in a future phase") wrote down the cautious default before P5 was
   designed. But #682's 40G IS backfill by definition — pre-index weights
   nobody remembers pulling are *precisely* the forgotten disk hogs the
   issue names — so exempting `SourceBackfill` guts P5 against its own
   motivating incident. §8.5 already built the ordering safety
   (mtime-as-`PulledAt`, unknown-not-coldest); §10.3's residency + min-age +
   Verify checks apply identically. Worst case for a wrongly-cold backfill
   entry: a self-provisioning container re-downloads on its next start —
   cost, not corruption, the §8.2-blessed direction. This is §11 Q3, the
   headline P5 fork.
5. **Verify-before-delete.** `Index.Verify` (recorded files still exist)
   immediately before deletion; a stale entry is dropped from the index
   (worker-side, we ARE the writer) and skipped, never "deleted" (which
   would count phantom bytes as reclaimed and could, under a path collision,
   remove someone else's file).

### 10.4 Execution, structure, and the GC-races-a-pull problem

**Structure mirrors #577's planner/executor split** (`status.PlanPreemption`
+ `ServiceHandler.preemptForVRAM`): a pure, unit-testable planner and a
side-effectful executor.

- **Planner:** `cacheindex.PlanGC(entries []Entry, in GCInputs) GCPlan` —
  pure; `GCInputs` carries now, min-age, pinned set, per-family residency
  facts (running engines + served models, as data), and the ordering rule.
  Unit tests drive every exemption and ordering case with no filesystem.
- **Executor:** `jobs.RunCacheGC` — owns Verify, the per-eviction residency
  re-check, the statfs remeasure loop, and deletion via the SAME per-family
  cores `MODEL_CACHE_EVICT` uses. Concretely: factor the deletion bodies of
  `evictHuggingFace`/`evictLlamaCppGGUF`/`evictOllama` into
  JobContext-independent helpers both callers share — one deletion
  implementation per family, and the index updates
  (`removeCacheIndexEntry`-equivalent) ride the shared helpers. GC never
  grows a second `os.RemoveAll` with its own opinion of HF layout.

**The race:** GC deletes cache files and writes the index from OUTSIDE the
serialized lane — exactly the writer-concurrency hazard that moved
`MODEL_CACHE_EVICT` onto the lane (§8.2): an eviction racing an in-flight
multi-GB `hf download` into the same hub dir. Options considered:

- *Dispatch GC as a job onto the serialized lane:* structurally clean, but
  there is NO local job-submission path into `worker.Runner`
  (`docs/design-model-exclusivity.md` §2.3 established this and judged
  building one real plumbing, not a wrapper). Not built for this either.
- **Chosen: a process-wide cache-mutation mutex in `internal/jobs`**
  (`cacheMutationMu`), taken (blocking `Lock`) by the handler bodies of
  `MODEL_CACHE_PULL`/`MODEL_CACHE_EVICT`/`ensureOllamaModel` — zero new
  contention among themselves, the serialized lane already runs them one at
  a time; the lock only materializes the lane's implicit exclusivity so a
  non-lane participant can observe it — and **`TryLock`'d by GC**: a pull
  in flight ⇒ GC skips this cycle entirely and re-evaluates next tick.
  Fail-open in the safe direction (the #489 meeting-profile `TryLock`
  precedent: never block a reconciler behind a multi-GB pull; disk at 90%
  survives another 30s). Reverse direction — a pull arriving mid-GC blocks
  on the lane until GC releases — is bounded: GC holds the lock per *run*,
  runs are a few file deletions + statfs calls, and §11 Q4's low-water
  target bounds how long a run can be. If runs prove long in practice, the
  lock can move to per-eviction granularity without changing the contract.

**Trigger wiring:** the OnStatus fan-out (`SetOnStatus`, the
#416/#612/MarkUsed precedent — the pressure check is one statfs per ~30s
tick, no new sweep). On trigger, the actual GC runs on its own goroutine
with a single-flight guard (an `atomic.Bool`, the #858 `captureStdout`
posture: refuse-to-nest, don't queue) so heartbeat assembly is never delayed
by deletion I/O. Wired in `cmd/work.go` beside `cacheIndexMarkUsedReconciler`,
constructor gated on `CITADEL_CACHE_GC` (the `missingQueues`/#612 lesson: an
OFF node registers nothing rather than no-oping forever).

**Observability:** every eviction logged (engine, model, bytes, recency,
source); `CacheReport` (§9.4) gains an additive `gc` sub-struct
(`enabled`, `last_run_at`, `last_run_reclaimed_bytes`,
`total_reclaimed_bytes`, `last_skip_reason` — e.g. `"pull_in_flight"`,
`"no_candidates"`, `"below_low_water"`). A node that keeps hitting
high-water with `no_candidates` (everything pinned/resident/young) is
visible as such from the heartbeat — that state is P5's honest analogue of
the swap-rate-limited refusal: it must be *reportable*, not silent.

**CLI:** `citadel cache gc --dry-run` — read-only from any process
(`Load` + `PlanGC` + print; residency via a live `DiscoverLocalEngines`
probe), safe by construction. **No CLI execute path in v1**: a second
process running deletions + index writes reopens both the single-writer
hazard and the #832-documented worklock second-door class; if operators need
manual reclaim they already have `MODEL_CACHE_EVICT` via the platform and
direct `rm` (which the next `ReconcileScan` reconciles).

### 10.5 Interaction with MODEL_CACHE_EVICT, and non-goals

- `MODEL_CACHE_EVICT` stays the precise, targeted, platform-dispatched
  primitive (serialized lane, index-wired, as shipped). GC is the autonomous
  policy ABOVE it, sharing deletion cores and the mutation mutex. Neither
  path knows about the other's decisions; both converge through the index.
- A GC-evicted model that is later needed re-enters via the normal
  `MODEL_CACHE_PULL`/first-start path — idempotent re-pull, the direction of
  error §8.2 blesses. GC does not notify the platform beyond the heartbeat
  `gc` stats; a control-plane "this node evicted X" event stream is a
  platform-side consumer of those stats, not node work.
- **Non-goals, named:** the legacy `~/.cache/huggingface` duplicate is
  REPORTED (P3) but never auto-deleted by GC — it is outside every
  `EngineCacheDirs` dir and outside the index by design (§9.6), and deleting
  a directory citadel never wrote to crosses a line the operator should
  cross manually. `DOWNLOAD_MODEL`'s dir stays unindexed and untouched (§6
  Q5 stands). Catalog-module caches out of scope. Docker IMAGE GC (the other
  half of the #682 incident) is #683/P7 territory (image-present clause),
  not weight GC.

### 10.6 Thin-consumer verification for P5

Holds for: `LeastRecentlyUsed()` (ordering incl. unknown-handling),
`Verify()` (staleness), `Remove`/`RemoveFile` (post-delete index updates),
`Snapshot()` (plan input), `SourceBackfill`/`Pinned` (schema-ready).
Genuinely new, and expected to be (it IS the feature): `PlanGC` (pure
planner), the shared per-family deletion cores refactor, the
`cacheMutationMu` (a gap in the shipped design — §8.2 serialized the JOB
writers but exposed no primitive for a non-lane mutator to join that
exclusion), the `gc` heartbeat sub-struct, and the `resolveDiskPath` reuse.
Plus §9.3's scan-metadata fields, which P5's reporting shares.

## 11. Open questions for Jason (P3 + P5)

### P3

1. **Per-model rows in the heartbeat** (§9.4): top-32-by-size per dir
   (recommended — it's what makes the dashboard actionable), or per-dir
   aggregates only (smallest payload; model rows only in `citadel status`)?
2. **Worker-side freshness** (§9.3/§9.6): is scan-at-startup-only staleness
   acceptable for the reported numbers (a long-lived worker's scan metadata
   goes days/weeks stale against out-of-band container downloads), or
   should the WORKER re-run `ReconcileScan` on a slow timer (e.g. 24h)?
   §8.5 rejected per-tick rescans; a daily one inside the single-writer
   worker is a different cost profile (one du-scale walk/day). Recommended:
   add the daily rescan — P3's numbers are only as good as their scan.

### P5

3. **Backfill evictability — the headline fork** (§10.3.4): recommended
   EVICTABLE (the #682 40G is backfill; exempting it guts the feature),
   reversing the shipped comment's presumption. Confirm, or keep
   `SourceBackfill` exempt in v1 and accept that P5 then only manages
   weights pulled after P2a shipped?
4. **Trigger/budget** (§10.1, carries §6 Q2/§8.7 Q5): disk-percent
   high/low water on the cache volume (recommended: 90/80) — or do you also
   want an absolute byte budget (per cache dir or global) for proactive
   trimming on big-disk nodes where 90% is already catastrophic for
   neighbors?
5. **Pinning source** (§10.3.2, carries §6 Q1): manifest `pinned_models` in
   `citadel.yaml` (recommended — mirrors `pinned_services`, no index write,
   no new write path), vs a runtime `citadel cache pin` CLI (needs a
   cross-process index-write path that violates the single-writer rule
   today — would have to be a job type or wait for a local job-submission
   path)?
6. **LRU fidelity gate** (§10.2, resolves §8.7 Q3): OK to ship GC on the
   v1 resident-implies-used signal per the §10.2 sufficiency argument
   (recommended), or hold P5 until per-request `MarkUsed` touches (via the
   `RecordEngineRequest` call sites) land first?
