# Media input workspace freezer (inactive C1b)

`internal/mediainput.WorkspaceFreezer` consumes the strict C1a contract and
returns a job-private, in-memory snapshot. This source-only foundation does not
register edit tasks or handlers, advertise support, start a service, redeem a
capability or invoke a model. That workspace-only primitive refuses inbox and
mixed-origin contracts with `inbox_unavailable`. `media-input-contract.md`
describes the descriptor boundary. The separately prepared `Freezer` composes the
workspace primitive with the shared pinned inbox client; neither is a live
handler or an execution-authority boundary by itself.

On Linux, each read requires a non-nil live Files predicate. Parent resolution
uses `os.Root`; `openWorkspaceInput` pins the leaf against that parent with
`O_PATH|O_NOFOLLOW|O_CLOEXEC`, then checks the descriptor is regular before
opening it for reading. In particular, this flag pair pins a symlink itself,
which the type check refuses. It does not invoke a device's readable open
before refusing its type. See the Linux [open(2) O_PATH semantics](https://man7.org/linux/man-pages/man2/open.2.html).

The pin stays live while a directory descriptor anchors `/proc/self/fd` and
`fstatfs` verifies procfs. The only readable open follows that directory's
numeric FD alias, with `O_RDONLY|O_NONBLOCK|O_CLOEXEC`, and verifies the same
regular inode/device. Following this kernel magic link is intentional; the
untrusted workspace leaf is not followed or resolved a second time. Missing,
inaccessible or non-procfs FD access refuses; there is no pathname fallback.
Read permissions still apply when opening the pinned inode for reading. The
OS and mount namespace are trusted, not a sandbox against a privileged host
attacker. Other platforms refuse this primitive in v1.

The open descriptor must be regular and exactly the declared size, bounded to
16 MiB. Reads are chunked, context-checked and followed by an extra-byte check.
A second stat must match inode, size and modification time. The exact bytes'
SHA-256 must match the descriptor. No named input staging file is created and
the source pathname is never reopened. Files permission and cancellation are
rechecked before the snapshot can be returned.

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

## #1392 descriptor preflight follow-up plan

The #1393 review identified a gap: an `O_RDONLY` leaf open can invoke a device
driver before the subsequent regular-file check refuses that leaf. This
follow-up is source-only and does not settle owner UID or hard-link policy.

1. Pin the leaf using `O_PATH|O_NOFOLLOW|O_CLOEXEC` against the anchored parent.
   Inspect its descriptor type before any readable leaf open. Reject every
   non-regular type, including the symlink itself returned by this flag pair.
2. Keep that pin alive, anchor `/proc/self/fd` with a directory descriptor,
   verify procfs using `fstatfs`, and open only the pin's numeric FD alias for
   reading. Verify the readable descriptor is regular with the same device and
   inode. Never re-resolve the caller's leaf or fall back to a path-based open.
   A missing, inaccessible or non-procfs FD directory must fail closed. The
   process's OS and mount namespace remain trusted; this is not a sandbox
   against a privileged host attacker.
3. Add owned offline fixtures for syscall order/flags, synthetic non-regular
   type observations (without creating/opening devices), replacement and
   removal of the original pathname, procfs/open/stat failures and FD cleanup.
   Retain the existing freeze/hash/mask/cancellation/privacy tests and shared
   Go/Python contract replay. Only already-authorized offline tests and normal
   GitHub CI may execute; no formatter, model, native device or service run is
   included.

The source implements steps 1 and 2; owned offline fixtures cover step 3.
Consult the PR for exact-head offline and GitHub CI receipts; independent
review is still required. Even after this follow-up, owner/hard-link policy
and all live C1b/C5 acquisition,
publication and activation gates remain unresolved. The O_PATH change does
not establish that an accepted regular inode has no outside-workspace hard
links, nor does it restrict owner UID.

## #1314 C1c: shared inbox acquisition plan

This next source slice is stacked on the #1404 path/type hardening draft. It
does not complete C5 or enable a media task. The accepted input route and
lifecycle come from the [shared DoR](https://github.com/aceteam-ai/aceteam/issues/11260#issuecomment-6290142842),
as overridden by [Jason's recorded decisions](https://github.com/aceteam-ai/aceteam/issues/11260#issuecomment-6290189919).

1. Extract FILE_FETCH's fixed-origin, public-IP-pinned HTTPS GET policy into
   an internal shared client with a closed endpoint set and an `io.Writer`
   sink. Preserve the existing origin/URL, token, proxy, redirect, compression,
   TLS, DNS/all-answer/own-address, timeout, body-size and static-error rules.
   FILE_FETCH keeps its live Files checks, cooperative cancellation, anonymous
   staging, expected-hash verification, sync and create-only publication. Its
   existing tests and wire fixture must not change.
2. Prepare an inactive media freezer supporting workspace, inbox and mixed
   v1 inputs. Inbox uses only `/api/shared/media-input`, declared-size-plus-one
   bounded memory, exact size/hash and the existing image/header/mask guards.
   A live media-input predicate is mandatory; workspace inputs additionally
   require live Files. Inbox-only jobs require no workspace or Files grant.
   Preserve snapshot privacy and independent readers; no input disk staging,
   mount, task registration, permission mutation or capability minting.
3. Verify shared-client policy and every acquisition/permission/cancellation/
   privacy boundary with owned offline Go/Python fixtures. Use no real token,
   public network, model or service. Normal existing CI verifies FILE_FETCH
   compatibility and composed exact-head source. No local formatter/vet or
   standalone build is authorized by this plan.

The future C5 caller must acquire the shared node-wide holder admission before
any redemption, then enforce target-node/deadline/cancel, live publication and
upload gates. This library does not create a competing holder pool. C2 token
privacy must land before minting/dispatch; D6 upload and C3 withheld lifecycle
remain prerequisites. In particular, this does not expose #1374's unratified
video extension or settle owner/hard-link policy. The following prepared source
implements the acquisition phase; consult the PR for current exact-head test
receipts. Independent review remains required.

### Prepared C1c source

`coordinatorfetch.Get` owns the closed FILE_FETCH/media-input route set, exact
HTTPS origin, token syntax, all-answer public-IP/own-interface checks and
numeric reconnect pinning. It ignores proxies, refuses redirects/compression,
retains production TLS verification and bounds response headers, the
network/context-aware transfer deadline and bytes to declared-limit-plus-one.
It streams into an `io.Writer` and returns only
byte count/hash or a static refusal. It does not stage files or authenticate
capability claims; the coordinator still owns signature/purpose/expiry/nonce
and tenant/object binding. Trusted programmatic hooks are fixture seams, never
payload/configurable network-policy overrides.

The sink's `Write` is synchronous and need not honor context cancellation. A
blocked local writer can therefore outlast the request deadline; that deadline
is not proof of writer exit. Callers retain writer/holder custody until the
transfer returns and must not add a timeout goroutine that abandons the writer
or reuse/publish partial bytes. The in-memory inbox sink does not introduce a
blocking external writer; FILE_FETCH retains its existing local file-write
behavior and final cancellation/publication checks.

FILE_FETCH reuses that client but keeps its original filesystem and live Files
boundary, cooperative cancellation, maximum-size (not exact-size) semantics,
expected SHA, sync and create-only publication. Its existing tests and shared
wire fixture remain unchanged. No media permission is added to FILE_FETCH.

The new inactive `mediainput.Freezer` requires a non-nil live
`MediaInputsEnabled` predicate for every input. If any input is workspace-origin,
live Files and a workspace are also required, checked before **any** capability
redemption and again between inputs and before returning. Inbox-only snapshots
need neither Files nor a workspace. The trusted origin is sampled/validated
once per freeze; the job cannot select a URL or endpoint. Input memory reserves
only the declared bytes plus the over-limit probe, and exact size/hash and the
same existing image/mask guards apply. Both origins share the private reader
factory; partial snapshots never escape on failure/revocation/cancellation.

This is source preparation, not C5 activation or complete media acceptance.
The caller still needs shared holder admission before any redemption, target
node/deadline/duplicate/cancel and live publication gates, C2 credential privacy,
C3 withheld storage, D6 upload, a compatible sidecar and reviewed permissions.
The transport/freezer adds no further fixture revision beyond its composed
parent's proposed replay correction; see `media-input-contract.md` for accepted
versus proposed pins. That composition is not paired platform ratification.
No video schema, live token or permission is introduced.
See the stacked draft for exact-head receipts and independent review status.
