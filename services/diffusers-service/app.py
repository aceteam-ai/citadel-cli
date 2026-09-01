"""Node-local text-to-image / text-to-video sidecar for Citadel.

A small FastAPI service that wraps the HuggingFace ``diffusers`` library and
serves generation on the node's GPU. It mirrors the proven node-local sidecar
pattern already shipped in Citadel (see services/whisper-service/app.py): load
a model once, expose a ``/health`` check and inference endpoint(s), and keep
the heavy Python/ML dependency inside a Docker container reachable over
localhost.

Built and published as ``ghcr.io/aceteam-ai/diffusers-service`` and launched on
a node via ``SERVICE_START "diffusers"`` (see services/compose/diffusers.yml).

Contract (aceteam #4468, `diffusers-text-to-image` provisioning template):
  - engine/service name: ``diffusers``
  - listens on port 7860 (the container/server port; the node compose file may
    map a different host port to avoid colliding with the terminal server)
  - ``GET /health`` liveness/readiness probe
  - ``POST /generate`` text-to-image (base64 PNG)
  - ``POST /generate/video`` text-to-video (base64 MP4, citadel #958)

The image model is selected via the ``DIFFUSERS_MODEL`` env var (same knob as
sglang's ``SGLANG_COMMAND`` / whisper's ``WHISPER_MODEL``), defaulting to a
small, fast SDXL-Turbo model. The video model is selected independently via
``DIFFUSERS_VIDEO_MODEL``, defaulting to Wan2.1's 1.3B text-to-video model
(``Wan-AI/Wan2.1-T2V-1.3B-Diffusers``) -- small enough (~4GB transformer) to
fit alongside the image model on the same GPU. The two pipelines are separate
lazy singletons (see ``_get_pipe``/``_get_video_pipe`` below): loading one
never evicts or blocks the other, so ``/generate`` and ``/generate/video``
coexist on one running container. GPU is used when available for both; either
falls back to CPU otherwise so the service still starts (slowly) on a
GPU-less node for smoke testing.

Generated media never leaves the node: generation happens on the user's own
hardware and the result (PNG or MP4) is returned (base64) to the local Go
worker, which relays it back over the VPN mesh to the user's own AceTeam org.

AceTeam-side follow-up (a DIFFERENT repo, NOT done here -- mirrors how the
Bonsai engine's fabric catalog entry was handled as a separate follow-up, see
this repo's CLAUDE.md "Bonsai service" section): to make Wan2.1 deployable
from `/fabric/models`, add a ``wan2.1-t2v`` entry (``gpu_type=rtx3090``) to
aceteam's ``data/model_catalog.json`` / `fabric_catalog_models`. The
`generate_video` MCP tool + Generate node UI are tracked separately as
aceteam#8633.
"""

import base64
import io
import os
import tempfile
import threading

from fastapi import FastAPI, HTTPException
from pydantic import BaseModel, Field

import model_preflight

# Model repo id to serve. Defaults to SDXL-Turbo: small (~7GB), fast (1-4 steps),
# and a good default for a first render. Override for SD 3.5 Medium, SDXL, etc.:
#   DIFFUSERS_MODEL=stabilityai/stable-diffusion-3.5-medium
DIFFUSERS_MODEL = os.environ.get("DIFFUSERS_MODEL", "stabilityai/sdxl-turbo")

# Torch dtype for the pipeline. fp16 halves VRAM and is the norm for GPU serving;
# fp32 is used automatically on CPU (fp16 is not supported on most CPUs).
DIFFUSERS_DTYPE = os.environ.get("DIFFUSERS_DTYPE", "float16")

# Video model repo id to serve (citadel #958). Wan2.1's 1.3B text-to-video
# model: small enough to run on a consumer GPU (RTX 3090) alongside the image
# model above, unlike the 14B variant. A SEPARATE env var from DIFFUSERS_MODEL
# -- the two pipelines are independent lazy singletons (see _get_video_pipe),
# so an operator can override one without touching the other.
DIFFUSERS_VIDEO_MODEL = os.environ.get("DIFFUSERS_VIDEO_MODEL", "Wan-AI/Wan2.1-T2V-1.3B-Diffusers")

