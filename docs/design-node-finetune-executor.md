# Design: Node-side `FINETUNE_START` executor (headless Unsloth trainer)

Status: **DRAFT — for review.** No implementation until approved.
Tracks: aceteam-ai/citadel-cli#1090.

## 1. Problem

AceTeam's backend already ships a complete finetuning product — a `components/FineTuning/`
UI, the `finetune_model`/`finetune_status`/`finetune_cancel` MCP tools, dispatch wiring,
Redis job tracking, and a base-model allowlist — but **it executes nothing**, because no
Citadel node handles the `FINETUNE_START` job it dispatches. The backend fails closed
(HTTP 503) rather than enqueue where nothing consumes:

> `aceteam/python-backend/routes/fabric_finetune.py`: *"Today no Citadel node handles
> `FINETUNE_START` (the training executor is missing)."*
> `aceteam/python-backend/models/fabric_finetune.py`: *"The Citadel worker is expected to
> handle `FINETUNE_START` by running an Unsloth-based training container, streaming
> progress via Redis Pub/Sub on `stream:v1:{job_id}`, and storing adapter weights to a
> volume."*

This design adds that node-side executor. Unsloth (newly shipped as a Docker image /
`unsloth` Core pip package, Apache-2.0 core) is the intended trainer.

Explicitly NOT this design: the interactive **Unsloth Studio** GUI (a separate, orthogonal
catalog-app product — a finetuning GUI on your own GPU node — that shares only the `unsloth`
pip package with this executor and lights up none of the fabric pipeline above).

## 2. Fixed contract (the node conforms; it does not invent)

The backend contract is already merged and shipping. The node must match it exactly.

**Job envelope** on the target per-node stream: `{jobId, type: "FINETUNE_START", payload}`.

**`FINETUNE_START` payload** (`fabric_finetune.py` enqueue + `FineTuneCreateRequest`):

| Field | Type | Notes |
|---|---|---|
| `model` | string | one of `Qwen/Qwen3-8B`, `Qwen/Qwen3-0.6B` (v1 allowlist) |
| `method` | `lora`\|`qlora`\|`full` | default `lora` |
| `suffix` | string ≤64 | appended to the output model name |
| `hyperparameters` | object | `epochs`(1–100,d3) `learning_rate`(d2e-4) `batch_size`(1–128,d4) `lora_r`(d16) `lora_alpha`(d32) `lora_dropout`(d0.05) `max_seq_length`(128–32768,d2048) |
| dataset — exactly one | | `dataset_url` (https JSONL, SSRF-guarded) **or** `dataset_node_id`+`dataset_node_path` |
| `node_id` | string | target node |

Node-local dataset invariant: `dataset_node_id == node_id`, path is workspace-relative and
already lexically confined by the backend (`validate_node_path`, `allow_absolute=False`) —
so the dataset never transits and the handler reads a local workspace path. Dataset format
is **JSONL**.

**Progress the node must surface** — the UI reads these fields from the
`finetune:job:{job_id}` Redis hash: `status` (`pending→queued→running→{succeeded|failed|cancelled}`),
`progress_percent`, `current_epoch`, `current_loss`, `eta_seconds`, `error`, `started_at`,
`finished_at`.

