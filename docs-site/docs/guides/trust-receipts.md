---
sidebar_position: 6
title: Trust Receipts & Grounding Checks
---

# Trust Receipts & Grounding Checks

Running inference on your own hardware means nobody outside your
organization has to see a prompt or a response. It also means you can attach
an auditable, cryptographically signed record to every answer your node
produces -- proof of what was asked, what was returned, and that the node's
identity signed off on it, without sending the content itself anywhere.

This is opt-in and off by default. Turning it on adds two independent layers
to your node's chat-completion responses.

## Layer 1: Grounding Checks

A local, deterministic check -- no extra model call, no network request --
that flags numeric or statistical claims in a response that aren't backed by
anything in the prompt. It exists because language models occasionally turn
a vague source statement ("a majority of respondents") into a specific,
fabricated number ("68% of respondents") that was never in the input.

Enable it on a node:

```bash
export CITADEL_GROUNDING_GUARDRAIL=1
citadel work
```

Every chat-completion response from that node now carries a `grounding`
field and a `trust_verdict` field alongside the normal reply content:

- `grounding` -- whether the reply's claims are supported by the input, and
  which claims (if any) were flagged.
- `trust_verdict` -- a broader pass/flag verdict that also runs lightweight
  checks for secrets, personally identifiable information, and
  education-record-protected content potentially present in the response.

This is advisory, not a gate: a flagged response is still returned to the
caller. Nothing is blocked or redacted by default -- the signal is there for
downstream systems (or a human) to act on.

## Layer 2: Signed Receipts

On top of grounding, a node can cryptographically sign a compact receipt for
each response using a key generated locally when you ran `citadel init` --
the same key that never leaves your hardware.

```bash
export CITADEL_GROUNDING_GUARDRAIL=1
export CITADEL_SIGN_AEP_RECEIPTS=1
citadel work
```

Each response now also carries an `aep_receipt` object: a signed digest of
the input, the output, and the trust verdict, plus your node's public-key
fingerprint. Anyone holding your node's public key can verify that a given
response really was produced by your hardware and really does match its
recorded verdict -- without you having to hand over the original prompt or
response to prove it.

Signing fails safe: if the node's signing key is ever unavailable for any
reason, the response is still returned normally, just without the
`aep_receipt` field attached.

`aep_receipt` stands for **AceTeam Execution Proof** -- the receipt is a
signed proof of what a node's inference execution actually produced.

## Verifying a Receipt: `citadel aep verify`

You don't need the AceTeam platform, a network connection, or any credentials
to check that a receipt is genuine. `citadel aep verify` checks a receipt's
signature entirely offline, against a node's public identity:

```bash
citadel aep verify receipt.json
```

```
✓ signature valid
  signed by node   a1b2c3d4e5f6...
  verifying key    node identity
  receipt version  2
  engine/model     vllm / llama-3.1-8b
  grounded         true (score 1.000000, 0 claims checked)
  action           pass
  verdict_hash     sha256:9f8e7d...
```

### Resolving the verifying key

By default, `citadel aep verify` checks a receipt against the identity key of
**the node you're running it on** -- useful when you're the node operator
verifying your own node's output. To verify a receipt against a *different*
node's identity, supply that node's public key explicitly:

```bash
citadel aep verify receipt.json --pubkey exit-node-pubkey.pem   # a PKIX/SPKI PEM public key
citadel aep verify receipt.json --cert exit-node-leaf.pem       # or an X.509 certificate
```

Precedence is `--cert` > `--pubkey` > this node's own identity -- the first
one supplied wins.

### Other flags

| Flag | Effect |
|---|---|
| `--json` | Emit a machine-readable JSON result instead of the human-readable summary. |
| `--show-canonical` | Print the exact canonical bytes that were signed, to stderr -- useful for debugging a signature mismatch. |

### Reading a failure

`citadel aep verify` exits non-zero on any verification failure and reports
*why*, distinguishing a few different cases rather than a single generic
"invalid":

- **Signed by a different node.** The verifying key's fingerprint doesn't
  match the fingerprint recorded in the receipt. This means you're checking
  against the wrong node's key (or someone else's), not that the receipt was
  tampered with:

  ```
  ✗ receipt was signed by a different node (receipt fingerprint a1b2..., verifying key f9e8...)
  ```

- **Invalid signature.** The fingerprints match, but the signature itself
  doesn't verify against the canonical bytes -- this is the case that
  indicates real tampering or corruption:

  ```
  ✗ signature is invalid (does not verify against the signing key)
  ```

- **Unsigned receipt.** The receipt has no signature at all (produced with
  `CITADEL_GROUNDING_GUARDRAIL` on but `CITADEL_SIGN_AEP_RECEIPTS` off).

Checking the fingerprint before the signature is deliberate: it's what makes
"you're pointed at the wrong key" and "this receipt is actually invalid" two
distinguishable outcomes instead of one confusing failure.

## Current Status

Both layers, plus offline verification via `citadel aep verify`, are shipped
and working today. Layers 1 and 2 are gated behind the two environment
variables below.

| Variable | Default | Effect |
|---|---|---|
| `CITADEL_GROUNDING_GUARDRAIL` | off | Attach `grounding` + `trust_verdict` to chat-completion results. |
| `CITADEL_SIGN_AEP_RECEIPTS` | off | Also attach a signed `aep_receipt`. Requires the guardrail above to also be on. |

Set either to `1`, `true`, `yes`, or `on` to enable.
