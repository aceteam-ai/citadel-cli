"""Swap-logic tests for image_swap.ImageModelManager (citadel-cli#1335).

The pipeline loader, unloader, memory freer and free-VRAM probe are all fakes:
no torch, diffusers, GPU or network. Loads block on threading.Events so the
tests control exactly when a "cold load" finishes."""

from __future__ import annotations

import gc
import threading
import time
import weakref

import pytest

import image_models as im
from image_swap import ImageModelManager, InsufficientVram, ModelLoadFailed, ModelWarming

GIB = 1024**3
SDXL = "stabilityai/sdxl-turbo"
FLUX = "black-forest-labs/FLUX.2-klein-4B"
ZIMG = "Tongyi-MAI/Z-Image-Turbo"
QWEN = "Qwen/Qwen-Image"


class FakeLoader:
    """Records loads; a model id in ``gates`` blocks until its Event is set."""

    def __init__(self):
        self.loads = []
        self.gates = {}
        self.fail = {}
        self.unloaded = []
        self.freed = 0
        self.events = []
        self.prepare_fail = {}
        self.prepare_gates = {}
        self.prepare_entered = {}
        self.lock = threading.Lock()

    def prepare(self, spec, variant):
        self.events.append(("prepare", spec.model_id))
        entered = self.prepare_entered.get(spec.model_id)
        if entered is not None:
            entered.set()
        gate = self.prepare_gates.get(spec.model_id)
        if gate is not None:
            assert gate.wait(5), "test never released the prepare gate"
        if spec.model_id in self.prepare_fail:
            raise RuntimeError(self.prepare_fail.pop(spec.model_id))

    def load(self, spec, variant):
        self.events.append(("load", spec.model_id))
        gate = self.gates.get(spec.model_id)
        if gate is not None:
            assert gate.wait(5), "test never released the load gate"
        if spec.model_id in self.fail:
            raise RuntimeError(self.fail.pop(spec.model_id))
        with self.lock:
            self.loads.append((spec.model_id, variant.quant, variant.offload))
        return f"pipe:{spec.model_id}", "cuda"

    def unload(self, pipe):
        self.events.append(("unload", pipe))
        self.unloaded.append(pipe)

    def free(self):
        self.events.append(("free", None))
        self.freed += 1


class ManualTimers:
    """No sleeps or real timer threads; fire even canceled callbacks to race-test fencing."""

    def __init__(self):
        self.now = 100.0
        self.timers = []

    def __call__(self, delay, callback):
        owner = self

        class Timer:
            daemon = False
            canceled = False

            def start(self):
                owner.timers.append(self)

            def cancel(self):
                self.canceled = True

            def fire(self):
                callback()

        timer = Timer()
        timer.delay = delay
        return timer


@pytest.mark.parametrize("model_id", [SDXL, "owned/video"])
@pytest.mark.parametrize("idle", [0, 300])
def test_idle_eviction_clears_image_and_video_leases_and_cold_loads_again(model_id, idle):
    timers = ManualTimers()
    loader = FakeLoader()
    spec = im.CATALOG[SDXL] if model_id == SDXL else im.ImageModelSpec(
        model_id=model_id, license="test", permissive=False, pipeline_class=None,
        torch_dtype=None, variants=(im.LoadVariant("none", "none", 0),),
    )
    mgr = ImageModelManager(
        {model_id: spec}, model_id, loader=loader.load, unloader=loader.unload,
        free_memory=loader.free, warm_wait_seconds=5, idle_unload_seconds=idle,
        timer_factory=timers, clock=lambda: timers.now,
    )
    lease = mgr.lease(model_id)
    lease.release()
    assert lease.pipe is None  # keeping the Lease cannot keep weights alive
    timer = timers.timers[-1]
    assert timer.delay == idle
    timers.now += idle
    timer.fire()
    assert mgr.loaded_model() is None
    assert loader.unloaded == [f"pipe:{model_id}"]
    assert loader.events[-2:] == [("unload", f"pipe:{model_id}"), ("free", None)]
    # A later cold request uses the same retryable warming contract.
    mgr._warm_wait = 0
    gate = loader.gates[model_id] = threading.Event()
    try:
        with pytest.raises(ModelWarming):
            mgr.lease(model_id)
    finally:
        gate.set()
        assert mgr.wait_for_load(5)


def test_new_lease_cancels_idle_timer_and_inflight_work_cannot_be_evicted():
    timers = ManualTimers()
    mgr, loader = make(warm_wait=5, idle_unload_seconds=10,
                       timer_factory=timers, clock=lambda: timers.now)
    first = mgr.lease(SDXL)
    first.release()
    stale = timers.timers[-1]
    second = mgr.lease(SDXL)
    third = mgr.lease(SDXL)
    assert stale.canceled
    timers.now += 20
    stale.fire()
    second.release()
    stale.fire()
    assert not loader.unloaded  # third still holds the resident model
    third.release()
    current = timers.timers[-1]
    timers.now += 10
    current.fire()
    assert loader.unloaded == [f"pipe:{SDXL}"]