# Torch dtype for the video pipeline's transformer/text-encoder. bf16 (not
# fp16) is the default in diffusers' own WanPipeline documentation -- video
# diffusion transformers are more numerically sensitive than SDXL, and bf16's
# wider exponent range avoids the overflow/NaN failure mode fp16 is prone to
# here. The VAE is always loaded at float32 regardless of this setting (see
# _get_video_pipe) -- diffusers' own docs note this materially improves
# decode quality and the VAE is small relative to the transformer.
DIFFUSERS_VIDEO_DTYPE = os.environ.get("DIFFUSERS_VIDEO_DTYPE", "bfloat16")

# Server port. 7860 is the diffusers contract port (aceteam #4468). Matches the
# EXPOSE/CMD in the Dockerfile; the node compose file owns the host mapping.
PORT = int(os.environ.get("PORT", "7860"))

# Where model weights land -- HF_HOME (Dockerfile default:
# /root/.cache/huggingface, bind-mounted from the node's own
# ~/citadel-cache/huggingface) is also what the free-space preflight checks
# (citadel #902). Falls back to huggingface_hub's own default cache location
# so the preflight still resolves a real path if HF_HOME is ever unset.
DIFFUSERS_CACHE_DIR = os.environ.get("HF_HOME") or os.path.expanduser("~/.cache/huggingface")

app = FastAPI(title="citadel-diffusers-service")

# The pipeline is expensive to load (weights download + GPU upload), so we build
# it lazily on the first generation request and cache it. This lets /health
# answer immediately while a large model is still downloading, so container
# healthchecks pass during model pull (same lazy pattern as whisper's _get_model).
_pipe = None
_pipe_lock = threading.Lock()
_device = None


def _resolve_device_and_dtype():
    """Pick cuda+fp16 when a GPU is visible, else cpu+fp32."""
    import torch

    if torch.cuda.is_available():
        dtype = torch.float16 if DIFFUSERS_DTYPE == "float16" else torch.float32
        return "cuda", dtype
    # fp16 math is unsupported on most CPUs; force fp32 there.
    return "cpu", torch.float32


def _get_pipe():
    """Lazy-load the diffusers text-to-image pipeline once, thread-safely."""
    global _pipe, _device
    if _pipe is not None:
        return _pipe
    with _pipe_lock:
        if _pipe is not None:  # re-check inside the lock
            return _pipe
        from diffusers import AutoPipelineForText2Image

        device, dtype = _resolve_device_and_dtype()

        # citadel #902: free-space preflight + file-selection guard, mirroring
        # #840's Go-side MODEL_CACHE_PULL fix -- MODEL_CACHE_PULL has been a
        # no-op for engine:diffusers since #545, so this call site is the only
        # place that fix's protections can actually reach the LTX-Video-shaped
        # (multi-checkpoint, ~161GB) disk-fill incident. allow_patterns/
        # ignore_patterns are optional and additive: unset (the default), the
        # actual pull below behaves exactly as before -- from_pretrained
        # fetches its own unfiltered model_index.json-derived subset.
        #
        # run_preflight fails OPEN (never blocks) on a metadata/disk-probe
        # error, and its fail-CLOSED behavior (raises, propagated to
        # /generate's caller) is now narrower than "any confirmed shortfall"
        # (citadel #913): it only refuses when allow_patterns/ignore_patterns
        # is actually set, because only then is its size estimate a reliable
        # prediction of what from_pretrained will download. On the default,
        # unfiltered path it estimates the FULL repo tree, which
        # from_pretrained's own component-subset download rarely matches --
        # so a confirmed shortfall there is only logged as a warning and
        # never blocks the load. See run_preflight's docstring for the full
        # two-branch contract.
        allow_patterns, ignore_patterns = model_preflight.resolve_allow_ignore_patterns()
        hf_token = model_preflight.hf_auth_token()
        model_preflight.run_preflight(
            DIFFUSERS_MODEL,
            DIFFUSERS_CACHE_DIR,
            allow_patterns=allow_patterns,
            ignore_patterns=ignore_patterns,
            token=hf_token,
        )

        # NOTE: allow_patterns/ignore_patterns are deliberately NOT passed to
        # from_pretrained below -- verified against the pinned diffusers
        # library that from_pretrained's own download() ignores those kwargs
        # (it computes its own allow/ignore list from model_index.json) and
        # silently drops anything it doesn't recognize. When the operator has
        # set either pattern, pre-populate the cache ourselves via a direct
        # huggingface_hub.snapshot_download call so the filter actually
        # applies to what lands on disk; see model_preflight.
        # prefetch_filtered_weights's docstring for the full reasoning.
        #
        # NOTE: no cache_dir is passed here either (deliberately -- see
        # default_snapshot_download's docstring). from_pretrained below also
        # never passes cache_dir, so both resolve to huggingface_hub's own
        # default (HF_HUB_CACHE = HF_HOME/hub) identically. Passing
        # DIFFUSERS_CACHE_DIR (== HF_HOME, missing the /hub suffix) here was a
        # confirmed second bug in an earlier version of this fix: it wrote the
        # filtered prefetch to a directory from_pretrained never reads.
        model_preflight.prefetch_filtered_weights(
            DIFFUSERS_MODEL,
            allow_patterns=allow_patterns,
            ignore_patterns=ignore_patterns,
            token=hf_token,
        )

        pipe = AutoPipelineForText2Image.from_pretrained(
            DIFFUSERS_MODEL,
            torch_dtype=dtype,
        )
        pipe = pipe.to(device)
        _device = device
        _pipe = pipe
        return _pipe


