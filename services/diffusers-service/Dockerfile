# Node-local diffusers text-to-image sidecar for Citadel.
# Built and published as ghcr.io/aceteam-ai/diffusers-service (see
# services/compose/diffusers.yml). Launched on a node via SERVICE_START.
#
# Unlike the whisper sidecar (CPU, python:3.12-slim), text-to-image is a GPU
# workload, so we start from an official PyTorch CUDA runtime image that already
# bundles a CUDA-enabled torch build. We only layer the diffusers + serving deps
# on top. On a GPU-less node the service still starts and falls back to CPU
# (see app.py _resolve_device_and_dtype), just slowly.
FROM pytorch/pytorch:2.5.1-cuda12.4-cudnn9-runtime

WORKDIR /app

# torch is already provided by the base image; requirements.txt intentionally
# omits it so we don't pull a second (possibly CPU-only) torch wheel on top.
COPY requirements.txt /app/requirements.txt
RUN pip install --no-cache-dir -r requirements.txt

COPY app.py /app/app.py

# 7860 is the diffusers contract port (aceteam #4468). The node compose file
# maps a host port to this; see services/compose/diffusers.yml.
ENV PORT=7860
# Default model; override at deploy time with DIFFUSERS_MODEL.
ENV DIFFUSERS_MODEL=stabilityai/sdxl-turbo
# Cache HF weights under /root/.cache/huggingface, bind-mounted from the node's
# ~/citadel-cache/huggingface (shared with vllm/sglang/whisper).
ENV HF_HOME=/root/.cache/huggingface

EXPOSE 7860

CMD ["sh", "-c", "uvicorn app:app --host 0.0.0.0 --port ${PORT}"]
