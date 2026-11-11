# Stream definition digest v1

This shared source contract is used by Citadel ST4b and Aceteam ST3/ST4a.
`internal/stream/definition` owns validation and hashing; it neither deploys
nor authorizes a stream. It deliberately does not use the pack digest's
different manifest/file framing algorithm.

## Exact preimage

```text
definition_digest = "sha256:" + lowercase_hex(SHA256(
    UTF8("stream.definition.v1") || 0x00 || JCS({
        "definition": fully_materialized_definition,
        "organization_id": authenticated_organization_uuid
    })
))
```

JCS means RFC 8785, with no BOM, indentation or trailing newline. Keys at
every object depth sort by UTF-16 code units. Array order is preserved.
Strings use RFC 8785/ECMAScript escaping, literal UTF-8 for non-ASCII, and no
Unicode normalization. For example `<` stays literal, newline is `\n`, and
composed/decomposed Unicode remain different. Duplicate decoded keys,
malformed UTF-8 and lone UTF-16 surrogates are rejected before Go's ordinary
JSON decoder can overwrite or replace them.

All JSON numbers in this profile are integer literals in the range 0 through
9,007,199,254,740,991, further narrowed by each field below. No decimal or
exponent spellings, negative zero, NaN, Infinity, numeric strings, booleans as
numbers or coercion are accepted. The only monetary fields are integer
`max_run_cost_micro_usd` (1–1,000,000) and `daily_cap_micro_usd`
(1–5,000,000): 1 USD = 1,000,000 micro-USD. The earlier unmerged draft's
`max_run_cost_usd` and `daily_usd_cap` float fields are **not** v1 fields.

`organization_id` comes from server authentication on the platform and local
enrollment on the node. It is a canonical lowercase, non-nil UUID string.
ST4b must recompute and compare the digest, not blindly echo the job value.
Mutable deployment `status`, timestamps, the digest itself and outer
deployment metadata are excluded. A stopped record preserves its definition
and digest; a changed immutable definition receives a new digest.

## Fully materialized shape

All named fields below are required, even fields which a user-facing builder
may initially default. Defaults must be materialized **before hashing**; a
receiver never supplies defaults while verifying. Unknown fields and
case aliases are rejected at every fixed-shape object level.

- Definition: `stream_id`, `version`, `node_id`, `sources`, `pack`, `escalate`,
  `evidence`, `time`, `synthetic`.
- `sources` is an ordered array of 1–16 unique source IDs. Each source has
  `id`, `type`, `parser`, `path`, `bind`, `port`. File sources require an
  absolute path without NUL or `..` segments and explicit null bind/port;
  syslog sources require explicit null path, a bind and integer port
  1024–65535. No local path or listener policy approval is implied.
- Pack: `id`, `version`, `digest`; the digest is exactly `sha256:` plus 64
  lowercase hex characters and still needs trusted artifact resolution.
- Escalate: `flow_id`, `flow_version_id`, `input_map`, `max_per_hour` (1–60),
  `max_concurrent` (1–2), `max_run_cost_micro_usd`, `daily_cap_micro_usd`.
  The flow UUIDs are canonical and non-nil. `input_map` contains 1–64 named
  flow inputs with one of `$alert`, `$evidence`, `$evidence.samples`,
  `$evidence.digests`, `$match_count`, `$logs`; existence and tenancy of those
  inputs are separate platform checks.
- Evidence: `mode` (`digest` or `sampled`), `max_lines` (0–5),
  `max_bytes_per_line` (0–2048). NODE_LOCAL policy remains an independent
  mandatory restriction; having a valid hash cannot widen evidence policy.
- `version` is an integer from 1 through the safe maximum above. `node_id`
  is a canonical positive decimal **string** fitting uint64, with no leading
  zeroes. IDs/pack versions use bounded ASCII letters, digits, `_`, `.`, `-`.
- This first deployment profile requires `time: "ingest"` and
  `synthetic: true`. Offline ST1 event-time evaluation remains separate.

`Canonical` additionally bounds the input to 256 KiB, 16 nesting levels and
16,384 JSON tokens. File paths are bounded to 4096 UTF-8 bytes, bind strings
to 256 bytes, and flow input names to 128 Unicode scalar values. UUIDs,
IDs and parser names are validated without interpreting user code.

## Shared vectors

`internal/stream/testdata/contracts/definition-v1.json` is the complete
synthetic definition plus its organization. Its zero pack digest is an
intentional **unpublished fixture**, never a trusted deployable pack.
`definition-v1-vectors.json` pins its digest and a Unicode/escaping/safe-number
edge case. For the edge case, replace only definition `version` and
`escalate.input_map` using the listed values; retain all other base fields
and the authenticated organization. The expected canonical input-map bytes
are included to expose UTF-16 ordering and newline escaping mistakes.

Both repositories must commit these exact vector files and assert the
expected digests before enabling deployment. Tests also refuse duplicate
and escaped-duplicate fields, unknowns, omitted nulls, old dollar fields,
fractional costs, overflow, numeric coercion and invalid Unicode.

`negative_definitions` holds 27 reusable refusal cases. Each starts from the
original raw `definition` object bytes in `definition-v1.json`, not a
re-serialized object which may have lost duplicate keys or invalid Unicode.
Every case must be refused; `id` is unique. `replace_first` replaces exactly
one occurrence of `old` with `new` before parsing. `raw_text` supplies a whole
replacement definition; `append_text` appends to the original object bytes.
`nested_arrays` constructs `depth` opening brackets followed by the same
number of closing brackets. `pad_spaces` appends ASCII spaces to the original
valid object to reach the specified total `bytes` length. `raw_hex`
decodes `hex` to bytes so malformed UTF-8 can be tested without corrupting
the vector document itself. Unknown operations are a harness failure, not a
skipped test. In particular, `duplicate-key` and `escaped-duplicate-key` must
not be normalized away before calling the validating canonicalizer.

The four `structural_json` cases isolate the pre-decoding structure guard,
not full definition-schema validation: depth 16 is accepted and 17 refused;
an array of 16,382 zeroes yields 16,384 tokens and is accepted, while 16,383
zeroes is refused. `nested_arrays` uses the operation above; `zero_array`
constructs `count` comma-separated zeroes inside brackets. A separate full
definition control is padded to exactly 256 KiB and must retain the base
digest, while one extra space must be refused. These positive controls keep
invalid schema or malformed JSON from accidentally masking a missing cap.

The original `invalid_utf16_keys` list remains unchanged for existing
consumers. The positive base fixture, base/edge expected digests, domain
prefix and canonicalization profile are unchanged by these added cases.