# Separate lazy singleton for the video pipeline (citadel #958) -- its own
# globals/lock, entirely independent of _pipe/_pipe_lock/_device above. This
# is what makes /generate and /generate/video coexist: loading the video
# model never touches, evicts, or blocks on the image model's pipeline, and
# vice versa (each pays its own one-time load cost, on first use, exactly
# like the image path already does).
_video_pipe = None
_video_pipe_lock = threading.Lock()
_video_device = None


def _resolve_video_device_and_dtype():
    """Pick cuda+bf16 when a GPU is visible, else cpu+fp32. Mirrors
    _resolve_device_and_dtype() above, but the GPU dtype is bf16 by default
    (see DIFFUSERS_VIDEO_DTYPE's module-level comment for why)."""
    import torch

    if torch.cuda.is_available():
        dtype = {
            "bfloat16": torch.bfloat16,
            "float16": torch.float16,
            "float32": torch.float32,
        }.get(DIFFUSERS_VIDEO_DTYPE, torch.bfloat16)
        return "cuda", dtype
    # fp16/bf16 math is unreliable-to-unsupported on most CPUs; force fp32.
    return "cpu", torch.float32


def _get_video_pipe():
    """Lazy-load the diffusers text-to-video (WanPipeline) pipeline once,
    thread-safely. Mirrors _get_pipe() above -- same double-checked-locking
    shape and the identical citadel #902 preflight protections, just against
    DIFFUSERS_VIDEO_MODEL instead of DIFFUSERS_MODEL and a video-specific
    allow/ignore-pattern env var pair, since the two repos have unrelated
    file layouts and sharing one pattern pair would silently misfilter
    whichever model didn't set it."""
    global _video_pipe, _video_device
    if _video_pipe is not None:
        return _video_pipe
    with _video_pipe_lock:
        if _video_pipe is not None:  # re-check inside the lock
            return _video_pipe
        import torch
        from diffusers import AutoencoderKLWan, WanPipeline

        device, dtype = _resolve_video_device_and_dtype()

        # citadel #902/#958: identical free-space preflight + file-selection
        # guard as the image path above (see its comments for the full
        # fail-open/fail-closed contract), parameterized for the video model
        # via its own env vars so an operator-declared filter for one model
        # never applies to the other.
        allow_patterns, ignore_patterns = model_preflight.resolve_allow_ignore_patterns(
            allow_env="DIFFUSERS_VIDEO_ALLOW_PATTERNS",
            ignore_env="DIFFUSERS_VIDEO_IGNORE_PATTERNS",
        )
        hf_token = model_preflight.hf_auth_token()
        model_preflight.run_preflight(
            DIFFUSERS_VIDEO_MODEL,
            DIFFUSERS_CACHE_DIR,
            allow_patterns=allow_patterns,
            ignore_patterns=ignore_patterns,
            token=hf_token,
        )
        model_preflight.prefetch_filtered_weights(
            DIFFUSERS_VIDEO_MODEL,
            allow_patterns=allow_patterns,
            ignore_patterns=ignore_patterns,
            token=hf_token,
        )

        # VAE loaded separately at float32 regardless of DIFFUSERS_VIDEO_DTYPE
        # -- see DIFFUSERS_VIDEO_DTYPE's module-level comment. Both calls
        # resolve to the SAME cache (no cache_dir passed, same reasoning as
        # the image path's from_pretrained call above) so the preflight/
        # prefetch above governs both downloads.
        vae = AutoencoderKLWan.from_pretrained(
            DIFFUSERS_VIDEO_MODEL,
            subfolder="vae",
            torch_dtype=torch.float32,
        )
        pipe = WanPipeline.from_pretrained(
            DIFFUSERS_VIDEO_MODEL,
            vae=vae,
            torch_dtype=dtype,
        )
        pipe = pipe.to(device)
        _video_device = device
        _video_pipe = pipe
        return _video_pipe


