"""One-at-a-time image model residency with VRAM-aware swap (citadel-cli#1335).

The node's single 24 GB GPU is shared with ollama, vLLM and OmniVoice, so this
sidecar holds at most ONE image pipeline on the GPU. A request for a different
allowlisted model triggers a swap: wait for in-flight generations on the
resident model to finish, free it (and the CUDA cache), check the card really
has room, then load the new one.

The load runs on a background thread so a request NEVER hangs on a cold load.
``lease`` waits up to ``warm_wait_seconds`` for the load to finish (a quick
model often does), and otherwise raises ``ModelWarming``, which app.py turns
into a retryable 503 (the same model_warming contract the rest of the node
uses; aceteam #11404). The caller retries after ``retry_after``.

Heavy work is injected (``loader``, ``unloader``, ``free_memory``,
``free_vram_bytes``), so every state transition is unit-testable with fakes
and no torch, diffusers or GPU. app.py supplies the real implementations.

Concurrency contract:
  * one load at a time (``_load``); a request for a different model while a
    load is running gets ``ModelWarming(reason="busy")`` rather than queueing;
  * while a swap is pending the OUTGOING model accepts no new leases (it would
    starve the swap), so those requests also get a warming response;
  * leases count in-flight generations; the swap waits for the count to reach
    zero (bounded by ``drain_timeout_seconds``) before unloading.
"""

from __future__ import annotations

import errno
import logging
import sys
import threading
import time
from dataclasses import dataclass, field
from enum import Enum
from typing import Any, Callable

from image_models import GIB, ImageModelSpec, LoadVariant, choose_variant, fitting_variants
from model_preflight import InsufficientDiskSpaceError

logger = logging.getLogger(__name__)


class ModelWarming(Exception):
    """The model is not resident yet. Retryable: HTTP 503 + Retry-After."""

    def __init__(
        self,
        model_id: str,
        *,
        eta_seconds: int,
        retry_after: int,
        reason: str,
        loading: str | None = None,
        resident: str | None = None,
    ):
        self.model_id = model_id
        # The model currently on the GPU, when that is why we are warming.
        self.resident = resident
        self.eta_seconds = max(int(eta_seconds), 0)
        self.retry_after = max(int(retry_after), 1)
        # "loading": this model is being loaded.
        # "busy": a different model is loading, ours is next.
        # "swapping_out": this model is the one being replaced.
        self.reason = reason
        self.loading = loading
        super().__init__(f"{model_id} is warming ({reason})")


class FailureKind(Enum):
    LOAD_FAILED = "load_failed"
    ALL_FAILED = "all_candidates_failed"
    DISK_SPACE = "disk_space"
    REPO_GATED = "repository_access_denied"
    REPO_MISSING = "repository_not_found"
    GPU_UNAVAILABLE = "gpu_unavailable"
    CUDA_MEMORY = "cuda_memory"
    HOST_MEMORY = "host_memory"
    DEPENDENCY = "dependency_unavailable"
    CONFIGURATION = "invalid_model_configuration"
    API = "incompatible_model_api"
    RUNTIME = "model_runtime"
    MODEL_IO = "model_files_unavailable"


_FAILURE_SUMMARIES = {
    FailureKind.LOAD_FAILED: "pipeline load failed",
    FailureKind.ALL_FAILED: "all eligible pipeline loads failed",
    FailureKind.DISK_SPACE: "insufficient disk space for model download",
    FailureKind.REPO_GATED: "model repository access denied",
    FailureKind.REPO_MISSING: "model repository not found or inaccessible",
    FailureKind.GPU_UNAVAILABLE: "a GPU is required for the selected model variant",
    FailureKind.CUDA_MEMORY: "GPU memory allocation failed",
    FailureKind.HOST_MEMORY: "host memory allocation failed",
    FailureKind.DEPENDENCY: "model runtime dependency is unavailable",
    FailureKind.CONFIGURATION: "model configuration was refused",
    FailureKind.API: "model runtime API is incompatible",
    FailureKind.RUNTIME: "model runtime failed",
    FailureKind.MODEL_IO: "model files are unavailable",
}
_ACTIONABLE_FAILURES = frozenset(_FAILURE_SUMMARIES).difference({
    FailureKind.LOAD_FAILED, FailureKind.ALL_FAILED, FailureKind.RUNTIME,
})


