# RUN_JOB_TEMPLATE wire contract

The platform authority is `python-backend/utils/database/job_templates.py::compute_template_hash`.
The dispatcher is `python-backend/routes/aceteam_mcp_code.py::dispatch_run_job_template`,
and `RunNodeJobNode.run` forwards the registered schemas and file metadata.
Platform main already supplies both schema fields (aceteam#10445); no coordinator
schema change is needed for citadel-cli#1160. This is the node framework for
citadel-cli#1149 / aceteam#10427. The compiled-in builtin registry remains empty.

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

Schemas compile locally (default JSON Schema draft 2020-12, or the supported
draft named by `$schema`). Internal `$ref` works. External schema resources,
including filesystem and network URLs, are forbidden. Input params must satisfy
the approved schema before any ops call or filesystem effects. The output schema
is compiled for validity and remains part of the approved hash; output artifacts
follow the fixed result contract below.
Schema compilation and builtin params use the same normalized numbers as the
Python authority. Underflow or binary64 rounding cannot change schema limits
or dispatched params through a different spelling with the same manifest hash.

Every input must have `node_id` equal to the locally configured executing node
and a canonical workspace-relative `node_path`. `path` must be that same relative
path or `node:<node_id>/<node_path>`, matching platform `FileValue` references.
Missing or foreign identity, inconsistent paths, malformed references, traversal,
and workspace-escaping symlinks are refused. All reference metadata is validated
before resolving or hashing any input. Cross-node transfer is outside this slice.
The source queue must name the executing node and belong to its per-node stream.

Successful results contain `outputs: [{path, sha256, bytes}]`, `duration_ms`, and
`input_hashes`. Hashes use the `sha256:` prefix; output paths are workspace-relative.
Empty collections serialize as arrays. Large output bytes do not enter the result.
The platform turns outputs into `node:<node_id>/<path>` file references and reads
them lazily. Optional AEP v2 signing uses action `run_job_template`, binds the job,
manifest hash and ordered output digests, and remains fail-open for signing errors.

Regression tests include the four independent review probes at original head
`e6375a723be5894e392f98cfb19f52c386aa3a5c`. Run the guard-removal suite with
`go run ./scripts/test_job_template_mutations.go`; its temporary Go overlays do
not modify the checkout and require actual regression assertion failures.
