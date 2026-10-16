# Node-local diffusers text-to-image / text-to-video sidecar for Citadel.
# Built and published as ghcr.io/aceteam-ai/diffusers-service (see
# services/compose/diffusers.yml). Launched on a node via SERVICE_START.
#
# Unlike the whisper sidecar (CPU, python:3.12-slim), generation here is a GPU
# workload, so we start from an official PyTorch CUDA runtime image that already
# bundles a CUDA-enabled torch build. We only layer the diffusers + serving deps
# on top. On a GPU-less node the service still starts and falls back to CPU
# (see app.py's _resolve_device_and_dtype / _resolve_video_device_and_dtype),
# just slowly.
# torch 2.7.1 / CUDA 12.6 (was 2.5.1 / 12.4) for the diffusers 0.37.1 stack in
# requirements.txt (citadel-cli#1335). CUDA 12.6 needs NVIDIA driver >= 560 on
# the host; node 1795 runs 580.x. bitsandbytes 0.48.2 (nf4 / 8-bit loads)
# requires torch >= 2.3.
FROM pytorch/pytorch:2.7.1-cuda12.6-cudnn9-runtime

WORKDIR /app

# torch is already provided by the base image; requirements.txt intentionally
# omits it so we don't pull a second (possibly CPU-only) torch wheel on top.
# A C compiler is REQUIRED at runtime on a GPU node: bitsandbytes (imported by
# diffusers) pulls in triton, whose CUDA driver compiles a small C stub the first
# time the driver initializes and fails with "Failed to find C compiler" without
# one. That only happens with a GPU, so a CPU-only import test cannot catch it.
# gcc + libc6-dev is the minimal fix (the -devel base image is several GB larger).
RUN apt-get update \
    && apt-get install -y --no-install-recommends gcc libc6-dev \
    && rm -rf /var/lib/apt/lists/*
ENV CC=gcc

COPY requirements.txt /app/requirements.txt
RUN pip install --no-cache-dir -r requirements.txt

COPY app.py /app/app.py
COPY image_models.py /app/image_models.py
COPY image_swap.py /app/image_swap.py
COPY model_preflight.py /app/model_preflight.py

# 7860 is the diffusers contract port (aceteam #4468). The node compose file
# maps a host port to this; see services/compose/diffusers.yml.
ENV PORT=7860
# Default image model, used when a request omits `model`; override at deploy
# time with DIFFUSERS_MODEL. Requests may name any model on the allowlist
# (DIFFUSERS_ALLOWED_MODELS, default: the verified catalog in image_models.py).
ENV DIFFUSERS_MODEL=black-forest-labs/FLUX.2-klein-4B
# GB of VRAM the image pipeline may hold on the shared GPU (0 = unlimited). The
# sidecar picks the best load variant (full precision, 8-bit, nf4, CPU offload)
# that fits, and swaps one model at a time.
ENV DIFFUSERS_VRAM_BUDGET_GB=12
# Default text-to-video model (citadel #958); override with DIFFUSERS_VIDEO_MODEL.
ENV DIFFUSERS_VIDEO_MODEL=Wan-AI/Wan2.1-T2V-1.3B-Diffusers
# Cache HF weights under /root/.cache/huggingface, bind-mounted from the node's
# ~/citadel-cache/huggingface (shared with vllm/sglang/whisper).
ENV HF_HOME=/root/.cache/huggingface

EXPOSE 7860

# Exactly ONE uvicorn worker, on purpose (do not add --workers): the image model
# manager in app.py lives in this process, and a single worker is what enforces
# one resident image model on the shared GPU. Generation itself is serialized
# by a lock in app.py (sync endpoints otherwise run concurrently on a threadpool).
CMD ["sh", "-c", "uvicorn app:app --host 0.0.0.0 --port ${PORT}"]
