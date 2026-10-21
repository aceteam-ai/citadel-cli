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
bytes, including under unexported caller wrappers: byte buffers live behind
reader factories, not reflectable slice fields. A read-only reader still permits its authorized caller to
read the bytes; it is not a sandbox against that caller. No snapshot is written
to the withheld-output store or published by this package.

Remaining gates include #1392 path-contract hardening before live wiring, the
shared pinned inbox transport, capability privacy, global two-holder admission,
withheld storage/GC, the sidecar, live `media_inputs`, task and publication
gates, and the reviewed upload route. No edit image bytes may cross Redis.
Hardware/model acceptance and deployment remain outside this source slice.

## #1314 C2 capability privacy: phased source plan

This source-only child implements the accepted
[pre-mint privacy requirement](https://github.com/aceteam-ai/aceteam/issues/11260#issuecomment-6290142842).
It does not mint capabilities, register tasks or complete C5. C1c acquisition
is separately prepared in draft #1413; no unratified video schema is needed.

1. Establish one conservative private-media classifier: `MEDIA_GENERATE` with
   an `inputs` field is private even if its value is malformed, escaped,
   wrong-type or too large. Do not make retention safety depend on successful
   execution parsing. Legacy text-only jobs with no `inputs` keep existing
   behavior. Retain no execution payload or raw envelope for private media;
   typed receipts, not replayable diagnostic payloads, carry nonsecret identity.
   Reflected private-media error text is static, preserving classification via
   an explicit unwrap closure rather than a reflectable original error field.
2. Audit original-byte observations, handler execution projection and requeue
   consumers before sealing private-media inputs/raw envelopes behind closures.
   Direct `MarshalJSON`/`Format` methods alone cannot hide public raw maps from
   reflection through an unexported wrapper. Preserve exact original-byte
   digests/accounting and usable execution authority without retaining a token
   in an ordinary observable field. Do not call C2 complete without this phase.
3. Apply the shared policy to worker, Redis and API/WS job diagnostics, usage,
   retry/error/status and DLQ retention, including paths before handler dispatch.
   No loss of execution authority, accidental redispatch or legacy text-job
   retention change. Include supported and malformed input-envelope cases.
4. Make direct-Redis ACK+deletion atomic and classify from the original stream
   entry, never caller-supplied job kind/payload. Only an actually pending,
   acknowledged private delivery may be deleted. Cover max-attempts and
   multi-queue paths; preserve ordinary-job retention and no-pending behavior.
5. Coordinate API/WS authoritative purge with its backend owner and verify
   actual boundary coverage before any mint/dispatch. D6 upload capability
   fields/purpose are not invented here: their privacy remains a separately
   reviewed pre-mint dependency. Verify the exact composed head in ordinary CI
   and obtain independent review. No workflow/protection change or activation.

Use synthetic tokens and owned offline Go/Python fixtures only. Local tests
must not start a real Redis service or contact a node; normal existing GitHub
CI can run its disposable integration fixtures. Keep FILE_FETCH/passcode and
legacy text regressions. Exercise escaped/malformed inputs, diagnostic verbs
and private wrappers, original error classification, all retention paths and
authoritative deletion refusals. Source, tests, review and integration receipts
belong in the draft PR. The accepted input-only fixture pin is unchanged.

All phases above remain required. This plan is not a claim that wrappers,
remote ACK, privacy, holder admission, output upload or live acceptance are
complete. Minting/activation stays blocked until C2 and its paired gates close.
