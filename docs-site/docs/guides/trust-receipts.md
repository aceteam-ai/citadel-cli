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

## Current Status

Both layers are shipped and working today, gated behind the two environment
variables above. A dedicated CLI command for independently verifying a
receipt against a node's public key is planned but not yet released -- for
now, verification happens on the receiving end (e.g. the AceTeam platform),
which is the intended consumer of these receipts.

| Variable | Default | Effect |
|---|---|---|
| `CITADEL_GROUNDING_GUARDRAIL` | off | Attach `grounding` + `trust_verdict` to chat-completion results. |
| `CITADEL_SIGN_AEP_RECEIPTS` | off | Also attach a signed `aep_receipt`. Requires the guardrail above to also be on. |

Set either to `1`, `true`, `yes`, or `on` to enable.