def test_pinned_image_and_video_models_have_no_idle_eviction_timer():
    for model_id in (SDXL, "owned/video"):
        timers = ManualTimers()
        mgr, loader = make(warm_wait=5, idle_unload_seconds=0,
                           pinned_models=frozenset({model_id}),
                           timer_factory=timers, clock=lambda: timers.now)
        if model_id not in mgr._specs:
            mgr._specs[model_id] = im.ImageModelSpec(
                model_id=model_id, license="test", permissive=False, pipeline_class=None,
                torch_dtype=None, variants=(im.LoadVariant("none", "none", 0),),
            )
        mgr.lease(model_id).release()
        assert not timers.timers
        entry = next(m for m in mgr.status()["allowed_models"] if m["model"] == model_id)
        assert entry["resident"] and entry["pinned"] and entry["evicts_at"] is None
        assert not loader.unloaded


def test_idle_cleanup_drops_pipeline_references_during_replacement_preparation():
    timers = ManualTimers()
    references = []
    observed = []
    preparing, gate = threading.Event(), threading.Event()

    class Pipeline:
        pass

    def load(spec, variant):
        pipe = Pipeline()
        references.append(weakref.ref(pipe))
        return pipe, "cuda"

    def prepare(spec, variant):
        if spec.model_id == FLUX:
            preparing.set()
            assert gate.wait(5), "test never released the prepare gate"

    mgr = ImageModelManager(
        {SDXL: im.CATALOG[SDXL], FLUX: im.CATALOG[FLUX]}, SDXL,
        loader=load, prepare=prepare,
        free_memory=lambda: observed.append([ref() is None for ref in references]),
        warm_wait_seconds=5, idle_unload_seconds=0,
        timer_factory=timers, clock=lambda: timers.now,
    )
    lease = mgr.lease(SDXL)
    lease.release()
    mgr._warm_wait = 0
    try:
        with pytest.raises(ModelWarming):
            mgr.lease(FLUX)
        assert preparing.wait(5)
        timers.timers[-1].fire()
        assert observed[-1] == [True]
        assert lease.pipe is None
    finally:
        gate.set()
        assert mgr.wait_for_load(5)


def test_failed_swap_resumes_the_outgoing_models_original_idle_deadline():
    timers = ManualTimers()
    mgr, loader = make(warm_wait=5, idle_unload_seconds=10,
                       timer_factory=timers, clock=lambda: timers.now)
    mgr.lease(SDXL).release()
    timers.now += 3
    loader.prepare_fail[FLUX] = "owned prepare failure"
    with pytest.raises(ModelLoadFailed):
        mgr.lease(FLUX)
    timer = timers.timers[-1]
    assert timer.delay == 7
    timers.now += 7
    timer.fire()
    assert mgr.loaded_model() is None
    assert loader.unloaded == [f"pipe:{SDXL}"]


@pytest.mark.parametrize("prepare_failure", [False, True])
def test_idle_deadline_expires_while_replacement_preparation_is_blocked(prepare_failure):
    timers = ManualTimers()
    mgr, loader = make(warm_wait=5, idle_unload_seconds=10,
                       timer_factory=timers, clock=lambda: timers.now)
    released = mgr.lease(SDXL)
    released.release()
    deadline = timers.timers[-1]
    entered, gate = threading.Event(), threading.Event()
    loader.prepare_entered[FLUX] = entered
    loader.prepare_gates[FLUX] = gate
    if prepare_failure:
        loader.prepare_fail[FLUX] = "owned blocked prepare failure"
    mgr._warm_wait = 0
    try:
        with pytest.raises(ModelWarming):
            mgr.lease(FLUX)
        assert entered.wait(5)
        assert not deadline.canceled
        entry = next(m for m in mgr.status()["allowed_models"] if m["model"] == SDXL)
        assert entry["evicts_at"] is not None
        timers.now += 10
        deadline.fire()
        assert mgr.loaded_model() is None
        assert loader.unloaded == [f"pipe:{SDXL}"]
        assert released.pipe is None and released._resident.pipe is None
        assert mgr.loading_model() == FLUX
        assert len(loader.loads) == 1
    finally:
        gate.set()
        assert mgr.wait_for_load(5)
    if prepare_failure:
        assert mgr.loaded_model() is None
        with pytest.raises(ModelLoadFailed):
            mgr.lease(FLUX)
    else:
        assert mgr.loaded_model() == FLUX
    # Finishing/failing preparation must not unload the expired model twice.
    assert loader.unloaded == [f"pipe:{SDXL}"]


