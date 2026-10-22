"""Contract/unit tests for app.py's HTTP surface (citadel #958).

Hermetic: the pipeline-load functions (_get_pipe, _get_video_pipe) and the
video-export seam (_export_video_bytes) are always monkeypatched, so these
tests exercise only FastAPI request/response wiring -- never torch, diffusers,
imageio, or a real model/GPU. app.py itself only imports those heavy
dependencies lazily, inside the functions being mocked here (see app.py's
module docstring / _get_pipe / _get_video_pipe / _export_video_bytes), so this
file can import `app` directly with nothing but fastapi + pydantic installed.
This mirrors model_preflight.py's own "import-light, hermetically testable"
design documented in test_model_preflight.py's module docstring.

Run:  python3 -m pytest services/diffusers-service/test_app.py
(requires fastapi + pydantic + httpx -- see .github/workflows/ci.yml's
diffusers app-contract-tests step for the exact install command; deliberately
does NOT require torch/diffusers/accelerate/imageio.)
"""

from __future__ import annotations

import base64
import io
import sys
from types import ModuleType, SimpleNamespace

import pytest
from fastapi.testclient import TestClient

import app


@pytest.mark.parametrize("raw, expected", [
    ("-1", 0.0), ("inf", 300.0), ("-inf", 300.0),
    ("nan", 300.0), ("owned-invalid-setting", 300.0),
])
def test_invalid_idle_settings_warn_without_echoing_values(monkeypatch, caplog, raw, expected):
    monkeypatch.setenv("DIFFUSERS_IDLE_UNLOAD_SECONDS", raw)
    assert app._idle_unload_seconds() == expected
    assert len(caplog.records) == 1
    assert caplog.records[0].levelname == "WARNING"
    assert raw not in caplog.records[0].message


@pytest.mark.parametrize("raw, expected", [("0", 0.0), ("12.5", 12.5)])
def test_valid_idle_settings_have_no_warning(monkeypatch, caplog, raw, expected):
    monkeypatch.setenv("DIFFUSERS_IDLE_UNLOAD_SECONDS", raw)
    assert app._idle_unload_seconds() == expected
    assert not caplog.records


def test_pin_validation_warns_once_without_echoing_unknown_ids(caplog):
    app._warn_unknown_pins(frozenset({"owned/image", "owned/video"}), {"owned/image": None}, "owned/video")
    assert not caplog.records
    app._warn_unknown_pins(frozenset({"owned/unknown-secret", "owned/second-secret"}),
                           {"owned/image": None}, "owned/video")
    assert len(caplog.records) == 1
    assert "unknown model IDs" in caplog.records[0].message
    assert "owned/" not in caplog.records[0].message


@pytest.fixture(autouse=True)
def _reset_lazy_singletons(monkeypatch):
    """Every test gets independent video residency without real pipelines."""
    monkeypatch.setattr(app, "_device", None)
    monkeypatch.setattr(app, "_video_device", None)
    monkeypatch.setattr(app, "_video_manager", app.ImageModelManager(
        {app.DIFFUSERS_VIDEO_MODEL: app._video_spec}, app.DIFFUSERS_VIDEO_MODEL,
        loader=lambda spec, variant: (object(), "cuda"), warm_wait_seconds=5,
        pinned_models=frozenset({app.DIFFUSERS_VIDEO_MODEL}),
    ))


@pytest.fixture
def client():
    return TestClient(app.app)


class _FakeImage:
    """Stands in for a real PIL Image -- only .save(buf, format=...) is ever
    called on the result by /generate, so that's all this needs to provide."""

    def save(self, buf, format="PNG"):  # noqa: A002 - matches PIL's Image.save kw name
        buf.write(b"fake-png-bytes")


class _FakePipe:
    """Stands in for either AutoPipelineForText2Image's or WanPipeline's
    callable result -- records every call's kwargs (so tests can assert what
    the endpoint forwarded) and returns a SimpleNamespace shaped like
    whichever of .images/.frames the caller configured."""

    def __init__(self, images=None, frames=None):
        self._images = images
        self._frames = frames
        self.calls = []

    def __call__(self, **kwargs):
        self.calls.append(kwargs)
        if self._images is not None:
            return SimpleNamespace(images=self._images)
        return SimpleNamespace(frames=self._frames)


# ---------------------------------------------------------------------------
# /health
# ---------------------------------------------------------------------------


def test_health_reports_both_models_unloaded(client):
    resp = client.get("/health")
    assert resp.status_code == 200
    body = resp.json()
    assert body["status"] == "ok"
    assert body["model"] == app.DIFFUSERS_MODEL
    assert body["model_loaded"] is False
    assert body["device"] is None
    assert body["video_model"] == app.DIFFUSERS_VIDEO_MODEL
    assert body["video_model_loaded"] is False
    assert body["video_device"] is None


