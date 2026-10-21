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

An asynchronous cold load which has not yet served a lease gets at least one
`Retry-After` interval before idle eviction. This bounded grace prevents zero
idle time from repeatedly discarding a background load before its warming
caller can obtain the first lease. After a job, zero means zero: the old
minimum-residency protection for swaps does not delay idle unloading.

`DIFFUSERS_PINNED_MODELS` is a comma/whitespace-separated list of exact model IDs,
including the configured video model when desired. Empty means no pins. A pin
prevents idle eviction, not an explicit image-model swap. Graph/request fields
cannot grant pinning or change these operator settings. The compose template
passes both settings through the existing node environment mechanism.

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

The compose image digest deliberately remains unchanged in this source draft.
After reviewed source publication produces a verified image, follow #1346:
review the exact build/tag/digest, then bump the immutable compose image pin and
register that template hash in a separate reviewed source change. The new env
template hash here is not an image pin or rollout receipt. Until then, an
existing pinned image does not implement the new lifecycle policy. #1383 stays
open for image promotion and real RSS/VRAM acceptance; no deployment is implied.
