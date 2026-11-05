# Sovereign agent stack: PROPOSED connection and runtime design

Status: **PROPOSED; independent design review required. Not an accepted Design
of Record or a build approval.** This is the design lane of [#1214](https://github.com/aceteam-ai/citadel-cli/issues/1214),
observed 2026-10-22. Its separate Jason BUILD gate remains closed. No runtime,
provider configuration, credential, module, image, workflow or service is
created or activated by this document. No build children have been filed.

Citadel source baseline: `de2f1818a8b574bc0cfa362c9d10158743f03c3f`.
Upstream observations below are dated 2026-10-22, not accepted artifact pins.
All end-to-end, headless, tool-use, sandbox, cancellation, restart, performance
and native acceptance in this proposal are **NOT RUN**.

## 1. Outcome and boundaries

The intended outcome is an operator-approved coder, inference endpoint and,
optionally, browser and memory on the operator's fabric. Three different
contracts must not be conflated:

| Slice | Caller and operation | What this proposal supplies | What remains held |
| --- | --- | --- | --- |
| S5: local coder as client | Operator's coder requests inference | A connection/compatibility record and acceptance plan | Exact provider version, endpoint authorization and real interoperation |
| S0: hosted OpenCode | Platform delivers one turn to a node runtime | Proposed reuse of the hosted-turn boundary, with isolation and custody requirements | Build approval, wrapper/runtime artifacts, sandbox/admission and paired acceptance |
| S6: hosted coder uses fabric model | Runtime requests inference from an approved fabric endpoint | Separate inference authority and dialect contract | Exact endpoint/credential design and model/tool compatibility |
| S1/S2: Qwen Code / pi | Additional hosted coders | Reuse criteria, not assumed interchangeable implementations | Independent adapter/version/permission review and build approval |
| S3/S4: browser / memory | Approved agent accesses a companion | Ownership and data-boundary proposals | Product choice, per-agent isolation, storage/egress and service designs |

Harness2's experiment executor, broker and evaluation lifecycle belong to
[design draft #1415](https://github.com/aceteam-ai/citadel-cli/pull/1415), not a
second implementation here. Its proposed OpenCode reuse does not establish a
generic hosted-runtime, inference or companion-service contract. Declared
interactive sessions [#1165](https://github.com/aceteam-ai/citadel-cli/issues/1165)
and the node-session viewer [#1118](https://github.com/aceteam-ai/citadel-cli/issues/1118)
are also distinct: neither turns the one-shot hosted callback into durable
process supervision.

## 2. Verified current Citadel facts, not deployment claims

All source links in this section are pinned to the baseline above.

- [Gateway route registration](https://github.com/aceteam-ai/citadel-cli/blob/de2f1818a8b574bc0cfa362c9d10158743f03c3f/internal/gateway/chat_route.go#L172)
  implements `/v1/chat/completions`, `/v1/completions` and `/v1/models` when
  [Start has a chat lister](https://github.com/aceteam-ai/citadel-cli/blob/de2f1818a8b574bc0cfa362c9d10158743f03c3f/internal/gateway/gateway.go#L578).
  Work and serve wire that lister. This registers paths; it does not prove
  every node runs the gateway, every provider dialect works, or any deployed
  listener/credential policy is appropriate for a new caller.
- [Chat proxying](https://github.com/aceteam-ai/citadel-cli/blob/de2f1818a8b574bc0cfa362c9d10158743f03c3f/internal/gateway/chat_route.go#L249)
  preserves the requested chat/completions path to the selected engine. This
  is not a Responses-to-chat or Anthropic-to-chat translator. `/v1/responses`
  is not registered by this chat router. The provider-specific requirements
  of another facade must be verified separately, not inferred from a URL.
- [The mesh client](https://github.com/aceteam-ai/citadel-cli/blob/de2f1818a8b574bc0cfa362c9d10158743f03c3f/internal/mesh/client.go#L89)
  still sends `ChatCompletionTo` to the discovered model's IP and engine port.
  [CLI help and refusal advice](https://github.com/aceteam-ai/citadel-cli/blob/de2f1818a8b574bc0cfa362c9d10158743f03c3f/cmd/mesh.go#L90)
  incorrectly say the node gateway does not yet proxy chat. Correcting that
  wording would not by itself route this client through a verified gateway.
  This document does not change either the client or generated manual.
- [Gateway middleware](https://github.com/aceteam-ai/citadel-cli/blob/de2f1818a8b574bc0cfa362c9d10158743f03c3f/internal/gateway/gateway.go#L855)
  includes permissions, optional metering and route-specific exposure checks.
  The exposure identity policy is not a blanket inference-client authorizer;
  [metering extracts a consumer key](https://github.com/aceteam-ai/citadel-cli/blob/de2f1818a8b574bc0cfa362c9d10158743f03c3f/internal/gateway/metering.go#L67)
  for accounting, which must not be described as credential verification.
  A proposed inference credential in section 4 is **not** asserted to be an
  existing route grant. Reachability, TLS, mesh identity and application
  authorization must each be inspected at the chosen boundary.
- [Hermes's checked-in provider notes](https://github.com/aceteam-ai/citadel-cli/blob/de2f1818a8b574bc0cfa362c9d10158743f03c3f/services/hermes-service/README.md#L83)
  record `openai-api` requests to `/v1/responses`, with end-to-end fabric
  wiring unconfirmed. These are historical repository observations, not a
  new execution receipt. Setting `OPENAI_BASE_URL` does not prove compatibility
  with this node chat router.
- [The existing hosted wrapper](https://github.com/aceteam-ai/citadel-cli/blob/de2f1818a8b574bc0cfa362c9d10158743f03c3f/services/claudecode-service/wrapper.py#L9)
  separates an authenticated `/hooks/agent` delivery from a later instance
  `/reply`, preserves canonical `turnToken`, and scrubs configured credential
  values. Its background-thread delivery ACK is not a persisted result or
  restart-recovery receipt. Repeating that shape does not solve
  [#1222](https://github.com/aceteam-ai/citadel-cli/issues/1222).

## 3. Primary upstream observations and provenance limits

These observations motivate evaluation; none selects an installable release,
image, dependency closure or weight artifact. Repository LICENSE identifiers
do not cover every dependency, model, browser binary, extension or data source.
There is no blanket “all MIT” or commercial/distribution clearance.

| Candidate | Primary observation | Consequence for the proposed adapter |
| --- | --- | --- |
| OpenCode | [CLI docs](https://opencode.ai/docs/cli/) describe noninteractive `run`; [v1 provider docs](https://opencode.ai/docs/providers/) distinguish chat-compatible and Responses provider packages. [v2 docs](https://opencode.ai/v2/docs/providers) use different provider/package/settings fields. | Freeze one exact release and its own schema/dialect. Do not combine v1 examples with v2 fields or equate headless support with tested tool execution. |
| Qwen Code | [Upstream README](https://github.com/QwenLM/qwen-code/blob/main/README.md#for-automation-and-custom-integrations) documents headless prompt operation. [LICENSE header at observed revision](https://github.com/QwenLM/qwen-code/blob/15f3226db53883904b55990439c67198457ad9a4/LICENSE) identifies Apache-2.0. | Exact provider configuration, noninteractive permission behavior and cancellation remain to be reviewed for an eventual selected version. |
| pi | The old repository redirects to [earendil-works/pi](https://github.com/earendil-works/pi); its [coding-agent README](https://github.com/earendil-works/pi/blob/main/packages/coding-agent/README.md) describes print/JSON, RPC and SDK interfaces. [Observed LICENSE](https://github.com/earendil-works/pi/blob/58c0b02ce552adab76014e7ed712ab7981359aa8/LICENSE) is MIT. | Choose one version/interface deliberately. The epic's “YOLO” characterization is not evidence of the selected version's effective permission or sandbox behavior. |

[OpenCode permissions](https://opencode.ai/docs/permissions/) distinguish allow,
ask and deny; auto mode relaxes prompts, not explicit denies. That is an
application policy, not host isolation. Its observed [MIT LICENSE header](https://github.com/anomalyco/opencode/blob/c93aaae37157d785f76a8e9a9243a6560e286468/LICENSE)
does not establish artifact integrity or safe execution. The official v1/v2
documentation is moving; no configuration recipe is copied here.

Repository metadata observations were OpenCode `dev@c93aaae37157d785f76a8e9a9243a6560e286468`,
Qwen Code `main@15f3226db53883904b55990439c67198457ad9a4`, and pi
`main@58c0b02ce552adab76014e7ed712ab7981359aa8`. These identify observation
checkpoints, **not proposed release pins**. README/provider claims above come
from the linked documentation as observed on the stated date; immutable
runtime source/config correspondence still needs evaluation.

## 4. S5 proposed caller connection contract

First deliver a reviewed connection record for one coder/version and one
explicitly approved model endpoint. Do not publish a copy-and-run onboarding
recipe until that record has independently passed its own acceptance.

The record must identify:

1. **Caller:** local operator-owned process or hosted instance, its node/org
   relationship and the operation it is permitted to request. A hostname,
   model display name or successful model-list request is not authority.
2. **Destination:** exact origin, scheme, port, fixed API prefix and verified
   node/service identity. Distinguish a host loopback engine, a TLS gateway
   and a platform facade. Container loopback is not host loopback. Resolve
   transport through the existing supported path; no raw engine-port fallback,
   plaintext downgrade, TLS bypass or arbitrary URL supplied in a turn.
3. **Inference authorization:** identify the existing verified mechanism, or
   stop for a separately reviewed mechanism. The proposed least-privilege
   credential is limited to the selected inference operation/destination and
   has explicit expiry/revocation/reader ownership. That restriction is a
   **design requirement, not a currently implemented key type or grant**.
   Do not use a Headscale admin key, device enrollment token, wrapper callback
   key or browser/memory credential as an inference key. A provider requiring
   a nonempty string does not authorize a dummy bearer or prove enforcement.
4. **Protocol and model:** exact provider package/schema, wire dialect, model
   identifier and tested tool-call/stream/error behavior. Engine discovery is
   inventory, not proof of gateway reachability, authorization or compatibility.
   Model/tool/context limits must come from the accepted model/engine, not
   optimistic provider catalog defaults. No silent alternate model/provider.
5. **Execution and data limits:** finite request/output/context/queue/deadline
   limits, tool/egress policy and diagnostic retention. Exact values need a
   scoped review before build. Credentials stay in operator-controlled secret
   storage; do not print them, put them in URLs/argv, commit generated config,
   or forward them to an unapproved proxy or provider.

For local same-node inference, propose the existing approved local serving
endpoint only when its actual bind/access policy is suitable for the caller.
For remote fabric inference, propose an identity-verified gateway contract,
not “same tailnet means every peer may infer.” LAN access is a separate
exposure decision. No listener or permission changes are authorized here.

Failure distinctions must remain truthful: absent listener, TLS/identity
failure, unauthorized principal, unsupported dialect, unknown model, warming,
capacity refusal and cancelled request are not interchangeable “offline.” A
retry must preserve caller/destination/model authority and a finite budget;
uncertain execution must not silently fail over or redispatch tool actions.

## 5. S0/S6 proposed hosted-runtime boundary

Preserve the existing platform-to-wrapper-to-reply seam rather than inventing
a second job type or accepting a caller-selected callback URL. A future design
must explicitly bind the admitted instance, canonical turn token and reply
identity. Malformed or stale authority refuses before launching a harness;
success/error callbacks preserve their originating identity and scrub secrets
before serialization, retry retention or logs. Upstream authentication,
inference credentials and platform callback authority remain separate.

Propose one admitted active turn per isolated runtime initially. “Delivered”
means only accepted delivery under the reviewed contract, not completed work.
Define saturation, duplicate delivery, bounded queue/reply retry, shutdown and
crash behavior jointly with the platform before implementation. Existing
daemon threads and synchronous dispatch are not durable custody. Do not
invent receipt storage, replay authority or P0 recovery on this branch;
the frozen durability design remains held.

Host execution is not a generic shell service. A future adapter must use a
fixed executable/argument structure in a disposable, explicitly owned workspace
with reviewed CPU/memory/PID/disk/time bounds. No raw host Docker socket,
privileged container, root home, SSH-agent mount or arbitrary host directory.
Only approved tool capabilities and exact model/browser/memory egress are
available. No automatic plugin/package installation, arbitrary MCP endpoints,
cloud fallback or ambient credential inheritance. The isolation mechanism and
host kernel/container threat model require independent acceptance; a permission
prompt, nonroot UID or container label alone is insufficient.

Cancellation must stop admitting tools and inference, propagate to owned work,
and retain its admission/resources until actual owned execution exit and output
drain are established. A timer, callback failure or cancelled UI state is not
exit proof. Freeze process-tree/escaped-child handling, cleanup ownership and
unsupported-platform refusal before a build. Do not kill unrelated host work,
claim process-group containment as identity-pinned isolation, or reuse a slot
while native work may survive. Artifact collection and terminal publication
must use the same admitted workspace/turn authority.

S6 then adds the accepted S5 inference connection inside that runtime. It does
not convert the wrapper gateway key into model authority, install a second
inference proxy, or assume the existing Hermes Responses path works. Preserve
the operator's selected model, existing billing/accounting boundary and
revocation behavior; no automatic serving start, model pull, GPU reservation
or active-job preemption is implied by choosing the runtime.

## 6. Companion boundaries: proposed, not adopted

### S3 browser

PinchTab's [security guide at observed source](https://github.com/pinchtab/pinchtab/blob/a62662027f233444c579c29fbe42ad90df0d5bc7/docs/guides/security.md)
describes a trusted local-first control plane, token authentication and disabled
high-risk capabilities. It explicitly does not treat agent sessions as a
hostile multi-tenant sandbox. Its [LICENSE](https://github.com/pinchtab/pinchtab/blob/a62662027f233444c579c29fbe42ad90df0d5bc7/LICENSE)
identifies MIT; browser binary/dependency licensing is separate.

Propose comparing PinchTab with a bounded snapshot/action extension of Citadel's
existing CDP transport before adopting either. Require a dedicated disposable
profile and per-agent isolation; no personal logged-in profile or publicly
reachable dashboard/admin token. Mesh transport alone is insufficient.
Page text is untrusted input: actions resolve from observed targets and recheck
freshness, rather than executing model-supplied selectors, JavaScript or shell.
Navigation/egress, cookies, file upload/download and browser lifetime need
explicit authority. Record which process owns Chrome and who can stop it;
stopping an adapter must not imply ownership of an externally attached browser.
No browser service, port, credential or companion choice is approved here.

### S4 memory

The [upstream README](https://github.com/TencentCloud/TencentDB-Agent-Memory/blob/feat/server_team/README.md)
describes Memory Core, Hub and Proxy plus separate LLM parameter groups; a
base-URL proxy participates in conversation processing. The [LICENSE header](https://github.com/TencentCloud/TencentDB-Agent-Memory/blob/6a4bb55ea54834d8265ce52024d3e3c75db30d38/LICENSE)
says MIT, while observed GitHub license metadata reports `NOASSERTION`. That
detector result is not proof of a different license or distribution clearance.
Observed source was `feat/server_team@6a4bb55ea54834d8265ce52024d3e3c75db30d38`.

Propose no automatic interception, transcript import or dual-write with
AceTeam memory. Resolve ownership with [aceteam#7160](https://github.com/aceteam-ai/aceteam/issues/7160)
first. A node-local option needs explicit dataset/principal isolation, current
reader authorization, export/deletion/retention, backup/restore and encrypted
secret handling. Extraction/embedding inference must satisfy the same declared
egress policy as the coder; self-hosting the database does not prove model
requests stay local. Freeze the actual backend/migration/crash-consistency
contract, disk bounds and restore test before any adoption. Persistent storage
must not share the disposable runtime cleanup root. No existing Memory key,
platform record or credential is changed by this design.

## 7. Review decisions and later evidence

Automated-citadel should review the source/dialect/permission inventory before
Jason decides product authority. Proposed sequence, **not filed build children**:

1. Review S5 with one explicit OpenCode version/provider dialect; settle exact
   inference endpoint/principal/auth ownership with the gateway/platform owners.
   Decide whether stale mesh help is a separate bounded source bug; do not
   silently change mesh routing as documentation work.
2. Review S0 isolation, finite limits, actual-exit custody and duplicate/crash
   behavior. Identify exact release/artifact/license/dependency provenance and
   signed or registry-verified hashes through an independently authorized lane.
   No current image architecture/availability or turnkey sandbox is asserted.
3. Review S6 with that accepted connection, including tool-call, streaming,
   warming/capacity/error and revocation behavior. No Responses adapter is
   presumed necessary or approved; pick a proven dialect or separately design it.
4. Choose browser approach and memory ownership separately. Reuse the accepted
   adapter contract for Qwen Code/pi only after their own interface/security
   evaluation; do not mechanically copy environment or permission settings.
5. Only after independent design acceptance **and a separate Jason BUILD yes**
   create bounded implementation children. Design acceptance alone does not
   authorize implementation, mark children automated or activate services.

Future source fixtures must exercise actual registered adapter boundaries:
wrong instance/token/callback identity; noncanonical token; full queues and
duplicate deliveries; malformed/bounded outputs and credential leakage across
stdout/stderr/JSON/retry/logs; exact endpoint/dialect/model; denied tools/egress;
cancel/timeout/exit races and post-cancel callbacks. Preserve existing Claude
Code/Hermes contracts and test the composed route rather than a disconnected
helper. Deterministic synthetic tests are not live model/tool compatibility.

Separately authorized acceptance must record exact node/runtime/provider/model
identities and versions; complete dependency/image provenance; cold/warm
single-turn and real tool/stream behavior; measured resource use; denied
cross-principal access; cancellation with actual exit; browser isolation;
memory restore/deletion/egress; and crash/reconnect disposition. Never claim
restart-safe completion without the independently accepted durability consumer
contract. Catalog publication, native runs, keys, service changes, releases
and deployment each remain separately gated. No such acceptance was run for
this proposal, and #1214 remains OPEN/WIP.
