# Design: the long-term canonicalization framework for the AEP receipt "bytes-pin" (aceteam #8253)

Status: design only, no implementation. Part of aceteam-ai/aceteam#8253.
Companion to [docs/design-trust-receipt-v2.md](design-trust-receipt-v2.md)
(PR #1018), whose S3 recommendation — **match the deployed hand-rolled
aceteam verifier (#9287): `'f' 6`, newline-delimited canon over the fifteen v2
fields — is unchanged by this doc.** This doc answers the owner's follow-on
question: *what is the long-term approach for the bytes-pin, and is there a
framework we are missing?* It is about the v3-and-beyond direction only.

Every "byte-identical" / "diverges" claim below was run, not recalled. §9 is
the appendix: exact inputs, library versions, query date, outputs, and one
methodology near-miss a re-runner will hit.

---

## 0. Context — the problem, stated as a property, and what ships today

The AEP receipt is signed on the node in Go and verified on the platform in
Python. The signature covers a byte sequence, so *both sides must derive the
identical bytes from the same logical receipt, forever, as the field set
evolves.* That is the "bytes-pin". Today it is held by hand:

| Owner | What it decides | Where |
|---|---|---|
| `aep.Canonicalize` | the v1 signed bytes: nine fields, declaration order, `\n`-joined, no trailing newline; `bool → FormatBool`, `float64 → FormatFloat 'f' 6`, `int → Itoa`, strings raw | `internal/aep/receipt.go` |
| `trust.computeVerdictHash` | the unsigned `verdict_hash` preimage: `json.Marshal` of a typed struct — Go float rendering, so `score=1.0` hashes as `1` while Python's `json.dumps` would give `1.0` (#1018 §3.2, the live hazard S3 fixes) | `internal/trust/verdict.go` |
| `nodeidentity.Store.Sign` | ECDSA P-256 over `sha256(payload)`, ASN.1 DER out; the receipt carries it base64 | `internal/nodeidentity/nodeidentity.go` |
| `canonicalize_receipt` / `canonicalize_receipt_v2` + `_resolve_receipt_version` | the Python mirror of the same bytes (v1 nine fields; v2 fifteen fields), `f"{float(v):.6f}"` for score, dispatch on `receipt_version` | aceteam `python-backend/utils/aep_receipt_verify.py` (#9287, #9409) |
| the tamper-sweep harness | per-field mutation of the receipt **dict**, asserting each canonical field is load-bearing | aceteam `tests/test_aep_receipt_verify_v1_harness.py` (#9371/#9461) |
| `aep_bundle` / `to_attestation` | consume the receipt as a dict; never re-canonicalize it | aceteam `utils/aep_bundle.py`, `aep_receipt_verify.py` |
| S9 `aceteam-aep verify-bundle` | a third consumer, **not yet built** (no `verify-bundle` in `aceteam-aep` at the time of writing) | `aceteam-aep` |

Note the shape of the shipped contract: the verifier receives a **dict** (the
receipt as it arrived inside the JSON job `Output` map over Redis/the API
proxy) and **recomputes** the bytes from the dict's fields. That shape —
*detached payload, recompute on verify* — is what makes cross-language
canonicalization load-bearing at all; §6 returns to it.

#1018 §12 evaluated one candidate framework (protobuf single-source-of-truth)
and concluded it moves the canonicalization problem rather than removing it,
recommending a `fields.txt` drift-guard instead. This doc evaluates the two
frameworks §12 did not: RFC 8785 JCS over the plain object, and COSE_Sign1 over
deterministic CBOR — head-to-head against that hand-rolled + drift-guard
baseline.

## 1. What a "bytes-pin" actually requires

Naming the properties first is what makes the three options comparable
rather than a matter of taste. A canonical form `E` over the logical receipt
value must be:

- **P1 Injective and total.** Two distinct logical receipts never produce the
  same bytes; every well-formed receipt has bytes. (A non-injective canon
  lets one signature vouch for two different receipts.)
- **P2 Identically implemented in Go and Python** — ideally because a
  *specification* pins every byte and a *conformance corpus* guards the
  implementations, not because two engineers eyeballed each other's code.
- **P3 Type-faithful across the transport hop.** The receipt travels as JSON
  inside the job `Output` map. If `E` depends on a distinction the transport
  erases (Go `json.Marshal(1.0)` emits `1`; Python `json.loads("1")` yields
  `int`), the verifier cannot reconstruct the emitter's input. This property
  is the one the three options differ on most, and the one most easily
  missed when "binary encoding" is proposed as the fix.
- **P4 Extensible.** Adding a scalar field, and — the harder case — adding
  the first list or nested object, must not require inventing a new rule.

And, orthogonal to `E`: **the wrapper** (which key, which algorithm, how the
signature and key id are carried) — today `signature` + `public_key_fingerprint`
fields on the same JSON object, DER/base64.

## 2. Option 1 — status quo: hand-rolled field-wise canon + `fields.txt` drift-guard (baseline)

The #1018 §12.5 middle path: keep `Canonicalize{,V2}` as is, commit the
ratified field list once as `internal/aep/testdata/v2/fields.txt`, and have
both repos' tests assert their own field list equals it.

**What it has going for it.** Zero dependencies on either side. The bytes are
trivially greppable (`cat canonical.bin`). It is already deployed and
golden-pinned. The attack surface is a 20-line function. Cost-to-verify for a
new consumer is ~10 lines in any language.

**Residual risks, each verified rather than assumed:**

- **R1.1 — Not injective (P1 fails).** String fields are written raw and `\n`
  is the field delimiter, so a `\n` inside a value shifts the field
  boundaries without changing the bytes. Demonstrated against the real
  `aep.Canonicalize` (scratch test, §9.4): `{engine:"bonsai\nx", model:"y"}`
  and `{engine:"bonsai", model:"x\ny"}` canonicalize to the identical
  `n\nj\nt\nbonsai\nx\ny\ntrue\n1.000000\n0\nh`, so one signature verifies
  both. `model` is `payload.Model` — request-supplied, not node-controlled
  (#1018 §1.1 field table). Today's practical exposure is low (a model name
  containing a newline would not route to an engine, and the receipt is only
  emitted on a successful completion), but it is a real property failure of
  the canon, and every hand-rolled delimiter scheme has it until a framing
  rule (length prefix or escaping) is added — at which point one is
  reinventing a canonical JSON, badly.
- **R1.2 — `'f' 6` is lossy.** The signature binds `score` only to ±5e-7;
  two receipts differing in the seventh decimal share a signature. Cosmetic
  for a guardrail score, but it is a second, quieter injectivity gap.
- **R1.3 — No path to a non-scalar field (P4 fails hard, not soft).** The
  flat canon cannot carry `checks[]` inline, structured per-check evidence,
  or #8033 DAG links without a framing rule. #1018's `verdict_hash`
  indirection (hash the nested object separately, sign the hex) is exactly
  the workaround — and it means every nested structure needs its *own*
  cross-language canonicalizer (today: the aceteam-aep `canonical_json` port,
  #1018 §3.3). The flat canon does not remove the nested-canon problem; it
  relocates it one level down.
- **R1.4 — Three hand-synced copies, per-field render rules.** Every field
  addition is a `receipt_version` bump plus a same-PR edit in citadel,
  aceteam, and (once built) aceteam-aep; every new *type* is a new render
  rule (`'f' 6` was decided once for v1 and re-decided for `verdict_hash`).
  `fields.txt` catches field-*list* drift; it cannot catch a render-rule
  drift (one side switching `'f' 6` → `'f' 8` passes the field-list check
  and fails only at the golden).
- **R1.5 — Bespoke.** A fourth consumer, or a generic audit tool, has nothing
  to import; it must reimplement the bespoke rules from a prose spec.

## 3. Option 2 — RFC 8785 JCS over the plain logical object

Not the proto3-JSON flavour #1018 §12.3(i) rejected; **pure JCS**: build the
logical receipt (minus `signature`/`public_key_fingerprint`), serialize it per
RFC 8785, sign those bytes. The receipt on the wire *is* the signed object.

### 3.1 What the spec pins, and why the number rule is the crux

RFC 8785 §3.2 fixes: no whitespace; object keys sorted by UTF-16 code units;
strings escaped exactly as ECMAScript `JSON.stringify` does (only `"`, `\`,
and U+0000–U+001F; short forms for `\b\f\n\r\t`, `\u00XX` lowercase otherwise;
everything else raw UTF-8); and — the crux — **numbers rendered by ECMAScript
`Number::toString`** (ECMA-262 §6.1.6.1.20): the shortest decimal digit
string that round-trips to the same double, with a fully specified tie rule
(closest value; if two, the even one), fixed-notation for 10⁻⁶ ≤ |v| < 10²¹,
exponent form `e+21` / `e-7` outside. That algorithm is *deterministic and
total* over doubles (NaN/±Inf excluded), so two conformant implementations
cannot disagree on any finite double — the guarantee is spec-level (P2), and
the cyberphone reference project ships a 100-million-value corpus that the
implementations are tested against.

The baseline experiment (§9.1) shows why this matters: Go's `strconv 'g' -1`
and Python's `repr` already agree on the *digits* of every value tested (both
are shortest-round-trip) but not on the *format* (`1` vs `1.0`,
`1.23456789e+08` vs `123456789.0`, `1e+15` vs `1000000000000000.0`). JCS is
precisely the missing format rule; `gowebpki/jcs`'s `es6numfmt.go` is
literally `strconv.FormatFloat(v, 'g', -1, 64)` plus an ES6 reshaper.

### 3.2 Empirical result (§9.2): three libraries, byte-identical

`gowebpki/jcs` v1.0.1 (Go), `jcs` 0.2.1 (Python), `rfc8785` 0.1.4 (Python,
Trail of Bits) produced **byte-identical output** — same sha256 — for: the
full fifteen-field v2 receipt with `score=1.0` (→ `"score":1`) and with
`score=2/3` (→ `0.6666666666666666`); seventeen literal doubles spanning the
`1e21` and `1e-6` notation boundaries, `5e-324`, the max double, `-0.0` (→
`0`, as ES6 requires); nine **runtime-computed** doubles including
`0.1+0.2` → `0.30000000000000004`; the escaping sample
`a"b\c\n\r\t\b\f\x01\x1f<>&/é😀` (short escapes for `\n\r\t\b\f`, lowercase `\u0001`/`\u001f` for the
other controls, **no** HTML escaping of `<>&`, raw UTF-8 for `é😀` — Go's
`encoding/json` would have emitted `\u003c\u003e\u0026`);
UTF-16 key ordering (`😀` U+1F600 sorts *before* `～` U+FF5E because its
surrogate `D83D` < `FF5E` — all three agree, i.e. all three sort by UTF-16
units, not code points); and `null`/`""`/`[]`/`{}` preserved as-is.

Two things the experiment surfaced that a prose evaluation would not have:

- **The `1` vs `1.0` question is settled by value, not type, and that cuts
  both ways.** The same receipt loaded from JSON text `"score": 1` (Python
  `int`) and from `"score": 1.0` (Python `float`) canonicalizes to identical
  bytes. So the Redis/API JSON hop that erases Go's float-ness is *harmless*
  under JCS (P3 holds for numbers) — the exact hazard `'f' 6` was introduced
  to dodge simply does not exist here. The flip side: JCS cannot express "this
  is an integer" versus "this is a float that happens to be integral". Any
  field whose *type* carries meaning must be a string.
- **Integers above 2⁵³ are outside JCS's number domain, and the libraries
  disagree on what to do about it.** `gowebpki/jcs` and Python `jcs` silently
  rendered `9007199254740993` as `9007199254740992` and `math.MaxInt64` as
  `9223372036854776000` (both round through a double); `rfc8785` raised
  `IntegerDomainError` (fails closed). This is the one spec gap: RFC 8785
  says numbers *are* doubles and leaves out-of-domain integers to the
  implementation. Rule required (§7 R1): integers in a v3 receipt must
  satisfy |n| ≤ 2⁵³, checked on emission; anything larger (a timestamp in ns,
  a byte count that could plausibly exceed it) is a string.

### 3.3 Residual risks

- **R2.1 — Library maturity is the honest weak spot, on both sides.**
  `gowebpki/jcs`: two tagged versions, last 2023-10. cyberphone's Go reference
  (`json-canonicalization`): untagged pseudo-versions, last 2024-12 — but it
  is what **Sigstore's `rekor` v1.5.4 and `sigstore-go` v1.3.0 depend on**
  (verified from their `go.mod`, §9.6), so it is production-exercised. Python
  `jcs`: three releases, last 2022-04, silent integer rounding. `rfc8785`:
  eight releases, last 2024-09, pure Python, typed, fail-closed — the better
  choice. Matrix's `canonicaljson` 2.0.0 is **not** RFC 8785 for numbers
  (renders `1.0` as `1.0` via Python `repr`, §9.5 — note #1018 §3.4 said it
  "refuses floats"; 1.x did, 2.0.0 accepts them non-conformantly) and is
  disqualified. Mitigation that makes the risk small: the Go side is ~300
  lines over `strconv` and can be vendored into `internal/aep/jcs` with the
  cyberphone corpus subset as its test; the Python side is ~200 lines.
- **R2.2 — No NaN/±Inf.** JSON cannot carry them; every library refuses.
  `score` is a ratio in [0,1] today; emission must reject non-finite values
  rather than let the signer fail open into an unsigned receipt.
- **R2.3 — No Unicode normalization.** JCS signs the code points it is given;
  `"é"` (U+00E9) and `"é"` are different receipts. Correct for a
  signature (we sign what we emitted), but a consumer that NFC-normalizes
  on ingest would break verification. Not a change from the status quo (the
  newline canon has the same property); stated for completeness.
- **R2.4 — RFC 8785 is Informational (ISE stream), not Standards Track.** Its
  normative force comes from the standards that mandate it — W3C Data
  Integrity's `*-jcs-*` cryptosuites, Sigstore — not from IETF status.
  Relevant if a future auditor asks "what standard"; irrelevant to bytes.

### 3.4 Fit with what exists

- **Signer:** unchanged. `Signer.Sign(payload []byte)` already takes an
  opaque byte string and hashes it; only what is passed changes. DER/base64
  `signature` and `public_key_fingerprint` stay as the wrapper.
- **Verifier contract:** unchanged in shape. `_resolve_receipt_version`
  gains a `"3"` branch whose canonicalizer is
  `rfc8785.dumps({k: v for k, v in receipt.items() if k not in SIG_FIELDS})`
  — one function, dict in, bytes out, exactly the seam #9287 already has.
  `to_attestation`, `aep_bundle`, and the harness consume dicts and never
  re-canonicalize, so they are untouched; the per-field tamper sweep keeps
  its dict-mutation shape and simply runs the v3 fixture too.
- **`verdict_hash` and the nested-canon problem (R1.3):** JCS handles lists
  and nesting natively, so a v3 could sign the verdict *object* inline and
  retire the separate `verdict_hash` preimage port. aceteam-aep's
  `canonical_json` (sorted keys, `None` dropped, compact, Python float
  `repr`) is "almost JCS but not": it is what `policy_hash` and `bundle_hash`
  use fleet-wide, so consolidating it is a separate, larger decision (§8 Q3),
  not part of this recommendation.

## 4. Option 3 — COSE_Sign1 (RFC 9052) over deterministic CBOR (RFC 8949 §4.2 / dCBOR)

The claim to test: binary numeric encoding removes the float-*string*
agreement problem entirely — no `'f' 6`, no ES6 algorithm.

### 4.1 What "deterministic CBOR" does and does not pin

RFC 8949 §4.2.1 *Core Deterministic Encoding* fixes: shortest integer
encoding, definite lengths, map keys sorted **bytewise-lexicographically over
their encoded form**, and *preferred serialization* for floats (the shortest
of float16/32/64 that round-trips). It deliberately does **not** fix:

- **int vs float.** `1` is `0x01`; `1.0` is `0xf93c00`. Distinct by design.
- **NaN.** §4.2.2 makes `0xf97e00` the preferred NaN but leaves other NaN
  payloads to the application.
- **−0.0.** Stays `0xf98000`, distinct from `0x00`.
- **Which "canonical".** RFC 7049 §3.9 (the older "Canonical CBOR") sorted
  keys *length-first*; RFC 8949 §4.2.1 sorts *bytewise*. Both are called
  "canonical" in library APIs.

**dCBOR** (`draft-mcnally-deterministic-cbor`, individual submission, rev-18
dated 2026-08-10 per the datatracker — not yet a WG document) closes exactly
those gaps: numeric reduction (an integral float **must** encode as an int, so
`1.0` → `0x01`; `-0.0` → `0`), a single NaN, no `undefined`, restricted tags.
It is the CBOR answer to the int/float question. But: **no Go or Python
dCBOR library exists on the public registries** under any of the obvious
names (§9.6; Blockchain Commons' reference implementations are Rust, Swift,
and TypeScript). Adopting dCBOR today means writing the reduction pass
oneself on both sides — i.e. hand-specified rules again.

### 4.2 Empirical result (§9.3): scalars agree; key order does not, in one case; the ecosystem is fragile

`fxamacker/cbor` v2.9.3 `CoreDetEncOptions()` (Go) vs `cbor2` `canonical=True`
(Python) produced identical bytes for every scalar case: shortest floats
(`1.0`→`f93c00`, `0.5`→`f93800`, `65504`→`f97bff`, `65505`→`fa477fe100`,
`2/3`→`fb3fe5…`), NaN→`f97e00`, +Inf→`f97c00`, `-0.0`→`f98000`, ints, bools,
nil, bytes, UTF-8 text — and for the receipt maps, whose keys are all text
strings. **They diverged on mixed-type keys**: `{300: 1, "z": 2}` → cbor2
`a2617a0219012c01` (`"z"` first: RFC 7049 length-first) vs fxamacker core
`a219012c01617a02` (`300` first: RFC 8949 bytewise). cbor2's only switch is
`canonical: bool` — it has no RFC 8949 mode; fxamacker's
`CanonicalEncOptions()` (7049) matches cbor2. For text-string-keyed maps the
two orders provably coincide (the length prefix is the leading byte of the
encoding, monotone in length within a major type), so a receipt would not
hit this — but COSE header maps are integer-keyed, and it is a spec-level
trap that "deterministic" resolves to two different orders across the two
ecosystems' defaults.

COSE_Sign1 interop itself worked: `veraison/go-cose` v1.3.0 ↔ `pycose` 1.1.0,
ES256 (`-7`), raw `r||s` 64-byte signatures, both directions verified, a
one-byte payload tamper detected (§9.3). **But pycose 1.1.0 (last release
2023-12-15) cannot decode any tagged COSE message under `cbor2` ≥ 6
(6.1.4, 2026-08-01)**: cbor2 6 decodes arrays nested inside a tag as
`tuple`, and `CoseMessage.decode` does `isinstance(cose_obj, list)` → `TypeError:
Bytes cannot be decoded as COSE message`. It works pinned to `cbor2<6`
(5.9.0). The actively maintained alternative, `cwt` (python-cwt 3.3.0,
2026-07-16, 57 releases), **also pins `cbor2<6.0.0`**. So the Python COSE
ecosystem as a whole is currently held below the current cbor2 — a live,
today maintenance risk, not a hypothetical one. The Go side is healthy
(fxamacker/cbor 2026-08, 24 versions; go-cose 2024-07; `ldclabs/cose` v1.4.0
2026-06 as a second option).

### 4.3 Answering the posed claim honestly

Binary encoding does remove the float-*string* problem: there is no decimal
rendering anywhere, and the IEEE bits are the canonical form. **But with the
shipped contract shape — detached payload, verifier recomputes from the JSON
dict — it reintroduces a *type-tag* problem that JCS does not have (P3
fails):** Go emits `score: 1.0`, `json.Marshal` writes `1`, Python
`json.loads` yields `int 1`, cbor2 encodes `0x01`, and the node signed
`0xf93c00`. The fix is either (a) schema-typed coercion on the verify side
(`float(receipt["score"])` — the same hand rule #9287 already applies, so
nothing is gained over today), (b) dCBOR numeric reduction (no libraries),
or (c) **an embedded payload**: the verifier checks the *carried* bytes and
then decodes them, never recomputing. (c) is where the float problem
genuinely disappears — and it disappears because the verifier stopped
recomputing, not because the encoding is binary. §6 makes that lever
explicit, since it is available without CBOR.

### 4.4 Residual risks

- **R3.1 — Debuggability.** The signed object is binary; it rides the JSON
  job `Output` as base64; inspection needs `cbor2.tool`/`cbor-diag`; the
  harness's "mutate one dict field" sweep no longer mutates what is signed
  and must be rewritten as decode → mutate → re-encode.
- **R3.2 — Python library fragility (§4.2).** Adopt only after re-checking
  the pycose/cwt vs cbor2 situation at that time.
- **R3.3 — Two "canonical"s (§4.2).** Mandate RFC 8949 core + text-string
  keys only, and pin it with a mixed-key negative test on both sides.
- **R3.4 — dCBOR's status.** The only CBOR profile that answers the
  int/float question is an individual I-D with no Go/Python implementation.
- **R3.5 — Signer fit is fine but not free.** `aep.Signer.Sign` returns DER;
  COSE ES256 wants raw `r||s`. go-cose takes a `crypto.Signer`, which
  `*ecdsa.PrivateKey` from `nodeidentity.Store.GetOrCreateKey` satisfies
  directly; pycose needs the raw `d/x/y` from the same PEM. The `aep.Signer`
  interface (payload in, DER out) would be bypassed, not reused.
- **R3.6 — Migration cost is the largest of the three** (§5 table): three
  repos, new dependencies on both sides, the verifier's *contract* changes
  from dict-in to bytes-in, `aep_bundle`/`to_attestation` must decode the
  payload to display it, and the wire stays binary-in-JSON because the
  transport is JSON.

**What COSE genuinely buys**, and the only reason to pay for it: it subsumes
the wrapper. `alg`, `kid`, the signature format, tag 18, counter-signatures,
and x5chain are standardized, and an embedded payload makes the receipt
self-verifying with an off-the-shelf tool. That is the right design **when the
receipt itself moves to a binary transport** (#1018 §12.5 trigger (c) —
alongside the proto `node_state` bodies), and not before.

## 5. Head-to-head

| Criterion | 1. Hand-rolled + `fields.txt` | 2. JCS (RFC 8785) | 3. COSE_Sign1 + det. CBOR |
|---|---|---|---|
| Cross-language byte-agreement guarantee | By construction + golden; no spec, no corpus | **Spec-level** (ECMA-262 number algorithm is total and deterministic over doubles) + 100M-value corpus; 3 libs byte-identical here | RFC 8949 §4.2 spec-level for scalars **given agreed types**; int/float and NaN left open unless dCBOR (no libs); two "canonical" key orders across ecosystems |
| float determinism | `'f' 6`: deterministic, lossy (±5e-7) | Shortest-round-trip, lossless, `-0`→`0`; value-based | IEEE bits, lossless; `-0.0` distinct; NaN payload app-defined |
| int determinism | `Itoa` | Exact to 2⁵³; above that libs differ (rfc8785 refuses, others round) — **rule needed** | Exact to 2⁶⁴ |
| bool / absent field | `FormatBool`; absent = malformed (verifier) | `true`/`false`; absent key ≠ `null` ≠ `""` (all distinct, all deterministic) | `f5`/`f4`; absent vs `f6` distinct |
| int-vs-float 1 / 1.0 across the JSON hop (P3) | N/A (field rule coerces) | **Erased by value** → transport-robust; type-meaningful fields must be strings | **Preserved** → the JSON hop breaks detached verification; needs coercion, dCBOR, or embedded payload |
| Injective (P1) | **No** (R1.1, demonstrated) | Yes | Yes |
| First non-scalar field (P4) | Needs a new framing rule | Native | Native |
| Human-debuggability | Greppable text | **Greppable JSON — the receipt is the signed object** | Binary, base64-in-JSON, needs tooling |
| Library maturity — Go | stdlib | small/quiet (gowebpki 2023-10; cyberphone ref used by Sigstore) — vendorable | healthy (fxamacker 2026-08, go-cose, ldclabs/cose) |
| Library maturity — Python | stdlib | `rfc8785` (Trail of Bits, 2024-09, fail-closed) — small | **fragile today**: pycose (2023-12) and cwt both pin `cbor2<6`; cbor2 6.x breaks pycose |
| Migration from #9287 | none (is #9287) | Small PR × 3 repos: one `"3"` branch in `_resolve_receipt_version` + one 5-line canonicalizer; harness/bundle/attestation unchanged | Medium-large PR × 3 repos: new deps, verifier contract changes shape, bundle/attestation decode payload, harness rewritten |
| Fit with ECDSA-P256 raw-bytes signer | as-is | as-is (`Sign(JCS bytes)`) | bypasses `aep.Signer` (raw `r||s`, `crypto.Signer`) |
| Subsumes the wrapper? | no | no (wrapper stays DER/base64 fields) | **yes** (alg/kid/sig/tag standardized) |
| New producer/consumer cost | reimplement bespoke rules | import a JCS lib | import CBOR + COSE libs |

## 6. The lever all three share: detached-recompute vs embedded-bytes

Every option above was evaluated in the shipped contract shape: the verifier
receives fields and recomputes bytes. There is a second shape, and it is the
honest answer to "is there a framework we are missing": **carry the exact
signed bytes, and let the verifier verify-then-decode instead of
rebuild-then-verify.** JWS compact serialization (base64url JSON payload) and
COSE_Sign1 with an embedded payload are both this shape. Under it, the
cross-language canonicalization requirement (P2/P3) vanishes *for signature
verification*; canonicalization becomes an emission-side nicety for stable
goldens and dedup, not a security-critical cross-repo contract. aceteam
already uses this pattern for a different receipt: the public
`/verify/receipt` route stores billing receipts' `canonical_json` and serves
the raw bytes so the digest check "has no copy-paste failure".

The cost is real and specific: the receipt carries its payload twice (or the
display fields become *derived from* the payload, which changes
`to_attestation`/`aep_bundle` from "read the dict" to "decode the payload,
then read"); the tamper harness must mutate the carried bytes, not dict keys;
and a consumer that trusts the display fields without re-deriving them from
the payload is a new class of bug. It is not recommended for v3 in this doc —
it is a contract-shape change, not a canonicalization framework — but it is
the design to reach for at the binary-transport trigger, and §8 Q4 asks the
owner whether v3 should carry the bytes anyway.

## 7. Recommendation

**Long-term target (v3): Option 2 — pure RFC 8785 JCS over the plain logical
receipt object, detached, with the existing ECDSA-P256 / DER / base64 wrapper
unchanged.** It is the only option that gives a *spec-level* cross-language
guarantee (P2), is transport-robust on the exact `1`/`1.0` hazard that
motivated `'f' 6` (P3), is injective (P1), extends to nested fields without a
new rule (P4), keeps the receipt a greppable JSON object, and migrates as a
one-function swap inside the verifier shape #9287 already has. Its weak spot
— small, quiet libraries — is bounded by vendoring ~300 lines over `strconv`
with the conformance corpus as the test.

The framework that was missing is not a codec; it is **a specification plus a
conformance corpus plus fail-closed libraries**, replacing per-field
hand-specified render rules. JCS is the smallest thing that supplies all
three for a JSON-transported object.

Rules a v3 canon must carry (they are what the experiments showed the spec
leaves to the implementation):

- **R1** Numbers are IEEE doubles. Integers must satisfy |n| ≤ 2⁵³, checked
  at emission (Go) and by the fail-closed library at verification (Python
  `rfc8785`). Anything larger is a string.
- **R2** Any field whose *type* is meaningful, or that must be lossless
  beyond double precision, is a string. Digests already are.
- **R3** Non-finite floats are rejected at emission (never fail open into an
  unsigned receipt); `-0.0` canonicalizes to `0` and that is fine for a score.
- **R4** `signature` and `public_key_fingerprint` are removed before
  canonicalization; nothing else is excluded, so the wire object minus those
  two keys *is* the signed object (a `receipt_version: "3"` string, first
  under UTF-16 sort or not — order no longer matters).
- **R5** One canonicalizer for every signed or hashed JSON object in the
  receipt: the receipt itself and, with the nested-canon problem (R1.3)
  dissolved, the verdict object inline rather than a separately-ported
  preimage.
- **R6** Golden fixture as in #1018 §8, plus a Python-generated JCS golden
  the Go side must reproduce, plus the cyberphone corpus subset in the
  vendored package's tests.

**COSE_Sign1 + deterministic CBOR: adopt only at the binary-transport
trigger, and then with an embedded payload** (§4.3/§6), RFC 8949 core with
text-string keys only, and a fresh check of the Python COSE ecosystem's cbor2
pin at that time. Do not adopt it for the payload canon of a JSON-transported
receipt — with a detached payload it is strictly worse than JCS on P3, and
the only CBOR profile that fixes that (dCBOR) has no libraries.

**Do not move S3.** S3 stays match-#9287, `'f' 6`, newline. v2's drift guard
stays `fields.txt`.

### 7.1 Migration trigger — what makes the three-repo cost worth paying

- **Primary (hard) trigger: the first non-scalar canonical field.** A
  `checks[]` list inline, structured per-check evidence, #8033 DAG
  parent/child links, a calibrated-verdict object (aep#136) — anything the
  flat newline canon cannot express without a framing rule. At that point
  the choice is "invent framing" (R1.1's problem, generalized) or "adopt
  JCS"; adopt JCS. This is the trigger to plan around, because it is the one
  the roadmap already points at.
- **Secondary triggers** (any one, from #1018 §12.5, restated): a v3 for any
  other reason with materially more fields (≈5+); a fourth producer or
  consumer of the receipt (a spec'd canon is importable, a bespoke one is
  not); the receipt moving to binary transport → COSE, per above.
- **Not a trigger:** adding one or two scalar fields (bump `receipt_version`,
  extend the newline canon, extend `fields.txt`); fixing `verdict_hash`'s
  float hazard (S3 does that with the `'f' 6` string per #1018 §3.4 A).

### 7.2 What the v3 migration costs, concretely

- citadel: `internal/aep/jcs` (vendored or dependency), `CanonicalizeV3`
  (marshal the struct minus signature fields, `jcs.Transform`), emission
  checks R1/R3, golden + corpus tests. v1/v2 paths untouched.
- aceteam: `rfc8785` dependency; `canonicalize_receipt_v3` (≈5 lines);
  `_resolve_receipt_version` gains `"3"`; harness runs the v3 fixture with
  its existing dict-mutation sweep; `to_attestation`/`aep_bundle` unchanged.
- aceteam-aep `verify-bundle` (S9): the same 5 lines, once it exists — and
  if S9 lands *after* v3 is decided, it should be written against JCS from
  the start rather than porting the newline canon a third time.

Roughly a small PR in each of three repos; smaller than #1018 §12.4's proto
SST estimate and much smaller than COSE.

## 8. Open questions for the owner

1. **Trigger.** Is "the first non-scalar canonical field" the right *hard*
   trigger, with field-count / fourth-consumer / binary-transport as
   secondary? Or should v3 be scheduled proactively so S9's `verify-bundle`
   is written against JCS once rather than against the newline canon and
   then migrated?
2. **Libraries.** Go: vendor `gowebpki/jcs` (~300 lines + corpus subset)
   into `internal/aep/jcs`, or depend on cyberphone's reference (Sigstore's
   choice, untagged)? Python: `rfc8785` (fail-closed on integer domain) over
   `jcs` (silent rounding) — agree?
3. **Scope of R5.** Should aceteam-aep's `canonical_json` (used fleet-wide
   for `policy_hash` and `bundle_hash`) also move to JCS at v3 so there is
   one canonicalizer across the whole receipt system, or stay as the
   "almost-JCS" it is with the Go port #1018 §3.3 specifies? This doc
   recommends deciding it separately; it is bigger than the receipt.
4. **Embedded bytes at v3?** Even under JCS, should the v3 receipt *also*
   carry `signed_payload` (the exact JCS bytes, base64) so a verifier can
   verify-then-decode (§6) and the cross-language recompute becomes a
   consistency check rather than the security boundary? Cost: payload
   carried twice; harness semantics change.
5. **Newline hardening now, independent of v3.** `BuildSignedReceipt{,V2}`
   could refuse to sign a receipt whose string fields contain `\n` (R1.1).
   It changes no bytes for any well-formed receipt, so it does not touch
   #9287 or S3's byte contract — but it is a behavior change on the node and
   should be an explicit decision, not slipped into S3.
6. **Deterministic ECDSA for goldens.** Signatures are randomized, so
   fixtures verify rather than byte-compare (#1018 §8). RFC 6979
   deterministic signing would make signature bytes pinnable; Go's stdlib
   does not offer it, Python `cryptography` does. Worth a vendored
   implementation, or leave fixtures as verify-only?
7. **Conformance test as a deliverable.** Should the §9 experiment scripts
   become a checked-in cross-language conformance test (Go + Python, run in
   both repos' CI against a shared corpus) as part of v3, rather than a
   one-off? This doc did not commit them (design-only PR).

## 9. Appendix — experiments (run 2026-09-09)

Environment: Go 1.24.11 linux/amd64; Python 3.11.14 (uv venv). Libraries
(registry metadata queried the same day): `gowebpki/jcs` v1.0.1 (2023-10-15,
2 versions); `cyberphone/json-canonicalization` untagged, latest
pseudo-version 2024-12-13; `fxamacker/cbor/v2` v2.9.3 (2026-08-18, 24
versions); `veraison/go-cose` v1.3.0 (2024-07-19, 12); `ldclabs/cose` v1.4.0
(2026-06-15). PyPI: `jcs` 0.2.1 (2022-04-10, 3 releases); `rfc8785` 0.1.4
(2024-09-27, 8); `canonicaljson` 2.0.0 (2023-03-15); `cbor2` 6.1.4
(2026-08-01, 49) and 5.9.0; `pycose` 1.1.0 (2023-12-15, 7); `cwt` 3.3.0
(2026-07-16, 57, requires `cbor2<6.0.0,>=5.4.2`). RFC 8785: Informational,
ISE stream. `draft-mcnally-deterministic-cbor`: rev 18, 2026-08-10, no
`draft-ietf-cbor-dcbor`. No package named `dcbor`, `bc-dcbor`, `bc_dcbor`,
`dcbor-py`, `pydcbor` on PyPI; no `bc-dcbor-go`/`dcbor-go` under
`blockchaincommons` on the Go proxy.

**Methodology near-miss (a re-runner will hit this).** The first JCS run
showed Go rendering `0.1 + 0.2` as `0.3` while both Python libraries gave
`0.30000000000000004`. That was not a library divergence: Go folds
`0.1 + 0.2` as *untyped constants* at compile time, exactly, and only then
converts to `float64`. Re-run with runtime values
(`strconv.ParseFloat("0.1") + strconv.ParseFloat("0.2")`) all three agreed.
Any Go-side conformance test must compute its inputs at runtime or parse
them from strings.

### 9.1 Baseline: stdlib shortest-float rendering, Go vs Python (no JCS)

Same digits, different format. Go `strconv 'g' -1` / `json.Marshal` vs Python
`repr` / `json.dumps`: `1` vs `1.0`; `1.23456789e+08` vs `123456789.0`;
`1e+15` vs `1000000000000000.0`; `1e+16` vs `1e+16` (agree); `1e-07` vs
`1e-07` (agree); `1e-06` vs `1e-06`; `100` vs `100.0`; `1.2345678901234568e+18`
vs `1.2345678901234568e+18` (agree). Go `'f' 6` of the max double is a
309-digit fixed string.

### 9.2 JCS: `gowebpki/jcs` vs Python `jcs` vs Python `rfc8785`

Inputs (Go via `json.Marshal` → `jcs.Transform`; Python via
`jcs.canonicalize` / `rfc8785.dumps`) and the shared sha256 prefix:

| case | all three libs | sha256[:16] |
|---|---|---|
| v2 receipt, `score=1.0`, `claims_checked=0` | `{"action":"pass","claims_checked":0,"engine":"bonsai","flagged_hash":"ee","grounded":true,…,"score":1,"verdict_hash":"sha256:dd"}` | `472ef3a1bcb7e728` |
| `{grounded:false, score:2/3, claims_checked:3}` | `{"claims_checked":3,"grounded":false,"score":0.6666666666666666}` | `e4877bd27b4507b4` |
| 17 literal doubles | `[1,0.5,0.6666666666666666,…,1e+21,1e-7,0.000001,123456789,10000000000000000,1000000000000000,5e-324,1.7976931348623157e+308,0,100,0.00001,0.1,1234567890123456800]` (`-0.0`→`0`) | (Go literal list differs only by the constant-folded `0.3`; see near-miss) |
| 9 runtime doubles | `[0.30000000000000004,0.6666666666666666,123456789012345680000,123456789012345680000,0.30000000000000004,0.3333333333333333,1e+21,0.000001,0.000001]` | `91db614e8a302da9` |
| strings | `{"s":"a\"b\\c\n\r\t\b\f\u0001\u001f<>&/é😀 "}` | `79d585b13a81345e` |
| key order | `{"B":6,"a":2,"aa":5,"b":1,"é":3,"éx":7,"😀":4,"～":8}` | `fb5f25a65b7b5926` |
| null/empty | `{"w":{},"x":null,"y":"","z":[]}` | `31530b18bfa7be0c` |
| JSON text `"score": 1` vs `"score": 1.0` (Python only) | both `{"claims_checked":0,"grounded":true,"score":1}` | `30ac435f4c15af99` |
| `{"n": 2⁵³}` | `{"n":9007199254740992}` — Go, jcs; **rfc8785: `IntegerDomainError`** | `66c87d9cb3014e05` |
| `{"n": 2⁵³+1}` | Go, jcs: `{"n":9007199254740992}` (silently rounded); rfc8785: `IntegerDomainError` | — |
| `{"n": MaxInt64}` | Go, jcs: `{"n":9223372036854776000}`; rfc8785: `IntegerDomainError` | — |

### 9.3 Deterministic CBOR and COSE_Sign1

`fxamacker/cbor` `CoreDetEncOptions()` (`Sort: SortBytewiseLexical,
ShortestFloat: ShortestFloat16, NaNConvert: 7e00, InfConvert: Float16`) vs
`cbor2.dumps(canonical=True)` — identical hex for: `1.0`→`f93c00`, int
`1`→`01`, `0.5`→`f93800`, `2/3`→`fb3fe5555555555555`, `0.1`→`fb3fb999999999999a`,
`-0.0`→`f98000`, `0.0`→`f90000`, NaN→`f97e00`, +Inf→`f97c00`,
`1e21`→`fb444b1ae4d6e2ef50`, `65504`→`f97bff`, `65505`→`fa477fe100`,
`1e-7`→`fb3e7ad7f29abcaf48`, `2⁵³+1`→`1b0020000000000001`, `-1`→`20`, `true`→`f5`,
nil→`f6`, bytes→`43010203`, `"é😀"`→`66c3a9f09f9880`, and both receipt maps
(text keys) e.g. `a46573636f7265f93c0066656e67696e65…`. Mixed-type keys:
`{300:1,"z":2}` → cbor2 `a2617a0219012c01`, fxamacker core `a219012c01617a02`,
fxamacker `CanonicalEncOptions()` `a2617a0219012c01`. `{-1:1,"a":2}`,
`{"a"*24:1,"zz":2}`, `{1000000:1,"abcd":2}`: all agree.

COSE_Sign1, ES256, protected `{1:-7, 4:"node-1297"}`, payload `hello receipt`:
go-cose sign → pycose verify **OK** (with `cbor2` 5.9.0); pycose sign →
go-cose verify **OK**; one payload byte tampered → go-cose `verification
error`. go-cose signature length 64 (raw `r||s`). With `cbor2` 6.1.4, pycose
fails to decode even its own message: `cbor2.loads` returns the tagged
array as `tuple`, `CoseMessage.decode` requires `list` →
`TypeError("Bytes cannot be decoded as COSE message")`.

### 9.4 Injectivity of the shipped newline canon

Scratch test against the real `aep.Canonicalize` (deleted after the run):
`{Engine:"bonsai\nx", Model:"y"}` and `{Engine:"bonsai", Model:"x\ny"}` (all
other fields equal) → identical bytes `"n\nj\nt\nbonsai\nx\ny\ntrue\n1.000000\n0\nh"`.

### 9.5 `canonicaljson` 2.0.0

`{"score": 0.5}` → `{"score":0.5}`; `{"score": 1.0}` → `{"score":1.0}` (Python
`repr`, not ES6 — non-conformant, not a refusal); `{"s":"é"}` → raw UTF-8.

### 9.6 Adoption / dependency checks

`github.com/sigstore/rekor` v1.5.4 and `github.com/sigstore/sigstore-go`
v1.3.0 `go.mod` both require `github.com/cyberphone/json-canonicalization`;
`in-toto-golang` v0.11.0 does not.
