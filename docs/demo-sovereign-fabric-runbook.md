# Demo runbook — The Sovereign Compute Fabric, in four acts

**Audience:** a technical prospect/partner (e.g. an investor or design-partner engineer).
**Duration:** ~12–15 minutes.
**Thesis:** *You can run real AI on hardware you own — join it in minutes, chat with
it over your own private network, get a cryptographically signed receipt for every
answer, and even route your traffic out through it — without a hyperscaler in the
middle.*

The strongest version of this demo is **one node telling all four stories in
sequence**, not four separate setups. A single machine joins the fabric on stage
(Act 1), immediately serves and answers a prompt (Act 2), signs that answer with
its own identity key (Act 3), and finishes as a sovereign internet exit (Act 4).

> **Golden rule for a live demo: every act has a captured fallback.** Network
> demos fail. Each act below ends with a recorded transcript you can paste if the
> live path stalls. Rehearse the whole thing end to end at least once and keep the
> captured outputs open in a second window.

---

## 0. Setup (do this before the audience is in the room)

You need **two throwaway machines** and one already-serving node:

| Role | What | Why |
|---|---|---|
| **Demo node** | A fresh Ubuntu box (a Proxmox LXC/VM works) with no Citadel state. | The star of Acts 1–3 — joins live, serves, signs. |
| **Client node** | A second fresh box on a *different* network/uplink. | Act 4 — proves egress actually changes your public IP. |
| **Serving node (optional)** | An existing GPU node (e.g. a lab node already running engines). | Fallback for Act 2 if the demo node can't serve a model cheaply. |

**Provisioning notes learned the hard way (see the pve scaffolding appendix):**
- Mint a **non-ephemeral, reusable** auth key before you start — an *ephemeral* key
  removes the node from the coordination server the moment the joining process
  exits, which breaks any second command (like `citadel proxy`).
- On a fresh unprivileged LXC, don't `apt install` during the demo — first-boot
  auto-upgrades hold the package lock. Everything below uses what ships in the
  base image plus the `citadel` binary.
- If a container is on a secondary uplink whose DNS resolver isn't reachable, set
  `nameserver 1.1.1.1` before joining.

---

## Act 1 — Add sovereign compute in minutes (node lifecycle)

**Story:** "Watch me turn a bare machine into a node on our fabric, live."

**Live:**
```bash
# On the fresh demo node:
curl -fsSL https://get.aceteam.ai/citadel | sh      # installs the citadel agent
citadel init                                         # network-only join, no sudo needed
```
`citadel init` prints a short **device code** and a URL. Enter the code at
`aceteam.ai/device` on the projector — the node authorizes and joins the **AceTeam
Network** (our private mesh). This on-stage code-entry beats a pre-baked auth key:
the audience watches the machine come online.

Show it joined:
```bash
citadel status          # network IP assigned, connected
citadel whoami          # node identity
```
And from the platform side, the node now appears in the fabric node list.

Then bring the worker online **with signing enabled** (this same process powers
Acts 2 and 3):
```bash
CITADEL_GROUNDING_GUARDRAIL=1 CITADEL_SIGN_AEP_RECEIPTS=1 citadel work
```

**Captured fallback:**
```
$ citadel init
  → Device code: WDJF-2098
  → Open https://aceteam.ai/device and enter the code to authorize this node.
  ✓ Authorized. Joining the AceTeam Network...
  ✓ Connected. Network IP: 100.64.0.61
$ citadel status
  AceTeam Network:   connected (100.64.0.61)
  Node:              demo-node
  Services:          (none yet)
```

---

## Act 2 — Chat with a model running on your own hardware (mesh inference)

**Story:** "This model isn't in someone's cloud — it's on that node, and I'm
reaching it over our own private mesh, node to node."

**Live** (run from the demo node or the client node — anything on the mesh):
```bash
citadel mesh models                       # every model served across the fabric
citadel mesh chat --model llama3.1:8b "Explain sovereign compute in one sentence."
```
`citadel mesh models` discovers the models served by *other* nodes on the mesh and
prints `model → node / engine / address`. `citadel mesh chat` routes the prompt
node-to-node to whichever node serves that model — no hyperscaler, no public
internet hop. (If a model name is unique you can use `--model`; otherwise pick the
node with `--node <name-or-network-ip>`.)

**Captured fallback:**
```
$ citadel mesh models
MODEL                               NODE         ENGINE          ADDRESS
llama3.1:8b                         demo-node    ollama          100.64.0.61:11434
qwen3.8:27b                         demo-node    ollama          100.64.0.61:11434
2 model(s) across 1 reachable node(s).

$ citadel mesh chat --model llama3.1:8b "Explain sovereign compute in one sentence."
Sovereign compute means running your AI workloads on infrastructure you own and
control, so your data and models never leave your trust boundary.
```

> If the demo node can't cheaply serve a model, point Act 2 at the pre-existing
> serving node instead — `citadel mesh models` shows it the same way, and the
> "on hardware you own, over your own mesh" story is unchanged.

---

## Act 3 — A signed receipt for every answer (provenance you can verify)

**Story:** "Every answer that node produces comes with a receipt — signed by the
node's own identity key — that says what was asked, what was answered, and whether
the claims were grounded. Anyone can verify it, offline, without trusting us."

