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
from types import SimpleNamespace

import pytest
from fastapi.testclient import TestClient

import app


@pytest.fixture(autouse=True)
def _reset_lazy_singletons(monkeypatch):
    """Every test gets a clean slate: app.py's lazy _pipe/_video_pipe globals
    must not leak a fake object set by one test into the next (in particular,
    into /health's *_loaded assertions)."""
    monkeypatch.setattr(app, "_device", None)
    monkeypatch.setattr(app, "_video_pipe", None)
    monkeypatch.setattr(app, "_video_device", None)


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
    vice versa -- the two lazy singletons are independent (citadel #958's
    coexistence requirement)."""
    monkeypatch.setattr(app, "_video_pipe", object())
    monkeypatch.setattr(app, "_video_device", "cuda")

    resp = client.get("/health")
    body = resp.json()
    assert body["video_model_loaded"] is True
    assert body["video_device"] == "cuda"
    assert body["model_loaded"] is False  # image pipe untouched
    assert body["device"] is None


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
    assert app.DIFFUSERS_MODEL in resp.json()["detail"]


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
    assert app.DIFFUSERS_VIDEO_MODEL in resp.json()["detail"]


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


def test_load_failure_surfaces_as_500_naming_the_model(client, monkeypatch):
    mgr = ImageModelManager(
        app.ALLOWED_IMAGE_MODELS,
        app.DIFFUSERS_MODEL,
        loader=lambda s, v: (_ for _ in ()).throw(RuntimeError("401 gated")),
        vram_budget_gb=24.0,
        warm_wait_seconds=5.0,
    )
    monkeypatch.setattr(app, "_manager", mgr)
    resp = _post(client, ZIMG)
    assert resp.status_code == 500
    assert ZIMG in resp.json()["detail"] and "401 gated" in resp.json()["detail"]


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
    monkeypatch.setattr(app, "_video_pipe", object())
    monkeypatch.setattr(app, "_video_device", "cuda")
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
