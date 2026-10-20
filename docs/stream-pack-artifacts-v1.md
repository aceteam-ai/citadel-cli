# Reviewed stream pack artifacts v1

This is the offline artifact contract for Citadel #1352. It is source data,
not authorization to deploy a stream, read logs, fetch content, or run a flow.
The initial artifact contains the existing synthetic Shield fixture rules.
It is not a complete production threat-detection pack or a license to use real
customer logs. The runtime, permission gates and deployment lookup remain
separate work.

## Publication authority and resolution

The only v1 origin is the repository `aceteam-ai/citadel-cli`. Pack changes
and index changes go through a reviewed PR and normal protected merge.
Aceteam must ship an explicit reviewed pair in consumer source:

```text
(exact 40-character lowercase Git commit, exact packs/index.json SHA-256)
```

There is deliberately no commit field inside the index: that would create a
self-reference when the index is committed. The consumer pin is chosen and
reviewed after publication. A PR branch, `main`, tag, user-controlled URL,
repository, filesystem path or redirect is never a substitute for that pin.
There is no live downloader or resolver added by this change. A future
resolver may obtain bytes only from the fixed repository at that pinned
commit, using the validated index's exact bundle path; it must fail closed on
unavailable or mismatched content. No fallback to another ref or unreviewed
caller-supplied rule bytes is allowed.

A deployment selects an exact `(id, version, digest)` already present in the
index. It cannot choose a locator. Digest verification proves byte identity,
not review, provenance signatures, licensing, or publisher authenticity by
itself. The reviewed consumer-source pin supplies the authority. The offline
helper checks the commit's syntax only; it cannot prove that the commit exists
or was merged. Do not populate a production pin from request arguments.

## Index schema

`packs/index.json` is UTF-8 JSON. The only allowed members are:

| Object | Required members |
| --- | --- |
| Index | `schema_version`, `origin`, `packs` |
| Each pack entry | `id`, `version`, `digest`, `bundle_path`, `bundle_sha256` |

`schema_version` is exactly `stream.pack-index.v1`; `origin` is exactly
`aceteam-ai/citadel-cli`. `packs` has 1 to 128 entries. `id` and `version`
use the existing bounded manifest identity syntax: 1 to 128 ASCII letters,
digits, `.`, `_`, or `-`. The initial version is `1.0.0`. Every identity pair
is unique. `digest` and `bundle_sha256` are `sha256:` followed by exactly 64
lowercase hexadecimal characters. `bundle_path` must be exactly:

```text
packs/<id>/<version>/bundle.json
```

It must also be a valid relative slash-separated path without traversal,
backslashes or empty segments. The index is at most 256 KiB. Unknown members,
duplicate object keys, invalid UTF-8 and malformed JSON are rejected. Its pin
is SHA-256 of the exact original index bytes, including indentation and final
newline; consumers must not hash a parsed/re-serialized approximation.

All member names are exact-case, including nested entries: `ID`, `File` and
`SCHEMA_VERSION` are not aliases. For this v1 publisher encoding, non-BMP
characters in outer JSON strings must be literal UTF-8, not JSON surrogate
escape pairs. The shared structural guard refuses the latter even when valid
JSON. Go's publisher encoding already uses literal UTF-8. This restriction
does not alter base64 rule bytes or permit replacing their original bytes.

## Bundle schema and limits

The only bundle members are:

```json
{
  "schema_version": "stream.pack-bundle.v1",
  "manifest_base64": "<canonical manifest bytes, padded base64>",
  "rules": [
    {"file": "rules/example.yml", "source_base64": "<exact rule bytes, padded base64>"}
  ]
}
```

There are no archives, executable hooks, include URLs, imports, or secondary
locators. The bundle has 1 to 128 rule entries. Unknown/duplicate members are
rejected. Its exact original bytes must match the index's `bundle_sha256`
before decoding the base64 payloads.

The JSON envelope is at most 12 MiB; decoded manifest plus all decoded rules
is at most 8 MiB. The manifest is at most 256 KiB and each rule at most 1 MiB.
Encoded lengths and aggregate decoded sizes are checked before base64 payload
allocation. Base64 is canonical padded RFC 4648 standard-alphabet encoding:
no whitespace, URL alphabet, missing padding or non-zero unused padding bits.

The bundle's rule list must exactly match the canonical manifest's rule list
in order. No missing, duplicate, additional, reordered or escaping filenames
are allowed. Decoded source bytes retain original line endings and final
newlines. The shared compiler validates every rule and dependency and
reproduces the inner pack digest. Optional refused rules retain the existing
`LoadPack` behavior and remain visible; they are not half-loaded. The initial
synthetic bundle has no refusals.

## Canonical manifest bytes

The artifact manifest is the **existing LoadPack digest representation**, not
new JSON canonicalization. `CanonicalManifestBytes` is the Go reference.
After strict decoding, the helper reproduces these bytes and compares them
byte-for-byte to the decoded `manifest_base64` value. Whitespace, alternate
field order, an explicit empty optional member or an appended newline is not
accepted as an alternative representation.

The representation is compact UTF-8 JSON, without a trailing newline, using
these Go `Manifest` field positions:

```text
id, version, parser, field_map?, enrichments?, rules
```

`digest` is cleared and omitted. `field_map` and `enrichments` are omitted when
nil or empty. `rules` is sorted by rule ID using ASCII lexical order, and each
`RuleEntry` uses this field order:

```text
id, version, file, optional?, fixtures?
```