def test_health_video_loaded_state_is_independent_of_image_state(client, monkeypatch):
    """Loading the video pipe must not report the image pipe as loaded, and
    vice versa -- the two residency managers are independent (citadel #958's
    coexistence requirement)."""
    app._video_manager.lease(app.DIFFUSERS_VIDEO_MODEL).release()

    resp = client.get("/health")
    body = resp.json()
    assert body["video_model_loaded"] is True
    assert body["video_device"] == "cuda"
    assert body["model_loaded"] is False  # image pipe untouched
    assert body["device"] is None


@pytest.mark.parametrize("failure", [None, "inference", "export"])
def test_video_requests_release_real_lease_on_success_and_failure(client, monkeypatch, failure):
    callbacks = []

    def timer_factory(delay, callback):
        callbacks.append(callback)
        return SimpleNamespace(start=lambda: None, cancel=lambda: None)

    class Pipe(_FakePipe):
        def __call__(self, **kwargs):
            if failure == "inference":
                raise RuntimeError("owned inference refusal")
            return super().__call__(**kwargs)

    manager = app.ImageModelManager(
        {app.DIFFUSERS_VIDEO_MODEL: app._video_spec}, app.DIFFUSERS_VIDEO_MODEL,
        loader=lambda spec, variant: (Pipe(frames=[["owned-frame"]]), "cpu"),
        warm_wait_seconds=5, idle_unload_seconds=0, timer_factory=timer_factory,
    )
    monkeypatch.setattr(app, "_video_manager", manager)
    monkeypatch.setattr(app, "_host_rss_bytes", lambda: 123456)

    def export(frames, fps):
        if failure == "export":
            raise RuntimeError("owned export refusal")
        return b"owned-video"

    monkeypatch.setattr(app, "_export_video_bytes", export)
    response = client.post("/generate/video", json={"prompt": "owned-test"})
    assert response.status_code == (200 if failure is None else 500)
    assert manager._resident.inflight == 0
    callbacks[-1]()  # release timer, not the superseded initial-load timer
    assert manager.loaded_model() is None
    health = client.get("/health").json()
    assert health["host_rss_bytes"] == 123456
    assert health["pipeline_residency"]["video"][0]["resident"] is False


def test_video_cold_load_uses_the_existing_retryable_warming_response(client, monkeypatch):
    def warming():
        raise app.ModelWarming(app.DIFFUSERS_VIDEO_MODEL, eta_seconds=120, retry_after=5, reason="loading")

    monkeypatch.setattr(app, "_get_video_pipe", warming)
    response = client.post("/generate/video", json={"prompt": "owned-test"})
    assert response.status_code == 503
    assert response.headers["retry-after"] == "5"
    assert response.json()["status"] == "model_warming"


def test_health_rss_reads_current_resident_pages_not_peak(monkeypatch):
    def statm(path, **kwargs):
        assert path == "/proc/self/statm"
        return io.StringIO("10000 17 4 0 0 0 0\n")

    monkeypatch.setattr("builtins.open", statm)
    monkeypatch.setattr(app.os, "sysconf", lambda name: 4096)
    assert app._host_rss_bytes() == 17 * 4096


def test_health_rss_unavailable_is_unknown_not_a_peak_fallback(monkeypatch):
    def missing(*args, **kwargs):
        raise OSError("owned missing proc fixture")

    monkeypatch.setattr("builtins.open", missing)
    assert app._host_rss_bytes() is None


# ---------------------------------------------------------------------------
# /generate (image path -- pre-existing behavior, pinned here so citadel #958
# cannot regress it)
# ---------------------------------------------------------------------------


def test_generate_image_returns_base64_png(client, monkeypatch):
    fake_pipe = _FakePipe(images=[_FakeImage()])
    monkeypatch.setattr(app, "_get_pipe", lambda: fake_pipe)
    monkeypatch.setattr(app, "_device", "cpu")

    resp = client.post("/generate", json={"prompt": "a cat"})
    assert resp.status_code == 200
    body = resp.json()
    assert body["content_type"] == "image/png"
    assert base64.b64decode(body["image_base64"]) == b"fake-png-bytes"
    assert body["model"] == app.DIFFUSERS_MODEL
    assert body["device"] == "cpu"
    assert body["width"] == 1024 and body["height"] == 1024  # FLUX.2-klein default size
    assert body["seed"] is None

    call = fake_pipe.calls[0]
    assert call["prompt"] == "a cat"
    assert call["num_inference_steps"] == 4  # FLUX.2-klein (distilled) default
    assert call["guidance_scale"] == 1.0  # FLUX.2-klein default


def test_generate_image_load_failure_is_500(client, monkeypatch):
    def _boom():
        raise RuntimeError("out of memory")

    monkeypatch.setattr(app, "_get_pipe", _boom)
    resp = client.post("/generate", json={"prompt": "a cat"})
    assert resp.status_code == 500
    assert resp.json()["detail"] == f"failed to load model {app.DIFFUSERS_MODEL}: pipeline load failed"
    assert "out of memory" not in resp.text