def failure_summary(kind) -> str:
    return _FAILURE_SUMMARIES[kind if isinstance(kind, FailureKind) else FailureKind.LOAD_FAILED]


def _loaded_exception(exc: Exception, module_name: str, class_name: str) -> bool:
    # Inspect only fixed exports already loaded by the real operation. Never
    # import a dependency just to classify an error, or trust an error's name.
    module = sys.modules.get(module_name)
    cls = vars(module).get(class_name) if module is not None else None
    return isinstance(cls, type) and issubclass(cls, Exception) and isinstance(exc, cls)


def classify_failure(exc: Exception) -> FailureKind:
    if isinstance(exc, ModelLoadFailed):
        return exc.kind if isinstance(exc.kind, FailureKind) else FailureKind.LOAD_FAILED
    if isinstance(exc, InsufficientDiskSpaceError) or (
        isinstance(exc, OSError) and exc.errno == errno.ENOSPC
    ):
        return FailureKind.DISK_SPACE
    for name, kind in (("GatedRepoError", FailureKind.REPO_GATED),
                       ("RepositoryNotFoundError", FailureKind.REPO_MISSING)):
        if any(_loaded_exception(exc, module, name) for module in
               ("huggingface_hub.errors", "huggingface_hub.utils._errors")):
            return kind
    if any(_loaded_exception(exc, module, "OutOfMemoryError") for module in ("torch", "torch.cuda")):
        return FailureKind.CUDA_MEMORY
    for cls, kind in (
        (MemoryError, FailureKind.HOST_MEMORY), (ImportError, FailureKind.DEPENDENCY),
        (ValueError, FailureKind.CONFIGURATION), (TypeError, FailureKind.API),
        (RuntimeError, FailureKind.RUNTIME), (OSError, FailureKind.MODEL_IO),
    ):
        if isinstance(exc, cls):
            return kind
    return FailureKind.LOAD_FAILED


class ModelLoadFailed(RuntimeError):
    """The load itself failed (bad repo id, auth, disk, library error). HTTP 500."""

    def __init__(self, model_id: str, kind=FailureKind.LOAD_FAILED):
        self.model_id = model_id
        self.kind = kind if isinstance(kind, FailureKind) else FailureKind.LOAD_FAILED
        self.message = failure_summary(self.kind)
        super().__init__(f"failed to load model {model_id}: {self.message}")


class InsufficientVram(RuntimeError):
    """After freeing the previous model the card still lacks the VRAM the model
    needs (other services hold it). HTTP 503, not a warming state."""

    def __init__(self, model_id: str, need_gb: float, free_gb: float):
        self.model_id = model_id
        self.need_gb = need_gb
        self.free_gb = free_gb
        super().__init__(
            f"{model_id} needs about {need_gb:.1f} GB of free VRAM but only {free_gb:.1f} GB is free"
        )


@dataclass
class _Resident:
    spec: ImageModelSpec
    variant: LoadVariant
    pipe: Any
    device: str
    loaded_at: float
    load_seconds: float
    measured_vram_gb: float | None
    inflight: int = 0
    # Set once any request has leased this pipeline. Until then (and for
    # min_residency_seconds) a different model may not evict it, so two clients
    # alternating models cannot evict each other's fresh load before it serves.
    served: bool = False
    idle_since: float | None = None


@dataclass
class _Load:
    spec: ImageModelSpec
    variant: LoadVariant
    started_at: float
    event: threading.Event = field(default_factory=threading.Event)
    error: Exception | None = None
    # True once the load has started displacing the resident model. Until then
    # (while it is only downloading) the resident model keeps serving.
    evicting: bool = False
    # Every variant that fits the budget, best first; ``variant`` is narrowed to
    # what fits the measured free VRAM once the previous model is out.
    candidates: tuple = ()