def test_new_outgoing_lease_fences_idle_timer_during_blocked_preparation():
    timers = ManualTimers()
    mgr, loader = make(warm_wait=5, idle_unload_seconds=10,
                       timer_factory=timers, clock=lambda: timers.now)
    mgr.lease(SDXL).release()
    stale = timers.timers[-1]
    entered, gate = threading.Event(), threading.Event()
    loader.prepare_entered[FLUX] = entered
    loader.prepare_gates[FLUX] = gate
    mgr._warm_wait = 0
    renewed = None
    try:
        with pytest.raises(ModelWarming):
            mgr.lease(FLUX)
        assert entered.wait(5)
        renewed = mgr.lease(SDXL)
        assert stale.canceled
        timers.now += 11
        stale.fire()
        assert not loader.unloaded
        renewed.release()
        current = timers.timers[-1]
        assert current.delay == 10
        stale.fire()
        assert not loader.unloaded
        timers.now += 10
        current.fire()
        assert loader.unloaded == [f"pipe:{SDXL}"]
    finally:
        if renewed is not None:
            renewed.release()
        gate.set()
        assert mgr.wait_for_load(5)


def test_pinned_outgoing_model_stays_resident_during_blocked_preparation():
    timers = ManualTimers()
    mgr, loader = make(warm_wait=5, idle_unload_seconds=10,
                       pinned_models=frozenset({SDXL}), timer_factory=timers,
                       clock=lambda: timers.now)
    mgr.lease(SDXL).release()
    entered, gate = threading.Event(), threading.Event()
    loader.prepare_entered[FLUX] = entered
    loader.prepare_gates[FLUX] = gate
    mgr._warm_wait = 0
    try:
        with pytest.raises(ModelWarming):
            mgr.lease(FLUX)
        assert entered.wait(5)
        timers.now += 1000
        assert not timers.timers
        assert mgr.loaded_model() == SDXL
        assert not loader.unloaded
    finally:
        gate.set()
        assert mgr.wait_for_load(5)
    assert mgr.loaded_model() == FLUX  # pins still allow explicit swaps
    assert loader.unloaded == [f"pipe:{SDXL}"]


@pytest.mark.parametrize("prepare_failure", [False, True])
def test_finished_preparation_waits_for_idle_teardown_before_memory_work(prepare_failure):
    timers = ManualTimers()
    mgr, loader = make(warm_wait=5, idle_unload_seconds=10,
                       timer_factory=timers, clock=lambda: timers.now)
    mgr.lease(SDXL).release()
    entered, prepare_gate = threading.Event(), threading.Event()
    unloading, unload_gate = threading.Event(), threading.Event()
    phase_entered = threading.Event()
    loader.prepare_entered[FLUX] = entered
    loader.prepare_gates[FLUX] = prepare_gate
    if prepare_failure:
        loader.prepare_fail[FLUX] = "owned blocked prepare failure"

    def blocked_unload(pipe):
        unloading.set()
        assert unload_gate.wait(5), "test never released the unload gate"
        loader.unload(pipe)

    enter_phase = mgr._enter_replacement_phase

    def observed_phase(load):
        phase_entered.set()
        enter_phase(load)

    mgr._unloader = blocked_unload
    mgr._enter_replacement_phase = observed_phase
    mgr._warm_wait = 0
    worker = None
    try:
        with pytest.raises(ModelWarming):
            mgr.lease(FLUX)
        assert entered.wait(5)
        before = loader.freed
        timers.now += 10
        worker = threading.Thread(target=timers.timers[-1].fire)
        worker.start()
        assert unloading.wait(5)
        prepare_gate.set()
        assert phase_entered.wait(5)
        assert mgr._idle_evicting
        assert not mgr._load.event.is_set()
        assert len(loader.loads) == 1
        assert loader.freed == before  # neither load nor failure cleanup may overlap
    finally:
        prepare_gate.set()
        unload_gate.set()
        if worker is not None:
            worker.join(5)
            assert not worker.is_alive()
        assert mgr.wait_for_load(5)
    assert loader.unloaded == [f"pipe:{SDXL}"]
    assert mgr.loaded_model() == (None if prepare_failure else FLUX)


def test_idle_cleanup_blocks_new_load_until_memory_release_finishes():
    timers = ManualTimers()
    entered, release = threading.Event(), threading.Event()
    mgr, loader = make(warm_wait=5, idle_unload_seconds=0,
                       timer_factory=timers, clock=lambda: timers.now)
    mgr.lease(SDXL).release()

    def blocked_unload(pipe):
        entered.set()
        assert release.wait(5)
        loader.unload(pipe)

    mgr._unloader = blocked_unload
    worker = threading.Thread(target=timers.timers[-1].fire)
    worker.start()
    try:
        assert entered.wait(5)
        with pytest.raises(ModelWarming) as err:
            mgr.lease(FLUX)
        assert err.value.reason == "busy"
        assert len(loader.loads) == 1
    finally:
        release.set()
        worker.join(5)
        assert not worker.is_alive()