def test_generate_image_inference_failure_is_500(client, monkeypatch):
    class _BoomPipe:
        def __call__(self, **kwargs):
            raise RuntimeError("cuda oom")

    monkeypatch.setattr(app, "_get_pipe", lambda: _BoomPipe())
    monkeypatch.setattr(app, "_device", "cuda")
    resp = client.post("/generate", json={"prompt": "a cat"})
    assert resp.status_code == 500
    assert "generation failed" in resp.json()["detail"]


# ---------------------------------------------------------------------------
# /generate/video (citadel #958)
# ---------------------------------------------------------------------------


def test_generate_video_returns_base64_mp4(client, monkeypatch):
    fake_frames = ["frame-sentinel"]
    fake_pipe = _FakePipe(frames=[fake_frames])
    monkeypatch.setattr(app, "_get_video_pipe", lambda: fake_pipe)
    monkeypatch.setattr(app, "_video_device", "cpu")

    captured = {}

    def fake_export(frames, fps):
        captured["frames"] = frames
        captured["fps"] = fps
        return b"fake-mp4-bytes"

    monkeypatch.setattr(app, "_export_video_bytes", fake_export)

    resp = client.post("/generate/video", json={"prompt": "a cat baking a cake"})
    assert resp.status_code == 200
    body = resp.json()
    assert body["content_type"] == "video/mp4"
    assert base64.b64decode(body["video_base64"]) == b"fake-mp4-bytes"
    assert body["model"] == app.DIFFUSERS_VIDEO_MODEL
    assert body["device"] == "cpu"
    # WanPipeline documented defaults (diffusers WanPipeline.__call__).
    assert body["height"] == 480 and body["width"] == 832
    assert body["num_frames"] == 81
    assert body["fps"] == 16
    assert body["seed"] is None

    assert captured["frames"] is fake_frames
    assert captured["fps"] == 16

    call = fake_pipe.calls[0]
    assert call["prompt"] == "a cat baking a cake"
    assert call["guidance_scale"] == 5.0
    assert call["num_inference_steps"] == 50
    assert call["height"] == 480 and call["width"] == 832
    assert call["num_frames"] == 81


def test_generate_video_accepts_overrides(client, monkeypatch):
    # Deliberately omits `seed` -- the seed branch imports torch directly
    # (`import torch` inside the handler, mirroring /generate's identical
    # pattern), which this hermetic test environment does not install; see
    # test_generate_video_seed_constructs_generator_via_injected_torch below
    # for seed-branch coverage via an injected fake torch module instead.
    fake_pipe = _FakePipe(frames=[["f"]])
    monkeypatch.setattr(app, "_get_video_pipe", lambda: fake_pipe)
    monkeypatch.setattr(app, "_video_device", "cpu")
    monkeypatch.setattr(app, "_export_video_bytes", lambda frames, fps: b"x")

    resp = client.post(
        "/generate/video",
        json={
            "prompt": "a robot dancing",
            "negative_prompt": "blurry, low quality",
            "num_frames": 33,
            "fps": 24,
            "height": 320,
            "width": 576,
            "num_inference_steps": 20,
            "guidance_scale": 3.5,
        },
    )
    assert resp.status_code == 200
    body = resp.json()
    assert body["num_frames"] == 33
    assert body["fps"] == 24
    assert body["height"] == 320 and body["width"] == 576
    assert body["seed"] is None

    call = fake_pipe.calls[0]
    assert call["negative_prompt"] == "blurry, low quality"
    assert call["num_frames"] == 33
    assert call["height"] == 320 and call["width"] == 576
    assert call["num_inference_steps"] == 20
    assert call["guidance_scale"] == 3.5
    assert call["generator"] is None


def test_generate_video_seed_constructs_generator_via_injected_torch(client, monkeypatch):
    """Covers the req.seed-is-not-None branch (`import torch;
    torch.Generator(device=...).manual_seed(...)`) without requiring a real
    torch install: injects a minimal fake `torch` module into sys.modules for
    the duration of this test only, matching how the rest of this file avoids
    the heavy ML dependency stack entirely."""
    import sys

    class _FakeGenerator:
        def __init__(self, device=None):
            self.device = device
            self.seed = None

        def manual_seed(self, seed):
            self.seed = seed
            return self

    fake_torch = SimpleNamespace(Generator=_FakeGenerator)
    monkeypatch.setitem(sys.modules, "torch", fake_torch)

    fake_pipe = _FakePipe(frames=[["f"]])
    monkeypatch.setattr(app, "_get_video_pipe", lambda: fake_pipe)
    monkeypatch.setattr(app, "_video_device", "cuda")
    monkeypatch.setattr(app, "_export_video_bytes", lambda frames, fps: b"x")

    resp = client.post("/generate/video", json={"prompt": "x", "seed": 42})
    assert resp.status_code == 200
    assert resp.json()["seed"] == 42

    generator = fake_pipe.calls[0]["generator"]
    assert isinstance(generator, _FakeGenerator)
    assert generator.device == "cuda"
    assert generator.seed == 42


