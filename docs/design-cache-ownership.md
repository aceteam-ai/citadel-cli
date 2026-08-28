# Design: Model-Cache Ownership + GC (citadel #682 / #683)

Status: DRAFT for review. No code in this doc; it defines the shape of the fix so
the phased issues below can be filed and implemented independently.

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
