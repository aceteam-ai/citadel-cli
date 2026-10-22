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

### C2 phase 1: prepared shared diagnostic policy

`jobprivacy.IsPrivateMedia` uses field presence, not successful JSON/token
parsing. The shared retention helpers omit the entire private-media payload
and arbitrary failure fields; private-media text is static without invoking
the original error formatter. This includes malformed/escaped/oversized inputs
and workspace-only inputs, rather than guessing which representation is safe.
Legacy text jobs without `inputs` keep their diagnostic behavior. This is a
conservative source proposal, not a ratified upload/fixture contract.

The shared `redactedError` now keeps its explicit `Unwrap` projection behind a
closure, not a reflectable original-error field. A synthetic value-error
regression first reproduced a FILE_FETCH leak through `%#v`; the source fix
preserves `errors.Is`/`errors.As` and passes the same wrapper/format cases for
FILE_FETCH, passcode and private media. No original error object is mutated.

These helpers do not seal `Job.Payload`, `RawPayload` or `RawData`, wire every
diagnostic path, or delete a Redis entry. Phases 2 through 5 remain required.
Do not call C2 complete or mint a media capability based on this phase's tests
or green CI. See the draft for exact-head receipts and independent review.

### C2 phase 2: explicit owned payload boundary

`jobprivacy.CaptureMedia` owns a copied execution map, source metadata and exact
original wire bytes behind a private closure. Explicit execution projections
return independent copies with the original Go value types. Original bytes are
never reserialized, including raw-only, nil and empty inputs. Raw-only captures
cannot acquire executable fields. The passive delivery observer is unchanged.

Classification examines canonical root `inputs` presence without decoding large
prompt values. Nonempty invalid/non-object media envelopes fail closed for
retention; valid legacy text-only objects retain their previous policy. Copying
accepts only known scalar/container types within depth/value bounds. Unsupported
or cyclic projections refuse without invoking caller formatting or marshaling.

This foundation does not by itself seal any queue or worker Job. Phase 3 must
use it at intake and preserve routing, deadlines, GPU selection, receipts and
handler projection while keeping diagnostic data non-executable. ACK deletion
and the paired API/WS purge remain required pre-mint gates.

### C2 phase 3: parsed intake and trusted control projections

Direct Redis and API parsing now seal input-bearing media before returning a
parsed Job. Redis, API and WS worker conversion transfer the opaque snapshot,
not a passive observation or diagnostic map. Public payload fields hold only
the private marker; raw payload and raw-data fields are cleared. Repeated
sealing preserves the original capture. Ordinary parsed-job JSON/fmt and
reflection through unexported caller wrappers cannot traverse its closure.
Original wire carrier DTOs are transport inputs, not diagnostic representations.
They still require a separate call-site retention audit before C2 completion.

Malformed HTTP consume and WS envelope decoding now uses
`jobprivacy.DeliveryError` before any job kind is known. It returns static
diagnostics while preserving explicit `errors.Is`/`errors.As` classification
behind the same private closure boundary. WS decoding still uses the unchanged
JSON decoder and captures passive observations from the exact original frame.
An injected HTTP RoundTripper and direct WS decoder fixtures test this without
a network listener, DNS lookup or real service. A malformed WS interface-valued
number fixture proves the original decoder error contains its wire spelling,
while retained wrapper/log diagnostics do not.

`DeliveryErrorContext` keeps trusted HTTP route/status context in the same
opaque wrapper, not a second wrapping error. The existing decoder compatibility
fixture still compares one-hop original type, field, offset, error text and
partial decoded state unchanged; its injected transport avoids a listener.

The runner validates explicit execution projection before routing or admission,
refusing raw-only/invalid captures without handler execution. Routing, explicit
deadlines, GPU requests/assignment, org and flow attribution use original owned
fields. Receipt digest input uses exact original bytes, never a sanitized or
normalized serialization; nil/empty fallback uses original execution fields.
Runtime GPU selection cannot be overwritten through the observable marker map.

The legacy adapter explicitly projects original values with its existing
coercion rules, not new input parsing. Private-media handler logs and failure
output are static. Arbitrary progress fields and unreviewed structured usage/
receipt echoes are not retained for these new inputs; C5 must supply a reviewed
typed output path, rather than re-enable generic map copying. Nonsecret flow
identity and numeric accounting remain separate from private input content.
Legacy text, FILE_FETCH, passcode and other job behavior remain unchanged.

Offline mock/parse tests cover Redis/API/WS conversions, exact raw bytes,
malformed/escaped inputs, wrapper reflection, immutable copies, target routing,
deadline/GPU/receipt projection, handler failures, retry/terminal usage, and
legacy compatibility. These tests do not prove actual stream deletion, complete
wire/log/DLQ boundary coverage, a typed output path, paired API/WS purge or live
acceptance. Complete the remaining retention audit and phases 4 and 5 before
minting/dispatch. No handler/task activation is added by this change.

The follow-up retention matrix exercises actual single/multi-queue DLQ command
assembly through a pre-dial hook and HTTP consume responses through a no-dial
transport. Inbox/workspace, null, wrong-type, escaped/duplicate keys, oversized,
malformed/non-object and legacy text cases check retained fields, nonsecret
source identity and exact original-byte projection. Matching stored DLQ XRANGE
tests are reserved for normal GitHub CI, not local real-Redis execution.

Nested payload decoding can fail before fallback kind discovery. Its unknown,
MEDIA_GENERATE and FILE_FETCH error paths now retain only trusted static context
in a single opaque wrapper, preserving the original one-hop decoder error.
Other known kinds keep their previous diagnostic behavior. Likewise, the actual
WS server-error callback has no trusted job kind: it retains one observable
error-level event with static text, not the untrusted server detail, which may
echo a delivery. Its direct callback fixture binds no listener and leaves the
original wire carrier unchanged. These boundary fixtures are not a claim that
all wire carrier call sites or the paired backend have been audited.

### C2 phase 4 preparation: pure classifier, no deletion wiring

`internal/redis/media_ack_classifier.lua` is an unregistered pure boolean
retention predicate for an effective kind derived from the original stream
entry. It contains no Redis calls, does not acknowledge or delete an entry, and
cannot grant handler execution authority. The existing ACK paths are unchanged.

Its iterative JSON grammar matches the Go private-media classifier, including
root-only escaped/duplicate `inputs`, malformed/non-object envelopes, strict
number syntax and the container nesting limit. It does not use cjson coercion
or decode/retain prompt values. Valid legacy text objects preserve retention,
including large strings, large valid number lexemes and surrogate spellings.
These are classification rules, not new execution-parser allowances.

Offline tests run the pinned existing Lua interpreter with only its string
library. OS, IO, loading, network, cjson and Redis APIs are absent. Property tests
compare its boolean to the actual Go classifier. The local fuzzer bounds its
work size only; that is not a production envelope-size policy. This evidence is
not real-Redis execution, atomicity, queue ownership or purge acceptance.

Deletion wiring awaits the Citadel owner's design call: the current runner ACKs
wrong-target deliveries on shared streams/per-node consumer groups. A generic
private-media ACK+XDEL could remove that entry before the addressed node
consumes it. Confirm the original queue/target and authenticated owner contract
before wiring deletion; do not add caller-supplied kind/purge flags or infer
authority from this predicate. Paired API/WS purge, exact composed verification,
independent review and all ship/minting/native/human gates remain required.
