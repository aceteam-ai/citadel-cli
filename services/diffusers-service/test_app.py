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
    monkeypatch.setattr(app, "_pipe", None)
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
    assert body["width"] == 512 and body["height"] == 512
    assert body["seed"] is None

    call = fake_pipe.calls[0]
    assert call["prompt"] == "a cat"
    assert call["num_inference_steps"] == 4  # sdxl-turbo default
    assert call["guidance_scale"] == 0.0  # turbo default


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
