# Spirula reconstruct worker

This is a one-shot, node-local pipeline for a video or frame directory:

```text
video -> ffmpeg frames -> Spirula SfM -> quality gate -> Spirula 3DGS -> splat.ply
```

It is the engine half of `citadel-cli#1210`. The signed reconstruct lifecycle,
manifest/keyframe retention contract, and platform state machine remain in
`aceteam-ai/aceteam#9819`.

## Execution contract

The container runs to completion and emits one JSON result. A successful result
contains only the node-local artifact path and its SHA256, never PLY bytes:

```json
{
  "status": "success",
  "quality": {
    "registered_images": 120,
    "total_images": 120,
    "registered_percent": 100,
    "mean_reprojection_px": 0.49,
    "accepted_for_training": true
  },
  "artifact": {
    "path": "/data/reconstruct-results/scan-001/splat.ply",
    "sha256": "sha256:..."
  }
}
```

SfM is always invoked with `--sequence . --device 0`. Training is always invoked
with `--device 0 --disable-viewer 1 --keep-viewer-alive 0`; the viewer flags are
load-bearing because the upstream CLI otherwise stays alive serving its viewer.

The wrapper parses Spirula's registered-image and mean-reprojection summaries.
The default go/no-go threshold is at least 50 percent registered and no more
than 2 px mean reprojection error. A rejected capture exits before training.

Every child starts in its own process group. SIGINT, SIGTERM, or lifecycle
context cancellation kills the full ffmpeg/Spirula process tree. Work and
publish directories are private, unpredictable siblings of the requested
output, and are removed on failure. The requested output directory must not
already exist and is atomically renamed into place only after a complete regular
PLY has been hashed. A partial artifact is therefore never returned or exposed
at the requested path.

This workload has no internal deadline. A high-quality, multi-hundred-frame 4K
scan is opaque-long and must run in the reconstruct lifecycle's unbounded tier.
Do not dispatch it through `RUN_JOB_TEMPLATE`, whose exec-1 lane and four-hour
watchdog are intentionally sized for bounded builtins.

## Pinned engine and GPL boundary

The Dockerfile downloads the unmodified Spirula Studio `v2026.9.30` Linux Vulkan
release and verifies the upstream GitHub asset digest
`sha256:123d6d0b826388abb64129b6fcf2a8d34fe0662a57ba34e53212a148c891431d`.
The exact corresponding source commit, GPL text, and attribution are recorded in
`THIRD_PARTY_NOTICES.md` and copied into the image. Citadel invokes the separate
binary as a subprocess; it does not vendor or link Spirula source.

## Build and first-image Vulkan verification

Build from the repository root because the image also compiles the Go wrapper:

```sh
docker build --platform linux/amd64 \
  -f services/spirula-reconstruct/Dockerfile \
  -t spirula-reconstruct:2026.9.30 .
```

On the first image build, verify all three NVIDIA Vulkan requirements on a GPU
node. A CUDA `compute` capability alone is not sufficient:

```sh
docker run --rm --gpus all \
  --entrypoint /bin/sh \
  -e NVIDIA_DRIVER_CAPABILITIES=graphics,display,utility \
  spirula-reconstruct:2026.9.30 -ec '
    test -r /etc/vulkan/icd.d/nvidia_icd.json
    ldconfig -p | grep -F libGLX_nvidia
    vulkaninfo --summary
    spirula --help
  '
```

The check must show the physical NVIDIA adapter as Vulkan device 0. If
`nvidia_icd.json` or `libGLX_nvidia` is absent, fix the host NVIDIA Container
Toolkit/capability configuration before running a reconstruction. Do not accept
llvmpipe as a fallback.

## Manual RTX 3090 acceptance

Set a host input path and a writable output parent. The job ID must name a path
which does not exist yet:

```sh
export RECONSTRUCT_INPUT=/data/orbit.mp4
export RECONSTRUCT_OUTPUT=/data/reconstruct-results
export RECONSTRUCT_JOB_ID=scan-001
export PUID="$(id -u)"
export PGID="$(id -g)"

docker compose -f services/spirula-reconstruct/compose.yml run --rm reconstruct
```

For a quick repeat of the validated 3090 smoke settings, add:

```sh
export TRAIN_QUALITY=low CAP_MAX=300000 ITERATIONS=7000 TRAIN_RESOLUTION_DIVISOR=2
```

Acceptance checks:

1. The JSON quality signal reports registered percent and mean reprojection.
2. Training begins only when `accepted_for_training` is true.
3. The result references `splat.ply` by path and `sha256:` digest only.
4. `sha256sum` of that PLY matches the JSON digest.
5. During a second run, stop the container while SfM or training is active.
   No `spirula`/`ffmpeg` GPU process and no requested output path may remain.

## Hermetic tests

No GPU, container runtime, ffmpeg, or Spirula binary is used by unit tests:

```sh
go test ./internal/reconstruct ./cmd/spirula-reconstruct
```

The fake executor proves argv, video/frame branching, the pre-training quality
gate, reference-only publication, hashing, and cancellation cleanup. A Unix
unit assertion separately pins dedicated process-group cancellation.