def test_pinning_does_not_prevent_an_explicit_image_model_swap():
    timers = ManualTimers()
    mgr, loader = make(warm_wait=5, idle_unload_seconds=0,
                       pinned_models=frozenset({SDXL}), timer_factory=timers,
                       clock=lambda: timers.now)
    mgr.lease(SDXL).release()
    assert not timers.timers
    mgr.lease(FLUX).release()
    assert loader.unloaded == [f"pipe:{SDXL}"]
    assert mgr.loaded_model() == FLUX


def test_unclaimed_background_load_has_only_bounded_retry_grace_at_idle_zero():
    timers = ManualTimers()
    loader = FakeLoader()
    gate = loader.gates[SDXL] = threading.Event()
    mgr, loader = make(loader, idle_unload_seconds=0, timer_factory=timers,
                       clock=lambda: timers.now)
    with pytest.raises(ModelWarming):
        mgr.lease(SDXL)
    mgr._warm_wait = 3
    gate.set()
    assert mgr.wait_for_load(5)
    timer = timers.timers[-1]
    assert timer.delay == 10  # Retry-After: 7 + warm-wait: 3; still bounded
    timers.now += timer.delay
    timer.fire()
    assert mgr.loaded_model() is None
    assert loader.unloaded == [f"pipe:{SDXL}"]


def make(loader=None, *, budget=24.0, quant="auto", free_gb=None, warm_wait=0.0, drain=5.0,
         min_residency=0.0, backoff=0.0, **kw):
    loader = loader or FakeLoader()
    specs = im.parse_allowlist(None, SDXL)
    free_fn = (lambda: None) if free_gb is None else (lambda: int(free_gb * GIB))
    mgr = ImageModelManager(
        specs,
        SDXL,
        loader=loader.load,
        unloader=loader.unload,
        free_memory=loader.free,
        free_vram_bytes=free_fn,
        vram_budget_gb=budget,
        quant_mode=quant,
        warm_wait_seconds=warm_wait,
        drain_timeout_seconds=drain,
        retry_after_seconds=7,
        min_residency_seconds=min_residency,
        failure_backoff_seconds=backoff,
        prepare=loader.prepare,
        **kw,
    )
    return mgr, loader


def test_cold_request_returns_warming_then_serves_after_load():
    loader = FakeLoader()
    gate = loader.gates[SDXL] = threading.Event()
    mgr, _ = make(loader)

    with pytest.raises(ModelWarming) as err:
        mgr.lease(SDXL)
    assert err.value.model_id == SDXL
    assert err.value.reason == "loading"
    assert err.value.retry_after == 7
    assert err.value.eta_seconds >= 0
    assert mgr.loading_model() == SDXL
    assert mgr.loaded_model() is None

    gate.set()
    assert mgr.wait_for_load(5)
    with mgr.lease(SDXL) as lease:
        assert lease.pipe == f"pipe:{SDXL}"
        assert lease.device == "cuda"
    assert mgr.loaded_model() == SDXL


def test_request_waits_for_a_quick_load_within_warm_wait():
    mgr, loader = make(warm_wait=5.0)
    with mgr.lease(SDXL) as lease:  # loader returns immediately
        assert lease.model_id == SDXL
    assert loader.loads == [(SDXL, "none", "none")]


def test_concurrent_requests_for_one_model_load_it_once():
    loader = FakeLoader()
    gate = loader.gates[SDXL] = threading.Event()
    mgr, _ = make(loader)
    for _ in range(4):
        with pytest.raises(ModelWarming):
            mgr.lease(SDXL)
    gate.set()
    mgr.wait_for_load(5)
    assert len(loader.loads) == 1


def test_swap_unloads_previous_and_frees_cuda_memory():
    mgr, loader = make()
    with pytest.raises(ModelWarming):
        mgr.lease(SDXL)
    mgr.wait_for_load(5)
    mgr.lease(SDXL).release()

    with pytest.raises(ModelWarming):
        mgr.lease(FLUX)
    assert mgr.wait_for_load(5)

    assert loader.unloaded == [f"pipe:{SDXL}"]
    assert loader.freed >= 1
    assert mgr.loaded_model() == FLUX
    assert [m for m, *_ in loader.loads] == [SDXL, FLUX]


