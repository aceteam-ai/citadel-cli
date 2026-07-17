# services/compose/bonsai/Dockerfile
#
# Builds an OpenAI-compatible llama-server for PrismML's Bonsai-27B (1-bit
# quantized Qwen3.6-27B) on CUDA. Stock llama.cpp does NOT support the
# Q1_0_g128 hybrid-attention kernels the Bonsai-27B-Q1_0.gguf needs, so we build
# the PrismML fork (https://github.com/PrismML-Eng/llama.cpp), whose default
# branch is `prism`. Verified against the model card + fork on 2026-07: no
# release tag is published for Bonsai support, so we pin to the branch (override
# LLAMACPP_REF with a commit for a reproducible build once one is cut).
#
# This Dockerfile is self-contained: it clones the fork itself and COPYs nothing
# from the build context, so the context can be an otherwise-empty directory.
# citadel materializes it to <config>/services/bonsai/Dockerfile alongside
# bonsai.yml (services.WriteAuxFiles) so the compose `build.context: ./bonsai`
# resolves on the node.

ARG CUDA_VERSION=12.4.0
FROM nvidia/cuda:${CUDA_VERSION}-devel-ubuntu22.04

ARG LLAMACPP_REPO=https://github.com/PrismML-Eng/llama.cpp
ARG LLAMACPP_REF=prism
# Target GPU compute capability. Default 86 = RTX 3090/Ampere (citadel's target
# node). This also cuts build time ~5x vs. compiling every arch. Override to
# build for other GPUs, e.g. 89 (Ada/4090) or "86;89".
ARG CUDA_ARCHITECTURES=86

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
        git cmake build-essential ca-certificates \
        libcurl4-openssl-dev \
    && rm -rf /var/lib/apt/lists/*

# The -devel image ships the CUDA Driver API only as a link stub under
# lib64/stubs (libcuda.so). Without it on the link path, the final llama-server
# link fails with "undefined reference to cuGetErrorString / cuMemMap / ...".
# LIBRARY_PATH lets ld resolve -lcuda at build time; the real driver is injected
# by the NVIDIA container runtime at runtime, so the stub is build-only.
ENV LIBRARY_PATH=/usr/local/cuda/lib64/stubs:${LIBRARY_PATH}
RUN ln -sf /usr/local/cuda/lib64/stubs/libcuda.so /usr/local/cuda/lib64/stubs/libcuda.so.1

WORKDIR /opt
RUN git clone --depth 1 --branch "${LLAMACPP_REF}" "${LLAMACPP_REPO}" llama.cpp

WORKDIR /opt/llama.cpp
# -DGGML_CUDA=ON matches the fork's documented build. Build only the server
# target to keep the image + build time down; llama-server lands at
# build/bin/llama-server.
RUN cmake -B build -DGGML_CUDA=ON -DLLAMA_CURL=ON \
        -DCMAKE_CUDA_ARCHITECTURES="${CUDA_ARCHITECTURES}" \
        -DCMAKE_EXE_LINKER_FLAGS="-L/usr/local/cuda/lib64/stubs" \
    && cmake --build build --config Release -j "$(nproc)" --target llama-server \
    && cp build/bin/llama-server /usr/local/bin/llama-server

EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/llama-server"]
