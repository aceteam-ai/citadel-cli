# Synthetic point-cloud capture source proposal

Refs #1386 and the accepted paired #11830 revision-2 design. This is the first
inactive node-path source slice: `internal/pointcloudcapture` has no production
caller, live transport, dialer, launcher, executable pin, registration, encode
path or native admission authority. It does not import or depend on DRAFT
#1455. Future execution and native acceptance remain separately held.

## Fixed test contract

`Run` accepts an injected CDP `CommandTransport`, a frame sink, a lowercase
32-hex-character run identity and refusal-only environment assertions. It tests
exactly 60 frames at 30 FPS: two seconds, 320 by 180 pixels. There is no caller
frame-rate/resolution/duration override, 10-FPS reuse, arbitrary expression,
scene source, URL or uploaded code. Environment privilege is an explicit enum:
missing, unknown or elevated privilege refuses. Only the fixed Linux,
unprivileged `angle-swiftshader-webgl2` candidate is considered. Passing those
checks is not permission to launch; supplied strings cannot establish a real
executable, tested flag tuple, sandbox or software-renderer identity.

The fixed namespace is `window.CitadelPointcloudCaptureTest`. One init call
echoes run identity with frame -1. Each integer Step n must return its own run
identity and n before one screenshot and one sink delivery. Evaluate requests
`returnByValue`, `awaitPromise`, `generatePreview:false` and
`allowUnsafeEvalBlockedByCSP:false`. No unsafe-eval bypass, retry, frame skipping
or automatic renderer fallback is supplied.

The CDP Evaluate result is the documented `result:RemoteObject` shape: required
type `object` and by-value summary, with optional `className:Object` and
`description:Object`. Other wrapper fields, including exceptions, object IDs,
preview and nonplain-object subtypes, refuse. The closed summary has exactly
kind, run, frame, ready, width, height, scale, mode and renderer. Integers arrive
as exact matching JSON-decoded float64 values; dimensions/scale are fixed and
ready must be true. Renderer is printable ASCII, at most 256 bytes, contains
ANGLE and SwiftShader and remains unchanged after init. This is only an
observed string refusal check, not native build/mode or sandbox proof.

The accepted summary's useful fields fit below 4 KiB. The transport returns
**already parsed maps**: this slice cannot reject upstream wire/JSON expansion
before it happens, supply the future method-aware bounded reader or establish
the R2 parent-buffer budget. It does not serialize arbitrary maps or include
peer diagnostics in errors. Stage errors are static sentinels; cancellation
uses the caller's context. Only one transaction is outstanding. A transport
that ignores context cannot be preempted by this sequential package; native
custody and joined teardown are future adapter requirements.

Screenshot requests fix PNG, surface capture, no beyond-viewport capture and a
320x180 clip at scale one. One PNG is limited to 1 MiB for this small synthetic
test. Encoded length is checked before base64 decoding, with a padding-aware
decoded preflight and actual decoded-byte check. The distinct static size
sentinel lets malformed oversized fixture data prove refusal precedes decode.
Decoder allocation may have two rounding bytes of slack. `png.DecodeConfig`
must report exactly 320x180 **before** full PNG validation allocates its raster.
These bounds are not a whole-process RSS or aggregate transport budget.

## Test-only host and stamp

`internal/pointcloudcapture/testdata/synthetic.html` is a test-only HTML/WebGL2
shell. It is not the shared Aceteam scene, a production shell, a CSP/sandbox
implementation or a runtime flag. It has no external dependencies, network
calls, timers or animation loop. It requires a 320x180 viewport, device scale
one, `preserveDrawingBuffer:true` and antialiasing off. An explicit WebGL2/
renderer probe refuses unsupported or lost contexts. No launch args, executable
hashes or actual tested capability tuple are invented.

Step draws a tiny fixed three-point cloud and a test stamp synchronously,
finishes GL commands and resolves in the same task. Nothing schedules a later
mutation between that resolution and capture. This source ordering does not
prove compositor/CDP paint freshness on a real Chromium build; that is the
purpose of the later stamped native acceptance.

The stamp occupies eighteen 8x8 black/white opaque cells along the PNG's top
edge. GL scissor uses y=180-8 for that strip. Cells are two white anchors,
six least-significant-first index bits, six inverse bits and asymmetric tail
0/1/0/0. The verifier exists **only in Go test source**, checks all cell pixels
and recovers its own index. Literal expected cells pin indices 0, 1, 31, 32 and
59 independently. The production package validates PNGs but never verifies a
stamp or treats it as metadata: a permissive sink can accept repeated images.

Fake CDP fixtures generate valid PNGs and the test sink independently verifies
all 60 indices and evaluate/capture/sink ordering. Negative controls cover stale,
repeated and skipped pixels; ack disagreement; mirrors/vertical flips; absent
black stamps; corrupt anchors/complements and nonbinary/alpha values; malformed,
oversized and wrong-dimension PNGs; closed control fields; unresolved Step;
stage failure and cancellation. A valid wide one-row PNG tests dimension refusal
without a potentially unbounded image allocation. No fake fixture claims that
HTML was rendered or that a native screenshot fence passed.

## Remaining gates

Normal GitHub CI and independent source review are needed for this draft.
No local parser, formatter, tests, build, JavaScript, socket, browser, media,
model/GPU or native execution accompanied the source work. Later native
execution needs explicit custody authorization, a reviewed exact Chromium build
and software-WebGL flag/capability tuple, actual unprivileged sandbox/egress
proof, bounded target discovery and method-aware CDP/aggregate allocation
accounting, plus joined process/cgroup teardown and resource measurements.
The HTML must be hosted through a separately reviewed test adapter; this
package provides none. A failed or unavailable stamp test holds that mode.

The actual two-second 60-frame stamped test, fifteen-second depth/music artifact
and staging workflow remain unperformed. Shared scene/DSP, #1405 manifest/chunk
authority, Range/PCM transport, AAC mux, MP4/poster/render receipt, full resource
budgets, registration/approval and later browser/live acceptance remain open.
No fallback changes the renderer, effects, sandbox or capture mechanism.

Protocol shapes are described by the primary [Runtime schema](https://github.com/ChromeDevTools/devtools-protocol/blob/master/json/js_protocol.json)
and [Page schema](https://github.com/ChromeDevTools/devtools-protocol/blob/master/json/browser_protocol.json).
Those schema links are not pins for a tested native implementation.