def test_swap_waits_for_inflight_generation_to_finish():
    mgr, loader = make()
    with pytest.raises(ModelWarming):
        mgr.lease(SDXL)
    mgr.wait_for_load(5)
    busy = mgr.lease(SDXL)  # an in-flight generation holds the pipeline

    with pytest.raises(ModelWarming):
        mgr.lease(FLUX)  # starts the swap, which must wait for `busy`
    assert not mgr.wait_for_load(0.2)  # still draining
    assert loader.unloaded == []
    assert mgr.loaded_model() == SDXL

    busy.release()
    assert mgr.wait_for_load(5)
    assert loader.unloaded == [f"pipe:{SDXL}"]
    assert mgr.loaded_model() == FLUX


def test_outgoing_model_accepts_no_new_leases_while_swap_is_pending():
    mgr, loader = make()
    with pytest.raises(ModelWarming):
        mgr.lease(SDXL)
    mgr.wait_for_load(5)
    busy = mgr.lease(SDXL)
    with pytest.raises(ModelWarming):
        mgr.lease(FLUX)
    deadline = time.time() + 5
    while not (mgr._load and mgr._load.evicting) and time.time() < deadline:
        time.sleep(0.01)  # the load thread has reached the drain step
    with pytest.raises(ModelWarming) as err:
        mgr.lease(SDXL)  # would starve the swap
    assert err.value.reason == "swapping_out"
    assert err.value.loading == FLUX
    busy.release()
    mgr.wait_for_load(5)


def test_third_model_while_a_load_runs_is_busy_not_queued():
    loader = FakeLoader()
    gate = loader.gates[FLUX] = threading.Event()
    mgr, _ = make(loader)
    with pytest.raises(ModelWarming):
        mgr.lease(FLUX)
    with pytest.raises(ModelWarming) as err:
        mgr.lease(ZIMG)
    assert err.value.reason == "busy"
    assert err.value.loading == FLUX
    gate.set()
    mgr.wait_for_load(5)
    assert [m for m, *_ in loader.loads] == [FLUX]  # ZIMG was not started


def test_load_failure_is_reported_once_then_a_retry_loads_fresh():
    loader = FakeLoader()
    loader.fail[SDXL] = "bad repo id"
    mgr, _ = make(loader)
    with pytest.raises(ModelWarming):
        mgr.lease(SDXL)
    mgr.wait_for_load(5)

    with pytest.raises(ModelLoadFailed) as err:
        mgr.lease(SDXL)
    assert err.value.message == "all eligible pipeline loads failed"
    assert "bad repo id" not in str(err.value)
    assert mgr.loaded_model() is None

    with pytest.raises(ModelWarming):  # failure consumed; this starts a new load
        mgr.lease(SDXL)
    mgr.wait_for_load(5)
    mgr.lease(SDXL).release()
    assert mgr.loaded_model() == SDXL


def test_warm_wait_surfaces_a_failure_in_the_same_request():
    loader = FakeLoader()
    loader.fail[SDXL] = "disk full"
    mgr, _ = make(loader, warm_wait=5.0)
    with pytest.raises(ModelLoadFailed):
        mgr.lease(SDXL)


def test_insufficient_free_vram_after_unload_is_not_a_warming_state():
    mgr, loader = make(free_gb=3.0)
    with pytest.raises(ModelWarming):
        mgr.lease(FLUX)  # needs ~16.5 GB with a 24 GB budget
    mgr.wait_for_load(5)
    with pytest.raises(InsufficientVram) as err:
        mgr.lease(FLUX)
    assert err.value.free_gb == pytest.approx(3.0)
    assert err.value.need_gb == 7.0  # the smallest variant that fits the budget
    assert loader.loads == []  # never attempted a load that would OOM other services


def test_budget_selects_quantized_variant_for_big_model():
    mgr, loader = make(budget=12.0, warm_wait=5.0)
    with mgr.lease(QWEN) as lease:
        assert (lease.variant.quant, lease.variant.offload) == ("nf4", "model")
    assert loader.loads == [(QWEN, "nf4", "model")]


def test_model_over_budget_raises_budget_error_without_loading():
    mgr, loader = make(budget=8.0)
    with pytest.raises(im.VramBudgetError):
        mgr.lease(QWEN)
    assert loader.loads == []
    assert mgr.loading_model() is None


def test_forced_quant_mode_is_honoured():
    mgr, loader = make(budget=24.0, quant="nf4", warm_wait=5.0)
    with mgr.lease(ZIMG) as lease:
        assert lease.variant.quant == "nf4"