def test_generate_video_load_failure_is_500(client, monkeypatch):
    def _boom():
        raise RuntimeError("no space left on device")

    monkeypatch.setattr(app, "_get_video_pipe", _boom)
    resp = client.post("/generate/video", json={"prompt": "x"})
    assert resp.status_code == 500
    assert resp.json()["detail"] == f"failed to load model {app.DIFFUSERS_VIDEO_MODEL}: pipeline load failed"
    assert "no space left on device" not in resp.text


def test_generate_video_inference_failure_is_500(client, monkeypatch):
    class _BoomPipe:
        def __call__(self, **kwargs):
            raise RuntimeError("cuda oom")

    monkeypatch.setattr(app, "_get_video_pipe", lambda: _BoomPipe())
    monkeypatch.setattr(app, "_video_device", "cuda")
    resp = client.post("/generate/video", json={"prompt": "x"})
    assert resp.status_code == 500
    assert "video generation failed" in resp.json()["detail"]


def test_generate_video_export_failure_is_500(client, monkeypatch):
    """Export (ffmpeg/imageio) failures must surface as a distinct 500, not
    be swallowed or mistaken for an inference failure -- proves
    _export_video_bytes is on its own try/except in the handler."""
    fake_pipe = _FakePipe(frames=[["f"]])
    monkeypatch.setattr(app, "_get_video_pipe", lambda: fake_pipe)
    monkeypatch.setattr(app, "_video_device", "cpu")

    def _boom(frames, fps):
        raise RuntimeError("ffmpeg not found")

    monkeypatch.setattr(app, "_export_video_bytes", _boom)
    resp = client.post("/generate/video", json={"prompt": "x"})
    assert resp.status_code == 500
    assert "video export failed" in resp.json()["detail"]


def test_generate_video_request_validation_rejects_zero_frames(client):
    # Regression guard against a future accidental widening of num_frames'
    # lower bound that would let a request ask for a zero/negative-length
    # render.
    resp = client.post("/generate/video", json={"prompt": "x", "num_frames": 0})
    assert resp.status_code == 422


def test_generate_video_requires_prompt(client):
    resp = client.post("/generate/video", json={})
    assert resp.status_code == 422


# ---------------------------------------------------------------------------
# Coexistence: calling one endpoint must never touch the other pipeline.
# ---------------------------------------------------------------------------


def test_generate_video_does_not_load_image_pipe(client, monkeypatch):
    def _fail_if_called():
        raise AssertionError("_get_pipe (image) must not be called by /generate/video")

    monkeypatch.setattr(app, "_get_pipe", _fail_if_called)
    monkeypatch.setattr(app, "_get_video_pipe", lambda: _FakePipe(frames=[["f"]]))
    monkeypatch.setattr(app, "_video_device", "cpu")
    monkeypatch.setattr(app, "_export_video_bytes", lambda frames, fps: b"x")

    resp = client.post("/generate/video", json={"prompt": "x"})
    assert resp.status_code == 200


def test_generate_image_does_not_load_video_pipe(client, monkeypatch):
    def _fail_if_called():
        raise AssertionError("_get_video_pipe (video) must not be called by /generate")

    monkeypatch.setattr(app, "_get_video_pipe", _fail_if_called)
    monkeypatch.setattr(app, "_get_pipe", lambda: _FakePipe(images=[_FakeImage()]))
    monkeypatch.setattr(app, "_device", "cpu")

    resp = client.post("/generate", json={"prompt": "x"})
    assert resp.status_code == 200


# ---------------------------------------------------------------------------
# Per-request image model selection, swap, warming and health (citadel-cli#1335).
# The swap manager is replaced by a fake-loader instance: no torch, diffusers
# or GPU.
# ---------------------------------------------------------------------------

import threading  # noqa: E402

import image_models  # noqa: E402
from image_swap import ImageModelManager  # noqa: E402

SDXL = "stabilityai/sdxl-turbo"
FLUX = "black-forest-labs/FLUX.2-klein-4B"
ZIMG = "Tongyi-MAI/Z-Image-Turbo"
QWEN = "Qwen/Qwen-Image"


class _RecordingPipe(_FakePipe):
    def __init__(self, name):
        super().__init__(images=[_FakeImage()])
        self.name = name


@pytest.fixture
def swap(monkeypatch):
    """A real manager over fake pipelines, wired into app. Loads can be gated."""
    state = SimpleNamespace(gates={}, loads=[], unloaded=[], pipes={})

    def loader(spec, variant):
        gate = state.gates.get(spec.model_id)
        if gate is not None:
            assert gate.wait(5)
        pipe = _RecordingPipe(spec.model_id)
        state.pipes[spec.model_id] = pipe
        state.loads.append((spec.model_id, variant.quant, variant.offload))
        return pipe, "cuda"

    mgr = ImageModelManager(
        image_models.parse_allowlist(" ".join(image_models.CATALOG), app.DIFFUSERS_MODEL),
        app.DIFFUSERS_MODEL,
        loader=loader,
        min_residency_seconds=0.0,
        unloader=lambda pipe: state.unloaded.append(pipe.name),
        vram_budget_gb=24.0,
        warm_wait_seconds=0.0,
        retry_after_seconds=3,
    )
    monkeypatch.setattr(app, "_manager", mgr)
    # The default allowlist excludes sdxl-turbo; these tests exercise it too.
    monkeypatch.setattr(
        app,
        "ALLOWED_IMAGE_MODELS",
        image_models.parse_allowlist(" ".join(image_models.CATALOG), app.DIFFUSERS_MODEL),
    )
    state.mgr = mgr
    return state


