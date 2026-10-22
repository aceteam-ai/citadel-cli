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

Image models (citadel-cli#1335): a request may name any ALLOWLISTED model in
its optional ``model`` field (``DIFFUSERS_ALLOWED_MODELS``, see
image_models.py for the verified catalog and licences). Omitting it selects
``DIFFUSERS_MODEL`` (default black-forest-labs/FLUX.2-klein-4B). The sidecar keeps ONE image pipeline
on the GPU at a time: a request for another allowlisted model swaps it in
(image_swap.py), unloading the previous one and checking the card has room
under ``DIFFUSERS_VRAM_BUDGET_GB``. Big models load quantized (nf4 / 8-bit via
bitsandbytes) when their full-precision size would not fit the budget. A cold
load runs in the background and the request gets a retryable 503
``model_warming`` response instead of hanging. The video model is selected
independently via
``DIFFUSERS_VIDEO_MODEL``, defaulting to Wan2.1's 1.3B text-to-video model
(``Wan-AI/Wan2.1-T2V-1.3B-Diffusers``) -- small enough (~4GB transformer) to
fit alongside the image model on the same GPU. The two pipelines have separate
lease/residency managers (see ``_get_pipe``/``_get_video_pipe`` below): loading one
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
import gc
import inspect
import io
import logging
import math
import os
import tempfile
import threading

from fastapi import FastAPI, HTTPException
from fastapi.responses import JSONResponse
from pydantic import BaseModel, Field

import image_models
import model_preflight
from image_swap import (
    ImageModelManager,
    InsufficientVram,
    ModelLoadFailed,
    ModelWarming,
)

logger = logging.getLogger(__name__)

# Default image model, used when a request omits `model`. FLUX.2-klein-4B is
# Apache-2.0 and a 4-step distilled model. (Before citadel-cli#1335 this was
# stabilityai/sdxl-turbo, whose Stability AI Community License is not
# permissive; set DIFFUSERS_MODEL back to it to keep the old behavior.)
# Override for any catalog model, or e.g. stabilityai/stable-diffusion-3.5-medium.
DIFFUSERS_MODEL = os.environ.get("DIFFUSERS_MODEL", "black-forest-labs/FLUX.2-klein-4B")

# Torch dtype for the pipeline. fp16 halves VRAM and is the norm for GPU serving;
# fp32 is used automatically on CPU (fp16 is not supported on most CPUs).
DIFFUSERS_DTYPE = os.environ.get("DIFFUSERS_DTYPE", "float16")

# Video model repo id to serve (citadel #958). Wan2.1's 1.3B text-to-video
# model: small enough to run on a consumer GPU (RTX 3090) alongside the image
# model above, unlike the 14B variant. A SEPARATE env var from DIFFUSERS_MODEL
# -- the two pipelines have independent lease/residency managers,
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

# Image-model allowlist and swap policy (citadel-cli#1335). All optional.
#   DIFFUSERS_ALLOWED_MODELS   comma/space separated catalog ids; unset = all
#                              verified catalog models (image_models.CATALOG).
#   DIFFUSERS_VRAM_BUDGET_GB   max GB of VRAM the image pipeline may hold
#                              (default 12; 0/none = unlimited). Picks the
#                              highest-quality load variant that fits.
#   DIFFUSERS_QUANT            auto (default) | none | int8 | nf4.
#   DIFFUSERS_WARM_WAIT_SECONDS  how long a request waits for a cold load
#                              before getting a retryable warming response.
#   DIFFUSERS_MIN_RESIDENCY_SECONDS / DIFFUSERS_FAILURE_BACKOFF_SECONDS /
#   DIFFUSERS_MIN_FREE_DISK_GB  see below.
#   DIFFUSERS_DEFAULT_VRAM_GB  VRAM estimate for a custom DIFFUSERS_MODEL that
#                              is not in the catalog (default 8).
DIFFUSERS_ALLOWED_MODELS = os.environ.get("DIFFUSERS_ALLOWED_MODELS")
DIFFUSERS_VRAM_BUDGET_GB = image_models.parse_budget_gb(os.environ.get("DIFFUSERS_VRAM_BUDGET_GB"))
DIFFUSERS_QUANT = image_models.env_quant_mode()


def _env_float(name: str, default: float) -> float:
    try:
        value = float(os.environ.get(name, default))
        return value if math.isfinite(value) else default
    except ValueError:
        return default


DIFFUSERS_WARM_WAIT_SECONDS = _env_float("DIFFUSERS_WARM_WAIT_SECONDS", 15.0)
# A freshly loaded model is kept at least this long unless its requester has
# used it; and a failed load is not retried for this long (both seconds).
DIFFUSERS_MIN_RESIDENCY_SECONDS = _env_float("DIFFUSERS_MIN_RESIDENCY_SECONDS", 120.0)
DIFFUSERS_FAILURE_BACKOFF_SECONDS = _env_float("DIFFUSERS_FAILURE_BACKOFF_SECONDS", 30.0)


def _idle_unload_seconds():
    try:
        value = float(os.environ.get("DIFFUSERS_IDLE_UNLOAD_SECONDS", 300.0))
    except ValueError:
        logger.warning("DIFFUSERS_IDLE_UNLOAD_SECONDS is invalid; using the default idle timeout")
        return 300.0
    if not math.isfinite(value):
        logger.warning("DIFFUSERS_IDLE_UNLOAD_SECONDS is non-finite; using the default idle timeout")
        return 300.0
    if value < 0:
        logger.warning("DIFFUSERS_IDLE_UNLOAD_SECONDS is negative; using immediate idle eviction")
        return 0.0
    return value


DIFFUSERS_IDLE_UNLOAD_SECONDS = _idle_unload_seconds()
DIFFUSERS_PINNED_MODELS = frozenset(os.environ.get("DIFFUSERS_PINNED_MODELS", "").replace(",", " ").split())
# Free disk that must remain AFTER a model's download (replaces the 2 GiB
# default margin of model_preflight for image models). The node's disk is shared
# with every other service's weights, so leave real headroom.
DIFFUSERS_MIN_FREE_DISK_GB = _env_float("DIFFUSERS_MIN_FREE_DISK_GB", 20.0)
DIFFUSERS_DEFAULT_VRAM_GB = _env_float("DIFFUSERS_DEFAULT_VRAM_GB", 8.0)
ALLOWED_IMAGE_MODELS = image_models.parse_allowlist(
    DIFFUSERS_ALLOWED_MODELS, DIFFUSERS_MODEL, custom_vram_gb=DIFFUSERS_DEFAULT_VRAM_GB
)


def _warn_unknown_pins(pinned_models, allowed_image_models, video_model):
    if pinned_models.difference(allowed_image_models).difference({video_model}):
        logger.warning("DIFFUSERS_PINNED_MODELS contains unknown model IDs; unmatched pins have no effect")


_warn_unknown_pins(DIFFUSERS_PINNED_MODELS, ALLOWED_IMAGE_MODELS, DIFFUSERS_VIDEO_MODEL)

app = FastAPI(title="citadel-diffusers-service")

# The image pipeline is expensive to load (weights download + GPU upload), so it
# is built on demand and cached. /health answers immediately while a large model
# is still downloading (same lazy pattern as whisper's _get_model). Residency
# and swapping live in image_swap.ImageModelManager. _device is the device of
# the pipeline most recently handed to a request (used for the response body and
# the seed generator). The pipeline object itself is deliberately NOT stored in
# a module global: a stray reference would keep an unloaded model's weights, and
# its VRAM, alive across a swap.
_device = None


def _resolve_device_and_dtype(spec=None):
    """Pick cuda + the model's dtype when a GPU is visible, else cpu+fp32."""
    import torch

    if torch.cuda.is_available():
        name = (spec.torch_dtype if spec is not None else None) or DIFFUSERS_DTYPE
        dtype = {
            "bfloat16": torch.bfloat16,
            "float16": torch.float16,
        }.get(name, torch.float32)
        return "cuda", dtype
    # fp16 math is unsupported on most CPUs; force fp32 there.
    return "cpu", torch.float32


def _patterns_for(spec):
    """Allow/ignore patterns for the free-space preflight and the filtered
    prefetch. The legacy DIFFUSERS_ALLOW_PATTERNS / DIFFUSERS_IGNORE_PATTERNS
    env vars keep applying to the configured default model only (a single
    pattern pair misfilters every other repo); every other model uses the
    catalog's own patterns."""
    if spec.model_id == DIFFUSERS_MODEL:
        allow, ignore = model_preflight.resolve_allow_ignore_patterns()
        if allow or ignore:
            return allow, ignore
    return (
        list(spec.allow_patterns) if spec.allow_patterns else None,
        list(spec.ignore_patterns) if spec.ignore_patterns else None,
    )


def _quantization_config(variant, dtype, components):
    """Pipeline-level bitsandbytes config (diffusers PipelineQuantizationConfig)
    for the components the spec says are worth quantizing."""
    from diffusers import PipelineQuantizationConfig

    if variant.quant == image_models.QUANT_NF4:
        return PipelineQuantizationConfig(
            quant_backend="bitsandbytes_4bit",
            quant_kwargs={
                "load_in_4bit": True,
                "bnb_4bit_quant_type": "nf4",
                "bnb_4bit_compute_dtype": dtype,
            },
            components_to_quantize=list(components),
        )
    if variant.quant == image_models.QUANT_INT8:
        return PipelineQuantizationConfig(
            quant_backend="bitsandbytes_8bit",
            quant_kwargs={"load_in_8bit": True},
            components_to_quantize=list(components),
        )
    return None


def _prepare_pipeline(spec, variant):
    """Disk side of a load: the free-space preflight and the weight download.
    Runs on the swap manager's load thread BEFORE the resident model is
    evicted, so a long or failed download never leaves the node without a
    model."""
    # citadel #902: free-space preflight + file-selection guard, mirroring
    # #840's Go-side MODEL_CACHE_PULL fix. Per model (citadel-cli#1335): every
    # catalog entry declares allow patterns, so the size estimate is reliable
    # and a confirmed shortfall fails CLOSED before any download starts (a
    # custom operator-supplied model has no patterns and keeps the old
    # warn-only behavior). Files already in the cache are netted out, and
    # DIFFUSERS_MIN_FREE_DISK_GB must remain free afterwards.
    allow_patterns, ignore_patterns = _patterns_for(spec)
    hf_token = model_preflight.hf_auth_token()
    model_preflight.run_preflight(
        spec.model_id,
        DIFFUSERS_CACHE_DIR,
        allow_patterns=allow_patterns,
        ignore_patterns=ignore_patterns,
        token=hf_token,
        margin_bytes=int(DIFFUSERS_MIN_FREE_DISK_GB * 1024**3),
        fail_closed=spec.allow_patterns is not None,
        is_cached_fn=model_preflight.default_is_cached,
    )
    # from_pretrained ignores allow/ignore kwargs (it derives its own list from
    # model_index.json), so a filtered pull is pre-populated into the shared
    # cache here; see model_preflight.prefetch_filtered_weights. No cache_dir is
    # passed anywhere so both resolve to HF_HUB_CACHE identically.
    model_preflight.prefetch_filtered_weights(
        spec.model_id,
        allow_patterns=allow_patterns,
        ignore_patterns=ignore_patterns,
        token=hf_token,
    )


def _load_pipeline(spec, variant):
    """Builds the pipeline for ``spec`` using ``variant`` from the already
    prefetched cache. Returns (pipe, device). Called on the swap manager's load
    thread, never on a request thread, after the previous model was unloaded."""
    import diffusers

    device, dtype = _resolve_device_and_dtype(spec)
    if device == "cpu" and (variant.quant != image_models.QUANT_NONE or variant.offload != "none"):
        raise RuntimeError(
            f"{spec.model_id} needs a GPU for its {variant.label()} load variant; none is visible"
        )

    pipeline_cls = getattr(diffusers, spec.pipeline_class) if spec.pipeline_class else diffusers.AutoPipelineForText2Image
    kwargs = {"torch_dtype": dtype}
    quant_config = _quantization_config(variant, dtype, spec.quantize_components)
    if quant_config is not None:
        kwargs["quantization_config"] = quant_config

    pipe = pipeline_cls.from_pretrained(spec.model_id, **kwargs)
    if variant.offload == image_models.OFFLOAD_MODEL:
        # Keeps weights in system RAM and moves one component at a time to the
        # GPU, so the resident VRAM is the largest component, not the sum.
        pipe.enable_model_cpu_offload()
    else:
        pipe = pipe.to(device)
    return pipe, device


def _unload_pipeline(pipe):
    """Detach accelerate offload hooks so the weights have no live references."""
    for name in ("remove_all_hooks",):
        fn = getattr(pipe, name, None)
        if callable(fn):
            fn()


def _free_cuda_memory():
    gc.collect()
    try:
        import torch

        if torch.cuda.is_available():
            torch.cuda.empty_cache()
            torch.cuda.ipc_collect()
    except Exception:  # noqa: BLE001 - torch absent on CPU smoke tests
        pass
    _trim_host_memory()


def _trim_host_memory():
    """Return freed glibc arenas to the OS when supported; not a CUDA action."""
    try:
        import ctypes

        trim = ctypes.CDLL(None).malloc_trim
        trim.argtypes = [ctypes.c_size_t]
        trim.restype = ctypes.c_int
        trim(0)
    except (AttributeError, OSError):  # non-glibc platforms have no malloc_trim
        pass


def _host_rss_bytes():
    """Current Linux RSS, not ru_maxrss (which is a historical high-water mark)."""
    try:
        with open("/proc/self/statm", encoding="ascii") as statm:
            return int(statm.read().split()[1]) * os.sysconf("SC_PAGE_SIZE")
    except (OSError, ValueError, IndexError):
        return None


def _cuda_free_bytes():
    """Device-wide free VRAM (all processes), or None when unknown (no GPU)."""
    try:
        import torch

        if torch.cuda.is_available():
            free, _total = torch.cuda.mem_get_info()
            return int(free)
    except Exception:  # noqa: BLE001
        pass
    return None


# One uvicorn worker (see the Dockerfile CMD) is load-bearing: this manager and
# the pipeline it guards live in this process, so a single worker is what
# enforces "one image model on the GPU at a time". It does NOT serialize
# generation: sync endpoints run on Starlette's threadpool, so concurrent
# requests would call the pipeline at once. _gpu_gen_lock below does that.
# Running several workers would each load their own model.
_gpu_gen_lock = threading.Lock()
_manager = ImageModelManager(
    ALLOWED_IMAGE_MODELS,
    DIFFUSERS_MODEL,
    loader=lambda spec, variant: _load_pipeline(spec, variant),
    prepare=lambda spec, variant: _prepare_pipeline(spec, variant),
    min_residency_seconds=DIFFUSERS_MIN_RESIDENCY_SECONDS,
    failure_backoff_seconds=DIFFUSERS_FAILURE_BACKOFF_SECONDS,
    unloader=lambda pipe: _unload_pipeline(pipe),
    free_memory=lambda: _free_cuda_memory(),
    free_vram_bytes=lambda: _cuda_free_bytes(),
    vram_budget_gb=DIFFUSERS_VRAM_BUDGET_GB,
    quant_mode=DIFFUSERS_QUANT,
    warm_wait_seconds=DIFFUSERS_WARM_WAIT_SECONDS,
    idle_unload_seconds=DIFFUSERS_IDLE_UNLOAD_SECONDS,
    pinned_models=DIFFUSERS_PINNED_MODELS,
)

# The lease _get_pipe takes for the in-flight /generate, released in generate()'s
# finally. Thread-local because a FastAPI sync endpoint runs start to finish on
# one worker thread, which lets _get_pipe stay a plain "model id in, pipeline
# out" seam.
_leases = threading.local()


def _get_pipe(model_id=None):
    """Returns the resident pipeline for ``model_id`` (None = the configured
    default), loading or swapping on demand. Raises ModelWarming when the model
    is not resident yet. Takes a lease that _release_pipe() gives back."""
    global _device
    _release_pipe()  # never leak a lease from an earlier call on this thread
    lease = _manager.lease(model_id or DIFFUSERS_MODEL)
    _leases.held = lease
    _device = lease.device
    return lease.pipe


def _release_pipe():
    held = getattr(_leases, "held", None)
    if held is not None:
        _leases.held = None
        held.release()


# Video has its own residency manager and leases. No permanent global retains
# the pipeline after the final request releases it.
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


def _load_video_pipeline(spec, variant):
    """Build on the video manager's background load thread, not a request."""
    import torch
    from diffusers import AutoencoderKLWan, WanPipeline

    device, dtype = _resolve_video_device_and_dtype()
    # Preserve the separate video file-selection and disk preflight contract.
    allow_patterns, ignore_patterns = model_preflight.resolve_allow_ignore_patterns(
        allow_env="DIFFUSERS_VIDEO_ALLOW_PATTERNS",
        ignore_env="DIFFUSERS_VIDEO_IGNORE_PATTERNS",
    )
    hf_token = model_preflight.hf_auth_token()
    model_preflight.run_preflight(
        DIFFUSERS_VIDEO_MODEL, DIFFUSERS_CACHE_DIR,
        allow_patterns=allow_patterns, ignore_patterns=ignore_patterns, token=hf_token,
    )
    model_preflight.prefetch_filtered_weights(
        DIFFUSERS_VIDEO_MODEL,
        allow_patterns=allow_patterns, ignore_patterns=ignore_patterns, token=hf_token,
    )
    # Wan's VAE stays float32 independently of transformer/text encoder dtype.
    vae = AutoencoderKLWan.from_pretrained(
        DIFFUSERS_VIDEO_MODEL, subfolder="vae", torch_dtype=torch.float32,
    )
    pipe = WanPipeline.from_pretrained(DIFFUSERS_VIDEO_MODEL, vae=vae, torch_dtype=dtype)
    return pipe.to(device), device


# This spec is a lifecycle adapter, not a new video VRAM-budget claim. Video
# retains its existing full-precision loading policy and separate preflight.
_video_spec = image_models.ImageModelSpec(
    model_id=DIFFUSERS_VIDEO_MODEL, license="unverified", permissive=False,
    pipeline_class="WanPipeline", torch_dtype=DIFFUSERS_VIDEO_DTYPE,
    variants=(image_models.LoadVariant("none", "none", 0.0),),
)
_video_manager = ImageModelManager(
    {DIFFUSERS_VIDEO_MODEL: _video_spec}, DIFFUSERS_VIDEO_MODEL,
    loader=lambda spec, variant: _load_video_pipeline(spec, variant),
    unloader=lambda pipe: _unload_pipeline(pipe),
    free_memory=lambda: _free_cuda_memory(),
    warm_wait_seconds=DIFFUSERS_WARM_WAIT_SECONDS,
    failure_backoff_seconds=DIFFUSERS_FAILURE_BACKOFF_SECONDS,
    idle_unload_seconds=DIFFUSERS_IDLE_UNLOAD_SECONDS,
    pinned_models=DIFFUSERS_PINNED_MODELS,
)


def _get_video_pipe():
    global _video_device
    _release_video_pipe()
    lease = _video_manager.lease(DIFFUSERS_VIDEO_MODEL)
    _leases.video = lease
    _video_device = lease.device
    return lease.pipe


def _release_video_pipe():
    held = getattr(_leases, "video", None)
    if held is not None:
        _leases.video = None
        held.release()


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
    # Optional allowlisted image model id (citadel-cli#1335). Omitted or blank
    # selects DIFFUSERS_MODEL. Anything not on the allowlist is a 400. GET
    # /health lists the allowed ids.
    model: str | None = None
    # Optional negative prompt (ignored by turbo/distilled models but accepted
    # so callers can send a uniform request shape).
    negative_prompt: str | None = None
    # The numeric fields default to the SELECTED model's own settings (see
    # image_models.ImageModelSpec): SDXL-Turbo renders in 4 steps at 512px with
    # guidance 0.0; Qwen-Image wants ~30 steps at 1024px with true CFG 4.0.
    num_inference_steps: int | None = Field(default=None, ge=1, le=150)
    guidance_scale: float | None = Field(default=None, ge=0.0, le=30.0)
    width: int | None = Field(default=None, ge=64, le=2048)
    height: int | None = Field(default=None, ge=64, le=2048)
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


# Boot-time import self-test (found by GPU acceptance on node 1795: a missing C
# compiler made every pipeline import fail at request time, long after the image
# built). Imports bitsandbytes and every pipeline class the catalog and the video
# path use, in the background so /health still answers immediately, and reports
# the result in /health as self_test ok | error | pending.
_self_test = {"status": "pending", "detail": None}


def _self_test_imports():
    import bitsandbytes  # noqa: F401 - triton/compiler problems surface here
    import diffusers

    names = {"AutoPipelineForText2Image", "WanPipeline", "AutoencoderKLWan", "PipelineQuantizationConfig"}
    names |= {m.pipeline_class for m in ALLOWED_IMAGE_MODELS.values() if m.pipeline_class}
    missing = sorted(n for n in names if getattr(diffusers, n, None) is None)
    if missing:
        raise ImportError(f"diffusers is missing: {', '.join(missing)}")


def _run_self_test(imports=None):
    global _self_test
    try:
        (imports or _self_test_imports)()
    except BaseException as exc:  # noqa: BLE001 - any failure, including SystemExit from a C extension
        _self_test = {"status": "error", "detail": f"{type(exc).__name__}: {exc}"[:1000]}
    else:
        _self_test = {"status": "ok", "detail": None}


@app.on_event("startup")
def _start_self_test():
    threading.Thread(target=_run_self_test, name="diffusers-self-test", daemon=True).start()


@app.get("/health")
def health():
    """Liveness/readiness probe.

    Returns immediately (does NOT force any model to load) so container
    healthchecks pass while a large model is still downloading. ``model`` is the
    configured default image model; ``model_loaded`` is whether ANY allowlisted
    image model is resident. ``image_models`` is the honest advertising surface
    (citadel-cli#1335): every allowlisted model with its state (``warm`` =
    resident now, ``loading``, ``cold`` = loads on demand and the first request
    gets a retryable warming response), licence, planned quantization under the
    VRAM budget, and whether it fits that budget. ``video_model_loaded`` is
    independent: the video pipeline has its own lease/residency manager.
    """
    image = _manager.status()
    video = _video_manager.status()
    residency_fields = ("model", "state", "resident", "pinned", "idle_seconds", "evicts_at")
    return {
        "status": "ok",
        "self_test": _self_test["status"],
        "self_test_detail": _self_test["detail"],
        "model": DIFFUSERS_MODEL,
        "model_loaded": image["loaded_model"] is not None,
        "device": _manager.resident_device(),
        "loaded_model": image["loaded_model"],
        "loading_model": image["loading_model"],
        "allowed_models": [m["model"] for m in image["allowed_models"]],
        "image_models": image["allowed_models"],
        "vram_budget_gb": image["vram_budget_gb"],
        "quant_mode": image["quant_mode"],
        "last_load": image["last_load"],
        "video_model": DIFFUSERS_VIDEO_MODEL,
        "video_model_loaded": video["loaded_model"] is not None,
        "video_device": _video_manager.resident_device(),
        "video_loading_model": video["loading_model"],
        "pipeline_residency": {
            "image": [{key: model[key] for key in residency_fields} for model in image["allowed_models"]],
            "video": [{key: model[key] for key in residency_fields} for model in video["allowed_models"]],
        },
        "pipeline_cleanup": {"image": image["idle_evicting"], "video": video["idle_evicting"]},
        "host_rss_bytes": _host_rss_bytes(),
        "idle_unload_seconds": DIFFUSERS_IDLE_UNLOAD_SECONDS,
    }


def _warming_response(exc: ModelWarming) -> JSONResponse:
    """The retryable cold-load answer: HTTP 503 with a Retry-After header and the
    same top-level fields the node's other model_warming responses carry
    (status / model / eta_seconds / retry_after), so existing callers can parse
    it. Not an error: the model is loading in the background."""
    body = {
        "status": "model_warming",
        "model": exc.model_id,
        "eta_seconds": exc.eta_seconds,
        "retry_after": exc.retry_after,
        "reason": exc.reason,
    }
    if exc.loading:
        body["loading_model"] = exc.loading
    if exc.resident:
        body["resident_model"] = exc.resident
    return JSONResponse(body, status_code=503, headers={"Retry-After": str(exc.retry_after)})


def _filter_call_kwargs(pipe, kwargs: dict) -> dict:
    """Drops kwargs the pipeline's __call__ does not accept (Flux2KleinPipeline
    has no negative_prompt, for example). A pipeline taking **kwargs, or one
    whose signature cannot be read, receives everything."""
    try:
        params = inspect.signature(pipe.__call__).parameters
    except (TypeError, ValueError):
        return kwargs
    if any(p.kind is inspect.Parameter.VAR_KEYWORD for p in params.values()):
        return kwargs
    return {k: v for k, v in kwargs.items() if k in params}


def _build_pipe_kwargs(spec, req: GenerateRequest, generator) -> dict:
    steps = req.num_inference_steps if req.num_inference_steps is not None else spec.default_steps
    guidance = req.guidance_scale if req.guidance_scale is not None else spec.default_guidance
    kwargs = {
        "prompt": req.prompt,
        "num_inference_steps": steps,
        spec.cfg_param: guidance,
        "width": req.width if req.width is not None else spec.default_size,
        "height": req.height if req.height is not None else spec.default_size,
        "generator": generator,
    }
    if spec.supports_negative_prompt:
        negative = req.negative_prompt
        if negative is None and spec.requires_negative_prompt and guidance > 1.0:
            negative = " "  # Qwen-Image only runs true CFG when a negative prompt exists
        kwargs["negative_prompt"] = negative
    return kwargs


@app.post("/generate")
def generate(req: GenerateRequest):
    """Render a single image from a text prompt and return it as base64 PNG.

    ``model`` (optional) picks an allowlisted image model; see GET /health.
    Responses: 200 image; 400 model not allowed; 422 model cannot fit the VRAM
    budget; 503 ``model_warming`` (retryable, honour Retry-After) while the
    model loads or a swap is in progress; 503 ``insufficient_vram`` when other
    services hold the memory the model needs; 500 on load or inference failure.
    """
    try:
        spec = image_models.resolve_model(req.model, ALLOWED_IMAGE_MODELS, DIFFUSERS_MODEL)
    except image_models.ModelNotAllowedError as exc:
        return JSONResponse(
            {
                "error": "model_not_allowed",
                "detail": str(exc),
                "model": exc.requested,
                "allowed_models": exc.allowed,
            },
            status_code=400,
        )

    try:
        # No model named: _get_pipe() with no argument means the configured default.
        pipe = _get_pipe(spec.model_id) if req.model and req.model.strip() else _get_pipe()
    except ModelWarming as exc:
        return _warming_response(exc)
    except image_models.VramBudgetError as exc:
        return JSONResponse(
            {
                "error": "exceeds_vram_budget",
                "detail": str(exc),
                "model": exc.model_id,
                "vram_budget_gb": exc.budget_gb,
                "min_required_gb": exc.min_required_gb,
            },
            status_code=422,
        )
    except InsufficientVram as exc:
        return JSONResponse(
            {
                "status": "insufficient_vram",
                "detail": str(exc),
                "model": exc.model_id,
                "need_gb": round(exc.need_gb, 1),
                "free_gb": round(exc.free_gb, 1),
            },
            status_code=503,
        )
    except Exception as exc:  # model load failure (OOM, bad repo id, auth)
        raise HTTPException(500, f"failed to load model {spec.model_id}: {exc}") from None

    result = None
    try:
        generator = None
        if req.seed is not None:
            import torch

            generator = torch.Generator(device=_device).manual_seed(req.seed)

        call_kwargs = _build_pipe_kwargs(spec, req, generator)
        failure = None
        try:
            with _gpu_gen_lock:  # one generation on the shared GPU at a time
                result = pipe(**_filter_call_kwargs(pipe, call_kwargs))
        except Exception as exc:  # inference failure (OOM at generate time, etc.)
            failure = f"generation failed: {exc}"
        if failure is not None:
            # Raise outside the except block: a retained original traceback
            # can otherwise keep pipeline/component weights alive at eviction.
            raise HTTPException(500, failure)

        image = result.images[0]
        buf = io.BytesIO()
        image.save(buf, format="PNG")
        image_b64 = base64.b64encode(buf.getvalue()).decode("ascii")
    finally:
        # Drop this frame's references to the pipeline before giving the lease
        # back, so a swap that starts the instant the lease is released is not
        # holding the old weights alive through this stack frame.
        del pipe, result
        _release_pipe()

    return {
        "model": spec.model_id,
        "device": _device,
        "width": call_kwargs["width"],
        "height": call_kwargs["height"],
        "seed": req.seed,
        # PNG bytes, base64-encoded. The Go worker relays this back to the org.
        "image_base64": image_b64,
        "content_type": "image/png",
    }


@app.post("/generate/video")
def generate_video(req: GenerateVideoRequest):
    """Render a short video clip from a text prompt and return it as base64
    MP4 (citadel #958).

    Coexists with /generate above: this calls the SEPARATE video manager,
    with its own leases and idle-unload timer, so this endpoint never
    loads, evicts, or otherwise disturbs the image pipeline, and vice versa.
    """
    try:
        pipe = _get_video_pipe()
    except ModelWarming as exc:
        return _warming_response(exc)
    except Exception as exc:  # model load failure (OOM, bad repo id, auth)
        raise HTTPException(500, f"failed to load model {DIFFUSERS_VIDEO_MODEL}: {exc}")

    result = frames = None
    try:
        generator = None
        if req.seed is not None:
            import torch

            generator = torch.Generator(device=_video_device).manual_seed(req.seed)
        failure = None
        try:
            with _gpu_gen_lock:  # shared with /generate
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
        except Exception as exc:
            failure = f"video generation failed: {exc}"
        if failure is not None:
            raise HTTPException(500, failure)

        frames = result.frames[0]
        failure = None
        try:
            video_bytes = _export_video_bytes(frames, req.fps)
        except Exception as exc:
            failure = f"video export failed: {exc}"
        if failure is not None:
            raise HTTPException(500, failure)
        video_b64 = base64.b64encode(video_bytes).decode("ascii")
    finally:
        del pipe, result, frames
        _release_video_pipe()

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