def test_status_reports_warm_loading_cold_and_budget_fit():
    loader = FakeLoader()
    gate = loader.gates[FLUX] = threading.Event()
    mgr, _ = make(loader, budget=12.0)
    with pytest.raises(ModelWarming):
        mgr.lease(SDXL)
    mgr.wait_for_load(5)
    with pytest.raises(ModelWarming):
        mgr.lease(FLUX)

    status = mgr.status()
    by_id = {m["model"]: m for m in status["allowed_models"]}
    assert by_id[FLUX]["state"] == "loading"
    assert by_id[ZIMG]["state"] == "cold"
    assert by_id[SDXL]["default"] is True
    assert status["loading_model"] == FLUX
    assert status["vram_budget_gb"] == 12.0
    assert by_id[QWEN]["fits_vram_budget"] is True  # nf4+offload ~12 GB
    gate.set()
    mgr.wait_for_load(5)
    status = mgr.status()
    assert status["loaded_model"] == FLUX
    assert {m["model"]: m["state"] for m in status["allowed_models"]}[FLUX] == "warm"
    assert status["last_load"]["model"] == FLUX


def test_status_marks_models_that_cannot_fit_the_budget():
    mgr, _ = make(budget=8.0)
    by_id = {m["model"]: m for m in mgr.status()["allowed_models"]}
    assert by_id[QWEN]["fits_vram_budget"] is False
    assert by_id[SDXL]["fits_vram_budget"] is True


def test_measured_vram_is_recorded_from_free_memory_delta():
    free = iter([20 * GIB, 13 * GIB])  # before load, after load
    loader = FakeLoader()
    specs = im.parse_allowlist(None, SDXL)
    mgr = ImageModelManager(
        specs, SDXL, loader=loader.load, free_vram_bytes=lambda: next(free), vram_budget_gb=24, warm_wait_seconds=5
    )
    mgr.lease(SDXL).release()
    assert mgr.status()["last_load"]["measured_vram_gb"] == 7.0


def test_lease_release_is_idempotent():
    mgr, _ = make(warm_wait=5.0)
    lease = mgr.lease(SDXL)
    lease.release()
    lease.release()
    mgr.lease(FLUX)  # swap must not be blocked by a negative count


def test_does_not_evict_a_working_model_for_a_load_that_cannot_fit():
    # quant=none: FLUX's smallest variant is bf16 + offload (9 GB). Resident sdxl
    # holds ~7 GB and 1 GB is free: evicting leaves 8 GB, still short.
    # The warm model must survive.
    mgr, loader = make(budget=24.0, quant="none", free_gb=1.0, warm_wait=5.0)
    # sdxl needs 7 GB but only 4 are free, so seed the resident directly.
    mgr._free_vram_bytes = lambda: 40 * GIB
    mgr.lease(SDXL).release()
    mgr._free_vram_bytes = lambda: 1 * GIB

    with pytest.raises(InsufficientVram):
        mgr.lease(FLUX)
    assert mgr.loaded_model() == SDXL
    assert loader.unloaded == []


def test_failed_load_frees_cuda_memory():
    loader = FakeLoader()
    loader.fail[SDXL] = "partial allocation then error"
    mgr, _ = make(loader)
    with pytest.raises(ModelWarming):
        mgr.lease(SDXL)
    mgr.wait_for_load(5)
    assert loader.freed >= 1  # also on the failure path, not only after an unload


def test_free_memory_runs_even_when_nothing_was_unloaded():
    mgr, loader = make(warm_wait=5.0)
    mgr.lease(SDXL).release()  # cold start: no previous model to unload
    assert ("free", None) in loader.events
    assert loader.events.index(("free", None)) < loader.events.index(("load", SDXL))


def test_download_happens_before_the_resident_model_is_evicted():
    loader = FakeLoader()
    gate = loader.prepare_gates[FLUX] = threading.Event()
    mgr, _ = make(loader)
    with pytest.raises(ModelWarming):
        mgr.lease(SDXL)
    mgr.wait_for_load(5)
    mgr.lease(SDXL).release()
    loader.events.clear()

    with pytest.raises(ModelWarming):
        mgr.lease(FLUX)  # blocks in prepare (the download), SDXL still resident
    # The old model keeps serving while the new one downloads.
    with mgr.lease(SDXL) as still_there:
        assert still_there.model_id == SDXL
    assert loader.unloaded == []
    gate.set()
    assert mgr.wait_for_load(5)
    order = [e for e in loader.events if e[0] in ("prepare", "unload", "load")]
    assert order == [("prepare", FLUX), ("unload", f"pipe:{SDXL}"), ("load", FLUX)]


def test_failed_download_leaves_the_resident_model_loaded():
    loader = FakeLoader()
    loader.prepare_fail[FLUX] = "insufficient disk space"
    mgr, _ = make(loader)
    with pytest.raises(ModelWarming):
        mgr.lease(SDXL)
    mgr.wait_for_load(5)
    mgr.lease(SDXL).release()

    with pytest.raises(ModelWarming):
        mgr.lease(FLUX)
    mgr.wait_for_load(5)
    with pytest.raises(ModelLoadFailed) as err:
        mgr.lease(FLUX)
    assert "insufficient disk space" in str(err.value)
    assert mgr.loaded_model() == SDXL
    assert loader.unloaded == []


