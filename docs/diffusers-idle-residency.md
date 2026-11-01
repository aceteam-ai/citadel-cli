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
the exception and returns only an allowlisted failure kind; it must leave no original
traceback or partial pipeline reference in the retry frame. Garbage/cache/host
cleanup completes before the next attempt, and actual free VRAM is rechecked
for each candidate. All failed candidates produce a static `ModelLoadFailed`
under the existing backoff, not an unbounded retry loop. Explicit quantization
retains its selected candidate and never falls back after a load exception.
There is still one replacement thread and no overlapping candidate loads.
Fake tests cover first-choice success, failed int8/nf4 falling back to offload,
all-failed and explicit-quant failures, partial-reference reclamation before
the next candidate, and concurrent callers sharing one load.
The image/video HTTP load-error boundary emits one trusted requested/configured
model prefix with a static failure classification; original loader diagnostics
and an exception-provided model identity are not returned to clients.

The B1/L4/L5 follow-up retains actionable diagnostics without truncating or
echoing arbitrary exception text. Known exception classes map to finite static
kinds for disk capacity, gated/missing repository, unavailable GPU/dependency,
CUDA/host memory, configuration/API, runtime or model-file failures. Unknown
classes stay generic. HF/CUDA matching uses class identity from already-loaded
fixed module exports, without importing ML/dependency modules for diagnostics;
a same-named class is not enough. Failed candidate logs carry the trusted model,
catalog variant, safe kind and fixed summary, never a traceback, response body,
reflected path or credential. Logs and retained metadata contain no exception
object, including when a fallback succeeds. Disk/repository prepare failures
carry their typed static kind through HTTP 500 rather than masquerading as a
generic pipeline load; Go still surfaces that safe non-200 body unchanged.
The video loader includes its own preflight. If all attempted variants fail
with the same actionable allowlisted kind, that common static kind survives
aggregation; mixed causes or generic runtime/unknown failures remain
`all_candidates_failed`. No message-text inference is used.
The class-identity fixtures install inert fake HF/CUDA module exports; they
exercise the classifier without importing those dependencies. They are not
proof that any particular installed library version exports these classes, or
that a real gated repository, CUDA allocation or model load succeeds.

An outgoing-generation drain timeout gets a dedicated code-owned static
`drain_timeout` reason, not a generic filesystem error.
Known already-loaded HF `LocalEntryNotFoundError`/`EntryNotFoundError` exports
map to model-file availability before the generic ValueError bucket, including
their multiple-inheritance form. An arbitrary loader timeout is not evidence
of a drain failure. Inert class fixtures plus actual fake-manager drain/HTTP
fixtures cover that distinction without importing HF or ML libraries.
The diagnostic hardening uses a stdlib-only helper with
finite literal class-export labels and fixed stages. It compares exact
exception types with builtins or already-loaded fixed exports, without
reflecting module/qualname/message, invoking metaclass subclass hooks or adding
a raw-error flag. Unknown subclasses get `unknown_exception` even if their
broad failure kind is known. Alias labels name the chosen fixed export, not
an assertion about the class's actual module or installed-library behavior.
Original candidate labels are captured before the helper's exception frame
exits and retained only as strings in failed-attempt diagnostics. An outer
`raised_exception_class` can name an aggregate wrapper; it is not reported as
the original candidate cause. Fixed stages distinguish preparation, draining,
loading, unloading, cleanup and preflight estimate/disk probing. Preflight's
existing fail-open policy and lifecycle cleanup/unload behavior are unchanged;
their former raw exception/cache-path warning sinks and lifecycle tracebacks
are replaced with static diagnostics. This contract describes these targeted
paths, not every sidecar endpoint or external library's log output.
Known disk refusals log only bounded plain-integer required/free/margin/needed
values at the actual decision point; invalid/unbounded counts are unknown, and
an arbitrary ENOSPC exception cannot invent preflight measurements.

The helper is explicitly copied into the image by the Dockerfile; a hermetic
source fixture checks that packaging and its stdlib-only imports under the
existing sidecar CI selector. No image build is requested by the feature push:
the unchanged image workflow runs only for main/service paths or manual action.
Class labels still cannot distinguish different kernel/dtype/bitsandbytes
causes wrapped in the same plain RuntimeError. They do not waive the native
diagnostic or image-promotion gate.

When the final remaining candidates are skipped for current VRAM after a failed
attempt, the disposition is the existing `503 insufficient_vram`, not an
all-candidates-failed 500. This is not `model_warming` and does not change Go's
existing warming-only retry policy. Successful `/health.last_load` includes
preferred variant, attempted/fallback counts, capacity-skip count and bounded
failed-variant safe kinds; `seconds` includes the entire candidate loop and its
failed-attempt cleanup. These observations cannot certify native load success.

Fallback does not certify bitsandbytes support, output quality, allocator
behavior or acceptable resource contention. It can use CPU offload only as the
last AUTO fallback, and those weights must still be idle-evicted fully.

`/health.pipeline_residency` reports image/video model state, `resident`, `pinned`,
`idle_seconds` and `evicts_at` (Unix epoch seconds, or null when not scheduled).
`host_rss_bytes` is approximate current process RSS on Linux, not a per-model
attribution or the historical peak. It is null if unavailable. Both cold paths
retain the retryable `503 model_warming`/`Retry-After` contract. Video inference,
seed setup and export errors release their lease just like successful requests.
Unlike the former synchronous video singleton, a cold video request may return
this 503 after warm-wait; the existing Go image/video handler retries it within
its unchanged request budget. Unclaimed-load grace is Retry-After plus warm-wait.

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