**Output:** adapter weights to a **mounted node volume** (per the backend docstrings).
**v1 keeps the adapter node-local — there is no upload.** This is consistent with the
sovereign posture (weights stay on the operator's hardware) and removes the multi-GB
artifact-channel problem from v1 entirely.

## 3. Node-side design

### 3.1 Job type + handler

- Add `FINETUNE_START` to `internal/worker/job.go` (const block + `allKnownJobTypes`).
- New handler `internal/jobs/finetune_handler.go`, modeled on the run-to-completion
  `build_handlers.go` shape (`CanHandle`/`Execute`, `docker run` to completion, parse
  output, return terminal result). Registered in `buildNodeJobHandlers` (`cmd/nodejobs.go`).
- **`FINETUNE_STATUS`/`FINETUNE_CANCEL` need no node job type.** STATUS is a backend read
  of the Redis hash; CANCEL flips the hash to `cancelled` and is observed on the node via
  the existing cancellation flag (see 3.4).

### 3.2 Trainer image (new, Apache-2.0)

A headless training container — **Unsloth Core, not the AGPL Studio image** — published to
GHCR, mirroring the `citadel-inference-server` pattern but **run-to-completion** rather than
a persistent server. Proposed repo: `aceteam-ai/citadel-finetune` (or a `train` task/adapter
inside `citadel-inference-server` if that runtime is extended to non-serving tasks — open
question, §5).

Container contract (handler → container):
- Inputs via args/env: base model, dataset path (mounted), `method`, hyperparameters,
  output dir (mounted), HF cache dir (mounted, reuses `~/citadel-cache/huggingface`).
- Emits **NDJSON progress on stdout** the handler parses: `{epoch, step, loss, pct, eta_s}`
  → mapped to `current_epoch`/`current_loss`/`progress_percent`/`eta_seconds`.
- Writes the LoRA adapter to the mounted output dir on success; non-zero exit ⇒ `failed`.
- GPU: NVIDIA runtime, driver ≥570.26 (Unsloth's stated floor).

### 3.3 Progress + terminal reporting

The handler emits progress through the worker's existing `StreamWriter` on
`stream:v1:{jobId}` (the channel the backend docstring names), and the terminal result
carries the final status. **Open seam (§5):** confirm/implement the backend consumer that
folds `stream:v1:{jobId}` events into the `finetune:job:{job_id}` hash the UI reads — or,
in direct-Redis mode, have the node write the hash fields directly.

### 3.4 Cancellation

The handler must be interruptible — `select` on `ctx.Done()` / poll `IsJobCancelled`
during the training loop and kill the container (the #488 wait-loop-cancellation pattern),
reporting `cancelled`. A `docker run` with a process-group kill on ctx-cancel is the
mechanism.

### 3.5 Deadline / lane

- **Unbounded watchdog tier** (`internal/worker/deadline.go`) — training is multi-hour,
  like `MODEL_CACHE_PULL`/`SERVICE_START`. Add `FINETUNE_START` to the unbounded set.
- **Always-async + GPU-aware:** training holds VRAM for hours, so it must not block the
  fetch loop and must not silently collide with inference. Options: add to
  `gpuBoundJobTypes` (inference lane), or drive #832 `ReserveExclusive`/#577 preemption to
  hold VRAM for the training's lifetime. Decision deferred to review (§5).

### 3.6 Output storage + GC

Adapter output dir (proposed `~/citadel-cache/finetune/{job_id}/`) must be represented in
the `cache_gc` allowlist (`internal/jobs/cache_gc.go`) or explicitly excluded, so a
finetuned adapter is never swept as an orphan. The output-model name is `model + suffix`.

## 4. Phasing

1. **P0 — executor happy path:** job type + handler + trainer image; node-local dataset
   (`dataset_node_path`) only; LoRA on Qwen3-0.6B (fast to validate on a 24GB card);
   progress stream; adapter to volume; `succeeded`/`failed`. Deadline + cancellation.
2. **P1 — robustness:** `dataset_url` download path (SSRF guard already backend-side, plus
   node-side size/format checks); Qwen3-8B; GPU reservation/preemption wiring; `cache_gc`
   allowlist; the backend progress-hash consumer (cross-repo).
3. **P2 (separate issues):** serve/deploy the node-local adapter (vLLM LoRA load or
   merge→GGUF); QLoRA/full; larger base-model allowlist; artifact export if a use case
   ever needs weights off-node.

## 5. Open questions (for review before P0)

1. **Trainer image home:** new `aceteam-ai/citadel-finetune` repo, or a `train` task in
   `citadel-inference-server` (only if that runtime is meant to host non-serving,
   run-to-completion tasks — its design doc isn't in this repo to confirm)?
2. **Progress-hash consumer:** does an aceteam-side consumer already fold
   `stream:v1:{jobId}` → `finetune:job:{job_id}`, or is that new backend work / a
   direct-Redis-mode node write?
3. **VRAM policy:** GPU-slot gate vs. exclusive reservation for the training's lifetime —
   how should a long training coexist with (or preempt) node inference?
4. **API mode vs direct-Redis:** does the fleet run finetune nodes in API-proxy mode
   (progress via StreamWriter) or direct-Redis (node writes the hash)? Affects §3.3.

## 6. References

- Backend: `aceteam/python-backend/routes/fabric_finetune.py`,
  `aceteam/python-backend/models/fabric_finetune.py`,
  `aceteam/python-backend/routes/fabric_dispatch.py` (`ALLOWED_JOB_TYPES`).
- Node handler shape: `internal/jobs/build_handlers.go`, `internal/jobs/model_cache_pull.go`.
- Job types: `internal/worker/job.go`. Deadline tiers: `internal/worker/deadline.go`.
- Cancellation pattern: citadel-cli#488. GPU reserve/preempt: citadel-cli#832 / #577.
- Reference image pattern: `citadel-inference-server` (omnivoice wiring).
