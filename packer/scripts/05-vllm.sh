#!/bin/bash
# 05-vllm.sh - Pre-pull the vLLM Docker image
#
# Pulling at build time saves 5-10 minutes on first VM boot. The image is
# large (~8 GB) so this is one of the slower provisioning steps.
set -euo pipefail

echo "==> Pre-pulling vLLM Docker image..."

VLLM_IMAGE="vllm/vllm-openai:latest"

# Docker should already be running from 03-docker.sh
if ! docker info >/dev/null 2>&1; then
    echo "ERROR: Docker is not running. Cannot pull images."
    exit 1
fi

echo "==> Pulling ${VLLM_IMAGE} (this may take a while)..."
docker pull "${VLLM_IMAGE}"

echo "==> Image pulled:"
docker images "${VLLM_IMAGE}" --format "table {{.Repository}}\t{{.Tag}}\t{{.Size}}"

echo "==> vLLM image pre-pull complete."
