# Performer depth to point-cloud sequence (#1385)

Status: proposed source design; no registered job, service, deployment or GPU
acceptance. The whole #1385 deliverable remains open, including its worker,
sidecar, cancellation/custody, workspace publication and real clip acceptance.
This contract also needs the #1386 renderer and platform viewer owners' review.

## Phases and boundaries

1. Specify the packed sequence and manifest, implement bounded Go/Python
   reference codecs and synthetic projection/round-trip tests. No model imports.
2. Review job input/output identity and filesystem policy with the existing
   Files/FILE_FETCH/#1314 boundaries. Implement acquisition and create-only
   publication without wiring a live handler ahead of those decisions.
3. Implement the pinned depth/mask adapter and isolated per-job execution;
   connect authenticated/local control, progress, cancellation, heavy GPU lane
   and v2 work-receipt accounting. Source review and hermetic fake tests first.
4. Under separate authority, build/verify the image, promote its real digest,
   and measure a 15-second 1080p clip on a 24 GiB node. Record actual VRAM,
   wall time, artifact sizes and a rendered frame. None is authorized here.

Do not describe phase 1 as a working depth service or claim a native measurement
from an upstream benchmark, reference codec, mock or estimate.

## Model provenance and unresolved depth mapping

Proposed exact depth model: `depth-anything/Video-Depth-Anything-Small`, not
Base/Large. Its [publisher model card](https://huggingface.co/depth-anything/Video-Depth-Anything-Small/blob/main/README.md)
labels it Apache-2.0. The [official project](https://github.com/DepthAnything/Video-Depth-Anything#license)
distinguishes the Small licence from noncommercial Base/Large variants.
Source revision observed on 2026-10-21:
`DepthAnything/Video-Depth-Anything@4f5ae23172ba60fd7bc11ef671cca678842c7072`.
This is source provenance, not a verified checkpoint digest or permission to
download/load it. Model/architecture code, weight digest and dependency pins
must be independently reviewed before startup. No unsafe pickle fallback.

The [upstream runner](https://github.com/DepthAnything/Video-Depth-Anything/blob/4f5ae23172ba60fd7bc11ef671cca678842c7072/run.py)
distinguishes relative and metric modes and only demonstrates point-cloud
back-projection in metric mode. Therefore the Small relative output must NOT
silently be treated as metric distance. A clip-global relative-to-distance
transform, or explicit approval of the distinct metric-Small checkpoint, is a
design gate. Per-frame min/max normalization would destroy temporal scale.
The reference projector accepts already converted positive camera distance in
declared scene units; synthetic distance input proves no model calibration.

Proposed optional mask: `ZhengPeng7/BiRefNet`, whose
[publisher card](https://huggingface.co/ZhengPeng7/BiRefNet/blob/main/README.md)
labels it MIT. It is a foreground/background model, not a semantic person
classifier: the single-performer use case and possible extra foreground objects
must be disclosed. Its suggested remote-code loading path is not adopted.
Architecture source and weight identities need pins and independent review;
metadata licensing does not prove artifact safety. Mask defaults/threshold and
temporal stability need acceptance. `mask=false` remains an explicit option.

## Proposed binary `citadel.pointcloud.v1`

All integers and IEEE-754 binary16 coordinates are little-endian. No alignment
padding, compression, executable content, URLs or trailing bytes are allowed.
The binary has this 32-byte header:

| Offset | Bytes | Meaning |
| --- | --- | --- |
| 0 | 8 | magic `CITPCD1` followed by one zero byte |
| 8 | 2 | version = 1 |
| 10 | 2 | header size = 32 |
| 12 | 4 | frame count, 1 through 1800 |
| 16 | 8 | total point count |
| 24 | 4 | positive FPS numerator |
| 28 | 4 | positive FPS denominator |

FPS is a reduced rational between 1 and 30. Header is followed by
`frame_count + 1` unsigned 64-bit **point-index offsets**: first zero, monotonic,
final equal to total points. A repeated offset represents an empty frame.
Every frame has at most 150000 points. Point data starts immediately after the
offset table. Every point is exactly 9 bytes: x/y/z binary16 (6 bytes), then
red/green/blue uint8 (3 bytes). Frame i uses points `[offset[i], offset[i+1])`.
Offsets are not byte offsets. Total file length must be exactly
`32 + 8*(frame_count+1) + 9*total_points`, at most 2 GiB. Validate arithmetic,
counts and declared length before allocations. Refuse nonfinite coordinates
and values outside binary16's finite range. Never allocate all declared frames
or points based solely on a header.

Coordinates are right-handed: +X image-right, +Y up, -Z forward from the camera.
Projection uses pixel centres and square pixels:
`fx=fy=(width/2)/tan(horizontal_fov/2)`, `cx=width/2`, `cy=height/2`;
`x=(u+0.5-cx)*distance/fx`, `y=-(v+0.5-cy)*distance/fy`, `z=-distance`.
Skip nonpositive/nonfinite distance or masked-out pixels; clamp other distances
to declared near/far planes. If over budget, choose evenly spaced eligible
row-major pixels deterministically, not a random subset or just the first rows.
RGB is copied from the same selected pixel. Bounds must describe the actual
quantized coordinates, not a pre-quantization approximation.

## Proposed JSON manifest

Schema `citadel.pointcloud-manifest.v1`; UTF-8 JSON, at most 1 MiB, finite
numbers only and strict keys. It contains: relative binary path, byte size,
lowercase SHA-256; reduced FPS; frame/point counts and each frame's count;
quantized bounds (null for an all-empty sequence); image dimensions, assumed
intrinsics/FOV, coordinate convention and scene-unit/depth-transform labels;
depth and optional mask model IDs, revisions, weight hashes and licence labels.
Manifest metadata must match the verified binary, not override its counts or
relax bounds. Canonical JSON/manifest self-digest and the paired renderer's
strict field schema need review before publication. No frame pixels/point
arrays or bearer credentials go into Redis/job results.

## Worker/service design gates

Proposed params: workspace-relative immutable input identity; FPS cap default
15/max30, max-seconds default15/max60, point budget default/max150000, FOV
default60 degrees, explicit mask setting and a bounded near/far transform.
These are proposals, not approved runtime defaults. #1314's 16 MiB image
freezer cannot be reused unchanged for video. Input byte limits, hard links,
owner/device policy and a frozen descriptor-to-decoder handoff need review.
No URL, absolute path, symlink escape or shared-pool fallback is acceptable.

Outputs use an owned private staging area and create-only publication of a
unique job directory containing binary and manifest, with sync and verified
hash/length before receipt release. Existing paths cannot be overwritten.
Cancellation/failure leaves no publishable success manifest. Receipt returns
only relative paths, hashes, sizes and typed v2 work accounting; recheck Files
authority before publication. Job ID, org/node, input identity, service
incarnation and request identity must remain bound across retries and progress.

The upstream whole-clip inference API has no cancellation hook. Proposed source
adapter uses an isolated per-job child, so cancellation/deadline can terminate
and join the actual model process. The sidecar parent must never inherit/load
CUDA state before spawning. Control/ack identity, termination grace and unknown
exit handling need review. HTTP cancellation or lost contact is not proof that
GPU work ended and cannot release the node's heavy-lane custody. Do not invent
a timeout-based success or permit overlapping writers while exit is unknown.
Progress is bounded per frame/window and excludes input paths or media bytes.

## Acceptance ledger

- Pending: source design review by automated-citadel and paired format consumers.
- Pending: source codecs and synthetic cross-language, malformed length/offset,
  nonfinite, FPS, mask and deterministic projection tests; normal CI only.
- Pending: complete source worker/sidecar, confined publication, custody/fault
  tests, approved model mapping and exact artifact/dependency pins.
- Held: local candidate execution, model downloads, GPU inference, service or
  image startup/publication, measured clip/VRAM/RSS and rendered-still acceptance.
- Held: undraft/merge, deployment, live credentials/schema or SQL application.