class Clock:
    def __init__(self):
        self.now = 1000.0

    def __call__(self):
        return self.now


def test_fresh_model_is_not_evicted_before_its_requester_uses_it():
    clock = Clock()
    mgr, loader = make(min_residency=120.0, clock=clock)
    with pytest.raises(ModelWarming):
        mgr.lease(FLUX)  # client A
    mgr.wait_for_load(5)

    with pytest.raises(ModelWarming) as err:  # client B wants another model
        mgr.lease(ZIMG)
    assert err.value.reason == "resident_protected"
    assert mgr.loaded_model() == FLUX
    assert [m for m, *_ in loader.loads] == [FLUX]
    assert loader.unloaded == []

    mgr.lease(FLUX).release()  # A finally uses it; protection ends
    with pytest.raises(ModelWarming):
        mgr.lease(ZIMG)
    mgr.wait_for_load(5)
    assert mgr.loaded_model() == ZIMG


def test_unused_model_becomes_evictable_after_min_residency():
    clock = Clock()
    mgr, loader = make(min_residency=120.0, clock=clock)
    with pytest.raises(ModelWarming):
        mgr.lease(FLUX)
    mgr.wait_for_load(5)
    clock.now += 119
    with pytest.raises(ModelWarming) as err:
        mgr.lease(ZIMG)
    assert err.value.reason == "resident_protected"
    clock.now += 2
    with pytest.raises(ModelWarming) as err2:
        mgr.lease(ZIMG)
    assert err2.value.reason == "loading"
    mgr.wait_for_load(5)
    assert mgr.loaded_model() == ZIMG


def test_failed_load_is_not_retried_inside_the_backoff_window():
    clock = Clock()
    loader = FakeLoader()
    loader.fail[ZIMG] = "boom"
    mgr, _ = make(loader, quant="none", backoff=30.0, clock=clock)
    with pytest.raises(ModelWarming):
        mgr.lease(ZIMG)
    mgr.wait_for_load(5)
    for _ in range(3):  # reported, then repeated without starting new loads
        with pytest.raises(ModelLoadFailed):
            mgr.lease(ZIMG)
    assert loader.loads == []
    assert mgr.loading_model() is None
    clock.now += 31
    with pytest.raises(ModelWarming):
        mgr.lease(ZIMG)  # window over: a fresh load starts
    mgr.wait_for_load(5)
    assert mgr.loaded_model() == ZIMG


def test_falls_back_to_a_smaller_variant_that_fits_free_vram():
    # Budget 12 prefers FLUX int8 on GPU (9.5 GB); only 8 GB is actually free,
    # so nf4 (7 GB) loads instead of refusing.
    mgr, loader = make(budget=12.0, free_gb=8.0, warm_wait=5.0)
    with mgr.lease(FLUX) as lease:
        assert lease.variant.quant == "nf4"
    assert loader.loads == [(FLUX, "nf4", "none")]


def test_prefers_the_budget_choice_when_free_vram_allows():
    mgr, loader = make(budget=12.0, free_gb=30.0, warm_wait=5.0)
    with mgr.lease(FLUX) as lease:
        assert (lease.variant.quant, lease.variant.offload) == ("int8", "none")


@pytest.mark.parametrize("failures", [0, 1, 2])
def test_auto_attempts_gpu_first_then_offload_and_clears_partial_references(failures):
    timers = ManualTimers()
    attempts, references, cleanup = [], [], []

    class Pipeline:
        pass

    def load(spec, variant):
        assert all(ref() is None for ref in references)
        attempts.append((variant.quant, variant.offload))
        pipe = Pipeline()
        references.append(weakref.ref(pipe))
        if len(attempts) <= failures:
            raise RuntimeError("owned-private-loader-detail")
        return pipe, "cuda"

    def free():
        gc.collect()
        cleanup.append([ref() is None for ref in references])

    mgr, _ = make(budget=12, warm_wait=5, idle_unload_seconds=0,
                  timer_factory=timers, clock=lambda: timers.now)
    mgr._loader = load
    mgr._unloader = lambda pipe: None  # recording a pipe would itself retain it
    mgr._free_memory = free
    order = [("int8", "none"), ("nf4", "none"), ("none", "model")]
    lease = mgr.lease(FLUX)
    assert attempts == order[:failures + 1]
    assert (lease.variant.quant, lease.variant.offload) == order[failures]
    assert all(all(snapshot) for snapshot in cleanup)
    # Even fallback CPU-offloaded weights obey immediate idle cleanup.
    lease.release()
    timers.timers[-1].fire()
    assert all(ref() is None for ref in references)
    assert all(cleanup[-1])


