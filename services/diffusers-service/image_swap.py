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

import logging
import threading
import time
from dataclasses import dataclass, field
from typing import Any, Callable

from image_models import GIB, ImageModelSpec, LoadVariant, choose_variant, fitting_variants

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


class ModelLoadFailed(RuntimeError):
    """The load itself failed (bad repo id, auth, disk, library error). HTTP 500."""

    def __init__(self, model_id: str, message: str):
        self.model_id = model_id
        self.message = message
        super().__init__(f"failed to load model {model_id}: {message}")


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
    ):
        self._specs = specs
        self._default_model = default_model
        self._loader = loader
        self._unloader = unloader
        self._free_memory = free_memory
        self._free_vram_bytes = free_vram_bytes
        # Disk-side preparation (free-space preflight + weight prefetch). Runs
        # BEFORE anything is evicted, so a slow or failed download never leaves
        # the node without a model.
        self._prepare = prepare
        self._min_residency = max(min_residency_seconds, 0.0)
        self._failure_backoff = max(failure_backoff_seconds, 0.0)
        self._budget_gb = vram_budget_gb
        self._quant_mode = quant_mode
        self._warm_wait = max(warm_wait_seconds, 0.0)
        self._drain_timeout = drain_timeout_seconds
        self._retry_after = max(int(retry_after_seconds), 1)
        self._clock = clock

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
                    }
                )
            return {
                "allowed_models": models,
                "loaded_model": resident_id,
                "loading_model": loading_id,
                "vram_budget_gb": self._budget_gb,
                "quant_mode": self._quant_mode,
                "last_load": dict(self._last_load) if self._last_load else None,
            }

    # -- internals ----------------------------------------------------------

    def _plan_or_none(self, spec: ImageModelSpec) -> LoadVariant | None:
        try:
            return choose_variant(spec, self._budget_gb, self._quant_mode)
        except Exception:  # VramBudgetError: reported as fits_vram_budget=false
            return None

    def _try_locked(self, spec: ImageModelSpec) -> "Lease | _Load":
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
            self._cond.notify_all()

    def _run_load(self, load: _Load) -> None:
        spec, variant = load.spec, load.variant
        try:
            candidates = load.candidates or (variant,)
            smallest = min(candidates, key=lambda v: v.est_vram_gb)
            # Disk first: free-space preflight and weight download happen while
            # the resident model keeps serving, so a slow or failed download
            # never leaves the node with no model.
            self._prepare(spec, variant)
            self._refuse_if_eviction_cannot_help(spec, smallest.est_vram_gb * GIB)
            self._swap_out_previous(load)
            # Unconditional, so cached blocks left by a previous failed load or
            # an earlier unload never make free VRAM look smaller than it is.
            self._free_memory()
            free_before = self._free_vram_bytes()
            if free_before is not None:
                # The budget picked the preferred variant; what the card has free
                # right now decides which one actually loads. Fall back to the
                # next smaller variant that fits before refusing.
                fitting = [v for v in candidates if v.est_vram_gb * GIB <= free_before]
                if not fitting:
                    raise InsufficientVram(spec.model_id, smallest.est_vram_gb, free_before / GIB)
                variant = fitting[0]
                with self._cond:
                    load.variant = variant

            started = self._clock()
            pipe, device = self._loader(spec, variant)
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
                )
                self._last_load = {
                    "model": spec.model_id,
                    "seconds": round(elapsed, 2),
                    "quantization": variant.quant,
                    "cpu_offload": variant.offload != "none",
                    "est_vram_gb": variant.est_vram_gb,
                    "measured_vram_gb": round(measured, 2) if measured is not None else None,
                }
                self._load = None
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
                err = ModelLoadFailed(spec.model_id, str(exc))
            logger.warning("diffusers: load of %s failed: %s", spec.model_id, exc)
            try:
                # A failed from_pretrained can leave partially allocated CUDA
                # blocks cached in this process.
                self._free_memory()
            except Exception:  # noqa: BLE001
                logger.exception("diffusers: freeing memory after a failed load raised")
            with self._cond:
                self._failures[spec.model_id] = [err, self._clock() + self._failure_backoff, False]
                load.error = err
                self._load = None
                load.event.set()
                self._cond.notify_all()

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