def _post(client, model=None, **extra):
    body = {"prompt": "a synthetic sagittal lumbar MRI", **extra}
    if model is not None:
        body["model"] = model
    return client.post("/generate", json=body)


def test_unknown_model_is_a_clear_400(client, swap):
    resp = _post(client, "totally/not-allowed")
    assert resp.status_code == 400
    body = resp.json()
    assert body["error"] == "model_not_allowed"
    assert "totally/not-allowed" in body["detail"]
    assert FLUX in body["allowed_models"]
    assert swap.loads == []  # nothing was loaded for a rejected request


def test_non_allowlisted_catalog_model_is_rejected(client, swap, monkeypatch):
    narrow = image_models.parse_allowlist(SDXL, SDXL)
    monkeypatch.setattr(app, "ALLOWED_IMAGE_MODELS", narrow)
    resp = _post(client, QWEN)
    assert resp.status_code == 400
    assert resp.json()["allowed_models"] == [SDXL]


def test_cold_model_returns_retryable_warming_then_image(client, swap):
    gate = swap.gates[FLUX] = threading.Event()
    resp = _post(client, FLUX)
    assert resp.status_code == 503
    assert resp.headers["retry-after"] == "3"
    body = resp.json()
    assert body["status"] == "model_warming"
    assert body["model"] == FLUX
    assert body["retry_after"] == 3
    assert body["eta_seconds"] > 0
    assert body["reason"] == "loading"

    gate.set()
    assert swap.mgr.wait_for_load(5)
    resp = _post(client, FLUX)
    assert resp.status_code == 200
    body = resp.json()
    assert body["model"] == FLUX
    assert body["content_type"] == "image/png"
    assert body["width"] == 1024 and body["height"] == 1024  # per-model default size


def test_omitted_model_serves_the_configured_default(client, swap):
    assert _post(client).status_code == 503  # cold
    swap.mgr.wait_for_load(5)
    resp = _post(client)
    assert resp.status_code == 200
    assert resp.json()["model"] == app.DIFFUSERS_MODEL
    assert swap.loads[0][0] == app.DIFFUSERS_MODEL


def test_switching_models_unloads_the_previous_one(client, swap):
    _post(client, SDXL)
    swap.mgr.wait_for_load(5)
    assert _post(client, SDXL).status_code == 200

    assert _post(client, ZIMG).status_code == 503
    swap.mgr.wait_for_load(5)
    assert swap.unloaded == [SDXL]
    assert _post(client, ZIMG).status_code == 200
    assert swap.mgr.loaded_model() == ZIMG


def test_per_model_defaults_and_cfg_param_mapping(client, swap):
    for model in (QWEN, ZIMG):
        _post(client, model)
        swap.mgr.wait_for_load(5)
    # QWEN was displaced by ZIMG; load QWEN again to inspect its call.
    _post(client, QWEN)
    swap.mgr.wait_for_load(5)
    assert _post(client, QWEN).status_code == 200
    call = swap.pipes[QWEN].calls[-1]
    assert call["true_cfg_scale"] == 4.0
    assert "guidance_scale" not in call
    assert call["num_inference_steps"] == 30
    assert call["negative_prompt"] == " "  # Qwen-Image true CFG needs one


def test_pipeline_without_negative_prompt_support_never_receives_it(client, swap):
    _post(client, FLUX, negative_prompt="blurry")
    swap.mgr.wait_for_load(5)
    assert _post(client, FLUX, negative_prompt="blurry").status_code == 200
    call = swap.pipes[FLUX].calls[-1]
    assert "negative_prompt" not in call
    assert call["guidance_scale"] == 1.0
    assert call["num_inference_steps"] == 4


def test_explicit_request_values_override_model_defaults(client, swap):
    _post(client, ZIMG)
    swap.mgr.wait_for_load(5)
    resp = _post(client, ZIMG, num_inference_steps=12, guidance_scale=1.5, width=640, height=768)
    assert resp.status_code == 200
    call = swap.pipes[ZIMG].calls[-1]
    assert (call["num_inference_steps"], call["guidance_scale"]) == (12, 1.5)
    assert (call["width"], call["height"]) == (640, 768)


def test_lease_is_released_after_each_request_so_a_swap_can_proceed(client, swap):
    _post(client, SDXL)
    swap.mgr.wait_for_load(5)
    for _ in range(3):
        assert _post(client, SDXL).status_code == 200
    _post(client, FLUX)  # starts the swap; drains only if no lease leaked
    assert swap.mgr.wait_for_load(5)
    assert swap.mgr.loaded_model() == FLUX


