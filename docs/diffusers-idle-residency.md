# Diffusers model residency

`ImageModelManager` owns both image and video lifecycle policy. Each pipeline
has its own manager, active-request leases and generation-safe idle timer.
Image model swaps remain independent of the video model. This does not reserve
GPU resources against other services or introduce a video VRAM budget.

`DIFFUSERS_IDLE_UNLOAD_SECONDS` defaults to 300 seconds. The final lease release
starts that interval; zero schedules immediate cleanup. A new lease cancels the
old timer. A canceled timer cannot evict a newly leased/replaced model. Idle
cleanup and model replacement cannot load a new pipeline over ongoing cleanup.
Failed replacement preparation restores the outgoing model's original idle
deadline rather than extending it indefinitely.

Replacement preparation does not grant residency: while the next model is
being downloaded/prepared, the outgoing idle model keeps its original eviction
deadline. A new outgoing-model lease still cancels that timer, and pins remain
exempt. The replacement thread enters an exclusive replacement phase only
after preparation: it first waits for any idle teardown to finish, then fences
further idle callbacks before memory probing, draining, unloading and loading.
Preparation failures enter the same serialized phase before memory cleanup,
then restore any still-resident idle model's original deadline. Deterministic
blocked-preparation tests cover expiration, renewed leases, pins, failure and
cleanup/load exclusion; they are source/CI tests, not native memory acceptance.

An asynchronous cold load which has not yet served a lease gets at least
`Retry-After + warm-wait` seconds before idle eviction. This bounded grace prevents zero
idle time from repeatedly discarding a background load before its warming
caller can obtain the first lease. After a job, zero means zero: the old
minimum-residency protection for swaps does not delay idle unloading.

`DIFFUSERS_PINNED_MODELS` is a comma/whitespace-separated list of exact model IDs,
including the configured video model when desired. Empty means no pins. A pin
prevents idle eviction, not an explicit image-model swap. Graph/request fields
cannot grant pinning or change these operator settings. The compose template
interpolates both settings from the environment of the worker invoking Compose;
there is no new `citadel.yaml` knob. Pins do not preload a model at startup or
restore it after an explicit swap. Startup emits a static warning if any pin
matches neither an allowed image model nor the configured video model. Negative
idle values clamp to zero with a warning; invalid/non-finite values warn and use
the default. Warnings do not echo operator-provided values or model IDs.

Cleanup removes pipeline offload hooks, clears manager/lease references,
collects Python garbage, releases CUDA caches and asks glibc's `malloc_trim(0)`
to return freed arenas where supported. A released lease must not retain the
pipeline. Callers must drop their own pipeline/result references before
releasing the lease. This is source behavior, not proof of a particular RSS
decrease: allocator/library references and real GPU behavior require measurement.

In `auto`, GPU-only variants retain catalog quality ordering, ahead of all CPU
offload variants. Thus the existing FLUX estimate chooses GPU int8 at a 12 GiB
budget rather than full-precision CPU offload. Explicit precision settings
still constrain the choices. Estimates are not observed GPU/RAM measurements.

## Source hardening plan after independent review

AUTO attempts each eligible catalog candidate at most once, in the same
GPU-first/offload-last order. If a loader raises, the attempt helper discards
the exception and returns only failure state; it must leave no original
traceback or partial pipeline reference in the retry frame. Garbage/cache/host
cleanup completes before the next attempt, and actual free VRAM is rechecked
for each candidate. All failed candidates produce a static `ModelLoadFailed`
under the existing backoff, not an unbounded retry loop. Explicit quantization
retains its selected candidate and never falls back after a load exception.
There is still one replacement thread and no overlapping candidate loads.
Fake tests cover first-choice success, failed int8/nf4 falling back to offload,
all-failed and explicit-quant failures, partial-reference reclamation before
the next candidate, and concurrent callers sharing one load.

Fallback does not certify bitsandbytes support, output quality, allocator
behavior or acceptable resource contention. It can use CPU offload only as the
last AUTO fallback, and those weights must still be idle-evicted fully.

`/health.pipeline_residency` reports image/video model state, `resident`, `pinned`,
`idle_seconds` and `evicts_at` (Unix epoch seconds, or null when not scheduled).
`host_rss_bytes` is approximate current process RSS on Linux, not a per-model
attribution or the historical peak. It is null if unavailable. Both cold paths
retain the retryable `503 model_warming`/`Retry-After` contract. Video inference,
seed setup and export errors release their lease just like successful requests.

## Acceptance still required

Normal existing Python CI covers faked lifecycle/HTTP behavior without torch.
Local candidate execution is held; no GPU/model download/inference/service
execution is authorized by this source work. On a separately authorized GPU
node, measure idle/pinned image AND video requests: current process RSS must
return near measured cold baseline and GPU allocation near zero after eviction.
Check auto FLUX int8 selection at the configured 12 GiB budget. Do not replace
these measurements with reference-drop tests or historical VRAM estimates.
Actual FLUX int8 loading, quality, speed and shared-GPU contention are hard
prerequisites of ANY future image pin bump containing this policy, including a
bump otherwise motivated by an unrelated change. Fallback tests do not lift it.

The compose image digest deliberately remains unchanged in this source draft.
After reviewed source publication produces a verified image, follow #1346:
review the exact build/tag/digest, then bump the immutable compose image pin and
register that template hash in a separate reviewed source change. The new env
template hash here is not an image pin or rollout receipt. Until then, an
existing pinned image does not implement the new lifecycle policy. #1383 stays
open for image promotion and real RSS/VRAM acceptance; no deployment is implied.