Because the demo node is running `citadel work` with
`CITADEL_GROUNDING_GUARDRAIL=1 CITADEL_SIGN_AEP_RECEIPTS=1`, its inference jobs
attach a signed **AEP receipt** to their output. The impressive, deterministic
part is the **verify** — so generate the receipt during rehearsal and verify it
live:

**Rehearsal step (produces `receipt.json`):** dispatch an `llm_inference` job to
the demo node and capture `output.aep_receipt` to a file. (Do this on the demo
node — never flip the signing env vars on a production worker.)

**Live:**
```bash
citadel aep verify receipt.json
```
The verifier recomputes the exact bytes the node signed, resolves the node's public
key (from the local identity, or a supplied `--cert`/`--pubkey` for off-node
verification), and checks the ECDSA signature. It reports the signing node, the
grounding score, the trust action, and the verdict hash — and distinguishes "signed
by a different node" from "signature is invalid."

Show the tamper story too: change one character of the receipt and re-run — it
fails. That's the whole point.

**Captured fallback:**
```
$ citadel aep verify receipt.json
✓ signature valid
  signed by node   sha256:9f2c…a1
  verifying key    node identity
  receipt version  2
  engine/model     ollama / llama3.1:8b
  grounded         true (score 1.000000, 0 claims checked)
  action           pass
  verdict_hash     sha256:4b91…e7

$ citadel aep verify receipt.tampered.json
✗ signature is invalid (does not verify against the signing key)
```

> Verification is offline and local today — the backend's public-key registry that
> lets *anyone* verify a fleet node's receipt is the next integration step; the
> math and the CLI are already shipped.

---

## Act 4 — Your traffic, your exit (sovereign egress)

**Story:** "I can also route this machine's internet traffic *out through* a node I
own on a completely different ISP. My apparent public IP becomes the node's, not
mine — an exit I control."

This uses the two boxes on **different uplinks**. Turn the demo node into a pure
egress relay (no worker, no Redis needed):
```bash
# On the exit node (the one whose ISP you want to appear from):
citadel egress-relay serve        # relay-only mode — mesh-only, same-org, deny-LAN by default
```
On the client (on the other uplink), tunnel through it and prove the IP changed:
```bash
# Before: the client's own public IP
curl -s https://api.ipify.org ; echo

# Route a local SOCKS5 port through the exit node's mesh address:
citadel proxy 1055 <exit-node-network-ip>:7861 --bind 127.0.0.1 &

# After: the SAME request now exits from the relay's ISP
curl -s --socks5-hostname 127.0.0.1:1055 https://api.ipify.org ; echo
```
The apparent egress IP moves to the relay's. The relay only accepts **same-org
verified peers** over the mesh, and denies LAN/mesh destinations by default — it's
a sovereign exit, not an open proxy.

**Captured fallback (this is the real result from validation on a dual-uplink host):**
```
# client direct (its own ISP)
142.181.124.149
# client through the relay (the exit node's ISP)
24.141.120.77
```

---

## The one-line pitch to close on

> "A machine you own, on your fabric in minutes, serving AI over your own private
> network, signing every answer with its own key, and acting as your own internet
> exit — no hyperscaler anywhere in that sentence."

---

## Appendix A — Rehearsal checklist

- [ ] Fresh, non-ephemeral reusable auth key minted.
- [ ] Both throwaway boxes on **different** uplinks; DNS reachable on each.
- [ ] `citadel` binary is **v2.157.0 or newer** on every box (needs `egress-relay
      serve` and `aep verify`). Disable auto-update on demo boxes only if you're
      pinning an exact build.
- [ ] Act 1 device-auth code entry rehearsed on the projector account.
- [ ] Act 2: `citadel mesh models` shows the intended model from the intended node.
- [ ] Act 3: `receipt.json` (valid) and `receipt.tampered.json` generated and both
      `citadel aep verify` outputs captured.
- [ ] Act 4: relay up on the exit node, `citadel proxy` tunnel verified, both IPs
      captured.
- [ ] All four captured fallbacks pasted into a scratch doc, open in a side window.

## Appendix B — The dual-uplink test scaffolding (validated 2026-09-11)

Act 4 was validated end to end on a Proxmox host with two independent uplinks
(Bell Fiber `142.181.124.149` and Cogeco Cable `24.141.120.77`) using two Ubuntu
LXCs. Full setup notes (VMIDs, per-uplink bridges, the DNS gotcha, the
non-ephemeral-key and `systemd-run`-persistence lessons) live in the maintenance
memory for this work. Relay port is `services.EgressRelayPort` (7861). Before
`citadel egress-relay serve` shipped (#1033), the relay had to run inside
`citadel work`, which required a reachable Redis — `serve` removes that.

## Appendix C — What's shipped vs. next

- **Shipped (v2.157.0):** `citadel aep verify`, the egress relay + relay-only
  `serve` mode, mesh discovery + node-to-node chat, node lifecycle via device auth.
- **Next (backend integrations, not blocking this demo):** the platform-side
  public-key registry so receipts verify against a fleet node from anywhere; the
  backend surfacing `aep_receipt` on standard inference responses.
