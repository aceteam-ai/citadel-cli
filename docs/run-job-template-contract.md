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
HTTP(S), WebSocket, FTP, and non-proxied WebRTC UDP before navigating to the
OS-correct local file URL. It captures a PNG whenever the 10 fps render key
changes, repeats those frames into a 30 fps image pipe, and encodes H.264
(`libx264`, CRF 17, slow/animation, `yuv420p`) with optional AAC audio. Outputs
are `landscape.mp4` and
`portrait.mp4`. Chromium profile/cache/temp files stay inside the run output
directory and are removed after each format. Cancellation terminates the browser
process tree and waits before profile removal; cleanup failures fail the run.
Only a root process on Linux receives Chromium's required `--no-sandbox` switch.
Runtime dependencies are Chromium or Chrome and ffmpeg.

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
