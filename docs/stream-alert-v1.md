# Stream alert v1

Source contract for Citadel #1352 (ST1), consumed by ST2 and the paired
Aceteam ST3 intake. This is not deployment authorization. It does not add a
network sender, consumer, action sink, or database schema.

Deployment identity uses [the organization-bound definition digest
profile](stream-definition-v1.md), not the pack framing below. Trusted
pack publication and resolution are specified in
[the pack artifact contract](stream-pack-artifacts-v1.md).

## Two kinds of evaluator output

The shared evaluator returns matches with `schema_version = stream.alert.v1`.
`escalate: false` is a local match only. A live driver must never forward it.
An escalating match becomes eligible for the durable outbox only after the
driver has supplied its enrolled stream ID and definition digest. Offline
evaluation leaves those two fields empty, so its records cannot be used as
live escalation requests. The intake must reject empty or stale deployment
identity, even when a record contains a plausible alert ID.

There is no node-supplied authoritative org or node identity in this object.
ST3 must trust only the authenticated route's server-stamped org, node and
purpose-bound HMAC. It must reject device-supplied reserved identity fields.

## Fields

| Field | JSON representation | Meaning |
| --- | --- | --- |
| `schema_version` | string | Exactly `stream.alert.v1` |
| `alert_id` | 64 lowercase hex characters | Deterministic identity below |
| `stream_id` | string | Enrolled deployment's stream ID |
| `definition_digest` | `sha256:` plus lowercase hex | Exact active deployment |
| `pack_digest` | `sha256:` plus 64 lowercase hex | Reviewed canonical pack |
| `rule_id`, `rule_version` | strings | Manifest rule identity and version |
| `group_key` | `sha256:` plus 64 lowercase hex | Digest of the local grouping key, never its values |
| `window_start_ns`, `window_end_ns` | decimal strings | Evaluation window bounds |
| `match_count` | nonnegative integer | Count, distinct estimate, or sequence length |
| `approximate` | boolean | This match or an ancestor depended on HyperLogLog |
| `t_mono_ns`, `t_wall_ns` | decimal strings | Originating ingest clocks, or the logical timer deadline |
| `clock_state` | string | Driver-supplied clock state, not verified attestation |
| `source_id` | string | Logical configured source ID, not an absolute path |
| `raw_refs` | array of strings | Bounded local references, not raw records |
| `evidence` | object | Evidence policy below |
| `escalate` | boolean | Whether the rule requests escalation |

Nanoseconds are decimal strings to preserve all int64 bits through Next.js
and other JSON consumers. A receiver must validate canonical nonnegative
decimal int64 syntax before interpreting them. Do not round through a
JavaScript `Number`. Window time may be event time, while envelope clocks
remain ingest clocks. The deployment defines which window time mode applies.

## Canonical identities

The local grouping key is compact JSON of alternating field names and scalar
values in the rule's declared grouping order, for example
`["source.ip","192.0.2.1"]`. No grouping fields is `[]`. The public
`group_key` is `sha256:` followed by SHA-256 of those UTF-8 JSON bytes. Hashes
are pseudonymous, not a claim that low-entropy identifiers are anonymous.
Numeric values normalize to a typed object such as `{"$number":"1/10"}`
using the reduced exact rational value; `1`, `1.0` and `1e0` therefore group
together but remain distinct from the string `"1"`. Distinct counts use the
same typed normalization. Missing or null distinct fields are not counted.

`alert_id` is the lowercase SHA-256 hex digest of compact UTF-8 JSON:

```json
["<pack_digest>","<rule_id>","<rule_version>","<group_key>","<window_start_ns>"]
```

All five tuple members are strings. The start is canonical decimal, with no
leading zeroes except `0`. The Go encoder's HTML escaping is part of the
canonical JSON representation. Manifest IDs, versions and public key digests
are restricted to ASCII, so these identities do not depend on Unicode
normalization or object-member ordering. Input references and timestamps
other than window start do not change the identity. The outbox uses this ID
as its uniqueness key; the consumer uses `stream:<alert_id>` to derive the
ordinary flow run identity.

## Evidence and privacy

ST1 always emits:

```json
{"mode":"digest","count":1,"digests":["sha256:<raw-line-digest>"]}
```

There are no raw line samples. The digest covers the exact parser input
bytes for the event that completed the match. `raw_refs` likewise covers
only that event, not the complete correlation history. A timer has count
zero, an empty digest list and no raw references. Do not treat these bounded
references as a complete matched-event receipt or a signed attestation.
Signed episode receipts and recording are ST6, not implemented by ST1.

ST2's sampled policy may add at most five redacted lines of at most 2,048
bytes each. NODE_LOCAL deployments must force digest-only evidence and no
samples. Grouping values stay local in either mode. Original-source evidence
must not be replaced by unrelated events when a timer fires. No customer
logs are approved for the initial demo.

## Pack and evaluator determinism

`pack.yml` is a strict manifest with `id`, `version`, `parser`, optional
`field_map` and `enrichments`, and rule entries containing `id`, `version`,
`file`, optional `optional` and `fixtures`. JSON is valid YAML and is used by
the synthetic fixture pack. A directory resolves to `pack.yml`.

To calculate the pack digest, clear the manifest `digest`, sort its rule
entries by ID and compact-JSON encode the typed manifest. Feed that byte
string to SHA-256, then each rule's relative filename and exact source bytes
in the same order. Each component is framed as its decimal byte length,
one colon and the bytes. Prefix the final hex digest with `sha256:`.
Optional refused rules still contribute their source bytes. Rule-file order,
filesystem enumeration and YAML map ordering cannot change the digest.

Rule source permits neither aliases nor multiple YAML documents. Rule reads
are confined to the selected pack root. Required unsupported rules refuse
the whole pack. Optional unsupported rules are listed in `refused`, never
half-loaded. Cycles and missing dependencies are compilation failures.

Windows default to ingest time. The offline driver supplies a fixed replay
epoch and one millisecond per line instead of reading the machine clock.
Event-time replay uses an explicitly bounded reorder queue and counts events
older than its watermark. Pending queue exhaustion is a failure, not a drop.

Counts have sliding `(t - span, t]` or epoch-anchored tumbling windows.
Ordered sequences require distinct input observations and use an inclusive
maximum span. State caps and every capacity eviction appear in evaluator
statistics. Distinct values are exact through 1,024 hashes, then use a
deterministic 1,024-register HyperLogLog over the retained bounded window.
An estimate is not an exact threshold proof; `approximate` is retained through
dependent windows and ordered sequences, even when a later event is exact.
Deduplication drops the detector signal as well as its emitted match; rate
limiting suppresses escalation only and does not alter downstream detection.

## Verification status

The synthetic Shield inputs and expected flags are preserved in
`internal/stream/testdata/shield`. History matching remains outside ST1.
Tests cover parsers, modifiers, operators, correlations and evaluator
determinism. After bounded offline verification was authorized, the stream
package tests and race checks, targeted CLI tests, stream-package vet checks
and a Linux amd64 build passed. The original seven Shield scenarios pass in
both ingest and event time. Measured throughput and reproducible commands
are recorded in [ST1 offline verification](stream-st1-verification.md).
Required exact-head CI and independent Opus review still precede ready and
merge. ST2 and ST4b remain separate dependent issues.
