# Design: Node Identity Persistence + Signed AEP Receipts (aceteam #8139 / #8253)

## Context

Two adjacent-but-distinct asks, both already partially shipped on citadel:

- **#8139** ("local node self-identity"): `citadel whoami` should answer "what is
  this node's numeric AceTeam fabric ID" in one call. `citadel whoami` itself
  shipped (citadel#844) and works for everything EXCEPT this one field, because
  the backend never gives the node that number. That gap is fully diagnosed in
  [`docs/whoami-fabric-id-gap.md`](./whoami-fabric-id-gap.md) — this doc treats
  that file as the authority on the gap and does not re-litigate the finding,
  only the fix.
- **#8253** ("Trust Engine guardrails on-node + sign the AEP receipt"): the
  guardrail half shipped (citadel#847, `internal/trust`) — a fabricated-number
  detector that attaches a `grounding` map to `llm_inference` job output. The
  signing half was explicitly deferred at merge time with a one-line placeholder
  ("overlaps the nodevault node-identity lane, out of scope here" —
  `internal/trust/grounding.go:14-20`). This doc is that deferred design.

**Scope of this doc:** citadel-Go design only, markdown, no code. #8253's
signing overlaps `internal/nodevault` (the master-PIN/browser-session lane,
owned by a different agent per the 2026-08-25 lane split — see
`internal/nodevault`'s recent history below). This doc reads `internal/nodevault`
read-only, does not modify it, and treats "which key signs the receipt" as an
open decision for Jason, not something resolved here.

---

## 1. Current state

### 1a. `citadel whoami` and the fabric-ID gap (#8139)

`cmd/whoami.go`'s `gatherIdentity` (`cmd/whoami.go:123-218`) assembles
`NodeIdentity` (`cmd/whoami.go:64-117`) from four sources: `citadel.yaml`
(`findAndReadManifest`), the device-config file (`getDeviceConfigFromFile`),
`SSHSyncConfig` (`nexus.LoadSSHSyncConfig`, opportunistic), and a live
Headscale probe (`network.GetGlobalStatus`). Two identity fields on that struct:

- `HeadscaleNodeID` (`cmd/whoami.go:73-78`) — the mesh/coordination-server
  numeric ID, resolvable but only LIVE (nothing persists it).
- `PlatformNodeID` (`cmd/whoami.go:79-83`) — the AceTeam-platform numeric node
  ID (what `fabric_node_status`/`nexus_get_node` and friends key on). Sourced
  from `SSHSyncConfig.NodeID`, which is **empty on essentially every real
  node today**, per the exhaustive grep in
  [`whoami-fabric-id-gap.md`](./whoami-fabric-id-gap.md#the-finding).

The root cause, restated at the mechanism level (not just "it's missing"):
`nexus.TokenResponse` (`internal/nexus/deviceauth.go:135-147`, the `/token`
response `citadel init`/`citadel login` actually receive) carries
`OrgID`/`OrgName`/`UserEmail`/`UserName`/`DeviceAPIToken`/`RedisURL`/
`APIBaseURL` — no node-ID field exists on the wire type at all. There is
nothing to parse even if the node wanted to persist it.

**The slot the gap doc points to needs a correction, verified this session.**
`SSHSyncConfig.NodeID` (`internal/nexus/sshkeys.go:46-51`) looks like the
obvious persistence target, but its read/write pair has a trap:
- `LoadSSHSyncConfig` (`internal/nexus/sshkeys.go:232-279`) returns `nil, nil`
  — i.e. "not configured" — **unless both `APIToken` and `NodeID` are
  non-empty** (`sshkeys.go:273-276`). A node-ID-only write is silently
  discarded on every subsequent read.
- `SaveSSHSyncConfig` (`internal/nexus/sshkeys.go:281-298`) writes all three
  fields (`api_token`, `node_id`, `base_url`) from the struct it's given, with
  no partial-update path. A caller that only knows the node ID (not the SSH
  sync API token, a different credential) and calls `SaveSSHSyncConfig` with
  the rest zeroed would **clobber `api_token` to `""`**, breaking SSH key
  sync for any node that had it configured, to write a field it wasn't
  designed to carry. `SaveSSHSyncConfig` already has zero non-test callers
  (`whoami-fabric-id-gap.md`); this is why — nothing was safe to hang off it.

**The safer existing slot is the device-config file**, not `SSHSyncConfig`.
`getDeviceConfigFromFile`/`saveDeviceConfigToFile`
(`cmd/devicecreds_hooks.go`) already carry `OrgID`/`OrgName`/`UserEmail`/
`UserName` from the *same* `/token` response, are machine-convergent under
`network.GetNodeConfigDir()` (citadel#845 — see CLAUDE.md's Device/org config
note), and `gatherIdentity` already reads this file first for those fields
(`cmd/whoami.go:149-157`). Adding one more optional field here is
structurally identical to fields already flowing through it — no clobber
risk, no new file, no new read-path branch in `whoami`.

### 1b. The grounding receipt (#8253, guardrail half — shipped, citadel#847)

`trust.CheckGrounding(input, output string) GroundingResult`
(`internal/trust/grounding.go`) is a pure, local, non-LLM check. Its result
shape (`grounding.go:108-133`):

```go
type GroundingResult struct {
    Grounded      bool
    Score         float64   // supported/eligible, 1.0 when ClaimsChecked==0 (vacuous)
    ClaimsChecked int       // denominator — disambiguates "nothing to check" from "all clean"
    Flagged       []Claim   // {Value, Kind, Reason} per unsupported claim
}
```

The ONE wired call site, `LLMInferenceHandler.bufferedChatCompletions`
(`internal/worker/llm_inference.go:788-807`), gates on
`groundingGuardrailEnabled()` (env `CITADEL_GROUNDING_GUARDRAIL`, default OFF,
`llm_inference.go:991-1005`) and shapes the result into a plain
`map[string]any` (`groundingReceipt`, `llm_inference.go:1022-1038`):

```go
output["grounding"] = map[string]any{
    "grounded": bool, "score": float64, "claims_checked": int,
    "flagged": []map[string]any{ {"value","kind","reason"}, ... },
}
```

This map is attached to `output` alongside `content`/`finish_reason`/`usage`
and returned as ordinary job output — **it is never signed, hashed, persisted
independently, or transmitted as anything other than a field of the job
result**. There is no `node_id`, `timestamp`, or `job_id` in it today; nothing
identifies which node or which run produced it beyond whatever wrapper the
job envelope itself carries.

### 1c. What signing primitive nodevault provides — read-only finding

This is the load-bearing finding for section 3. `internal/nodevault` (recent
history: `e250b3e` #818, `aa61501` #796/#808 — the active browser-session/
master-PIN epic) is a **symmetric secrets-at-rest vault, not a signing
primitive**:

- `Vault` (`internal/nodevault/vault.go:167-229`) wraps a random DEK behind an
  Argon2id-derived KEK from a user-entered master PIN (`SetPIN`,
  `vault.go:210-260`). `Unlock(pin)` (`vault.go:324`) returns a `Session`.
- `Session` (`internal/nodevault/session.go:20-143`) holds the DEK **only in
  memory**, never on disk. `DeriveSubkey(context)`
  (`session.go:58-69`) returns a 32-byte **HKDF-SHA256** subkey bound to a
  context string; `Seal`/`Unseal` (`session.go:71-133`) are **AES-256-GCM**
  built on such a subkey. There is no ECDSA/Ed25519/RSA anywhere in this
  package — no asymmetric keypair, nothing that produces a verifiable
  signature over arbitrary bytes.
- Critically, `Session.Lock()` (`session.go:49-56`) **discards the DEK**, and
  the package doc for the browser-session feature this vault backs states the
  design intent explicitly: this is what "keeps encrypted surfaces dark
  across an unattended restart." The vault is deliberately PIN-gated and
  short-lived by design, not an oversight.

**Consequence: nodevault cannot back unattended signing as currently
designed**, independent of whether touching it is in-lane. One could seed an
Ed25519 key from `session.DeriveSubkey("aep-receipt-signing")` — deterministic
across unlocks, technically straightforward — but every `citadel work`
running headless under systemd (the normal deployment: CLAUDE.md's Windows/
Linux service sections, no interactive PIN entry) would have no unlocked
`Session` at inference time. A receipt-signing design built on nodevault would
sign only during whatever windows the vault happens to be unlocked for
browser-session/PIN purposes — and "sometimes signed, silently unsigned the
rest of the time" is worse for a verifier than "never signed": it can't
distinguish "this node chooses not to sign" from "this isn't a real node,"
which defeats the acceptance criterion in #8253 ("verifies offline against
the node's public identity").

### 1d. The other candidate: `internal/nodeidentity` — unattended-capable, but dormant

`internal/nodeidentity.Store` (`internal/nodeidentity/nodeidentity.go`,
citadel#441, "#4583 P2 PR-0") is a **separate package** from nodevault, with
an actual asymmetric primitive:

- `GetOrCreateKey()` (`nodeidentity.go:105`) generates an **ECDSA P-256**
  keypair, persisted 0600, **no PIN gate, no unlock step** — available to any
  process that can read the node's own filesystem, exactly the availability
  an unattended `citadel work` needs.
- `GenerateCSR` / `StoreLeaf` / `LoadLeaf` (`nodeidentity.go:169-258`) exist
  for an mTLS CSR/leaf-cert flow that is **not yet backend-activated**:
  `ensureNodeIdentity` (`cmd/init.go:1672-1705`) is fully fail-open — every
  failure path is logged and swallowed because "the backend fabric CA is not
  activated fleet-wide yet... a node with no key, no CA, or a hung CA
  endpoint pairs and runs exactly as it does today" (`cmd/init.go:1679-1682`).
  So the keypair itself is real and generated at every `citadel init`, but
  the backend has never been given the public half through the CSR path in
  production.
- Git history (`git log -- internal/nodeidentity`) shows exactly one commit
  since its introduction (#441) — it has had no follow-on activity, unlike
  nodevault's recent commits from the active browser-session/master-PIN
  epic. Nothing currently claims this package as an active lane.

**This reframes the open question the task posed.** It is not "may I touch
nodevault, or should I hand this to the other agent" — it's "does AEP-receipt
signing belong in the master-PIN vault's lane at all, given that vault
structurally cannot do unattended signing as designed." Section 3 below
proposes `nodeidentity`'s existing ECDSA key as the signing primitive
precisely because it is the one node-local key that is both asymmetric and
available without a human present — and flags that this is a recommendation
for Jason to confirm, not a decision made unilaterally.

---

## 2. #8139: fabric node ID persistence

### The cross-repo contract

Two possible echo points on the aceteam side (this doc does not pick a
winner — see the open question below, which depends on backend-side ordering
this repo cannot verify):

1. **Device-auth `/token` response** — add a `node_id` (or `fabric_node_id`)
   field to `TokenResponse` (`internal/nexus/deviceauth.go:135-147`). Simplest
   wire change, but only correct if the backend already has a fabric node row
   at the moment it issues the authkey — and the node does not join Headscale
   (which is what actually creates a fabric node record, per the existing
   `HeadscaleNodeID` resolution path) until *after* receiving that authkey.
   If node creation is downstream of Headscale join, this field would be
   structurally unpopulatable at `/token` time.
2. **Heartbeat ack** — the platform already receives the node's heartbeat
   (`internal/heartbeat/`) on a ~30s cadence; an ack/response body carrying
   `node_id` would land after the node genuinely exists fabric-side, at the
   cost of a round-trip delay before `whoami` can show it (acceptable — this
   is a display convenience, not something anything blocks on).

**Citadel-Go side, bounded regardless of which echo point wins:**

- A field on `DeviceConfig` (the struct `getDeviceConfigFromFile`/
  `saveDeviceConfigToFile` read/write) — e.g. `FabricNodeID string`
  — populated wherever the chosen echo response is first parsed (either
  `internal/nexus/deviceauth.go`'s token-parse path, or the heartbeat ack
  handler, whichever the backend ships against). Additive to the existing
  device-config struct, same load-modify-save discipline `APPLY_DEVICE_CONFIG`
  already uses for other fields.
- `gatherIdentity` (`cmd/whoami.go:149-157`) gains one more read off the same
  `dc := getDeviceConfigFromFile()` it already has in hand — no new file
  read, no new I/O.
- `NodeIdentity.PlatformNodeID` stops relying on `SSHSyncConfig` (or keeps it
  as a documented last-resort fallback, since it costs nothing to leave the
  opportunistic read in place) and prefers the device-config value.
- `identity.json` (`writeIdentityCache`, `cmd/whoami.go:220-244`) stays a pure
  **derived cache** — this is important to keep explicit: it is not the
  authoritative store, so nothing about this design writes fabric-ID state
  into it that a re-run of `whoami` wouldn't reproduce from the device-config
  file.

No nodevault involvement in this half at all — it is a small, self-contained
persistence-and-read slice.

---

## 3. #8253: signed AEP receipt

### Receipt shape and canonicalization

The signed object should be a **canonical byte sequence over an explicit,
ordered field list** — not `json.Marshal` of the live `map[string]any` the
guardrail already produces. Two reasons: (a) Go's `encoding/json` does sort
map keys, so naive marshal-then-sign is *closer* to safe than it looks, but
(b) `Score` is a `float64`, and any intermediary re-serialization the receipt
passes through before the backend verifies it (a Redis round-trip, the
backend's own re-marshal of the enclosing job-output envelope) can change
byte representation without changing value — which breaks a naive
byte-signature even though nothing semantically changed. Concretely:

```
AEPReceiptV1 {
    node_id        string   // the fabric/platform node ID from §2, or the
                             // node's public-key fingerprint if #2 hasn't
                             // landed yet — see the phasing note below
    job_id         string   // binds the signature to ONE job; without this,
                             // a valid signature is copy-pasteable onto any
                             // other job's output
    issued_at      string   // RFC3339, node-local clock
    engine         string   // e.g. "bonsai", "vllm" — what actually served it
    model          string
    grounded       bool
    score          float64
    claims_checked int
    flagged_hash   string   // sha256 of the canonical Flagged list, not the
                             // full list inline — keeps the signed payload
                             // small and fixed-shape regardless of how many
                             // claims were flagged
}
```

Sign `sha256(canonical-concatenation-of-the-above-fields-in-this-order)` (or
adopt RFC 8785 JSON Canonicalization if a JSON-shaped signed payload is
preferred for readability — either works, but pick one explicitly rather than
signing whatever bytes happen to come out of a map). **The signature itself
is a sibling field, added AFTER signing, explicitly excluded from the signed
bytes** — a signature that covers its own field is the standard way this
class of scheme breaks silently.

### Which key signs it — the crux, flagged prominently

Per §1c/§1d: **nodevault's `Session`/`DeriveSubkey` is symmetric and PIN-gated
and cannot back unattended signing as currently designed.**
`internal/nodeidentity.Store`'s existing ECDSA P-256 keypair
(`GetOrCreateKey`, `nodeidentity.go:105`) is unattended-capable and already
generated at every `citadel init` — it is the recommended primitive, but this
is a recommendation for Jason to confirm, not a decision this doc makes
unilaterally, because:

- `internal/nodeidentity` was built for a different purpose (mTLS
  self-reenrollment via the fabric CA, `cmd/init.go:1672-1705`) that is not
  yet backend-activated. Repurposing its keypair for receipt-signing is a
  second consumer of a key whose primary intended consumer doesn't exist in
  production yet — worth an explicit yes from whoever owns that CA-activation
  roadmap, not just this doc.
- If the CA does activate later, the SAME key would then be doing double duty
  (mTLS client auth AND receipt signing). That may be fine (one node identity,
  one key, multiple uses is a defensible design) or may be something the mTLS
  design wants kept separate — another call for whoever owns #4583 P2.
- The nodevault-lane agent may have a different plan already in flight (e.g.
  extending nodevault to also manage an *unattended*, non-PIN-gated signing
  key alongside its PIN-gated secrets) that this doc has no visibility into.
  **This doc does not assume nodeidentity is free to repurpose without
  checking with that lane** — it only establishes that nodevault-as-built
  cannot be the answer, which narrows but does not close the decision.

**What citadel needs from whichever key is chosen, expressed as an
interface** (not committing to which package implements it):

```go
// Signer is what internal/worker (or wherever receipt assembly lives) needs
// to produce a signed AEP receipt. Implemented by nodeidentity.Store (or a
// nodevault-owned equivalent, or something new) — the caller should not need
// to know which.
type Signer interface {
    Sign(payload []byte) (signature []byte, err error)
    PublicKeyFingerprint() string // stable identifier the backend can look
                                   // up against a registered public key
}
```

### Backend verification and public-key registration

Signing is inert unless the backend holds the node's public key. Today it
doesn't for *either* candidate key: nodevault has no public key at all (it's
symmetric), and nodeidentity's `GenerateCSR`/`StoreLeaf` path exists but the
fabric CA is unactivated, so no leaf has ever been issued in production. This
means #8253's signing and #8139's node-ID echo actually want the **same wire
moment**: a node publishing its public-key fingerprint (or full SPKI) to the
backend, and the backend echoing the fabric node ID back, are naturally one
additive exchange rather than two separate round-trips — see §4.

Verification, aceteam-side: given `node_id` on the receipt, look up the
registered public key for that node, recompute the canonical digest from the
receipt's own fields, verify the signature. Offline verification (per the
issue's acceptance criterion) just means the verifier needs the node's public
key cached/distributed — it does not need to call back into the node itself.

### Threat model — stated honestly, not oversold

An on-disk 0600 ECDSA key (no TPM, no secure element, no HSM) attests **"this
filesystem produced this signature,"** not **"this specific hardware did."**
It is real protection against: tamper-in-transit (a MITM or compromised relay
altering the receipt after the node produced it), and backend-side forgery (a
malicious or buggy backend fabricating a receipt and claiming a node produced
it). It does **not** protect against: a node whose root/filesystem is fully
compromised (the attacker reads the key and signs whatever they want), or
proving the inference genuinely ran on the claimed GPU rather than being
replayed. The issue's acceptance criterion — "verifies offline against the
node's public identity" — is satisfiable by this design. "Provable
sovereignty" in the stronger hardware-attestation sense is not, and this doc
does not claim it is; that would need a TPM-backed key or similar, out of
scope here and not implied by #8253's acceptance text.

### Scope boundary: not designing the Merkle-DAG

#8033 ("AEP receipts... Merkle-DAG, epic #8033") is a separate, larger
aceteam-side epic for how receipts compose across a job's full provenance
chain. This doc signs **this one receipt** (the grounding/AEP output of a
single `llm_inference` job) and leaves DAG composition, cross-job linking, and
receipt storage/retrieval entirely to that epic. If #8033 lands a different
canonical receipt envelope, this design's `AEPReceiptV1` shape should be
treated as a candidate leaf node in that DAG, not a competing format.

---

## 4. Cross-repo contract summary

| Piece | Owner | Depends on |
|---|---|---|
| `TokenResponse`/heartbeat-ack carries `node_id` | aceteam-side | Whether a fabric node row exists at `/token` time (open question, backend-side) |
| `DeviceConfig.FabricNodeID` field + `gatherIdentity` read | **citadel-Go, this repo** | The above landing on one echo point |
| Node publishes public-key fingerprint to backend | **citadel-Go** (send) + aceteam-side (store/register) | Which key is chosen (§3) — natural to bundle with the node-ID echo, same round-trip |
| `Signer` interface + wiring into `groundingReceipt` | **citadel-Go**, behind a default-OFF toggle (see phasing) | Key decision (§3), nodevault-lane sign-off if nodeidentity's repurposing needs it |
| Canonical `AEPReceiptV1` signing + attach to job output | **citadel-Go** | Signer interface |
| Backend signature verification against registered pubkey | aceteam-side | Public-key registration landing |
| Merkle-DAG composition (#8033) | aceteam-side, separate epic | Not blocked by this doc; this doc's receipt is a candidate leaf |

---

## 5. Phased breakdown

**Phase 1 — #8139 persistence hook + read (citadel-Go, no backend dependency
to START, inert until backend echoes).**
Add `DeviceConfig.FabricNodeID`, wire it into whichever echo point the backend
picks, update `gatherIdentity`/`NodeIdentity.PlatformNodeID` to prefer it over
the `SSHSyncConfig` fallback. Small, mechanically similar to existing
device-config fields. Ships dark (empty field) until the backend sends
something.

**Phase 2 — public-key publication (citadel-Go + aceteam-side, bundled with
Phase 1's wire moment per §4).**
Once the signing key is chosen (§3's open question resolved), the node sends
its public-key fingerprint alongside whatever request/response is already
carrying the node-ID echo. Backend stores/registers it. This phase is
gated entirely on the key decision, not on engineering effort.

**Phase 3 — signing, default-OFF (citadel-Go).**
`Signer` implementation wired into `groundingReceipt`
(`internal/worker/llm_inference.go:1022`), gated behind a new env toggle
(e.g. `CITADEL_SIGN_AEP_RECEIPTS`) matching this codebase's existing
default-OFF advisory-signal convention (`CITADEL_GROUNDING_GUARDRAIL`,
`CITADEL_ENERGY_SAMPLING`, `CITADEL_RESOURCE_ISOLATION`). Additive/omitempty:
a receipt-signing-off node's job output stays byte-identical to today.
Blocked on Phase 2 (no point signing if the backend can't verify) and on the
key decision (§3).

None of these three phases requires modifying `internal/nodevault`. Phase 3
may require a small addition to `internal/nodeidentity` (an exported `Sign`
method on `Store` if one doesn't already fit the bill — `GetOrCreateKey`
already returns the raw `*ecdsa.PrivateKey`, so this may be as small as a
wrapper) — that is a nodeidentity-package change, not a nodevault one, and
still worth flagging to whoever else might be touching that package before
starting.

---

## Open questions for Jason

1. **Nodevault-lane / key-choice decision (blocks Phase 2/3 entirely, read
   this first):** nodevault as built cannot back unattended signing (§1c) —
   it's a PIN-gated symmetric vault, not an asymmetric signing primitive.
   `internal/nodeidentity`'s existing ECDSA key (§1d) is unattended-capable
   and dormant since its one commit (#441), but it was built for a different,
   not-yet-activated purpose (mTLS self-reenrollment). Do you want: (a)
   nodeidentity's key repurposed for receipt signing, (b) nodevault extended
   with a NEW non-PIN-gated signing capability (a real scope change to that
   package, and squarely the other agent's lane to design), or (c) a
   dedicated new key separate from both? This also determines whether I need
   to coordinate with the nodevault-lane agent before Phase 3 starts, or
   whether nodeidentity is genuinely free to extend unilaterally.
2. **Where does the backend echo the fabric node ID** — `/token` response, or
   heartbeat ack? Depends on backend-side ordering (does a fabric node row
   exist before Headscale join) that isn't visible from this repo.
3. Should public-key publication (§4) ride the SAME wire exchange as the
   node-ID echo (my recommendation — one round-trip serves both #8139 and
   #8253), or does the backend team want them as separate endpoints?
4. Is `AEPReceiptV1`'s field list (§3) the right shape, or does #8033's
   Merkle-DAG epic already have an envelope this should conform to instead of
   inventing its own?