def _export_video_bytes(frames, fps: int) -> bytes:
    """Renders a WanPipeline result's ``frames[0]`` to an MP4 and returns the
    raw bytes. Isolated into its own function (rather than inlined in the
    /generate/video handler below) purely as a test seam: it is the one call
    in the video path that needs a real diffusers+imageio install
    (``diffusers.utils.export_to_video``, which shells out to ffmpeg via
    imageio -- see requirements.txt), so unit tests can monkeypatch this one
    function instead of installing the full GPU/diffusers/imageio stack just
    to exercise the endpoint's request/response contract (see test_app.py).

    export_to_video needs a real file path (it writes an MP4 container via
    ffmpeg, not an in-memory buffer), so this renders to a temp file and
    reads the bytes back -- the video-path analogue of /generate's in-memory
    ``image.save(buf, format="PNG")``, just with an extra encode-to-disk hop
    the MP4 container format requires.
    """
    from diffusers.utils import export_to_video

    tmp = tempfile.NamedTemporaryFile(suffix=".mp4", delete=False)
    tmp_path = tmp.name
    tmp.close()
    try:
        export_to_video(frames, tmp_path, fps=fps)
        with open(tmp_path, "rb") as f:
            return f.read()
    finally:
        try:
            os.remove(tmp_path)
        except OSError:
            pass


class GenerateRequest(BaseModel):
    prompt: str = Field(..., description="Text prompt to render.")
    # Optional negative prompt (ignored by turbo/distilled models but accepted
    # so callers can send a uniform request shape).
    negative_prompt: str | None = None
    # SDXL-Turbo renders in 1-4 steps; larger models want ~20-50. Default keeps
    # the turbo default fast; callers override for quality.
    num_inference_steps: int = Field(default=4, ge=1, le=150)
    # Turbo models are trained for guidance_scale=0.0; standard SD uses ~7.5.
    guidance_scale: float = Field(default=0.0, ge=0.0, le=30.0)
    width: int = Field(default=512, ge=64, le=2048)
    height: int = Field(default=512, ge=64, le=2048)
    # Optional seed for reproducible output.
    seed: int | None = None


class GenerateVideoRequest(BaseModel):
    prompt: str = Field(..., description="Text prompt describing the video to render.")
    negative_prompt: str | None = None
    # WanPipeline's own documented default (diffusers WanPipeline.__call__)
    # is 50 steps -- unlike sdxl-turbo, Wan2.1 is not a distilled few-step
    # model, so this default is meaningfully slower than /generate's.
    num_inference_steps: int = Field(default=50, ge=1, le=150)
    # WanPipeline's documented default; standard classifier-free-guidance
    # range for this model family.
    guidance_scale: float = Field(default=5.0, ge=0.0, le=30.0)
    # 480x832 is WanPipeline's documented default and the resolution the 1.3B
    # model (this service's default DIFFUSERS_VIDEO_MODEL) is tuned for; 720p
    # is the 14B-model recommendation and not a good fit for the smaller model
    # this service ships by default. Bounds are generous enough for an
    # operator running a bigger DIFFUSERS_VIDEO_MODEL override.
    height: int = Field(default=480, ge=64, le=1280)
    width: int = Field(default=832, ge=64, le=1280)
    # 81 frames is WanPipeline's documented default (~5s at the also-default
    # fps=16 below). Upper bound is a generous ceiling on generation time/
    # VRAM, not a WanPipeline limit.
    num_frames: int = Field(default=81, ge=1, le=161)
    # Frame rate used only for the exported MP4 container (export_to_video's
    # own default is 10; WanPipeline's documented examples use 16 -- matched
    # here so the default output plays back at the intended speed).
    fps: int = Field(default=16, ge=1, le=60)
    # Optional seed for reproducible output.
    seed: int | None = None


