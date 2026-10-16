"""Swap-logic tests for image_swap.ImageModelManager (citadel-cli#1335).

The pipeline loader, unloader, memory freer and free-VRAM probe are all fakes:
no torch, diffusers, GPU or network. Loads block on threading.Events so the
tests control exactly when a "cold load" finishes."""

from __future__ import annotations

import threading

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
        self.lock = threading.Lock()

    def load(self, spec, variant):
        gate = self.gates.get(spec.model_id)
        if gate is not None:
            assert gate.wait(5), "test never released the load gate"
        if spec.model_id in self.fail:
            raise RuntimeError(self.fail.pop(spec.model_id))
        with self.lock:
            self.loads.append((spec.model_id, variant.quant, variant.offload))
        return f"pipe:{spec.model_id}", "cuda"

    def unload(self, pipe):
        self.unloaded.append(pipe)

    def free(self):
        self.freed += 1


def make(loader=None, *, budget=24.0, quant="auto", free_gb=None, warm_wait=0.0, drain=5.0, **kw):
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
    assert loader.freed == 1
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
    assert "bad repo id" in str(err.value)
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
    assert err.value.need_gb == 16.5
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
