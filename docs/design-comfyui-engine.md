# Headless ComfyUI engine and approved workflows (#1375)

Status: **proposal, design only**. Parent #1373; source base
`dc139d1de01239fc4d432990bef415d59f6b4e33`. No engine, capability, workflow,
permission, licence policy or artifact schema is registered here. Owner review
and accepted child definitions of ready precede any build. All identifiers and
defaults proposed below are unratified unless explicitly identified as existing.

## Primary-source evidence, observed 2026-10-22 UTC

These are observed source revisions, not approved build pins, verified weight
digests, a dependency audit or proof that the components work together:

| Component | Observed source identity | Evidence / limit |
| --- | --- | --- |
| ComfyUI | `Comfy-Org/ComfyUI@46aad33e4d4c0696582611bf450dd93742fa9090` | [repository](https://github.com/Comfy-Org/ComfyUI/tree/46aad33e4d4c0696582611bf450dd93742fa9090), [GPL version 3 text](https://github.com/Comfy-Org/ComfyUI/blob/46aad33e4d4c0696582611bf450dd93742fa9090/LICENSE); pyproject version 0.42.0 is not a substitute for the commit |
| Publisher H3 source | `MiniMax-AI/MiniMax-H3@d21241f0a4b3acbb34c97dae47fa417b7065e438` | [publisher repository](https://github.com/MiniMax-AI/MiniMax-H3/tree/d21241f0a4b3acbb34c97dae47fa417b7065e438); not the ComfyUI graph or a weight digest |
| Publisher H3 model metadata | `MiniMaxAI/MiniMax-H3@42ed227ee7df40d41602854ae760620d6eb651fe` | [pinned licence](https://huggingface.co/MiniMaxAI/MiniMax-H3/blob/42ed227ee7df40d41602854ae760620d6eb651fe/LICENSE); API card uses `license=other`, `license_name=minimax-h3-community-license-agreement` |
| RefMod custom nodes | `Luisacaotica/ComfyUI-MiniMaxH3Mod@517e8c419a8584fbf69fceb487bc39d83e6a6ec8` | [MIT licence](https://github.com/Luisacaotica/ComfyUI-MiniMaxH3Mod/blob/517e8c419a8584fbf69fceb487bc39d83e6a6ec8/LICENSE), [README](https://github.com/Luisacaotica/ComfyUI-MiniMaxH3Mod/blob/517e8c419a8584fbf69fceb487bc39d83e6a6ec8/README.md); evolving API, not an accepted adapter contract |

Important premise correction: the pinned RefMod README uses native ComfyUI H3
support and says the sibling `ComfyUI-MiniMaxH3` pack is not required. Its
`encode` mode stores VAE references; `training` refines a compressed latent, not
H3 weights. Initial proposal: image-only `encode`, with compressed refinement,
audio, arbitrary folder loaders and legacy sidecar metadata excluded pending
separate review. No character/voice-fidelity guarantee follows from that choice.
Pinned [serialization source](https://github.com/Luisacaotica/ComfyUI-MiniMaxH3Mod/blob/517e8c419a8584fbf69fceb487bc39d83e6a6ec8/core.py)
uses safetensors and a metadata format version of 4; that upstream version is
not automatically our artifact schema. Compatibility must be tested explicitly.

The issue's checkpoint-size, model-size, reference-limit and 24 GiB feasibility
figures are **not ratified here**. Exact Ref2VA checkpoint, encoder, VAE,
quantization producer, component hashes and admissible dependency versions remain
unselected. No weights, image layers or dependencies were downloaded or run.

## Packaging and network boundary

Propose a dedicated Linux GPU image containing an independently reviewed ComfyUI
revision, the allowlisted RefMod revision, a small control adapter and immutable
API-format workflow assets. Pin the base image, Python/runtime wheels and every
custom node by reviewed immutable identity; record patches and a build manifest.
Reproducible source inputs do not imply bit-reproducible GPU output.

No ComfyUI-Manager package, enable flag, startup installer, `pip`, `git pull`,
runtime custom-node discovery directory or writeable code/model installation
path is exposed to jobs. Audit custom-node import/startup code as arbitrary
Python. Build dependencies into the image; runtime network policy denies egress,
with no platform/device/HF credentials inherited by ComfyUI. Model artifacts are
separately verified, read-only mounts provisioned only under later authority.

The pinned [CLI source](https://github.com/Comfy-Org/ComfyUI/blob/46aad33e4d4c0696582611bf450dd93742fa9090/comfy/cli_args.py)
supports disabling custom nodes with an explicit folder allowlist.
`--disable-manager-ui` leaves background installs active; it is insufficient.
`--offline` limits the frontend and partner nodes, not arbitrary Python egress.
Enforce the image and network boundaries rather than treating flags as a sandbox.

Proposed topology:

```text
enrolled Citadel worker -> authenticated node-local adapter
                              -> ComfyUI 127.0.0.1 inside its container
```

Only the adapter may have a host-loopback-published endpoint; raw ComfyUI ports,
UI, upload, history, extension and management routes are never published. A
container's internal loopback is not reached by a host port mapping: the adapter
and ComfyUI share the private container namespace, while its external adapter
listener is constrained by host-loopback publication and per-incarnation local
authentication. Exact port/socket, key custody and incarnation handshake need
review. Host loopback alone is not org authorization. No raw proxy to arbitrary
ComfyUI routes, job-supplied endpoint or shared-pool fallback is allowed.

Separate-container packaging is an execution boundary, **not** a conclusion that
aggregation satisfies GPL or any other distribution obligations. Corresponding
source, notices and distribution review remain prerequisites of later packaging.

## Approved workflow contract: reuse the template trust model, not its wire hash

Propose `COMFY_WORKFLOW` as a dedicated long-render job with two initially
reviewable operations: build an image-reference artifact, and render Ref2VA from
an authorized artifact. Names and schemas remain proposals, not dispatchable IDs.
The platform sends an approved workflow ID/version/hash, typed bounded parameters
and authenticated input/artifact descriptors; it never sends executable workflow
JSON, Python, custom-node names, URLs, model paths or arbitrary output prefixes.

Reuse the closed-schema, local-registry and explicit-builtin ideas from
[RUN_JOB_TEMPLATE](run-job-template-contract.md). Do not reinterpret that job's
approved runner or change its hash to smuggle a ComfyUI graph into it: its current
manifest does not bind graph bytes, custom nodes, checkpoints or service custody.
A future compiled-in builtin could share the same adapter if independently
approved, but it is not necessary to broaden today's template wire contract.

The proposed approval manifest must bind graph digest, adapter protocol/schema,
required class types, exact component identities and legal parameter-to-node
slots. Only the node registry selects and constructs the API graph. Reject
unknown keys/versions, graph/class drift, unbounded sizes, incompatible artifacts
and unsupported operation/mode combinations before execution. Seed, dimensions,
frame/FPS limits, reference/token budgets, duration and timeouts need explicit
reviewed numerical caps; no upstream UI default becomes runtime policy here.

## Anchored inputs and publication: current acceptance is input-only

Existing #1314 input fixture pin remains exactly
`e386064cb2c371cadc8bdc4eb6475adf78510a14fa9ae9aac450bcd830c185e0`.
[Media input v1](media-input-contract.md) permits its existing single-source/edit
and source/mask shapes, not eight references, video or a RefMod bundle. Reuse its
strict descriptor/privacy principles, but do not relabel an unaccepted extension
or draft fixture as ratified. Multi-reference cardinality, ordering, image
decoding/header/EXIF policy and combined byte/token limits require a new reviewed
contract. Video inputs and upload/output ratification remain separately gated.

Acquire Files authority for the executing org/node/workspace; validate each
descriptor before descriptor-relative no-follow opening or authenticated fixed
route fetch. Freeze actual image bytes into an owned attempt snapshot, hash and
bind source identities, and map only those snapshots to fixed container paths.
Do not give graph loaders arbitrary workspace traversal or custom model-path
registries. Parent ownership/link policy remains a prerequisite, not assumed
solved by a successful parse or lexical path check.

Outputs are staged in an attempt-unique owned directory. After proven completion,
validate regular-file/link/type bounds, actual size/hash and declared formats
through anchored descriptors, recheck publication authority, then create-only
publish the artifact/video and bounded manifest. Never overwrite another job or
use a graph-returned absolute filename as authority. No input pixels, latent
tensors or output video enter Redis, progress events, error text or receipts.
Workspace retention and authenticated export/upload require their own accepted
contract; neither a path result nor existing FILE_FETCH grants output-upload
permission. Failed/cancelled work exposes no successful publication manifest.

## Persistent character-reference artifact

A RefMod can retain reconstructable appearance and identifying metadata; treat
it as private media, not harmless cache or an identity-authentication primitive.
The proposed immutable asset record binds opaque asset ID and version to org,
node, workspace, producer attempt/workflow, ordered source digests, exact H3/VAE
and custom-node identities, tensor schema/dtypes/shapes, safetensors digest/size,
and reviewed metadata schema. Names/prompts are optional bounded private data,
never filesystem authority. The platform needs a matching Character Reference
Set identity/authorization contract; a node-local filename is insufficient.

Re-use requires current explicit read/execute authority for that asset's scope,
exact supported version/component match and reverified immutable bytes. No
cross-org/node lookup by guessed digest, replacement-credential inheritance,
unreviewed pickle loader, legacy JSON-sidecar fallback, external-drive discovery
or automatic migration/re-encode is allowed. Review tensor/allocation budgets
before loading; safetensors is not an authorization or resource-safety proof.
Revocation stops new use; retention, deletion, sharing/export and compatibility
migration remain owner decisions. Hash equality alone is not a permission grant.

## Progress, cancellation, custody and finished results

[Upstream API messages](https://docs.comfy.org/development/comfyui-server/comms_messages)
include execution/progress/error notifications; [routes](https://docs.comfy.org/development/comfyui-server/comms_routes)
include prompt/queue/history and interruption. They are observations, not proof
of durable Citadel completion or GPU release. Correlate every accepted event to
the submitted prompt plus adapter/service incarnation and Citadel attempt.
Publish only bounded stage/node counters; do not forward previews, graph
payloads, raw exceptions, paths or unknown custom event bodies. Cached/skipped
nodes cannot imply a fabricated end-to-end percentage or successful artifact.

One owned execution at a time per adapter incarnation is the conservative first
proposal. Cancellation addresses the exact submitted prompt; never a global
interrupt that could cancel unrelated work. A cancellation response, disconnected
socket, watchdog expiry or empty queue is not actual model-process exit. Use a
reviewed isolated child/process-group boundary with terminate/join escalation;
an unknown exit retains custody and blocks replacement work/writers. The exact
ComfyUI process/prompt isolation design is unresolved; HTTP-only cancellation is
not an acceptable substitute. Review soft deadlines and hard escalation without
inventing a timeout that silently releases the GPU lane.

Respect P0 #1222/#1227: persist the bound completion/artifact receipt through the
authenticated control path and do not rerun work merely because a callback or
ACK was lost. Only the same currently valid key/device with its original delivery
token and exact job identity may recover the old read-only receipt; replacement
credentials inherit nothing. Native completion fencing, secure restart state,
cancel/redrive and paired backend recovery must be accepted before activation.
Direct-Redis durable mode refuses without enrolled authenticated API/WS recovery.
No claim that this document implements or activates that P0 protocol.

## GPU coexistence and health

Use the existing heavy-job accounting as an integration point, not proof of
exclusive service custody. #832 reservations and an inference slot alone do not
drain a resident diffusers/Ollama service. #1340's per-service admission closure,
active-request drain, pin policy, incarnation/actual-exit custody and restoration
remain **unaccepted** design dependencies. Do not auto-evict from idle utilization
or kill in-flight/pinned work. Before that protocol is accepted, any native test
requires a separately approved exclusive window; it is not a production fallback.

CPU offload, quantization, attention backend and shared host-RAM pressure must be
measured against exact artifacts. No 24 GiB fit, speed or coexistence guarantee is
inferred from issue research or upstream examples. Health distinguishes adapter
reachable, exact registry/components verified, model unloaded/loading/ready,
active attempt and custody unknown. Read-only health must not load/download a
model, install nodes or mark a capability ready just because HTTP responds.

## Surface-only licensing; no new policy enforcement

Follow owner decisions in #1368/#1377 and
[the licensing design](design-engine-licensing.md): label components, unknowns and
conditions; do not implement territorial/revenue blocking, marketplace exclusion
or org `block`. The licensing P0 build itself remains held. Record code, actual
weights and auxiliary components separately, with a proposed
`LicenseRef-MiniMax-H3-Community-2026-08-02` mapping needing catalogue review.
Heartbeat/catalog and an **unsigned** licence receipt sibling are proposed
surfaces; do not change AEP v2 canonical bytes or its signatures.

The pinned H3 agreement dated August 2, 2026 names excluded territories (EU, UK,
Republic of Korea, US), a separate-authorization condition above USD20M yearly
commercial revenue, commercial-UI attribution, restrictions on improving other
models with H3/output, downstream terms and safeguards. These are non-exhaustive
metadata labels, not legal advice or a legal-compliance warranty. Its encoder
note points separately to Qwen3-VL's Apache2 terms; exact served component pins
still need review. RefMod MIT does not relicense H3, reference images or outputs.
Node-owner acceptance never satisfies a consuming org's policy. A separate
container and surface-only policy do not waive third-party obligations; owner
and qualified legal review must resolve actual distribution/use questions.

## Owner review / definition-of-ready gates and proposed children

Route questions through automated-citadel/automated-aceteam, not automatic build
dispatch. Proposed children below exist **in this document only**:

1. Packaging/provenance: select an audited compatible source set, immutable base,
   dependency/model hashes, licence labels/notices and no-installer tests.
2. Approved-workflow contract: ratify operation IDs, typed limits, graph/component
   binding and platform registry authority; choose dedicated job vs future builtin.
3. Media/asset contract: approve multi-reference input, confidential RefMod schema,
   artifact authorization/versioning/retention and separate video publication.
4. Lifecycle/custody: define authenticated adapter identity, exact-prompt control,
   actual-exit isolation, GPU per-service drain and P0 durable receipt integration.
5. Hermetic implementation tests, only after accepted contracts: fake API drift,
   unknown nodes/params, private diagnostics, unsafe I/O/tensors, auth isolation,
   progress reordering, cancel/unknown exit, duplicate delivery and restart recovery.
6. Separately authorized native acceptance: on a specified 24 GiB card, create a
   reference from **eight images**, then render one H3 **Ref2VA clip**. Record
   exact pins, consented inputs, workflow/seed/shape/duration, measured peak VRAM
   and host RAM, actual offload settings, wall time, artifact hashes/sizes,
   quality review, cancellation/cleanup and restored original workloads.

The eight-reference build and clip, all measurements and compatibility claims
are **NOT RUN / held**. No child issues, model acquisition, image build/run,
runtime source, tests/formatter/build execution, GPU/inference/demo, service or
live configuration/key change, SQL/deploy, undraft or merge is authorized by
this proposal. Normal existing GitHub CI for this documentation draft is allowed.
