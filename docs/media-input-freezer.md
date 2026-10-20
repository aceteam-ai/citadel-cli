# Media input workspace freezer (inactive C1b)

`internal/mediainput.WorkspaceFreezer` consumes the strict C1a contract and
returns a job-private, in-memory snapshot. This source-only foundation does not
register edit tasks or handlers, advertise support, start a service, redeem a
capability or invoke a model. Inbox and mixed-origin contracts currently refuse
with `inbox_unavailable`; the shared pinned HTTPS acquisition adapter is still
required. `media-input-contract.md` describes the descriptor boundary.

On Linux, each read requires a non-nil live Files predicate. Parent resolution
uses `os.Root`; the leaf opens with `openat` against the anchored parent, with
`O_NOFOLLOW`, `O_NONBLOCK` and `O_CLOEXEC`. It never relies on a path validation
followed by an unrestricted open, nor on `Root.OpenFile` to enforce a caller's
leaf no-follow flag. Other platforms refuse this primitive in v1.

The open descriptor must be regular and exactly the declared size, bounded to
16 MiB. Reads are chunked, context-checked and followed by an extra-byte check.
A second stat must match inode, size and modification time. The exact bytes'
SHA-256 must match the descriptor. No named input staging file is created and
the source is never reopened. Files permission and cancellation are rechecked
before the snapshot can be returned.

PNG and JPEG dimensions use Go's header-only `DecodeConfig`; PNG chunk framing,
CRC and animation checks follow. WebP uses the cached, pinned BSD-licensed
`golang.org/x/image` header decoder for the underlying VP8/VP8L chunk, plus a
bounded RIFF walk. Extended canvas and underlying bitstream dimensions must
match. The container walk follows the
[WebP container specification](https://developers.google.com/speed/webp/docs/riff_container).
Neither code path decodes pixels or infers anything about a model. Width/height
are capped at 65535 and total pixels at 16,777,216; APNG and animated WebP refuse.

EXIF IFD0 orientation is checked without applying a transform. Non-1 orientation
and malformed orientation metadata refuse. Masks must be grayscale PNG at 1 or
8 bits and match source dimensions exactly, with no resize, inversion, palette
conversion or grayscale conversion. Pixel/mode/alpha validation in the sidecar
is still required; a header acceptance is not proof that compressed pixels can
be decoded or that a model supports editing them.

Snapshots expose only copied, path/token-free metadata and independent readers,
not backing slices. Snapshot and reader diagnostic formatting hides image
bytes. A read-only reader deliberately still permits its authorized caller to
read the bytes; it is not a sandbox against that caller. No snapshot is written
to the withheld-output store or published by this package.

Remaining gates include #1392 path-contract hardening before live wiring, the
shared pinned inbox transport, capability privacy, global two-holder admission,
withheld storage/GC, the sidecar, live `media_inputs`, task and publication
gates, and the reviewed upload route. No edit image bytes may cross Redis.
Hardware/model acceptance and deployment remain outside this source slice.