def test_lease_is_released_when_inference_fails(client, swap):
    _post(client, SDXL)
    swap.mgr.wait_for_load(5)

    class _Boom:
        def __call__(self, **kw):
            raise RuntimeError("cuda oom")

    swap.mgr._resident.pipe = _Boom()
    assert _post(client, SDXL).status_code == 500
    assert swap.mgr._resident.inflight == 0


def test_model_over_vram_budget_is_422(client, swap, monkeypatch):
    monkeypatch.setattr(
        app,
        "_manager",
        ImageModelManager(
            app.ALLOWED_IMAGE_MODELS,
            app.DIFFUSERS_MODEL,
            loader=lambda s, v: (None, "cuda"),
            vram_budget_gb=8.0,
            warm_wait_seconds=0.0,
        ),
    )
    resp = _post(client, QWEN)
    assert resp.status_code == 422
    body = resp.json()
    assert body["error"] == "exceeds_vram_budget"
    assert body["min_required_gb"] == 12 and body["vram_budget_gb"] == 8


def test_insufficient_free_vram_is_503_but_not_warming(client, monkeypatch):
    mgr = ImageModelManager(
        app.ALLOWED_IMAGE_MODELS,
        app.DIFFUSERS_MODEL,
        loader=lambda s, v: pytest.fail("must not load without room"),
        free_vram_bytes=lambda: 2 * 1024**3,
        vram_budget_gb=24.0,
        warm_wait_seconds=0.0,
    )
    monkeypatch.setattr(app, "_manager", mgr)
    _post(client, FLUX)
    mgr.wait_for_load(5)
    resp = _post(client, FLUX)
    assert resp.status_code == 503
    body = resp.json()
    assert body["status"] == "insufficient_vram"
    assert "Retry-After" not in resp.headers


def test_failed_attempt_followed_by_vram_skips_is_static_503_not_warming(client, monkeypatch, caplog):
    free = [12 * 1024**3]
    attempts = []

    def load(spec, variant):
        attempts.append(variant.quant)
        free[0] = 6 * 1024**3
        raise RuntimeError("owned-private-loader-detail")

    manager = ImageModelManager(
        app.ALLOWED_IMAGE_MODELS, FLUX, loader=load,
        free_vram_bytes=lambda: free[0], vram_budget_gb=12, warm_wait_seconds=5,
    )
    monkeypatch.setattr(app, "_manager", manager)
    resp = _post(client, FLUX)
    assert attempts == ["int8"]
    assert resp.status_code == 503
    body = resp.json()
    assert body["status"] == "insufficient_vram" and body["model"] == FLUX
    assert (body["need_gb"], body["free_gb"]) == (7.0, 6.0)
    assert "model_warming" not in resp.text
    assert "Retry-After" not in resp.headers
    assert "owned-private-loader-detail" not in resp.text + caplog.text
    assert "candidate int8 failed [model_runtime]: model runtime failed" in caplog.text


@pytest.mark.parametrize("pipeline", ["image", "video"])
@pytest.mark.parametrize("cause, kind, reason", [
    ("disk", "disk_space", "insufficient disk space for model download"),
    ("gated", "repository_access_denied", "model repository access denied"),
    ("missing", "repository_not_found", "model repository not found or inaccessible"),
])
def test_typed_load_refusals_keep_static_actionable_http_context(
    client, monkeypatch, caplog, pipeline, cause, kind, reason,
):
    # These inert exports model already-loaded HF classes, not an HF import or
    # authorization request. The real disk exception is import-light as well.
    class RepositoryNotFoundError(RuntimeError):
        pass

    class GatedRepoError(RepositoryNotFoundError):
        pass

    module = ModuleType("huggingface_hub.errors")
    module.RepositoryNotFoundError = RepositoryNotFoundError
    module.GatedRepoError = GatedRepoError
    monkeypatch.setitem(sys.modules, module.__name__, module)
    failure_class = {
        "disk": app.model_preflight.InsufficientDiskSpaceError,
        "gated": GatedRepoError,
        "missing": RepositoryNotFoundError,
    }[cause]
    attempts = []

    def refuse():
        raise failure_class("owned-private-loader-detail /owned/private/path hf_private-token")

    def prepare(spec, variant):
        if pipeline == "image":
            refuse()

    def load(spec, variant):
        attempts.append(variant.quant)
        refuse()  # the video loader contains its preflight in production

    model_id = ZIMG if pipeline == "image" else app.DIFFUSERS_VIDEO_MODEL
    specs = app.ALLOWED_IMAGE_MODELS if pipeline == "image" else {model_id: app._video_spec}
    manager = ImageModelManager(
        specs, model_id, loader=load, prepare=prepare, warm_wait_seconds=5,
    )
    monkeypatch.setattr(app, "_manager" if pipeline == "image" else "_video_manager", manager)
    resp = _post(client, model_id) if pipeline == "image" else client.post(
        "/generate/video", json={"prompt": "owned-test"},
    )
    assert resp.status_code == 500
    assert resp.json()["detail"] == f"failed to load model {model_id}: {reason}"
    assert resp.json()["detail"].count("failed to load model ") == 1
    assert attempts == ([] if pipeline == "image" else ["none"])
    assert f"[{kind}]: {reason}" in caplog.text
    for private in ("owned-private-loader-detail", "/owned/private/path", "hf_private-token"):
        assert private not in resp.text + caplog.text
    assert all(record.exc_info is None for record in caplog.records)


