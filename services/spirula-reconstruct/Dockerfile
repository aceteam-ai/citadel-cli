# syntax=docker/dockerfile:1

# Build the small, stdlib-only orchestration binary separately. Spirula remains
# an unmodified upstream program invoked as a subprocess (GPL mere aggregation).
FROM golang:1.26.6-bookworm AS runner-build
WORKDIR /src
COPY go.mod go.sum ./
COPY internal/reconstruct ./internal/reconstruct
COPY cmd/spirula-reconstruct ./cmd/spirula-reconstruct
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -buildvcs=false -trimpath -ldflags='-s -w' \
    -o /out/spirula-reconstruct ./cmd/spirula-reconstruct

FROM debian:bookworm-slim AS upstream
ARG SPIRULA_VERSION=2026.9.30
ARG SPIRULA_COMMIT=1943edaf83abf0d00b9ca2d2023424ba1e831d6a
ARG SPIRULA_ARCHIVE_SHA256=123d6d0b826388abb64129b6fcf2a8d34fe0662a57ba34e53212a148c891431d
ARG SPIRULA_LICENSE_SHA256=3972dc9744f6499f0f9b2dbf76696f2ae7ad8af9b23dde66d6af86c9dfb36986
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl unzip \
    && rm -rf /var/lib/apt/lists/* \
    && curl -fL --retry 3 \
       "https://github.com/harry7557558/spirula-studio/releases/download/v${SPIRULA_VERSION}/spirula-${SPIRULA_VERSION}-ubuntu-vulkan-x86_64.zip" \
       -o /tmp/spirula.zip \
    && echo "${SPIRULA_ARCHIVE_SHA256}  /tmp/spirula.zip" | sha256sum -c - \
    && unzip -p /tmp/spirula.zip spirula > /usr/local/bin/spirula \
    && chmod 0755 /usr/local/bin/spirula \
    && mkdir -p /usr/share/licenses/spirula-studio \
    && curl -fL --retry 3 \
       "https://raw.githubusercontent.com/harry7557558/spirula-studio/${SPIRULA_COMMIT}/LICENSE" \
       -o /usr/share/licenses/spirula-studio/LICENSE \
    && echo "${SPIRULA_LICENSE_SHA256}  /usr/share/licenses/spirula-studio/LICENSE" | sha256sum -c - \
    && rm -f /tmp/spirula.zip

FROM ubuntu:24.04

# The NVIDIA Container Toolkit injects nvidia_icd.json and libGLX_nvidia only
# when the graphics/display capabilities are requested. Vulkan is not covered
# by a compute-only CUDA capability set.
ENV NVIDIA_VISIBLE_DEVICES=all \
    NVIDIA_DRIVER_CAPABILITIES=graphics,display,utility \
    SS_LANG=en

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
       ca-certificates ffmpeg libegl1 libgomp1 libopengl0 libvulkan1 vulkan-tools \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /input /output \
       /usr/share/licenses/citadel-cli /usr/share/licenses/spirula-studio

COPY --from=runner-build /out/spirula-reconstruct /usr/local/bin/spirula-reconstruct
COPY --from=upstream /usr/local/bin/spirula /usr/local/bin/spirula
COPY --from=upstream /usr/share/licenses/spirula-studio/LICENSE /usr/share/licenses/spirula-studio/LICENSE
COPY LICENSE /usr/share/licenses/citadel-cli/LICENSE
COPY services/spirula-reconstruct/THIRD_PARTY_NOTICES.md /usr/share/doc/spirula-reconstruct/THIRD_PARTY_NOTICES.md

LABEL org.opencontainers.image.title="AceTeam Spirula reconstruct worker" \
      org.opencontainers.image.description="One-shot node-local frames-to-3DGS pipeline" \
      org.opencontainers.image.source="https://github.com/aceteam-ai/citadel-cli" \
      org.opencontainers.image.licenses="Elastic-2.0 AND GPL-3.0-only" \
      org.opencontainers.image.vendor="AceTeam.ai" \
      ai.aceteam.spirula.version="2026.9.30" \
      ai.aceteam.spirula.source="https://github.com/harry7557558/spirula-studio/tree/1943edaf83abf0d00b9ca2d2023424ba1e831d6a"

USER 65532:65532
ENTRYPOINT ["/usr/local/bin/spirula-reconstruct"]
