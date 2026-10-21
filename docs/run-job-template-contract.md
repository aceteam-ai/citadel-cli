# RUN_JOB_TEMPLATE wire contract

The platform authority is `python-backend/utils/database/job_templates.py::compute_template_hash`.
The dispatcher is `python-backend/routes/aceteam_mcp_code.py::dispatch_run_job_template`,
and `RunNodeJobNode.run` forwards the registered schemas and file metadata.
Platform main already supplies both schema fields (aceteam#10445); no coordinator
schema change is needed for citadel-cli#1160. This is the node framework for
citadel-cli#1149 / aceteam#10427. The compiled-in registry currently exposes
`papercraft-render` and `audio-mix`.

The platform sends string-valued payload fields:

| Field | Contract |
| --- | --- |
| `template_key` | Registered template key |
| `template_version` | Positive decimal version string |
| `content_hash` | SHA256 of the approved manifest; optional `sha256:` prefix |
| `input_schema`, `output_schema` | JSON-encoded schema objects from that version |
| `runner` | JSON object with exactly `kind` and `handler`, both exact strings |
| `params` | JSON-encoded object validated against `input_schema`; omission means `{}` |
| `input_files` | JSON-encoded array of `{path, node_id, node_path}`; omission means `[]` |

The node recomputes SHA256 over `{template_key, version, input_schema, output_schema, runner}`
using Python `json.dumps(sort_keys=True, separators=(",", ":"), ensure_ascii=True)`
semantics after JSON decoding. Integers are arbitrary precision and `-0` becomes
`0`. Floating-point literals decode as binary64, use Python repr notation, retain
`.0` and floating signed zero, and normalize exponent spelling. Overflow to
non-finite values is refused. Duplicate keys, trailing JSON, invalid UTF-8 and
unpaired UTF-16 surrogate escapes are refused; Unicode scalar strings and valid
surrogate pairs retain Python escaping semantics.
Reordering a runner cannot change the selected builtin under one approved hash.
Only `kind == "builtin"` is executable. Case aliases and unknown runner fields
are terminal errors. The handler and live adapter share this validation.

Schemas compile locally with a closed input-schema vocabulary. Every object is
closed with `additionalProperties: false`; every property and present array item
schema declares an explicit `type`; and local references are limited to one
`#/$defs/<name>` segment. `$schema`, external resources,
unwalked applicators, boolean/empty property schemas, and boolean/empty item
schemas are forbidden. Input params must satisfy the approved schema before any
ops call or filesystem effects. Each builtin also decodes exact keys itself, so
Go's case-insensitive struct binding is not part of the trust boundary. The
output schema is compiled for validity and remains part of the approved hash;
output artifacts follow the fixed result contract below.
Schema compilation and builtin params use the same normalized numbers as the
Python authority. Underflow or binary64 rounding cannot change schema limits
or dispatched params through a different spelling with the same manifest hash.

Every input must have `node_id` equal to the locally configured executing node
and a canonical workspace-relative `node_path`. `path` must be that same relative
path or `node:<node_id>/<node_path>`, matching platform `FileValue` references.
The node accepts at most 128 declared inputs and 4 GiB of declared input bytes
per attempt. Repeated references count repeatedly toward the byte ceiling even
though their snapshot is staged only once. Both limits are fixed execution
policy, not operator-tunable feature flags.
Missing or foreign identity, inconsistent paths, malformed references, traversal,
and workspace-escaping symlinks are refused. All reference metadata is validated
before resolving or hashing any input. The workspace's absolute path is fully
resolved before opening its `os.Root`; every path passed to a builtin is rebuilt
from that resolved root identity rather than the caller's possibly symlinked
spelling. Each source is copied into a read-only, attempt-unique snapshot while
it is hashed. Builtins consume only those staged paths, and the snapshots are
re-hashed before removal, so `input_hashes` describes the bytes available to the
builtin rather than an earlier by-name read. Cross-node transfer is outside this
slice. The source queue must name the executing node and belong to its per-node
stream.

Successful results contain `outputs: [{path, sha256, bytes}]`, `duration_ms`, and
`input_hashes`. Hashes use the `sha256:` prefix; output paths are workspace-relative
and are opened through an `os.Root` fixed at the per-attempt `out` directory, so
a returned sibling path or escaping symlink is refused. Every delivery gets a
random 128-bit attempt namespace beneath the template and job directory. A
deadline can therefore release the serialized lane without letting an older,
slow-to-cancel goroutine collide with a retry's paths. Staging, post-run input
verification, and output hashing check cancellation between reads and writes;
failed attempts remove only their own namespace and report cleanup failures.
At most 16 regular output files totaling 8 GiB are retained, within at most 64
total output-tree entries including directories. Every regular file left in `out`
must be reported by the builtin; duplicate paths, unreported files, symlinks,
special files, and boundary overruns fail the attempt and remove its namespace.
Successful output namespaces carry an exact node-owned completion marker and
remain available for lazy `node:<node_id>/<path>` reads for seven days. A sweep
before each new template attempt removes only completed namespaces strictly older
than that window, then removes only job/template parent directories left empty by
the sweep. Parents containing fresh, active, or unrelated siblings are retained.
Active or incomplete namespaces (identified by their retained input snapshot,
which takes precedence over any marker) are never swept, including
watchdog-abandoned attempts; the
pre-marker successful layout from v2.175.0 is also recognized and expired.
Empty collections serialize as arrays. Large output bytes do not enter the result.
The platform turns outputs into `node:<node_id>/<path>` file references and reads
them lazily. Optional AEP v2 signing uses action `run_job_template`, binds the job,
manifest hash and ordered output digests, and remains fail-open for signing errors.
Runs retain serialized-lane routing and use the generous long-tier watchdog
fallback when the payload does not provide `timeout_ms`.

## Compiled-in builtins

`papercraft-render` accepts params `{}` or a finite `duration_seconds` in
`(0, 3600]`. Its inputs include exactly one built Paper Trail player for each
required stage declaration, `1920x1080` and `1080x1920`, plus the referenced
asset files and at most one optional audio file. Each player must expose
`window.READY`, `window.DUR`, and `window.renderFrame(t)`. The runner blocks
HTTP(S), `file:`, `blob:`, WebSocket, FTP, and non-proxied WebRTC UDP
before navigating to the loopback asset route (`data:` is allowed; it never
leaves the renderer, and blocking it broke canvas-generated texture data
URIs, citadel-cli#1202). It captures a PNG whenever the
10 fps render key changes, repeats those frames into a 30 fps image pipe, and encodes H.264
(`libx264`, CRF 17, slow/animation, `yuv420p`) with optional AAC audio. Outputs
are `landscape.mp4` and
`portrait.mp4`. Chromium profile/cache/temp files stay inside the run output
directory and are removed after each format. Cancellation terminates the browser
process group and its named systemd scope before profile removal, so a `setsid`
descendant cannot escape; cleanup failures fail the run. Rootless scopes require
delegated cpu, memory, and pids controllers. Chromium and ffmpeg receive a fixed
minimal environment rather than worker credentials. Only a root process on
Linux receives Chromium's required `--no-sandbox` switch. Rendering fails closed
outside Linux; the Windows process helper is an unreachable compile stub.
Runtime dependencies are Chromium or Chrome, ffmpeg, systemd-run, and systemctl.

The matching closed input schema is:

```json
{"type":"object","additionalProperties":false,"properties":{"duration_seconds":{"type":"number","exclusiveMinimum":0,"maximum":3600}}}
```

`audio-mix` accepts only `{}` and one to sixteen input stems. It runs ffmpeg
directly, never through a shell: `amix` with longest-input duration and no
implicit normalization, followed by a measured two-pass EBU R128 `loudnorm` at
-14 LUFS integrated, -2 dBTP, and LRA 11. It writes one 48 kHz 24-bit PCM
`mix.wav`. A silent/non-finite loudness analysis fails explicitly rather than
claiming a normalized output.

The matching closed input schema is:

```json
{"type":"object","additionalProperties":false}
```

Regression tests include the four independent review probes at original head
`e6375a723be5894e392f98cfb19f52c386aa3a5c`. Run the guard-removal suite with
`go run ./scripts/test_job_template_mutations.go`; its temporary Go overlays do
not modify the checkout and require actual regression assertion failures.

## Proposed video-compose v1 contract (inactive source)

This is **part of aceteam-ai/aceteam#11481**, not a registered or template-ready
builtin. `runVideoComposeBuiltin` has no registry entry or approved manifest
hash. Its live dependency set refuses before tool discovery or output creation.
Activation is blocked on paper-trail card scenes (#11480) and the vendored v3
shell (#11296), including a real implementation of the card/disclosure seam.
The fake renderer in unit tests supplies fixtures only; it is not a card renderer.

Params are exactly `{}`. Proposed inputs are exactly one `film.json` (at most
1 MiB), one to 32 chapter MP4s named in it, and an optional `poster.png` override.
Files must be declared staged regular files with distinct basenames. Each
chapter's `file` is the exact final staged basename, including the unique name
returned by FILE_FETCH/NodeFileWrite. S3 (#11483) must stage first, assemble the
final film.json from those returned names, then obtain human approval over those
final inputs. No implicit `ch1.mp4` rename, case folding, directory traversal,
external URL, unlisted file lookup, extra input, or music `bed` is supported.
Chapter names use ASCII letters/digits/underscore/hyphen/dot, end in `.mp4`, and
cannot start with a dot or hyphen.

`videoComposeFilmSchema` owns the proposed closed film schema. Exact keys are
checked at every object level; duplicate keys, trailing JSON and invalid Unicode
are refused before struct binding. Top-level fields are `title`, `subtitle`,
`intro`, `outro`, `theme`, `chapters`, optional `loudness`, and optional `delivery`.
Intro/outro contain `lines` and positive `seconds` up to 60. Chapters require
`file`, `title`, `narrator: {name,line}`, `stamp: {label,by,at}`, positive
`card_seconds` up to 60, nullable `disclosure`, `narration_identity`, and
`speaks_as_person`. The identity enum is `stock|consented_clone|own_recording`
(#11484). These last two required fields repair their omission in #11481's JSON
example: `stock` plus `speaks_as_person:true` without a nonblank disclosure must
fail closed. A stamp requires `label:"SIGNED"`, a nonblank `by`, and a valid
`at` calendar date in `YYYY-MM-DD` form. The future renderer must place disclosure
on the card and burn it for the chapter's entire duration. Text/card-count limits are pinned by the
source schema; theme names currently use the exact existing v2 preset registry
and must be reconciled with the pinned v3 registry before activation.

For example, a chapter must now include:

```json
{
  "file": "staged-a71f-chapter.mp4",
  "title": "I signed my own homework",
  "narrator": {"name": "the Ace", "line": "Narrated by the Ace, the AI"},
  "narration_identity": "stock",
  "speaks_as_person": false,
  "disclosure": null,
  "stamp": {"label": "SIGNED", "by": "founder", "at": "2026-10-12"},
  "card_seconds": 2.5
}
```

Fixed ffprobe argv inspect every chapter before rendering, recording dimensions,
rational frame rate, pixel format, codec profile/level/time base/extradata,
audio codec/rate/channels/extradata and duration. Proposed v1 supports square-pixel
landscape inputs, even dimensions up to 8192 pixels wide, 1–120 fps and one video
plus one audio stream; mixed aspect, absent audio, extra streams, invalid metadata
and missing files fail. The current capture helper is still 30 fps; integrating
other reference frame rates is an activation requirement, not a tested capability.
The single-landscape capture entry point uses the same Chromium/cgroup lifecycle
as existing papercraft rendering and requests continuous stereo 48 kHz silent
AAC; the existing public papercraft entry point still renders both formats.

The source core copies chapter streams only when the inspected stream parameters
match the rendered intro reference; otherwise it requests libx264 CRF 17,
yuv420p and AAC 192k normalization. Prepared clips/cards are probed again and
must match before the safe concat demuxer consumes a node-written list of fixed
scratch basenames. These subprocesses use the existing limited-command runner,
minimal child environment and process-tree cleanup; no shell, NVENC, provider,
or credential path is introduced. Input protocols are limited to file/pipe.

Audio uses measured two-pass loudnorm, defaulting to -14 LUFS, -2 dBTP and LRA 11.
An explicit complete `loudness: {i,tp,lra}` can choose -16 LUFS instead.
`delivery.max_bytes` defaults to 48 MiB (50331648 bytes), including when delivery
is `{}`. **Proposed conservative v1 policy:** 48 MiB is also its maximum pending
S5, preserving the current upload ceiling; this upper bound needs contract
review because #11481 specifies the default but does not explicitly spell out
the maximum. Only an oversized result triggers one x264 bitrate cap pass; it
reserves mux/audio headroom, copies the normalized audio, and fails if actual
bytes still exceed the limit. Both final encode paths request `+faststart`.
No node performance measurement or live ffmpeg validation has been performed.

Proposed outputs are `film.mp4`, `poster.png`, and `chapters.json`. The poster is
the frame immediately after the intro or a decoded/re-encoded supplied override.
The timeline is an object with `chapters: [{index,title,start_s,end_s,card_start_s,
narrator,stamp,disclosure}]`, `duration_s`, `bytes`, `loudness_measured` (the first
loudnorm pass's input statistics), `loudness_measured_stage:"pre_normalization"`,
and `encode_passes` (one audio-normalization
encode, plus one if the delivery cap ran). Chapter indices start at one;
start/end exclude the chapter card, card_start includes its start. Times are
accumulated from prepared media durations, including intro/outro, and final
duration must agree within one frame. `chapters.json` is an output file: no fields
are added to the adapter's `outputs/duration_ms/input_hashes/receipt` envelope.
The five loudness statistic values remain strings; they describe the input
analysis and must never be presented as measurements of delivered loudness.

Before activation: integrate pinned real card/disclosure assets, reconcile the
reference format/rate/stream normalization with them, run real media acceptance
and the capped ten-minute CPU timing on the target node, then review the final
closed schema and registration manifest/hash. The independent render wall
timeout is two hours; RunNodeJob currently caps a supplied timeout at 1800 s.
Coordinate the eventual release with FILE_FETCH (#11482); source presence or a
version number alone does not establish node capability. Jason owns the eventual
template registration and approval on node 1795, after the release routed through
automated-citadel. This source change performs none of those actions.