def test_auto_all_candidate_failures_are_bounded_static_and_backed_off(caplog):
    attempts = []

    def load(spec, variant):
        attempts.append((variant.quant, variant.offload))
        raise RuntimeError("owned-private-loader-detail")

    mgr, _ = make(budget=12, warm_wait=5, backoff=30)
    mgr._loader = load
    for _ in range(3):
        with pytest.raises(ModelLoadFailed) as err:
            mgr.lease(FLUX)
        assert err.value.message == "all eligible pipeline loads failed"
        assert "owned-private-loader-detail" not in str(err.value)
    assert attempts == [("int8", "none"), ("nf4", "none"), ("none", "model")]
    assert mgr.loaded_model() is None
    assert "owned-private-loader-detail" not in caplog.text


@pytest.mark.parametrize("quant", ["int8", "none"])
def test_explicit_quantization_never_retries_after_a_load_exception(quant):
    attempts = []

    def load(spec, variant):
        attempts.append((variant.quant, variant.offload))
        raise RuntimeError("owned-private-loader-detail")

    mgr, _ = make(budget=24, quant=quant, warm_wait=5)
    mgr._loader = load
    with pytest.raises(ModelLoadFailed) as err:
        mgr.lease(FLUX)
    assert err.value.message == "pipeline load failed"
    assert attempts == [(quant, "none")]


def test_auto_fallback_rechecks_vram_after_failed_candidate_cleanup():
    attempts = []
    free = [12 * GIB]

    def load(spec, variant):
        attempts.append((variant.quant, variant.offload))
        if variant.quant == "int8":
            free[0] = 6 * GIB  # another service has claimed the remaining room
            raise RuntimeError("owned candidate refusal")
        raise AssertionError("a candidate exceeding current free VRAM must not load")

    mgr, _ = make(budget=12, warm_wait=5)
    mgr._loader = load
    mgr._free_vram_bytes = lambda: free[0]
    with pytest.raises(ModelLoadFailed):
        mgr.lease(FLUX)
    assert attempts == [("int8", "none")]


def test_concurrent_requests_share_one_serial_fallback_load():
    first, second = threading.Event(), threading.Event()
    release_first, release_second = threading.Event(), threading.Event()
    attempts = []
    active = [0]

    def load(spec, variant):
        active[0] += 1
        try:
            assert active[0] == 1
            attempts.append((variant.quant, variant.offload))
            if variant.quant == "int8":
                first.set()
                assert release_first.wait(5)
                raise RuntimeError("owned int8 refusal")
            second.set()
            assert release_second.wait(5)
            return object(), "cuda"
        finally:
            active[0] -= 1

    mgr, _ = make(budget=12)
    mgr._loader = load
    try:
        with pytest.raises(ModelWarming):
            mgr.lease(FLUX)
        assert first.wait(5)
        for _ in range(4):
            with pytest.raises(ModelWarming):
                mgr.lease(FLUX)
        release_first.set()
        assert second.wait(5)
        for _ in range(4):
            with pytest.raises(ModelWarming):
                mgr.lease(FLUX)
        assert attempts == [("int8", "none"), ("nf4", "none")]
    finally:
        release_first.set()
        release_second.set()
        assert mgr.wait_for_load(5)
    assert active[0] == 0
    assert mgr.loaded_model() == FLUX


def test_refuses_only_when_even_the_smallest_variant_does_not_fit():
    mgr, loader = make(budget=12.0, free_gb=5.0)
    with pytest.raises(ModelWarming):
        mgr.lease(FLUX)
    mgr.wait_for_load(5)
    with pytest.raises(InsufficientVram) as err:
        mgr.lease(FLUX)
    assert err.value.need_gb == 7.0
    assert loader.loads == []


def test_old_failure_is_not_delivered_to_a_fresh_request_later():
    clock = Clock()
    loader = FakeLoader()
    loader.fail[ZIMG] = "boom"
    mgr, _ = make(loader, quant="none", backoff=30.0, clock=clock)
    with pytest.raises(ModelWarming):
        mgr.lease(ZIMG)
    mgr.wait_for_load(5)
    # Nobody retried; a day later a new request must start a fresh load.
    clock.now += 86400
    with pytest.raises(ModelWarming) as err:
        mgr.lease(ZIMG)
    assert err.value.reason == "loading"
    mgr.wait_for_load(5)
    assert mgr.loaded_model() == ZIMG


def test_resident_protected_uses_resident_field_not_loading():
    clock = Clock()
    mgr, _ = make(min_residency=120.0, clock=clock)
    with pytest.raises(ModelWarming):
        mgr.lease(FLUX)
    mgr.wait_for_load(5)
    with pytest.raises(ModelWarming) as err:
        mgr.lease(ZIMG)
    assert err.value.resident == FLUX
    assert err.value.loading is None