@app.get("/health")
def health():
    """Liveness/readiness probe.

    Returns immediately (does NOT force either model to load) so container
    healthchecks pass while a large model is still downloading. ``model_loaded``
    /``video_model_loaded`` tell callers whether the first (cold) call to
    ``/generate``/``/generate/video`` will pay that model's load cost --
    independently, since the two pipelines are separate lazy singletons (see
    ``_get_pipe``/``_get_video_pipe``).
    """
    return {
        "status": "ok",
        "model": DIFFUSERS_MODEL,
        "model_loaded": _pipe is not None,
        "device": _device,
        "video_model": DIFFUSERS_VIDEO_MODEL,
        "video_model_loaded": _video_pipe is not None,
        "video_device": _video_device,
    }


@app.post("/generate")
def generate(req: GenerateRequest):
    """Render a single image from a text prompt and return it as base64 PNG."""
    try:
        pipe = _get_pipe()
    except Exception as exc:  # model load failure (OOM, bad repo id, auth)
        raise HTTPException(500, f"failed to load model {DIFFUSERS_MODEL}: {exc}")

    generator = None
    if req.seed is not None:
        import torch

        generator = torch.Generator(device=_device).manual_seed(req.seed)

    try:
        result = pipe(
            prompt=req.prompt,
            negative_prompt=req.negative_prompt,
            num_inference_steps=req.num_inference_steps,
            guidance_scale=req.guidance_scale,
            width=req.width,
            height=req.height,
            generator=generator,
        )
    except Exception as exc:  # inference failure (OOM at generate time, etc.)
        raise HTTPException(500, f"generation failed: {exc}")

    image = result.images[0]
    buf = io.BytesIO()
    image.save(buf, format="PNG")
    image_b64 = base64.b64encode(buf.getvalue()).decode("ascii")

    return {
        "model": DIFFUSERS_MODEL,
        "device": _device,
        "width": req.width,
        "height": req.height,
        "seed": req.seed,
        # PNG bytes, base64-encoded. The Go worker relays this back to the org.
        "image_base64": image_b64,
        "content_type": "image/png",
    }


@app.post("/generate/video")
def generate_video(req: GenerateVideoRequest):
    """Render a short video clip from a text prompt and return it as base64
    MP4 (citadel #958).

    Coexists with /generate above: this calls the SEPARATE _get_video_pipe
    lazy singleton (its own model/globals/lock), so this endpoint never
    loads, evicts, or otherwise disturbs the image pipeline, and vice versa.
    """
    try:
        pipe = _get_video_pipe()
    except Exception as exc:  # model load failure (OOM, bad repo id, auth)
        raise HTTPException(500, f"failed to load model {DIFFUSERS_VIDEO_MODEL}: {exc}")

    generator = None
    if req.seed is not None:
        import torch

        generator = torch.Generator(device=_video_device).manual_seed(req.seed)

    try:
        result = pipe(
            prompt=req.prompt,
            negative_prompt=req.negative_prompt,
            height=req.height,
            width=req.width,
            num_frames=req.num_frames,
            num_inference_steps=req.num_inference_steps,
            guidance_scale=req.guidance_scale,
            generator=generator,
        )
    except Exception as exc:  # inference failure (OOM at generate time, etc.)
        raise HTTPException(500, f"video generation failed: {exc}")

    frames = result.frames[0]

    try:
        video_bytes = _export_video_bytes(frames, req.fps)
    except Exception as exc:  # ffmpeg missing, encode failure, etc.
        raise HTTPException(500, f"video export failed: {exc}")

    video_b64 = base64.b64encode(video_bytes).decode("ascii")

    return {
        "model": DIFFUSERS_VIDEO_MODEL,
        "device": _video_device,
        "width": req.width,
        "height": req.height,
        "num_frames": req.num_frames,
        "fps": req.fps,
        "seed": req.seed,
        # MP4 bytes, base64-encoded. The Go worker relays this back to the org.
        "video_base64": video_b64,
        "content_type": "video/mp4",
    }