@pytest.mark.parametrize("pipeline", ["image", "video"])
def test_load_failure_surfaces_as_static_500_naming_the_trusted_model(client, monkeypatch, caplog, pipeline):
    model_id = ZIMG if pipeline == "image" else app.DIFFUSERS_VIDEO_MODEL
    specs = app.ALLOWED_IMAGE_MODELS if pipeline == "image" else {model_id: app._video_spec}
    mgr = ImageModelManager(
        specs,
        model_id,
        loader=lambda s, v: (_ for _ in ()).throw(RuntimeError("401 gated owned-private-loader-detail")),
        vram_budget_gb=24.0,
        warm_wait_seconds=5.0,
    )
    monkeypatch.setattr(app, "_manager" if pipeline == "image" else "_video_manager", mgr)
    resp = _post(client, model_id) if pipeline == "image" else client.post(
        "/generate/video", json={"prompt": "owned-test"},
    )
    assert resp.status_code == 500
    assert resp.json()["detail"] == f"failed to load model {model_id}: all eligible pipeline loads failed"
    assert resp.json()["detail"].count("failed to load model ") == 1
    assert "401 gated" not in resp.text
    assert "owned-private-loader-detail" not in resp.text
    assert "owned-private-loader-detail" not in caplog.text
    assert "[model_runtime]: model runtime failed" in caplog.text


@pytest.mark.parametrize("pipeline", ["image", "video"])
@pytest.mark.parametrize("failure_kind", ["generic", "foreign-model", "static"])
def test_http_load_errors_ignore_untrusted_message_and_model_identity(client, monkeypatch, pipeline, failure_kind):
    class PrivateFailure(RuntimeError):
        def __str__(self):
            raise AssertionError("the load-error HTTP boundary must not format an arbitrary exception")

    if failure_kind == "generic":
        failure = PrivateFailure()
    else:
        kind = app.FailureKind.ALL_FAILED if failure_kind == "static" else app.FailureKind.LOAD_FAILED
        failure = app.ModelLoadFailed("owned-foreign-model", kind)
        failure.message = "owned-private-loader-detail"  # HTTP classification must use only kind

    def refuse(*args):
        raise failure

    if pipeline == "image":
        monkeypatch.setattr(app, "_get_pipe", refuse)
        model_id = ZIMG
        resp = _post(client, model_id)
    else:
        monkeypatch.setattr(app, "_get_video_pipe", refuse)
        model_id = app.DIFFUSERS_VIDEO_MODEL
        resp = client.post("/generate/video", json={"prompt": "owned-test"})
    reason = "all eligible pipeline loads failed" if failure_kind == "static" else "pipeline load failed"
    assert resp.status_code == 500
    assert resp.json()["detail"] == f"failed to load model {model_id}: {reason}"
    assert "owned-foreign-model" not in resp.text
    assert "owned-private-loader-detail" not in resp.text


def test_health_lists_allowlist_loaded_model_and_warm_vs_cold(client, swap):
    body = client.get("/health").json()
    assert body["allowed_models"] == list(app.ALLOWED_IMAGE_MODELS)
    assert body["loaded_model"] is None and body["model_loaded"] is False
    assert {m["state"] for m in body["image_models"]} == {"cold"}
    by_id = {m["model"]: m for m in body["image_models"]}
    assert by_id[app.DIFFUSERS_MODEL]["default"] is True
    assert by_id[SDXL]["default"] is False
    assert by_id[FLUX]["license"] == "apache-2.0"
    assert body["vram_budget_gb"] == 24.0

    _post(client, ZIMG)
    swap.mgr.wait_for_load(5)
    body = client.get("/health").json()
    assert body["loaded_model"] == ZIMG
    assert body["model_loaded"] is True
    assert body["device"] == "cuda"
    states = {m["model"]: m["state"] for m in client.get("/health").json()["image_models"]}
    assert states[ZIMG] == "warm"
    assert states[SDXL] == states[FLUX] == states[QWEN] == "cold"
    assert body["last_load"]["model"] == ZIMG


def test_health_still_reports_the_video_pipeline_independently(client, swap, monkeypatch):
    app._video_manager.lease(app.DIFFUSERS_VIDEO_MODEL).release()
    body = client.get("/health").json()
    assert body["video_model_loaded"] is True and body["model_loaded"] is False