`optional` is omitted when false. `fixtures` is omitted when nil or empty.
Other required string fields are retained even when empty, though compilation
then rejects invalid identities. Arrays other than the sorted rule list retain
their declared order. Map keys are sorted lexically by Go `encoding/json`.
Strings use that encoder's escaping, including `\u003c`, `\u003e`, `\u0026`,
`\u2028` and `\u2029`; printable non-ASCII text otherwise remains UTF-8.
There is no Unicode normalization. A cross-language consumer must reproduce
this exact representation or compare against its reviewed canonical bytes;
generic sorted-object JSON/JCS is not equivalent.

For the pack digest, frame the canonical manifest bytes, then each canonical
rule's relative filename and exact source bytes, all in the same rule-ID
order. A frame is the ASCII decimal byte length, one colon and the bytes,
without an additional separator. SHA-256 of the concatenation, prefixed by
`sha256:`, must equal the selected index entry's `digest`. Optional refused
rules still contribute bytes. This is unchanged from local `LoadPack`.

## Offline API

The pure Go helpers in `internal/stream/rules/artifacts.go` perform no I/O:

```go
ParsePinnedPackIndex(pin ArtifactPin, indexData []byte) (*PackIndex, error)
(*PackIndex).Find(id, version, digest string) (PackArtifactEntry, error)
CompilePinnedPack(pin ArtifactPin, indexData, bundleData []byte, id, version, digest string) (*Pack, error)
```

`ArtifactPin` has `Commit` and `IndexDigest`. The combined compile helper
revalidates the pin/index rather than trusting a caller-constructed entry. It
uses exactly the same manifest normalization, rule compiler and graph ordering
as local `LoadPack`; the artifact is not an alternate evaluator.

## Shared vectors

`packs/vectors/v1.json` is the authoritative portable vector data. Both repos
must consume the same file unchanged. It identifies the exact index, bundle,
pack digest and `canonical_manifest_base64` and defines 22 positive/tamper
cases. Its `fixture_commit` is an intentionally synthetic syntax-test value,
not a published commit or a production pin.

Every case starts from the original pinned index/bundle bytes. Operations are:

| Operation | Mutation |
| --- | --- |
| `none` | No change; must compile |
| `index_append_space`, `bundle_append_space` | Append one ASCII space to the named original byte string |
| `rule_append_newline` | Append one LF byte to the first decoded rule source, then re-encode base64 |
| `remove_last_rule`, `append_unknown_rule` | Remove the final rule, or append `rules/unknown.yml` with the first rule's bytes |
| `swap_first_rules`, `duplicate_first_file` | Swap the first two entries, or replace the second filename with the first |
| `manifest_append_space` | Append one ASCII space to decoded canonical manifest bytes |
| `manifest_sort_keys` | Recursively serialize parsed manifest object keys in lexical order rather than the specified struct field order |
| `manifest_empty_field_map` | Insert `"field_map":{}` immediately before `enrichments`, leaving all other canonical bytes unchanged |
| `rule_base64_newline` | Append LF to the first encoded rule string |
| `unknown_bundle_field` | Add top-level `"unknown":true` to the bundle |
| `duplicate_index_origin` | Duplicate the index's `origin` member with the same value |
| `index_path_escape` | Change the entry's bundle path to `../bundle.json` |
| `non_commit_pin` | Change the external fixture pin's commit to `main` |
| `unknown_pack_digest` | Select `sha256:` plus 64 zeroes |
| `index_wrong_origin`, `index_future_schema` | Set index `origin` to `attacker/repository`, or schema to `stream.pack-index.v2` |
| `index_duplicate_entry` | Append a copy of the first pack entry |
| `index_uppercase_digest` | Uppercase the first entry's entire `digest` string |
| `index_url_path` | Change the first entry's bundle path to `https://example.test/bundle.json` |

When `refresh_outer` is true, this **test only** deliberately updates the
entry's bundle SHA and the external index SHA to the mutated exact bytes.
This isolates inner validation from outer hash failure; it does not model an
untrusted caller gaining permission to refresh production pins. The selected
inner pack digest remains unchanged. The duplicate-origin insertion happens
after serializing the mutated index, before updating its test SHA. Cases
without refresh preserve the original outer pins. The `reason` member is a
diagnostic substring used by the Go harness, not a required public error API.

`artifacts_test.go` implements those mutations and proves local-pack/artifact
equivalence against all seven original shared Shield log files, in both time
modes. The common original fixtures are not regenerated or duplicated inside
the bundle. This leaves history matching in the escalated flow as designed.

`json_schema_cases` additionally holds 17 portable parser-only cases for
index/bundle coercion, case aliases, duplicate and escaped-duplicate member
names, and the outer-artifact Unicode restriction. Each starts from the exact
original index or bundle bytes selected by `target`, replaces the first `old`
byte string with `new`, then invokes strict JSON artifact decoding without
compilation. `accept` specifies the expected decoding result. Missing mutation
targets, duplicate case IDs and unknown targets are harness failures. These
cases intentionally isolate parser behavior from the outer byte pins and
inner pack identity; they do not authorize refreshing production pins.

The valid escaped astral pair `\ud800\udc00` is refused in an outer artifact
filename, whereas its literal UTF-8 representation is accepted by the parser.
Literal backslash-u text is not a Unicode escape and remains parseable. Neither
positive parser case is a published pack: full compilation must still verify
the exact reviewed filename set, canonical manifest and pack digest. The base
bundle/index bytes, their SHA pins and all positive pack/definition digests
remain unchanged.

## Verification and remaining gate

Run the bounded offline suite from the repository root:

```bash
GOMAXPROCS=4 go test -p 4 ./internal/stream/rules
```

This suite passed after the artifact helper and vectors were added. Exact-head
CI and independent review remain required before protected merge. These
helpers do not perform live resolution, registry uploads, deployment,
credential changes or customer-log processing.
