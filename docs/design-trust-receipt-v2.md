# Design: Trust Engine S2/S3 — `AEPReceiptV2` and the signed receipt digests (aceteam #8253)

**Status:** design only, no code. Plan of record for citadel-cli#1002 (`[#8253 S3]`),
with the prerequisites and open decisions that must be settled before it is
dispatched. Read alongside
[`design-node-identity-receipts.md`](./design-node-identity-receipts.md) (the v1
receipt design; §"Threat model" and §"Scope boundary" there stand unchanged) and
the aceteam #8253 **delta Design of Record (2026-09-06)**, which is the authority
this doc builds against — see §0 for exactly which of its decisions are already
ratified and which this doc re-opens.

---

## 0. Context — what has actually shipped, and what this doc corrects

The brief for this doc assumed three things that a read of both repos shows are
no longer true. They change every section below, so they come first.

### 0.1 Slices that have landed

| Slice | Repo | State (verified 2026-09-09) | Where |
|---|---|---|---|
| S1 — single post-completion hook | citadel-cli | shipped (PR #1009, v2.150.0) | `applyTrustEngine` / `trustVerdictMap`, `internal/worker/llm_inference.go` |
| S6 — pure Secret/PII/FERPA detectors + unsigned `verdict_hash` | citadel-cli | shipped (PR #1011, v2.152.0) | `internal/trust/detectors.go`, `internal/trust/verdict.go` |
| **S2 — verifier learns `receipt_version`, v2 canon, second call site** | **aceteam** | **shipped** — aceteam#9287 CLOSED 2026-09-09 (PRs #9409, #9371, #9461) | `python-backend/utils/aep_receipt_verify.py` (`canonicalize_receipt_v2`, `_resolve_receipt_version`), `routes/fabric_dispatch.py:_verify_dispatch_aep_receipt` |
| A1/A2 — verifier + `execution_envelopes.envelope.attestation` storage | aceteam | shipped (PR #8965) | same module; `AEPReceiptVerification.to_attestation()` |
| Fabric CA + `fabric_node_certs` key registration (QR-pairing path) | aceteam | live | `routes/fabric_pairing.py` -> `utils/fabric_ca/authority.py` |

**Consequence:** citadel-cli#1002 (S3) is unblocked *today*. Its stated blocker
("S2 must deploy first", DoR G7) is satisfied.

### 0.2 Three corrections to the brief

1. **The V2 canonical byte format is not an open question — it is ratified and
   deployed on the verifier side.** The DoR §3 fixed the fifteen-field order and
   the formatting rules; aceteam's `canonicalize_receipt_v2` implements it and
   `tests/test_aep_receipt_verify.py::TestCanonicalizeReceiptV2::test_matches_go_canonical_format`
   pins the exact bytes. S3's job is to make the Go side *emit* those bytes, not
   to design them (§1). The brief's "S2 = receipt v1.1 with content digests but
   not yet signed" is the **superseded 2026-09-03 DoR's C1**; the 2026-09-06
   delta inverted the ordering (G7: verifier first, then the node) and re-sliced
   it as S2 (aceteam) / S3 (citadel). §6 explains the split.
2. **The float-formatting question for the receipt's own `score` field is
   closed** (`FormatFloat(v, 'f', 6, 64)` on the Go side, `f"{float(v):.6f}"` on
   the Python side, both for v1 and v2). The float question that is genuinely
   still open sits one level down: the **preimage** of `verdict_hash` (and,
   at S5, `policy_hash`), which the verifier carries as opaque strings today but
   which the DoR's audit story ("the verdict is recomputable") and S9's offline
   `verify-bundle` will recompute. That is where §3's options go.
3. **"Inert until the aceteam side lands" is wrong as a blanket statement.** The
   verifier, its storage, and the direct-dispatch call site are live. What is
   actually not-yet-live is narrower (§9): `content_bound` on the agent-chat
   call site (`core/agent_logic.py` passes no expected digests until S4),
   `policy_bound` everywhere (S4/S5), leaf certificates for `citadel login`
   nodes (S8) — and one item this doc is the first to surface, §4.

### 0.3 The finding that gates S3 (read §4 before anything else)

**The receipt-signing key and the CA-registered key are two different keys on
every real node**, as built. The DoR §3 says "the same key that backs the
pairing CSR"; CLAUDE.md's *"Machine-convergent by construction"* entry documents
the signing store as *deliberately separate* from `nodeidentity.Default()`. Both
are accurate descriptions of intent; together they mean the verifier can never
find a `fabric_node_certs` row matching a real receipt's
`public_key_fingerprint`. The cross-repo golden fixture (test key on both sides)
would pass green while proof-plan item 6 (a real turn on node 1297 producing
`verified: true`) fails. §4 has the mechanism and the options; it is an S3
prerequisite and Jason-gated (it touches which key is registered).

---

## 1. `AEPReceiptV2` shape and canonical bytes

### 1.1 The fields (ratified — DoR §3; pinned by aceteam's `_CANONICAL_FIELDS_V2`)

Fifteen canonical fields, in this order, newline-joined (`\n`), **no trailing
newline**, UTF-8. `receipt_version` is first so a verifier branches before
parsing the rest.

| # | field | canonical rendering | source on the node |
|---|---|---|---|
| 1 | `receipt_version` | the literal `2` | constant |
| 2 | `node_id` | string as-is | `aep.ResolveNodeID` (fabric node ID if echoed, else the signer's fingerprint — unchanged from v1) |
| 3 | `job_id` | string as-is | `job.ID` |
| 4 | `issued_at` | RFC3339 UTC, e.g. `2026-09-06T12:00:00Z` | `now.UTC().Format(time.RFC3339)` (unchanged) |
| 5 | `engine` | string as-is | `payload.Backend` |
| 6 | `model` | string as-is | `payload.Model` |
| 7 | `input_sha256` | `sha256:<64 lowercase hex>` | §2.2 |
| 8 | `output_sha256` | `sha256:<hex>` | §2.1 |
| 9 | `policy_hash` | `sha256:<hex>` | §2.3 |
| 10 | `action` | `pass` \| `flag` \| `block` | `trust.Verdict.Action` (`block` cannot occur before S5) |
| 11 | `verdict_hash` | `sha256:<hex>` | `trust.Verdict.VerdictHash`, preimage per §3 |
| 12 | `grounded` | `strconv.FormatBool` → `true`/`false` | unchanged from v1 |
| 13 | `score` | `strconv.FormatFloat(v, 'f', 6, 64)` → e.g. `0.500000` | unchanged from v1 |
| 14 | `claims_checked` | `strconv.Itoa` | unchanged from v1 |
| 15 | `flagged_hash` | **bare** hex (no `sha256:` prefix) | unchanged from v1 — see note |

Then, populated **after** signing and **excluded** from the canonical bytes
(same rule as v1): `signature` (base64-std of the ASN.1 DER ECDSA-SHA256
signature over exactly those bytes) and `public_key_fingerprint`
(`sha256:<hex>` of the DER SubjectPublicKeyInfo — `Store.PublicKeyFingerprint`,
matching the verifier's `_spki_fingerprint`; deliberately NOT the certificate
fingerprint).

**Worked example — these are the exact bytes the Python test pins**, reproduced
here so a Go golden can assert against the same literal
(`TestCanonicalizeReceiptV2.test_matches_go_canonical_format`):

```
2\nn1\nj1\n2026-09-06T12:00:00Z\nbonsai\nbonsai-27b\nsha256:in\nsha256:out\nsha256:pol\nflag\nsha256:vh\ntrue\n0.500000\n2\ndeadbeef
```

Two non-obvious constraints the verifier imposes that the Go emitter must honor:

- **Every one of the fifteen fields (plus the two signature fields) must be
  non-empty.** `verify_aep_receipt` treats `None` or `""` in any required
  field as *malformed* (`"receipt missing required field(s)"`), not as "not yet
  enriched". This is why §2.3 needs an interim `policy_hash` before S4/S5 exist,
  and why `output_sha256` must be emitted even for an empty `content` (§2.1).
- **`receipt_version` must be exactly the string `"2"`** on the wire.
  `_resolve_receipt_version` compares `str(raw_version) == "2"`, so a JSON
  number `2` would also pass — but emit the string, since the canonical
  rendering is the literal `2` either way and the DoR table shows `"2"`.

**Prefix inconsistency, accepted.** `flagged_hash` stays bare hex (v1
continuity, and S6 already made grounding's `evidence_hash` equal it
byte-for-byte), while every *new* digest is `sha256:`-prefixed to match
`provenance_receipts.py` (DoR G9). The verifier treats all of them as opaque
strings inside the canon, so the inconsistency is cosmetic — but it must not be
"fixed" in S3, because changing `flagged_hash`'s rendering would change the v2
canon the deployed verifier recomputes.

### 1.2 Go representation

`internal/aep` gains `AEPReceiptV2` alongside — not replacing — `AEPReceiptV1`:

```go
type AEPReceiptV2 struct {
    ReceiptVersion string  `json:"receipt_version"` // always "2"
    NodeID         string  `json:"node_id"`
    JobID          string  `json:"job_id"`
    IssuedAt       string  `json:"issued_at"`
    Engine         string  `json:"engine"`
    Model          string  `json:"model"`
    InputSHA256    string  `json:"input_sha256"`
    OutputSHA256   string  `json:"output_sha256"`
    PolicyHash     string  `json:"policy_hash"`
    Action         string  `json:"action"`
    VerdictHash    string  `json:"verdict_hash"`
    Grounded       bool    `json:"grounded"`
    Score          float64 `json:"score"`
    ClaimsChecked  int     `json:"claims_checked"`
    FlaggedHash    string  `json:"flagged_hash"`

    Signature            string `json:"signature,omitempty"`
    PublicKeyFingerprint string `json:"public_key_fingerprint,omitempty"`
}
```

`CanonicalizeV2(*AEPReceiptV2) []byte` walks the first fifteen fields in
declaration order, exactly as `Canonicalize` walks v1's nine. `BuildSignedReceiptV2`
mirrors `BuildSignedReceipt`'s contract (never returns a partially-signed
receipt; `now` injected; fail-open is the caller's decision) and takes a small
`V2Inputs{InputSHA256, OutputSHA256, PolicyHash, Action, VerdictHash}` struct
rather than five positional strings, so a caller cannot transpose two
`sha256:`-shaped arguments silently. `ToMap()` is unchanged in purpose (the
typed-pointer-in-a-`map[string]any` hazard from v1 still applies).

`AEPReceiptV1` and `Canonicalize` stay exactly as they are: the deployed
verifier still accepts v1, and the v1 tests (`TestCanonicalize_*`) keep pinning
that nothing about the v1 bytes moved.

---

## 2. The three content digests

### 2.1 `output_sha256` — settled

`sha256:` + hex(sha256(UTF-8 bytes of `result.Output["content"]`)). The node
already computes exactly this as the unsigned `trust_verdict.output_sha256`
(`sha256Hex(content)` in `trustVerdictMap`); the backend's
`_expected_output_sha256` hashes the same `content` string the same way. Every
one of the ten engine paths sets `Output["content"]` from the *final* content
(the streaming paths from the accumulated end-of-stream text, never from
re-joined chunks), which is what makes one hook site sufficient — the DoR G9
rule "hash the `end` event's content, never the concatenated chunks" is
satisfied by construction of `applyTrustEngine`.

Edge: for a tool-calls-only reply `content == ""`. The node still emits
`sha256:` + hex(sha256("")) (the canon requires a non-empty field); the backend
side returns `None` for empty content and simply skips content binding
(`content_bound` stays `false`, no failure). Correct and honest on both sides;
no special-casing needed.

### 2.2 `input_sha256` — a real cross-repo byte contract, and the brief's one genuinely open engineering question

**What the backend hashes today** (`routes/fabric_dispatch.py:_expected_input_sha256`):
`json.dumps(messages)` with Python defaults — separators `", "` / `": "`,
`ensure_ascii=True`, dict insertion order — when `messages` is present; else the
bare `prompt` string. This is what the DoR G9 means by "the exact bytes the
platform XADDed": the enqueue at `fabric_dispatch.py:~1110` writes
`"payload": json.dumps(outgoing_payload)` with the same defaults, and Python's
`json.dumps` is compositional, so the `messages` value inside that payload
string is byte-identical to `json.dumps(messages)` on its own.

**What the node has at `applyTrustEngine` time: none of those bytes.** Both job
sources decode the payload string into `map[string]any`
(`internal/redis/client.go` `json.Unmarshal` of the `payload` field;
`internal/redisapi/jobs.go` likewise), and `parseLLMInferencePayload` then
`json.Marshal`s that map (Go sorts map keys, uses compact separators, does not
`\u`-escape non-ASCII) before unmarshaling into `LLMInferencePayload`. G9 is
explicit that `promptTextFromPayload`'s `"\n"`-joined text must NOT be hashed
(a lossy rendering the platform cannot reproduce) — so today the node cannot
produce a matching `input_sha256` at all. This must be designed, not assumed.

**Option I-A — raw-byte retention (recommended for S3).**
- Both sources keep the payload string they already hold before unmarshaling it:
  a new `Job.RawPayload []byte` (set at `client.go`'s and `jobs.go`'s decode
  sites; nil for a source that does not carry it).
- `parseLLMInferencePayload` decodes from `RawPayload` when present (falling
  back to the existing map re-marshal), through a sibling struct
  `{ Messages json.RawMessage `json:"messages"` }` — `json.RawMessage`
  preserves the value's bytes verbatim, whitespace and `\uXXXX` escapes
  included. `LLMInferencePayload` gains `MessagesRaw json.RawMessage` (not
  forwarded to any engine; used only for the digest).
- `input_sha256 = sha256Hex(string(MessagesRaw))` when `messages` is present,
  else `sha256Hex(payload.Prompt)` (the decoded string — matching the backend's
  bare-`prompt` branch exactly, since a JSON string round-trips to the same Go
  string).
- Why the bytes survive the trip: the API proxy (`app/api/fabric/redis/jobs/consume/route.ts`)
  copies XREADGROUP field values into the response verbatim (the
  `data[fields[i]] = fields[i+1]` loop) and `redisapi.types.Payload` is a
  `string`, so API mode sees the same bytes direct-Redis mode does.
- Cost: zero aceteam change; matches the shipped `_expected_input_sha256`.
- **Fragility, stated plainly:** any re-serializer inserted between the XADD and
  the node silently breaks `content_bound` — not verification. Known candidates:
  the Go-side `PushJob` (`client.go:~612`, re-marshals `job.Payload` — only
  matters if a Go component ever enqueues an `llm_inference` job), the
  WebSocket consume path (`app/api/fabric/redis/ws/route.ts`, not audited
  here), or a future proxy rewrite in a language whose JSON serializer differs.
  The failure mode is `unverified` with reason `"content"` — loud, and the
  DoR's honest state — never a silently-green false `content_bound`.

**Option I-B — canonical-value hashing.** Both sides hash
`canonical_json(messages)` (the aceteam-aep canonicalizer, §3.3) of the
*decoded value*. Serializer-independent, survives any re-serialization, and
reuses the Go `canonical_json` port S5 needs anyway.
- Cost: an aceteam change to `_expected_input_sha256` (it currently uses
  `json.dumps` defaults), so S3 would depend on a second aceteam PR after S2;
  and it couples S3 to the Go port's correctness on arbitrary message content
  (tool-call arguments, multimodal parts) rather than on a fixed policy shape.
- `ensure_ascii=True` in the aep canonicalizer means every non-ASCII character
  in every user message goes through the Go port's `\uXXXX` path — the highest-
  surface-area place to get the port subtly wrong.

**Recommendation: I-A for S3**, with the fragility documented in the receipt
package doc and a proof-plan mutation (§8: re-serialize the payload through Go
and assert `input_sha256` changes). Revisit I-B if a second consumer of the
input digest appears (e.g. the #8033 provenance chain wanting a
position-independent content id), at which point the Go canonicalizer already
exists.

### 2.3 `policy_hash` before S4/S5 exist — the interim value

S4 (aceteam attaches `policy` to the dispatch payload) and S5 (citadel reads it,
ports `canonical_json`, hashes the received bytes) have not landed. The canon
requires a non-empty `policy_hash` regardless. Jason ratified DoR §7.4 on
2026-09-07: *absent policy means still governed at `gate: flag`, with
`policy_hash` of the explicit empty policy, never omitted.*

**Recommendation:** S3 emits `policy_hash = sha256:` + hex(sha256(`{}`)) =

```
sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a
```

whenever the payload carries no `policy` key. This value is
*canonicalizer-independent* — every JSON canonicalizer renders the empty object
as the two bytes `{}` — so S3 can emit it before the Go `canonical_json` port
exists, and the Go golden's committed `policy.json` can be `{}`, for which
Python's `canonical_json({})` is `b"{}"` → the honest fixture verifies with
`policy_bound: true`, satisfying citadel#1002's acceptance line.

Two things this interim does NOT settle (open question §11.5): whether the
"explicit empty policy" stays the literal `{}` at S5 or becomes the full-shape
default (`{"version":1,"checks":[...],"gate":"flag","block_on":[],"flag_on":[]}`),
which would change the hash; and a **sequencing hazard**: if S4 starts passing
`expected_policy_hash` (the hash of a real sent policy) to the verifier while
pre-S5 nodes are still hashing `{}`, every such receipt flips to
`unverified`/`policy` — loud, but a regression, not a signal. §9 states the
deploy-order rule that avoids it.

---

## 3. The `verdict_hash` preimage — the float question, and which canonical JSON

### 3.1 What ships today (S6) and why it cannot be the signed preimage

`trust.computeVerdictHash` hashes `json.Marshal` of a typed Go struct:
declaration-ordered keys (`action`, `checks`, `grounding`; within a check
`name, version, action, severity, evidence_hash`), Go's float rendering for
`grounding.score`, Go's HTML-safe escaping (`<`, `>`, `&` rendered as `\u003c`, `\u003e`, `\u0026`).
Deterministic within Go — S6 said so and pinned it — but not reproducible from
Python without re-implementing Go's `encoding/json`, which is the wrong
direction: the DoR G8 already commits the *Go* side to porting the *Python*
canonicalizer for `policy_hash`, so `verdict_hash` should ride the same port,
not add a second Go-shaped canon the Python side must mirror.

The verifier does not recompute `verdict_hash` today (it is an opaque string
inside the canon), and nothing in aceteam stores or compares it — a `grep`
shows it is only carried into `attestation` for display. So **changing the
unsigned S6 value is safe now**; it will not be safe once S9's `verify-bundle`
or the audit tool recompute it. S3 is the last cheap moment to fix the preimage.

### 3.2 The concrete hazard — the majority case, not a corner

`grounding.score` is a float64. For the single most common value — `1.0`, every
claim-free reply — Go `json.Marshal` emits `1`; Python `json.dumps` emits `1.0`;
JavaScript `JSON.stringify` emits `1`. Divergence also occurs across the
exponent thresholds (Go switches to `e` notation below `1e-6` / at or above
`1e21`; Python's `repr` below `1e-4` / at or above `1e16`) and in the exponent's
own rendering. So any preimage that carries `score` as a JSON *number* is
byte-divergent between the Go emitter and the Python verifier on the common
path, not just on pathological inputs.

### 3.3 Which `canonical_json`

Two Python canonicalizers already exist in aceteam and they differ:

| | `aceteam_aep.attestation.canonical_json` | `utils/provenance_receipts._canonicalize` |
|---|---|---|
| key order | sorted, every depth | sorted, every depth |
| separators | compact `,` `:` | compact |
| non-ASCII | `ensure_ascii=True` → `\uXXXX` (lowercase hex; astral as surrogate pairs) | `ensure_ascii=False` (raw UTF-8) |
| `None`/`null` | **dropped from dicts** (kept in lists) | kept as `null` |
| floats | Python `repr` via `json.dumps` | Python `repr` via `json.dumps` |
| used by | server-side verdict hashing; named by DoR G8 as the one Go must port for `policy_hash` | #8033 provenance chain (TS parity) |

**Recommendation: the aceteam-aep `canonical_json`, for both `policy_hash` (S5,
as the DoR already says) and `verdict_hash` (S3, this doc).** One Go port, one
Python-generated golden (proof-plan item 4), one discipline. The Go port
(`internal/trust/canonjson.go`, or `internal/aep`) must reproduce, and be
pinned against a Python-generated golden for: sorted keys at all depths;
compact separators; `ensure_ascii=True` escaping (`"`, `\`, `\n`, `\r`, `\t`,
`\b`, `\f`, other `< 0x20` as `\u00XX`, all non-ASCII as `\uXXXX`, non-BMP as
surrogate pairs — Go's `encoding/json` does none of the last two on its own);
**HTML escaping disabled** (`Encoder.SetEscapeHTML(false)`, or a hand encoder —
Go's default `\u003c` for `<` is NOT what Python emits); `nil` dropped from
maps but `""` retained (Python drops only `None`; an empty-string `severity` is
kept and is load-bearing in the check shape). A hand-written encoder over a
small value model (`map`, `[]any`, `string`, `bool`, `int64`, `nil`, and the
string-typed score below) is smaller and safer than post-processing
`encoding/json` output.

### 3.4 Options for the float, and the recommendation

**Option A — `score` as its `'f' 6` string inside the preimage object
(recommended).** The preimage carries `"score":"0.500000"`, produced by the
SAME `strconv.FormatFloat(v,'f',6,64)` the receipt canon already uses, and
recomputed on the Python side by the SAME `f"{float(v):.6f}"` the verifier
already contains. One float rule in the entire receipt system: *a score is
rendered `'f' 6`.* The wire `trust_verdict.grounding.score` stays a JSON number
for display; only the hash preimage stringifies it. Precedent: `canonVerdict`
in S6 is already a projection of the wire map, not the wire map itself.
- Pro: reuses a rule both sides already implement and test; the preimage
  contains no floats at all, so the Go `canonical_json` port needs no float
  branch (it can reject float64 outright, which is itself a guardrail).
- Con: a string-typed number in a hashed object looks odd to a reader; document
  it once in the package doc.

**Option B — integer-scaled score (`score_ppm`: `int(round(score*1e6))`).**
Integers render identically in Go, Python, and JS (well within 2^53).
- Pro: type-honest (a number stays a number).
- Con: introduces a *second* representation of score nobody else in the system
  uses; the verifier and S9 must learn a conversion rule instead of reusing the
  `'f' 6` line they have; rounding must be specified identically (`math.Round`
  half-away-from-zero vs Python's banker's `round`) — a fresh divergence hazard
  exactly where A has none.

**Option C — RFC 8785 (JCS) on both sides.** ES6 `Number.toString` semantics
for doubles. Principled and self-describing.
- Con: Go stdlib does not produce ES6 number formatting
  (`FormatFloat(v,'g',-1,64)` differs on exponent form and thresholds);
  Python's `json.dumps` does not either (`1.0` vs `1`). Both sides need a
  library (`cyberphone/json-canonicalization` for Go, `jcs` for Python — and
  matrix's `canonicaljson` refuses floats entirely, so library choice matters),
  which is a new dependency on each side to solve a problem A solves with zero.
  Also breaks the "one canonicalizer" rule of §3.3 unless the aep canonicalizer
  is itself replaced fleet-wide, which is out of this issue's scope.

**Recommendation: A**, on the grounds that it is the only option that adds no
new rule, no new dependency, and no new rounding semantics — and that the
receipt canon already forced this exact decision once (v1's `score` line) and
the answer should not differ one level down.

### 3.5 The v2 `verdict_hash` preimage, byte-exact

`verdict_hash = "sha256:" + hex(sha256(canonical_json(P)))` where `P` is:

```
{
  "action":      "<pass|flag|block>",
  "checks":      [ {"action":"..","evidence_hash":"<bare hex>","name":"..","severity":"..","version":1}, ... ],  // node's check order preserved (lists are not sorted)
  "grounding":   { "claims_checked": <int>, "grounded": <bool>, "score": "<'f' 6 string>" },
  "policy_hash": "sha256:<hex>"
}
```

rendered by the aep canonicalizer, i.e. sorted keys at every depth, compact,
`ensure_ascii`. Worked example for a two-check verdict (grounding flagged one of
two claims, secrets clean, no policy):

```
{"action":"flag","checks":[{"action":"flag","evidence_hash":"<bare hex>","name":"grounding","severity":"","version":1},{"action":"pass","evidence_hash":"<bare hex>","name":"secrets","severity":"","version":1}],"grounding":{"claims_checked":2,"grounded":false,"score":"0.500000"},"policy_hash":"sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a"}
```

What this changes versus S6's shipped preimage, all deliberate: keys sorted
rather than declaration-ordered; `score` as a string; `policy_hash` added at
the top level (the DoR's `trust_verdict` shape has it there — S6 left the slot
empty because S5 owns the real value, but the interim `{}` hash from §2.3 fills
it now so the preimage shape does not change again at S5). Still excluded, per
the DoR: `grounding.flagged` (raw evidence — bound through `evidence_hash`),
and the receipt-level `output_sha256`/`verdict_hash` themselves (a hash that
covers its own field). `Extras` on a check (grounding's display-only
`grounded/score/claims_checked` copies) stay excluded as in S6.

**`evidence_hash` / `flagged_hash` stay Go-json-defined and are NOT made
cross-language here.** Their preimages are `json.Marshal` of the finding/claim
lists, and their inputs (the findings themselves) never leave the node — only
the citadel binary can recompute them (the DoR §3 audit-tool mitigation), so
cross-language parity buys nothing today. They are carried opaque in both
canons. Note for whoever does eventually make them cross-language: Go's
HTML escaping of `<>&` in `Finding.Source` / `Claim.Value` is the byte
divergence that will bite.

---

## 4. Prerequisite: the signing key must be the registered key

### 4.1 Mechanism, by the functions that own it

- The receipt signer: `defaultAEPSigner()` (`internal/worker/llm_inference.go`)
  constructs `nodeidentity.New(aepSigningStoreDir(network.GetNodeConfigDir()))`.
  `GetNodeConfigDir()` is `nodeConfigDirFromStateDir(GetStateDir())` — the
  *parent of the tsnet state dir*, which `resolveStateDir` roots (pointer file,
  then global config, then legacy/fresh) at `<owner-home>/citadel-node/network`.
  So the signing key lives under `<owner-home>/citadel-node/identity/`.
- The CSR/registered key: every pairing and enrollment path — `cmd/device.go`
  (three call sites) and `cmd/init.go:ensureNodeIdentity` — uses
  `nodeidentity.Default()`, which roots at `platform.ConfigDir()/identity`.
  `resolveConfigDir` returns `<home>/.citadel-cli` for a non-root invoker
  (and, for root, only falls through to the same user dir when a
  `config.yaml` is already there). So the key whose SPKI the fabric CA signed
  into `fabric_node_certs` lives under `<home>/.citadel-cli/identity/`.
- Two `Store` roots → two independent `GetOrCreateKey()` calls → two keys. The
  receipt's `public_key_fingerprint` is the signing key's SPKI; the verifier's
  `_find_matching_cert` looks that fingerprint up among the org's
  `fabric_node_certs` rows, which hold the CSR key's SPKI. No row ever matches.
  Verdict: `unverified`, reason "no registered, non-revoked fabric certificate
  … matches the receipt's signing key" — on every real node, forever.

This was verified by reading both resolvers, not by running anything (this doc
does not touch a live node). The two CLAUDE.md entries and the DoR are each
individually correct about their own half; nobody had put the halves together.
It is exactly the class of failure the DoR's proof plan item 6 ("real path, not
a unit test") exists to catch — and it would pass items 1–5.

### 4.2 Options

**Option K-A — one convergent identity store (recommended).** Introduce a
single constructor (e.g. `nodeidentity.Convergent(nodeConfigDir string)`) that
both the AEP signer and every CSR/enrollment site call, rooted at
`network.GetNodeConfigDir()/identity`. On first use it performs a one-time
**read-through migration**: if no key exists at the convergent path but one
exists at the legacy `ConfigDir()/identity/node.key`, copy it (same bytes →
same SPKI → the already-issued leaf stays valid; no re-pair needed for
QR-paired nodes). This also gives S8 (`citadel login` sends a CSR) the same
key by construction. Pin with a test asserting the store the CSR path uses and
the store `defaultAEPSigner` uses resolve the same key file.
- Risk to check before doing it: CLAUDE.md records that `cmd/device.go` /
  `cmd/init.go` "depend on staying invoker-scoped/shared with `citadel init`'s
  own context" — `nodeidentity` is a leaf that must not import
  `internal/network`, so the convergent root has to be threaded in from `cmd`
  (the `config.DeviceConfigDirsHook` pattern), and the device-mode enrollment
  path needs a read to confirm nothing there relies on the key living beside
  `config.yaml`.

**Option K-B — signer prefers the legacy store when it holds a key.**
`defaultAEPSigner` tries `Default()` first, then the convergent store. Smallest
diff — and it reintroduces the invoker-scoping bug #845/#917 removed: a
systemd-root `citadel work`'s `ConfigDir()` can resolve to `/etc/citadel`,
where there is no key, so it silently mints a third key under the convergent
root and signs with that, while the user's interactive `citadel init` registered
`~/.citadel-cli`'s. Same failure, harder to see. Not recommended.

**Option K-C — dual-write at pairing time.** Pairing writes the key to both
roots. Papers over the split without removing it; the next path that opens a
`Store` picks a root at random. Not recommended.

**Option K-D — register the signing key separately (raw SPKI upload).** The DoR
G3 explicitly refuses this ("trust-on-first-use key upload is a weaker second
registration path and the CA already exists"). Out.

**Recommendation: K-A, as an S3 prerequisite PR (or S3's first commit), gated
on Jason** — the DoR says key material and which key is registered are
Jason-gated, and this changes which file the registered key is read from even
though it mints nothing new. Until it lands, S3's receipts verify only in the
golden fixture, never on a real node, and this doc says so rather than
letting the green fixture imply otherwise.

---

## 5. Version negotiation and backward compatibility

Answered by the DoR's G7 and already implemented verifier-side; this section
records the contract rather than designing one.

- **Per-receipt, self-describing; no advertisement channel.** A node does not
  announce "I emit v2" anywhere. Each receipt carries (or omits)
  `receipt_version`; the verifier's `_resolve_receipt_version` branches: absent
  → v1 nine-field canon; `"2"` → v2 fifteen-field canon; anything else →
  `unverified` ("not a supported receipt shape"), never a guess.
- **v1 stays valid indefinitely.** `AEPReceiptV1`/`Canonicalize` are untouched;
  a pre-S3 node's receipts keep verifying (`content_bound: false`, the honest
  v1 state — pinned by aceteam's
  `test_v1_verified_signature_is_not_content_bound`, which the S3 wave must
  not edit).
- **No dual emission, no flag.** From the S3 release on, a signing node emits
  v2 only. There is no "emit v1 for older backends" mode, because the older
  backend (pre-S2) no longer exists in production and adding a per-node format
  toggle is precisely the flag class C4/S5 is about to delete.
- **The documented mutation outcome:** deleting `receipt_version` off a v2
  receipt makes the verifier fall back to the v1 canon (v1's fields are a
  subset), recompute nine fields instead of fifteen, and fail the signature —
  loud, not a downgrade. The Go golden's Python side should include this
  mutation.
- **Fleet visibility (optional, not correctness).** If operators want to see
  which nodes emit v2 before a receipt arrives, the existing binary-version
  channel (`fleet_node_versions`, the heartbeat's version field) already answers
  it — v2 emission is a property of the citadel release, not a per-node
  setting. No new heartbeat field is proposed.
- **Backend knowing which to verify:** it does not need to know in advance;
  it reads the receipt. Both call sites (`fabric_dispatch.py`,
  `core/agent_logic.py`) already pass whatever they hold as `expected_*` and
  the verifier applies those checks only on v2.

---

## 6. S2 vs S3 — the split, and why it flipped

The 2026-09-03 DoR had **C1 "receipt v1.1"** first: citadel adds the digests,
then the verifier learns them ("A1 should target the v1.1 shape from day one").
The 2026-09-06 delta found G7 — *the receipt had no version field*, so any
extra signed field would break every deployed verifier's fixed nine-field
recompute — and inverted the order:

| | S2 (aceteam#9287, **landed**) | S3 (citadel-cli#1002, **this doc**) |
|---|---|---|
| owns | the verifier's knowledge of v2: `receipt_version` branching, `canonicalize_receipt_v2`, `expected_input/output_sha256` + `expected_policy_hash` kwargs, `content_bound`/`policy_bound` computed, `to_attestation` fields, `aep_bundle` mapping, the dispatch-path call site (G10) | the node's *emission* of v2: `AEPReceiptV2` + `CanonicalizeV2`, the three digests (§2), the fixed `verdict_hash` preimage (§3), and the **Go-generated golden fixture committed into the aceteam test tree in the same wave** |
| tested against | a hand-built v2 sample (its tests say so: "until S3 supplies the Go golden") | S2's deployed verifier, via that golden |
| what "v1.1" became | nothing — there is no v1.1; the version field made it a clean v2 | |

So the brief's "S2 = v1.1 with digests but unsigned" is the pre-delta framing.
There is no unsigned intermediate: v2 is signed from its first byte, and the
*unsigned* `verdict_hash`/`output_sha256` on `trust_verdict` (S1/S6) are the
display-side twins of the signed fields, not a stepping stone.

**Scope boundary with S5:** S3 does NOT read a `policy` key, does NOT port
`canonical_json` for policy bytes, does NOT honor `gate: block`, and does NOT
delete the env gates. §3.3's recommendation pulls the Go `canonical_json` port
forward into S3 *for the verdict preimage only* (open question §11.3); if Jason
prefers to keep S3 minimal, the fallback is to ship S3 with S6's Go-json
preimage plus the `'f' 6` string for `score`, and accept that `verdict_hash`'s
value changes once more at S5.

---

## 7. Wiring into `applyTrustEngine`

`applyTrustEngine` (`internal/worker/llm_inference.go`) stays the single site;
nothing is added to any of the ten engine functions. Inside it, after
`trust.BuildVerdict` has produced `v`:

1. Digests: `outputSHA := sha256Hex(content)` (already computed for the
   unsigned map — compute once, use twice); `inputSHA` per §2.2 from
   `payload.MessagesRaw` / `payload.Prompt`; `policyHash` = the §2.3 interim
   constant (S5 replaces this with the received-bytes hash).
2. `trustVerdictMap` continues to attach the unsigned `trust_verdict`; its
   `verdict_hash` is now the §3.5 preimage (the same value that gets signed —
   the unsigned copy and the signed field must never diverge, so they come
   from one call to `BuildVerdict`). The unsigned map additionally carries
   `input_sha256` and `policy_hash` for parity with the signed fields.
3. Gates unchanged: `CITADEL_GROUNDING_GUARDRAIL` (nothing to sign without a
   verdict) and, nested inside it, `CITADEL_SIGN_AEP_RECEIPTS`. Default OFF,
   byte-identical output when off — the same posture S1/S6 shipped and the
   posture C4/S5 will *replace* with policy-driven behavior. S3 does not touch
   either gate.
4. `buildAEPReceipt` becomes `buildAEPReceiptV2` and passes
   `aep.V2Inputs{...}` plus the existing `nodeID`/`jobID`/`engine`/`model`/
   `GroundingResult`/`now`. Fail-open and `ToMap()` are unchanged: a signing
   failure logs via `aepLogf` and the job still succeeds with `content`,
   `grounding`, and `trust_verdict` intact.
5. `Job.RawPayload` plumbing (§2.2) is the only change outside the handler and
   `internal/aep`: two source decode sites and `parseLLMInferencePayload`.

`TestApplyTrustEngine_HookCoverageAcrossAllEnginePaths` extends to assert that
every one of the ten paths yields `aep_receipt.receipt_version == "2"` and
`aep_receipt.output_sha256 == sha256Hex(Output["content"])` (proof-plan item 2).

---

## 8. Cross-repo golden fixture and proof plan (DoR §5 items 3 and 4)

**Source of truth lives in citadel:** `internal/aep/testdata/v2/` —
`receipt.json`, `leaf.pem` (a test-only self-signed P-256 leaf; the verifier
only extracts the SPKI, never chain-validates), `key.pem` (test-only, needed to
regenerate), `input.bin` (the exact raw `messages` bytes, Python-default-dumped
by hand so it looks like production), `output.txt`, `policy.json` (`{}` for
S3), `canonical.bin` (the fifteen-line byte sequence), and, once §3.3's port
lands, `verdict_preimage.json`.

**ECDSA signatures are randomized**, so the fixture cannot pin signature bytes.
The Go test regenerates the receipt from the fixed key and fixed `issued_at`,
asserts `CanonicalizeV2` output equals `canonical.bin` byte-for-byte and every
digest field equals the committed value, then *verifies* the committed
signature against the committed leaf (not byte-compares it). A `-update` flag
rewrites the fixture when the canon is deliberately changed.

**The aceteam half** (same wave, separate PR, sequenced after this doc's
prerequisites): copy the directory to
`python-backend/tests/fixtures/aep_receipt/v2/`; a test loads it through the
existing `_patch_lookups` shape and asserts `verified` + `content_bound` +
`policy_bound`; mutations, each with its distinct reason: flip each of the
fifteen canonical fields (signature); flip one signature byte (signature);
replace `output.txt` (content); replace `policy.json` (policy); revoke the leaf
before `issued_at`; delete `receipt_version` (falls to v1 canon, signature).
Plus the §2.2 mutation: re-serialize `input.bin` through Go's `json.Marshal`
and assert the recomputed `input_sha256` differs (proves the raw-byte path is
load-bearing). When §3.3's port lands: a Python-generated
`canonical_json(verdict_preimage)` golden that the Go port must reproduce
(proof-plan item 4), with a "reorder one key" mutation.

**Proof-plan item 6 is the real acceptance**, and it is blocked on §4: one
streaming agent turn on node 1297 with both gates on producing an
`execution_envelopes` row with `attestation.verified: true` and
`content_bound: true`. Green fixtures without that row are not done.

---

## 9. What stays inert, and cross-repo sequencing

| Piece | State after S3 | What lights it up |
|---|---|---|
| Signature verification on direct dispatch (`fabric_dispatch_job`, `inference_chat`, REST) | **live** (S2) | — |
| `content_bound` on direct dispatch | live once S3 ships **and §4 is fixed** | §4 (K-A) |
| `content_bound` on agent chat (`core/agent_logic.py`) | still `false` — that call site passes no `expected_*` | S4 records the XADDed input bytes at that site |
| `policy_bound` | `true` on the golden; `false` in production (no call site passes `expected_policy_hash`) | S4 (send + record) then S5 (node hashes received bytes) |
| Real receipts on `citadel login` (device-auth) nodes | `unverified` — "no fabric CA leaf" | S8 (aceteam#9290): CSR on the device-auth token path, same CA, same table; same key by construction once §4 K-A lands |
| Real receipts on QR-paired nodes | `unverified` — fingerprint never matches (§4) | §4 K-A |
| `node_id` on the receipt | the signer's fingerprint (no fabric-ID echo yet, #8139) | backend echo point; not on S3's path |

**Deploy-order rule (the §2.3 hazard):** S4's *verifier-side* comparison
(`expected_policy_hash=<real sent policy>`) must not be switched on until S5 is
fleet-wide — until then a healthy pre-S5 node hashes `{}` and would flip to
`unverified`/`policy` on every turn. S4 can land its payload-attach and
hash-*recording* halves any time; the *comparison* is the gated line. The DoR's
order (S3 → S4 + S5 → S7) is consistent with this as long as "S4 + S5" means S5
deploys before S4's comparison is enabled, or S4 treats a receipt whose
`policy_hash` equals the empty-policy constant as "pre-S5 node, skip" (a small,
explicit, time-boxed special case — open question §11.5).

**Sequencing, in order:**
1. Jason answers §11 (this doc's PR).
2. §4 K-A prerequisite PR (citadel; Jason-gated).
3. S3 emission PR (citadel, #1002) + Go golden in `internal/aep/testdata/v2/`.
4. aceteam golden-adoption PR (fixture copied in; hand-built v2 sample retired
   or kept alongside).
5. Citadel release; `CITADEL_GROUNDING_GUARDRAIL=1` + `CITADEL_SIGN_AEP_RECEIPTS=1`
   on node 1297 (DoR §7.6, owner's shell); proof-plan item 6 observed.
6. S9 (aceteam-aep#141) ports `CanonicalizeV2` + the §3.5 preimage into
   `verify-bundle`, against the same fixture.

---

## 10. Scope boundaries (deliberate)

- **Not designing S5** (policy receipt, `gate: block`, buffered-serving posture,
  env-gate deletion) beyond reserving the `policy_hash` slot and the preimage
  shape it needs. §3.3's `canonical_json` port is the one S5 piece this doc
  proposes pulling forward, and only for the verdict preimage.
- **Not the Merkle-DAG (#8033).** `AEPReceiptV2` is still one leaf receipt for
  one job; `input_sha256`/`output_sha256` happen to be the same `sha256:`
  digest shape as `provenance_receipts.payload_digest`, which is convenient for
  a future link, but no chaining is designed here.
- **Not the threat model.** An on-disk 0600 P-256 key with no TPM attests "this
  filesystem produced this signature" (v1 design doc; DoR §3). v2 adds
  *receipt transplant* to the defeated list (`job_id` + `output_sha256` bind
  the receipt to one job's actual output) and nothing else. Marketing language
  stays "tamper-evident, node-signed, offline-verifiable".
- **Not `evidence_hash`/`flagged_hash` cross-language parity** (§3.5, last
  paragraph).
- **Not `citadel login` CSR (S8), not the fabric-ID echo (#8139), not the
  sovereign-exemption tightening (S7).**

---

## 11. Open questions for the owner

1. **Key-store reconciliation (§4) — blocking, Jason-gated.** K-A (one
   convergent identity store with a one-time read-through of the legacy
   `ConfigDir()/identity/node.key`; same key bytes, so existing leaves stay
   valid) vs K-B (signer prefers the legacy store; reintroduces invoker-scoping).
   Recommendation: K-A, as an S3 prerequisite PR. Also: confirm that the
   device-mode enrollment path (`cmd/device.go`) may move off `Default()`.
2. **Verdict-preimage float rule (§3.4):** A (`score` as the `'f' 6` string
   inside the preimage — the rule both sides already implement), B (integer
   ppm), or C (RFC 8785 with a library on each side). Recommendation: A.
3. **Which `canonical_json`, and when (§3.3):** the aceteam-aep canonicalizer
   for both `verdict_hash` and `policy_hash`, with the Go port pulled forward
   into S3 (one value change for the unsigned `verdict_hash`, now, while nothing
   compares it) — or keep S3 minimal on S6's Go-json preimage and accept a
   second value change at S5. Recommendation: pull it forward.
4. **`input_sha256` (§2.2):** I-A raw-byte retention (matches the shipped
   `_expected_input_sha256`, zero aceteam change, fragile to any future
   re-serializer — fails loud as `unverified`/content) vs I-B canonical-value
   hashing (robust, needs an aceteam change and couples S3 to the Go port on
   arbitrary message content). Recommendation: I-A for S3.
5. **The "explicit empty policy" object (§2.3, §9):** does it stay the literal
   `{}` (hash `sha256:44136fa3…`) through S5, or become the full-shape default
   at S5 (changes the hash)? And which of the two deploy-order mitigations for
   the S4/S5 window: hold S4's verifier comparison until S5 is fleet-wide, or
   have S4 skip the comparison when `policy_hash` equals the empty-policy
   constant?
6. **Golden fixture ownership (§8):** source of truth in
   `internal/aep/testdata/v2/` with a copy under
   `python-backend/tests/fixtures/aep_receipt/v2/`, updated by hand in the same
   wave — or a generator script on one side (the `provenance` fixture has
   `scripts/generate-provenance-fixtures.js` as precedent). Recommendation:
   hand-copied for S3; a generator only if the canon changes a second time.
7. **Confirm S3 is now unblocked** (S2 closed 2026-09-09) and may be promoted
   from `blocked` to `automated` on citadel-cli#1002 once questions 1–4 are
   answered, with the §4 prerequisite as its first commit.
