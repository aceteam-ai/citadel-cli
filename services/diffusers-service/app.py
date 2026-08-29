"""Node-local text-to-image sidecar for Citadel.

A small FastAPI service that wraps the HuggingFace ``diffusers`` library and
serves text-to-image generation on the node's GPU. It mirrors the proven
node-local sidecar pattern already shipped in Citadel (see
services/whisper-service/app.py): load a model once, expose a ``/health`` check
and a single inference endpoint, and keep the heavy Python/ML dependency inside
a Docker container reachable over localhost.

Built and published as ``ghcr.io/aceteam-ai/diffusers-service`` and launched on
a node via ``SERVICE_START "diffusers"`` (see services/compose/diffusers.yml).

Contract (aceteam #4468, `diffusers-text-to-image` provisioning template):
  - engine/service name: ``diffusers``
  - listens on port 7860 (the container/server port; the node compose file may
    map a different host port to avoid colliding with the terminal server)
  - ``GET /health`` liveness/readiness probe

The model is selected via the ``DIFFUSERS_MODEL`` env var (same knob as sglang's
``SGLANG_COMMAND`` / whisper's ``WHISPER_MODEL``), defaulting to a small, fast
SDXL-Turbo model. GPU is used when available; falls back to CPU otherwise so the
service still starts (slowly) on a GPU-less node for smoke testing.

Images never leave the node: generation happens on the user's own hardware and
the resulting PNG is returned (base64) to the local Go worker, which relays it
back over the VPN mesh to the user's own AceTeam org.
"""

import base64
import io
import os
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


@app.get("/health")
def health():
    """Liveness/readiness probe.

    Returns immediately (does NOT force the model to load) so container
    healthchecks pass while a large model is still downloading. ``model_loaded``
    tells callers whether the first (cold) generation will pay the load cost.
    """
    return {
        "status": "ok",
        "model": DIFFUSERS_MODEL,
        "model_loaded": _pipe is not None,
        "device": _device,
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