@dataclass
class Lease:
    """A held reference to the resident pipeline. Always release it (use as a
    context manager) so a pending swap can proceed."""

    pipe: Any
    device: str
    model_id: str
    variant: LoadVariant
    _manager: "ImageModelManager"
    _resident: _Resident
    _released: bool = False

    def release(self) -> None:
        if not self._released:
            self._released = True
            # A released Lease may outlive its request. It must not retain
            # model weights after the manager evicts the resident pipeline.
            self.pipe = None
            self._manager._release(self._resident)

    def __enter__(self) -> "Lease":
        return self

    def __exit__(self, *exc: object) -> None:
        self.release()


LoaderFn = Callable[[ImageModelSpec, LoadVariant], "tuple[Any, str]"]


class ImageModelManager:
    # An unreported failure older than its backoff plus this is dropped, not delivered.
    _STALE_FAILURE_SECONDS = 60.0

    def __init__(
        self,
        specs: dict[str, ImageModelSpec],
        default_model: str,
        *,
        loader: LoaderFn,
        unloader: Callable[[Any], None] = lambda pipe: None,
        free_memory: Callable[[], None] = lambda: None,
        free_vram_bytes: Callable[[], int | None] = lambda: None,
        prepare: Callable[[ImageModelSpec, LoadVariant], None] = lambda spec, variant: None,
        min_residency_seconds: float = 120.0,
        failure_backoff_seconds: float = 30.0,
        vram_budget_gb: float | None = None,
        quant_mode: str = "auto",
        warm_wait_seconds: float = 15.0,
        drain_timeout_seconds: float = 600.0,
        retry_after_seconds: int = 5,
        clock: Callable[[], float] = time.monotonic,
        idle_unload_seconds: float = 300.0,
        pinned_models: frozenset[str] = frozenset(),
        timer_factory: Callable = threading.Timer,
    ):
        self._specs = specs
        self._default_model = default_model
        self._loader = loader
        self._unloader = unloader
        self._free_memory = free_memory
        self._free_vram_bytes = free_vram_bytes
        # Disk-side preparation (free-space preflight + weight prefetch). Runs
        # BEFORE replacement eviction. The outgoing model can keep serving,
        # but an unpinned idle model still expires during a slow download.
        self._prepare = prepare
        self._min_residency = max(min_residency_seconds, 0.0)
        self._failure_backoff = max(failure_backoff_seconds, 0.0)
        self._budget_gb = vram_budget_gb
        self._quant_mode = quant_mode
        self._warm_wait = max(warm_wait_seconds, 0.0)
        self._drain_timeout = drain_timeout_seconds
        self._retry_after = max(int(retry_after_seconds), 1)
        self._clock = clock
        self._idle_unload = max(idle_unload_seconds, 0.0)
        self._pinned = frozenset(pinned_models)
        self._timer_factory = timer_factory
        self._idle_timer = None
        self._idle_token = 0
        self._idle_evicting = False

        self._cond = threading.Condition()
        self._resident: _Resident | None = None
        self._load: _Load | None = None
        # model_id -> (error, retry_not_before, already_reported)
        self._failures: dict[str, list] = {}
        self._last_load: dict[str, Any] | None = None
        self._thread: threading.Thread | None = None

    # -- public API ---------------------------------------------------------

    @property
    def vram_budget_gb(self) -> float | None:
        return self._budget_gb

    @property
    def quant_mode(self) -> str:
        return self._quant_mode

    def lease(self, model_id: str) -> Lease:
        """Returns a lease on the pipeline for ``model_id``, starting a load if
        needed. Raises ModelWarming (retryable), InsufficientVram,
        ModelLoadFailed, or image_models.VramBudgetError. Never blocks longer
        than ``warm_wait_seconds`` plus scheduling noise."""
        spec = self._specs[model_id]
        with self._cond:
            outcome = self._try_locked(spec)
            if isinstance(outcome, Lease):
                return outcome
            load = outcome
        if self._warm_wait <= 0:
            with self._cond:
                raise self._warming_locked(spec, "loading")
        load.event.wait(self._warm_wait)
        with self._cond:
            outcome = self._try_locked(spec)
            if isinstance(outcome, Lease):
                return outcome
            raise self._warming_locked(spec, "loading")

    def wait_for_load(self, timeout: float | None = None) -> bool:
        """Blocks until no load is in flight. True when idle. Test and ops hook."""
        deadline = None if timeout is None else self._clock() + timeout
        with self._cond:
            while self._load is not None:
                remaining = None if deadline is None else deadline - self._clock()
                if remaining is not None and remaining <= 0:
                    return False
                self._cond.wait(remaining)
            return True

    def loaded_model(self) -> str | None:
        with self._cond:
            return self._resident.spec.model_id if self._resident else None

    def loading_model(self) -> str | None:
        with self._cond:
            return self._load.spec.model_id if self._load else None

    def resident_device(self) -> str | None:
        with self._cond:
            return self._resident.device if self._resident else None

    def status(self) -> dict[str, Any]:
        """Health-shaped snapshot. ``state`` per model:
        warm = resident on the GPU now; loading = load in flight;
        cold = allowlisted, loads on demand (first request returns warming)."""
        with self._cond:
            resident_id = self._resident.spec.model_id if self._resident else None
            loading_id = self._load.spec.model_id if self._load else None
            models = []
            for spec in self._specs.values():
                if spec.model_id == resident_id:
                    state = "warm"
                    variant = self._resident.variant if self._resident else None
                elif spec.model_id == loading_id:
                    state = "loading"
                    variant = self._load.variant if self._load else None
                else:
                    state = "cold"
                    variant = self._plan_or_none(spec)
                models.append(
                    {
                        "model": spec.model_id,
                        "license": spec.license,
                        "permissive_license": spec.permissive,
                        "state": state,
                        "default": spec.model_id == self._default_model,
                        "quantization": variant.quant if variant else None,
                        "cpu_offload": (variant.offload != "none") if variant else None,
                        "est_vram_gb": variant.est_vram_gb if variant else None,
                        "fits_vram_budget": variant is not None,
                        "est_disk_gb": spec.est_disk_gb or None,
                        "resident": spec.model_id == resident_id,
                        "pinned": spec.model_id in self._pinned,
                        **self._idle_status_locked(spec.model_id),
                    }
                )
            return {
                "allowed_models": models,
                "loaded_model": resident_id,
                "loading_model": loading_id,
                "vram_budget_gb": self._budget_gb,
                "quant_mode": self._quant_mode,
                "last_load": dict(self._last_load) if self._last_load else None,
                "idle_evicting": self._idle_evicting,
            }

    # -- internals ----------------------------------------------------------

    def _plan_or_none(self, spec: ImageModelSpec) -> LoadVariant | None:
        try:
            return choose_variant(spec, self._budget_gb, self._quant_mode)
        except Exception:  # VramBudgetError: reported as fits_vram_budget=false
            return None

    def _try_locked(self, spec: ImageModelSpec) -> "Lease | _Load":
        if self._idle_evicting:
            raise self._warming_locked(spec, "busy")
        entry = self._failures.get(spec.model_id)
        if entry is not None:
            err, not_before, reported = entry
            now = self._clock()
            # Drop an entry once its backoff has passed and it was reported (or
            # is long stale), so an old failure is never delivered to a fresh,
            # unrelated request.
            if now >= not_before and (reported or now >= not_before + self._STALE_FAILURE_SECONDS):
                del self._failures[spec.model_id]
            else:
                # Reported once to the request that triggered it, then repeated
                # for the backoff window so a retry loop cannot hammer a load
                # that just failed (and evict a working model each time).
                entry[2] = True
                raise err

        resident = self._resident
        if (
            resident is not None
            and resident.spec.model_id == spec.model_id
            and (self._load is None or not self._load.evicting)
        ):
            resident.served = True
            resident.inflight += 1
            resident.idle_since = None
            self._cancel_idle_locked()
            return Lease(
                pipe=resident.pipe,
                device=resident.device,
                model_id=spec.model_id,
                variant=resident.variant,
                _manager=self,
                _resident=resident,
            )

        if self._load is None:
            variant = choose_variant(spec, self._budget_gb, self._quant_mode)  # VramBudgetError
            if resident is not None and not resident.served:
                remaining = self._min_residency - (self._clock() - resident.loaded_at)
                if remaining > 0:
                    # The resident model has not served anyone yet: let its
                    # requester use it before another model displaces it.
                    raise ModelWarming(
                        spec.model_id,
                        eta_seconds=int(remaining) + spec.est_load_seconds,
                        retry_after=self._retry_after,
                        reason="resident_protected",
                        resident=resident.spec.model_id,
                    )
            load = _Load(
                spec=spec,
                variant=variant,
                started_at=self._clock(),
                candidates=tuple(fitting_variants(spec, self._budget_gb, self._quant_mode)),
            )
            self._load = load
            self._thread = threading.Thread(
                target=self._run_load, args=(load,), name=f"diffusers-load-{spec.model_id}", daemon=True
            )
            self._thread.start()
            return load

        if self._load.spec.model_id == spec.model_id:
            return self._load

        if resident is not None and resident.spec.model_id == spec.model_id:
            raise self._warming_locked(spec, "swapping_out")
        raise self._warming_locked(spec, "busy")

    def _warming_locked(self, spec: ImageModelSpec, reason: str) -> ModelWarming:
        load = self._load
        eta = spec.est_load_seconds
        if load is not None and load.spec.model_id == spec.model_id:
            eta = max(spec.est_load_seconds - int(self._clock() - load.started_at), 0)
        elif load is not None:
            eta = load.spec.est_load_seconds + spec.est_load_seconds
        return ModelWarming(
            spec.model_id,
            eta_seconds=eta,
            retry_after=self._retry_after,
            reason=reason,
            loading=load.spec.model_id if load else None,
        )

    def _release(self, resident: _Resident) -> None:
        with self._cond:
            if resident.inflight > 0:
                resident.inflight -= 1
                if resident.inflight == 0 and self._resident is resident:
                    resident.idle_since = self._clock()
                    self._arm_idle_locked(resident)
            self._cond.notify_all()

    def _cancel_idle_locked(self) -> None:
        self._idle_token += 1
        if self._idle_timer is not None:
            self._idle_timer.cancel()
            self._idle_timer = None

    def _idle_deadline(self, resident: _Resident) -> float:
        # A cold background load may finish after the caller got 503. Give
        # that caller Retry-After plus warm-wait to acquire its first lease,
        # even with idle=0, rather than evicting before it can ever serve.
        grace = self._idle_unload if resident.served else max(
            self._idle_unload, self._retry_after + self._warm_wait
        )
        return resident.idle_since + grace

    def _idle_status_locked(self, model_id: str) -> dict[str, Any]:
        resident = self._resident
        idle = resident.idle_since if resident and resident.spec.model_id == model_id else None
        remaining = None
        if (
            idle is not None and model_id not in self._pinned
            and (self._load is None or not self._load.evicting)
        ):
            remaining = max(self._idle_deadline(resident) - self._clock(), 0.0)
        return {
            "idle_seconds": round(max(self._clock() - idle, 0.0), 2) if idle is not None else None,
            # Epoch seconds, not a process-local monotonic clock reading.
            "evicts_at": time.time() + remaining if remaining is not None else None,
        }

    def _arm_idle_locked(self, resident: _Resident) -> None:
        self._cancel_idle_locked()
        if (
            resident.inflight or resident.spec.model_id in self._pinned
            or (self._load is not None and self._load.evicting)
        ):
            return
        token = self._idle_token
        delay = max(self._idle_deadline(resident) - self._clock(), 0.0)
        timer = self._timer_factory(delay, lambda: self._evict_idle(resident, token))
        timer.daemon = True
        self._idle_timer = timer
        timer.start()

    def _evict_idle(self, resident: _Resident, token: int) -> None:
        with self._cond:
            if (
                token != self._idle_token or self._resident is not resident
                or resident.inflight or resident.spec.model_id in self._pinned
                or (self._load is not None and self._load.evicting)
            ):
                return
            if self._clock() < self._idle_deadline(resident):
                self._arm_idle_locked(resident)
                return
            self._cancel_idle_locked()
            self._resident = None
            self._idle_evicting = True
        try:
            self._drop_pipeline(resident)
            # All manager, lease and unload-frame references are gone before
            # collection/cache release/host allocator trimming happens.
            self._free_memory()
        except Exception:
            logger.exception("diffusers: idle memory cleanup failed")
        finally:
            with self._cond:
                self._idle_evicting = False
                self._cond.notify_all()

    def _drop_pipeline(self, resident: _Resident) -> None:
        pipe = resident.pipe
        resident.pipe = None
        try:
            self._unloader(pipe)
        except Exception:
            logger.exception("diffusers: pipeline unload hook failed")
        finally:
            del pipe

    def _run_load(self, load: _Load) -> None:
        spec, variant = load.spec, load.variant
        try:
            candidates = load.candidates or (variant,)
            smallest = min(candidates, key=lambda v: v.est_vram_gb)
            # Disk first: the resident model can keep serving during preflight
            # and download, without extending its unpinned idle deadline.
            self._prepare(spec, variant)
            self._enter_replacement_phase(load)
            self._refuse_if_eviction_cannot_help(spec, smallest.est_vram_gb * GIB)
            self._swap_out_previous(load)
            # Unconditional, so cached blocks left by a previous failed load or
            # an earlier unload never make free VRAM look smaller than it is.
            self._free_memory()
            started = self._clock()
            attempted = 0
            failures = []
            failure_kinds = set()
            skipped_for_vram = 0
            trailing_shortfall = None
            for variant in candidates:
                # Recheck after every failed attempt's cleanup. Other services
                # can change their allocations while this load is running.
                free_before = self._free_vram_bytes()
                if free_before is not None and variant.est_vram_gb * GIB > free_before:
                    skipped_for_vram += 1
                    # Keep a coherent observed need/free pair for the smallest
                    # candidate skipped after the last actual load attempt.
                    if trailing_shortfall is None or variant.est_vram_gb < trailing_shortfall[0]:
                        trailing_shortfall = (variant.est_vram_gb, free_before / GIB)
                    continue
                trailing_shortfall = None
                with self._cond:
                    load.variant = variant
                attempted += 1
                loaded, failure_kind = self._attempt_pipeline_load(spec, variant)
                if loaded is not None:
                    pipe, device = loaded
                    del loaded
                    break
                # The helper has exited its except block and stack frame.
                # Collect partial allocations BEFORE another candidate loads;
                # only an allowlisted enum crosses here, never a raw diagnostic.
                failures.append({"quantization": variant.quant,
                                 "cpu_offload": variant.offload != "none",
                                 "kind": failure_kind.value})
                failure_kinds.add(failure_kind)
                logger.warning("diffusers: %s candidate %s failed [%s]: %s",
                               spec.model_id, variant.label(), failure_kind.value,
                               failure_summary(failure_kind))
                self._free_memory()
                if self._quant_mode != "auto":
                    raise ModelLoadFailed(spec.model_id, failure_kind)
            else:
                if trailing_shortfall is not None:
                    raise InsufficientVram(spec.model_id, *trailing_shortfall)
                if attempted:
                    common = next(iter(failure_kinds)) if len(failure_kinds) == 1 else None
                    raise ModelLoadFailed(
                        spec.model_id,
                        common if common in _ACTIONABLE_FAILURES else FailureKind.ALL_FAILED,
                    )
                raise InsufficientVram(spec.model_id, smallest.est_vram_gb, free_before / GIB)
            elapsed = self._clock() - started
            free_after = self._free_vram_bytes()
            measured = None
            if free_before is not None and free_after is not None:
                measured = max(free_before - free_after, 0) / GIB

            with self._cond:
                self._resident = _Resident(
                    spec=spec,
                    variant=variant,
                    pipe=pipe,
                    device=device,
                    loaded_at=self._clock(),
                    load_seconds=elapsed,
                    measured_vram_gb=measured,
                    idle_since=self._clock(),
                )
                del pipe
                self._last_load = {
                    "model": spec.model_id,
                    "seconds": round(elapsed, 2),
                    "quantization": variant.quant,
                    "cpu_offload": variant.offload != "none",
                    "est_vram_gb": variant.est_vram_gb,
                    "measured_vram_gb": round(measured, 2) if measured is not None else None,
                    "preferred_quantization": candidates[0].quant,
                    "preferred_cpu_offload": candidates[0].offload != "none",
                    "attempt_count": attempted,
                    "fallback_attempts": max(attempted - 1, 0),
                    "vram_skipped_candidates": skipped_for_vram,
                    "failed_attempts": failures,
                }
                self._load = None
                self._arm_idle_locked(self._resident)
                load.event.set()
                self._cond.notify_all()
            logger.info(
                "diffusers: loaded %s (%s) in %.1fs, measured VRAM %s GB",
                spec.model_id,
                variant.label(),
                elapsed,
                f"{measured:.1f}" if measured is not None else "unknown",
            )
        except Exception as exc:  # noqa: BLE001 - recorded and surfaced to the next caller
            err: Exception
            if isinstance(exc, InsufficientVram):
                err = exc
            else:
                err = ModelLoadFailed(spec.model_id, classify_failure(exc))
            if isinstance(err, ModelLoadFailed):
                logger.warning("diffusers: load of %s failed [%s]: %s",
                               spec.model_id, err.kind.value, err.message)
            else:
                logger.warning("diffusers: load of %s refused [insufficient_vram]", spec.model_id)
            try:
                # Preparation can fail while the idle timer is tearing down
                # the outgoing model. Do not run overlapping memory cleanup.
                self._enter_replacement_phase(load)
                # A failed from_pretrained can leave partially allocated CUDA
                # blocks cached in this process.
                self._free_memory()
            except Exception:  # noqa: BLE001
                logger.warning("diffusers: memory cleanup after a failed load was not confirmed")
            with self._cond:
                self._failures[spec.model_id] = [err, self._clock() + self._failure_backoff, False]
                load.error = err
                self._load = None
                if self._resident is not None and self._resident.inflight == 0:
                    self._arm_idle_locked(self._resident)
                load.event.set()
                self._cond.notify_all()

    def _attempt_pipeline_load(self, spec: ImageModelSpec, variant: LoadVariant):
        """Return only a safe enum from a failed loader, never its diagnostics.

        Returning outside the except block and this short-lived frame allows
        the caller's GC/cache cleanup to reclaim partial pipelines before a
        fallback. A successful loader returns its normal (pipeline, device).
        """
        try:
            return self._loader(spec, variant), None
        except Exception as exc:
            kind = classify_failure(exc)
        return None, kind

    def _enter_replacement_phase(self, load: _Load) -> None:
        """End preparation without racing an outgoing model's idle teardown.

        Preparation itself does not extend residency. Once it finishes (or
        fails), wait for an already-started teardown, then fence future idle
        callbacks before any memory probe/cleanup or pipeline load. Reentry
        on the failure path is harmless.
        """
        with self._cond:
            self._cond.wait_for(lambda: not self._idle_evicting)
            load.evicting = True
            self._cancel_idle_locked()

    def _refuse_if_eviction_cannot_help(self, spec: ImageModelSpec, need_bytes: float) -> None:
        """Do not evict a working model for a load that cannot fit anyway: if
        free VRAM plus what the resident model holds is still short (other
        services hold the rest), fail before touching it."""
        with self._cond:
            resident = self._resident
        if resident is None:
            return
        # Release this process's own cached-but-unused blocks first so the
        # reading counts them as free (they are reclaimable by definition).
        self._free_memory()
        free_now = self._free_vram_bytes()
        if free_now is None:
            return
        held_gb = resident.measured_vram_gb if resident.measured_vram_gb else resident.variant.est_vram_gb
        reclaimable = free_now + held_gb * GIB
        if reclaimable < need_bytes:
            raise InsufficientVram(spec.model_id, need_bytes / GIB, reclaimable / GIB)

    def _swap_out_previous(self, load: _Load) -> None:
        """Drain in-flight work on the resident model, then unload it. Runs on the load thread."""
        with self._cond:
            previous = self._resident
            if previous is None:
                return
            load.evicting = True  # stop leasing the outgoing model; it would starve the drain
            drained = self._cond.wait_for(lambda: previous.inflight == 0, timeout=self._drain_timeout)
            if not drained:
                raise TimeoutError(
                    f"timed out after {self._drain_timeout:.0f}s waiting for in-flight generations "
                    f"on {previous.spec.model_id} to finish"
                )
            self._resident = None
            pipe = previous.pipe
            previous.pipe = None
        try:
            self._unloader(pipe)
        except Exception:  # noqa: BLE001 - best effort; memory is reclaimed below
            logger.exception("diffusers: unloading %s raised; continuing", previous.spec.model_id)
        finally:
            del pipe
            del previous