def test_default_model_is_the_permissive_flux2_klein():
    assert app.DIFFUSERS_MODEL == FLUX
    assert image_models.CATALOG[FLUX].permissive


def test_prepare_pipeline_fails_closed_for_catalog_models_with_a_disk_floor(monkeypatch):
    seen = {}
    monkeypatch.setattr(app.model_preflight, "run_preflight", lambda repo, cache, **kw: seen.update(repo=repo, **kw))
    monkeypatch.setattr(app.model_preflight, "prefetch_filtered_weights", lambda repo, **kw: seen.update(prefetch=kw))
    for model in (FLUX, ZIMG, QWEN, SDXL):
        seen.clear()
        spec = image_models.CATALOG[model]
        app._prepare_pipeline(spec, spec.variants[0])
        assert seen["fail_closed"] is True, model
        assert seen["margin_bytes"] == 20 * 1024**3, model
        assert seen["allow_patterns"], model
        assert seen["prefetch"]["allow_patterns"] == seen["allow_patterns"], model
        assert seen["is_cached_fn"] is not None


def test_prepare_pipeline_keeps_warn_only_for_custom_models(monkeypatch):
    seen = {}
    monkeypatch.setattr(app.model_preflight, "run_preflight", lambda repo, cache, **kw: seen.update(kw))
    monkeypatch.setattr(app.model_preflight, "prefetch_filtered_weights", lambda repo, **kw: None)
    spec = image_models.custom_spec("some/custom-model", 8)
    app._prepare_pipeline(spec, spec.variants[0])
    assert seen["fail_closed"] is False


def test_concurrent_generations_are_serialized(client, swap):
    """Sync endpoints run on a threadpool; the pipeline must see one call at a time."""
    _post(client, FLUX)
    swap.mgr.wait_for_load(5)
    state = SimpleNamespace(active=0, peak=0, lock=threading.Lock())

    class _SlowPipe:
        def __call__(self, **kw):
            with state.lock:
                state.active += 1
                state.peak = max(state.peak, state.active)
            threading.Event().wait(0.05)
            with state.lock:
                state.active -= 1
            return SimpleNamespace(images=[_FakeImage()])

    swap.mgr._resident.pipe = _SlowPipe()
    results = []
    threads = [threading.Thread(target=lambda: results.append(_post(client, FLUX).status_code)) for _ in range(6)]
    [t.start() for t in threads]
    [t.join(10) for t in threads]
    assert results == [200] * 6
    assert state.peak == 1


def test_resident_protected_response_names_the_resident_model(client, monkeypatch):
    mgr = ImageModelManager(
        app.ALLOWED_IMAGE_MODELS,
        app.DIFFUSERS_MODEL,
        loader=lambda s, v: (object(), "cuda"),
        vram_budget_gb=24.0,
        warm_wait_seconds=5.0,
        min_residency_seconds=120.0,
    )
    monkeypatch.setattr(app, "_manager", mgr)
    monkeypatch.setattr(app, "ALLOWED_IMAGE_MODELS", image_models.parse_allowlist(f"{FLUX} {ZIMG}", FLUX))
    mgr._warm_wait = 0.0
    _post(client, FLUX)
    mgr.wait_for_load(5)
    resp = _post(client, ZIMG)
    assert resp.status_code == 503
    body = resp.json()
    assert body["reason"] == "resident_protected"
    assert body["resident_model"] == FLUX
    assert "loading_model" not in body


def test_self_test_reports_ok_and_error_in_health(client, monkeypatch):
    monkeypatch.setattr(app, "_self_test", {"status": "pending", "detail": None})
    assert client.get("/health").json()["self_test"] == "pending"

    app._run_self_test(lambda: None)
    body = client.get("/health").json()
    assert body["self_test"] == "ok" and body["self_test_detail"] is None

    def _boom():
        raise RuntimeError("Failed to find C compiler. Please specify via CC environment variable.")

    app._run_self_test(_boom)
    body = client.get("/health").json()
    assert body["self_test"] == "error"
    assert "Failed to find C compiler" in body["self_test_detail"]
    assert body["status"] == "ok"  # liveness is unchanged; the failure is reported, not hidden


def test_self_test_catches_systemexit_from_a_broken_extension(monkeypatch):
    monkeypatch.setattr(app, "_self_test", {"status": "pending", "detail": None})

    def _exit():
        raise SystemExit(1)

    app._run_self_test(_exit)
    assert app._self_test["status"] == "error"


def test_self_test_flags_missing_pipeline_classes(monkeypatch):
    import sys
    import types

    fake = types.ModuleType("diffusers")  # has none of the pipeline classes
    monkeypatch.setitem(sys.modules, "diffusers", fake)
    monkeypatch.setitem(sys.modules, "bitsandbytes", types.ModuleType("bitsandbytes"))
    monkeypatch.setattr(app, "_self_test", {"status": "pending", "detail": None})
    app._run_self_test()
    assert app._self_test["status"] == "error"
    assert "WanPipeline" in app._self_test["detail"]
