# Speakrail local full-duplex voice: source-first proposal

Status: **UNACCEPTED DESIGN, PARTIAL DELIVERABLE** for #1376, compared with
#1216 and epic #1214. Source/metadata inspected 2026-10-22; Citadel baseline
`dc139d1de01239fc4d432990bef415d59f6b4e33`. No runtime or catalog code changes.
The required 24 GB measurements and replacement-TTS listening study are
**NOT RUN / HELD**, so this is not a completed feasibility result or permission
to build. No implementation child issues are filed by this proposal.

## 1. Recommendation and evidence limits

Propose **Speakrail plus a separately validated Kokoro adapter as the first
full-duplex integration candidate**, not the production default yet. Its text
decision/tool seam is a useful fit for AceTeam's explicit tool authority and
existing TTS infrastructure. Keep PersonaPlex as a comparison candidate, not a
second always-resident stack on the same card. Retain the existing discrete
STT/LLM/TTS path for current use; do not silently switch existing agents.

This recommendation is conditional on reviewed supply-chain pins, authenticated
transport, drain custody, actual 24 GB fit and comparable human/audio results.
If the Kokoro swap loses acceptable interruption/prosody/latency behavior, or
the stack does not fit, revisit the choice using PersonaPlex measurements.
The existence of an HTTP speech endpoint is not proof of streaming compatibility.

The [Speakrail README][readme] reports a single RTX 4090, conversational score
94.0 and approximately 0.7–0.8 s median word-end-to-ear latency; it explicitly
says other 24 GB cards were not tested by its author. Its current README budgets
about 75 GB total disk, not just the roughly 22 GB model download. These are
**upstream claims**, not Citadel 3090 measurements. The issue's FDB-v3 pass@1
0.52 and PersonaPlex comparison are not independently reproduced here; benchmark
versions, workloads and score definitions must match before comparing numbers.
No numerical latency/VRAM/quality advantage is established by this design.

## 2. Verified upstream shape and proposed provenance inventory

Evidence pins below identify inspected source/metadata, **not accepted executable
artifacts**. No images, weights or packages were pulled. Source license metadata
is not artifact verification, compatibility testing, legal due diligence or
permission to activate a service.

Speakrail source pin: [`49798e8e72c8a8429cb4d005840a6af389ececb7`][source].
This is a cascade with learned microturn decisions, not a unified audio-token
speech model: Voxtral/audio.cpp plus a turn head feeds Gemma plus adapter;
streaming TTS renders replies. The listening-notes model in
[`micro_cfg`][server] uses the same configured base-model service rather than
proving a separate extra model allocation. Optional paid frontier inference is
a different route and must not be represented as local inference.

| Component | Inspected provenance | Declared license / remaining inventory |
| --- | --- | --- |
| Speakrail harness, app, initializer | Source pin above; [LICENSE][code-license] and [NOTICE][notice] | Apache-2.0 source; does not relicense downloaded models or every image layer |
| ASR runtime | [`services/asr/Dockerfile`][asr-build] pins `speakrail/audio.cpp` at `ba2ae40d4630874121d2dfadddd10fe97c358b89` | Fork LICENSE declares Apache-2.0; bundled ggml/cJSON/SentencePiece and other dependencies need individual notices/SBOM |
| ASR weights | `audio-cpp/audio.cpp-gguf@e36610ac69b5262e914a52635324050bee8f1ad2`, selected `Voxtral-Mini-4B-Realtime-2602-GGUF/voxtral-mini-4b-realtime-2602-q8_0.gguf` | Aggregate repo metadata says `other`, not a blanket license. [Selected package card][gguf-card] maps original Voxtral to Apache-2.0; retain conversion and [original model][voxtral] provenance separately |
| Turn head | [`speakrail/Voxtral-Mini-4B-Realtime-2602-TurnHead@ba89de14c626a362d4c7bc35094b0972c534e9d7`][turn-card] | Card declares Apache-2.0; separate auxiliary weights, not part of unmodified Voxtral |
| Gemma base QAT | [`google/gemma-4-12B-it-qat-w4a16-ct@1d2c2d7f2466070e69d6fb3fd5ce9a7d75f2f6ee`][gemma-card] | Pinned card declares Apache-2.0; do not substitute older Gemma-family terms by name |
| Gemma adapter, token rows and tokenizer | [`speakrail/gemma-4-12B-it-qat-Speakrail@f1004b2763dafee266f108bb2f5167554b984c48`][adapter-card] | Card declares Apache-2.0. Initializer patches 22 token rows in the base model; record original, adapter and derived artifact identities, not just the base name |
| Default TTS runtime | [`services/tts/Dockerfile`][tts-build] pins `speakrail/breeze-tts@be25971d96612319f7b65c94f92e0a653fdae239` | Fork source LICENSE is Apache-2.0; Python/CUDA/Torch/runtime components remain separate |
| Default TTS weights/tokenizer/codec | [`BreezeBlue/Breeze-TTS-2@3e28c5151381a722f1d8661b4118c298caa77aa4`][breeze-card] | `other`, BreezeBlue Research and Non-Commercial License v1.1, dated 2026-09-01. [Exact terms][breeze-license] cover model materials/derivatives and commercial use of outputs; no commercial rights are supplied by the public license |
| Default reference voice and phrase cache | `app/voices/female_a.wav`, transcript, generated cached audio | Upstream NOTICE identifies a Breeze-generated synthetic reference, subject to those terms. Replacing TTS must also replace/rebuild references and cached Breeze audio; do not carry a supposedly permissive cache across the swap |
| vLLM and GPU/base libraries | Compose vLLM `v0.26.0`; CUDA/PyTorch/Python base tags in Dockerfiles | vLLM source Apache-2.0 is not the complete container license. Record per-layer/component terms and CUDA/NVIDIA redistribution conditions; no image digest/scan/provenance verified here |
| Optional search/fonts/data | SearXNG, browser font loading, Open-Meteo, external inference/search | NOTICE lists AGPL-3.0 SearXNG and CC-BY-4.0 weather data. Inventory font/assets and every optional external service; “local models” alone does not prove zero egress |

The [initializer][model-init] pins five HF revisions and includes checksums for
major payloads and a patched Gemma checksum. This is useful upstream provenance,
not our verified model manifest: `fetch` can skip an existing destination by
size alone, some small files have no digest, and the patched-model marker can
skip revalidation. A future reviewed importer must validate every selected
artifact and derived output against a complete manifest on reuse, stage
immutably and publish only verified content. Never run downloaded custom code
or accept arbitrary model/path overrides from a voice-session request.

### License surface policy, including replacement candidates

Follow `docs/design-engine-licensing.md` / #1368 owner decisions / held #1377:
**surface only, no new license enforcement**. Proposed labels preserve source,
weights, auxiliary codec/head/adapter, references and output terms separately;
unknown is displayed as unverified, not silently called commercially usable.
Keep the existing signed v2 receipt bytes unchanged; any new license sibling is
unsigned until the separately coordinated v3 contract. No schema/SQL change is
part of this document. Existing engine-specific behavior is not altered.

Candidate replacement evidence only:

- [Kokoro-82M][kokoro-card] metadata at
  `f3ff3571791e39611d31c381e3a41a3af07b4987` declares Apache-2.0 weights;
  upstream [code LICENSE][kokoro-license] declares Apache-2.0. Primary proposal
  for a permissively labelled TTS swap, using a fresh permitted reference/voice.
- [ResembleAI/chatterbox][chatterbox-card] metadata at
  `5bb1f6ee58e50c3b8d408bc82a6d3740c2db6e18` declares MIT. Alternative candidate
  when clone/voice needs justify it; include watermark/voice-consent and optional
  aligner terms. Do not accidentally include the fabric's separately labelled
  non-commercial aligner or OmniVoice and call the composition MIT.

Neither candidate's metadata proves that a particular fabric image/checkpoint,
voice asset, dependency composition or generated output is cleared for a use.
Jason's named license review and any required legal review remain gates. This
proposal chooses no automatic exclusion/block mode, and grants no commercial
deployment authorization for Breeze or either swap.

## 3. Packaging proposal, not a build

The [upstream Compose][compose] uses four `speakrail/*:0.1.0` defaults and a
vLLM tag; these are not digest pins. The image inventory is:

| Compose image default | Source/build boundary and license evidence gap |
| --- | --- |
| `docker.io/speakrail/model-init:0.1.0` | `services/model-init`: Python `3.12-slim`, HF downloader, initializer; image layers and model license notices need separate inventory |
| `docker.io/speakrail/voxtral-stt-server:0.1.0` | `services/asr`: pinned audio.cpp fork, CUDA 12.9.2 / Ubuntu 24.04 build/runtime tags and Python bridge; model and bundled library terms remain separate |
| `docker.io/speakrail/breeze-tts-server:0.1.0` | `services/tts`: pinned Breeze fork over PyTorch `2.9.1-cuda12.8-cudnn9-runtime`; source license does not clear weights/reference/output terms |
| `docker.io/speakrail/app:0.1.0` | `app`: Python `3.12-slim`, ranged Python requirements, bundled voice/UI assets; Apache code label is not a container-wide clearance |
| `docker.io/vllm/vllm-openai:v0.26.0` | Upstream vLLM image; source, CUDA dependencies and selected Gemma artifacts must each be inventoried |
| Optional `docker.io/searxng/searxng:2026.9.19-e831fc2a1` | Separate AGPL-3.0 search service; omitted from proposed initial local serving profile |

None has a verified image digest or complete license/SBOM inventory here.
Propose rebuilding reviewed app, ASR, TTS
adapter and initializer source into AceTeam GHCR with immutable per-platform
image digests, signed provenance, dependency lock/hash manifest, SBOM and review
of base layers. Fork commits are already specified upstream, but base-image tags,
apt repositories and version ranges still do not give a reproducible build.
Select Linux NVIDIA amd64 as a candidate acceptance target; SM86 in the ASR
build is not proof of 3090 execution. No Mac/Windows GPU promise is made.

A future `citadel-services` module would group app/ASR/TTS/LLM health and own
only its exact labelled containers, mounts and pinned network; no host Docker
socket, host shell, user-home mount or generic host IPC assumption. Reconsider
upstream `ipc: host` explicitly instead of copying it as an unexplained default.
The initializer gets only a designated staging store and separately provisioned
download authority; serving containers mount verified models read-only. Runtime
dependencies/models never install during a caller's session. This document
supplies **no GHCR digest**, catalog row, Dockerfile, port assignment or image
claim. Any new host loopback slot must go through `services/ports.go` and its
collision guard, not upstream host port 8080.

Disable raw upstream UI/debug routes and session audio/event persistence in the
packaged serving surface. Optional shell/Claude/frontier/search tools are not
exposed; absent keys are not the sole security boundary. Self-host any required
UI assets and audit default weather/font/network paths before calling a profile
fully local. Download, optional network tools and telemetry need explicit,
separately reviewed egress/retention policy, not broad runtime internet access.

## 4. Integration and authority proposal

Current baseline is not a ready Speakrail gateway:

- `internal/jobs/huddle_realtime_conn.go:dialRealtime` authenticates to the
  trusted enrolled AceTeam origin and waits for both authentication and media
  readiness. `huddle_join.go` binds org/channel/call/agent/node/job/attempt and
  owns durable lifecycle cleanup; retain those identities rather than minting a
  global secret or borrowing a human's identity.
- `huddle_converse.go` presently bridges 24 kHz PCM16 via OpenAI-style JSON
  events. [Speakrail's server][server] uses binary 16 kHz input and 24 kHz output
  with an utterance header plus different JSON controls. It is **not** a drop-in
  OpenAI realtime endpoint; sample-rate/endianness/framing and interruption state
  need an explicit tested adapter. Its WS handler has no general session auth,
  uses unlimited message size and an unbounded outgoing queue in inspected source.
- `internal/gateway/gateway.go:proxyWebSocket` is a byte tunnel, not bounded
  semantic audio admission or a realtime-session authorization service.
  Existing module capability/proxy permission does not establish org/agent/call
  authority or authenticate the unmodified Speakrail server.
- `transcribe_audio.go` is discrete faster-whisper transcription, and
  `synthesize_speech.go` is discrete synthesis; neither proves learned full-duplex
  turn-taking. Keep that useful existing cascade rather than claiming it already
  runs Speakrail or PersonaPlex.

Propose a separately reviewed AceTeam realtime provider plus a node-local
adapter. Browser WebRTC terminates at the already authorized media/session
boundary; the node leg is an authenticated WSS audio/control connection via the
gateway. Speakrail itself offers WS, not browser WebRTC/ICE or a phone provider.
Phone/SIP/PSTN and meeting/huddle adapters must identify the actual participant,
consent and call lifecycle; no new direct public node listener or anonymous
module URL. Those cross-repo protocol choices are not accepted by this draft.

Before media acceptance, bind short-lived least-privilege session authority to
org, node, agent, call/session, attempt, selected provider/voice/config revision
and expiry. Require live grants/membership and readiness, trusted destination
selection, Origin policy and rechecks/revocation during the session. Browser
handshakes need an approved one-use, audience-bound exchange, not a long-lived
token in a query/log. Do not repurpose the optional upstream `?ck=` host-shell
key as AceTeam auth or forward device/global credentials into the model stack.

The adapter must impose separately ratified finite frame/message/sample bounds,
bounded audio/control queues, admission/idle/session deadlines, backpressure,
heartbeat and slow-client close behavior. Do not conceal overflow by dropping
arbitrary input audio while claiming a continuous conversation. Treat malformed
PCM, invalid utterance IDs/control transitions and oversized JSON as refusals.
No transcript, audio, prompt, bearer or raw provider exception enters generic
logs; opt-in org-scoped retention and approved audit destination are separate.

### Tool calls are requests, not authority

The [upstream tool catalog/executor][tools] is not an AceTeam MCP bridge. A model
may propose a name plus typed arguments, but a trusted broker must resolve an
allowlisted schema version and evaluate the live caller's org/agent/call grant
for **each** operation. Never let model text choose MCP URLs, credentials,
workspace paths or shell commands. Remote results are untrusted model input.
Prompt injection and speech saying “yes” cannot mint permissions.

Require explicit trusted-UI approval where the existing tool policy requires
it; permit no side effects from speculative/unadopted microturns. Attach stable
session/turn/call IDs and idempotency keys to approved side effects; reconcile
uncertain outcomes instead of retrying a tool blindly after disconnect. Abort
uncommitted speculation on interruption; disconnect does not undo an already
committed external mutation. How to map the trained finite tool vocabulary to
AceTeam schemas without degrading decisions requires a reviewed adapter and
tests, not arbitrary substitution of tool names.

## 5. GPU residency, drain, cancellation and recovery

Propose one exclusive **selected-GPU residency reservation for the complete
session stack**, including startup/warmup, rather than one independent worker
slot per ASR/LLM/TTS request. This is a proposal, not proven necessary/sufficient
VRAM sizing: vLLM's `0.48` setting is only one process's allocation policy.
`internal/worker/gpu_tracker.go` tracks chat job slots, not resident voice stacks;
`internal/jobs/reservation.go` / `model_exclusivity.go` do not yet prove all-ingress
service draining. Warm model allocations can remain after a socket/job ends.

Depend on the unaccepted #1340 per-service drain mechanism, not global
`ActiveJobs==0` or utilization heuristics. Settled owner policy: pinned services
are never evicted; all non-pinned candidates including Ollama/vLLM/OmniVoice can
be evicted, but busy work must drain naturally, never be cancelled to reclaim
capacity. Close/fence new worker, gateway and direct-runtime admission, retain
active leases until actual exit, and acknowledge unsupported/unmanaged ingress
as unresolved coverage. Do not silently replace this with an idle-only policy.

Reservation owner/incarnation/epoch, target GPU, exact prior service state,
pin recheck and restart recovery must survive the resident session. Stop and
restore only owned exact containers/services; no broad compose teardown or
restoring a service that was originally stopped. A draining voice session may
remain busy until it naturally finishes: finite capacity and operator-requested
termination policy need owner agreement, not forced cancellation under a timeout.
After authorized session termination, prove all adapter tasks/provider calls
and CUDA allocations are gone (including warm caches), before restoring evicted
services or transferring GPU custody. Unknown exit/timeout keeps custody/debt
observable; it is not success or permission to overlap allocations.

Distinguish barge-in (stop obsolete utterance/playback, continue listening) from
session cancellation (end media, fence tools, join tasks and verify exit).
Closing a TTS HTTP stream alone is not proof that GPU inference stopped; the
[Breeze worker][tts-client] retries busy responses and assumes server release,
which needs native verification for each replacement. On reconnect, freshly
authorize the exact session attempt and use sequence/utterance fences; do not
replay stale audio or resume under replacement credentials. An unconfirmed old
session must be reconciled before a new one can own its GPU/tool lease.

Use the existing P0 durable-result authorization/recovery design (#1222 and
follow-ups) for completed receipts and lifecycle debt, not to promise durable
live audio or exactly-once arbitrary tools. Recovery cannot rerun completed
inference, inherit old credential authority, or fabricate missing audio. The
exact new session/checkpoint retention protocol remains a reviewed cross-repo gate.

## 6. Comparison and measurement acceptance — NOT RUN

| Path | Source-level fit | Unproven cost / gate |
| --- | --- | --- |
| Existing discrete STT/LLM/TTS | Existing handlers and license surfaces; independently selectable models | No equivalent full-duplex microturn capability established; baseline latency/quality still must be measured on the same test |
| Speakrail + permissively labelled TTS | Explicit text/tool seam, swappable TTS boundary, learned turn decisions | Multi-service VRAM, streaming swap, cached phrase/reference replacement, interruption/tool correctness and hardened transport |
| [PersonaPlex source][personaplex] `3428dfd95309a7f3c84fd93259ded0f810d1ff91` | Unified Moshi-derived duplex speech model with role/voice conditioning | Not a native AceTeam MCP tool protocol; adapter, headless lifecycle, fit/latency and safety need separate acceptance |

PersonaPlex repository code is MIT; [model metadata][personaplex-card] at
`fdaf4090a61cb315c138a1faee287ffd6c716309` is gated (`auto`) and reports `other`.
The public card identifies NVIDIA Open Model License governing terms with
additional CC-BY-4.0 information; this is not plain MIT weights. Exact gated
license files were not accessible anonymously and are a review/approval gate,
not inferred clearance. Its [Dockerfile/Compose][personaplex-build] also have
mutable build inputs and do not provide a verified image digest. Do not compare
the issue's approximate 170 ms model claim with end-to-ear latency as if they
were the same metric.

Run only after separate execution custody, download/license and design approval:

| Required evidence | Reproducible method to agree before execution | Current result |
| --- | --- | --- |
| RTX 3090 24 GB fit; 4090 separately if available | Otherwise idle selected card, exact driver/CUDA/source/image/model pins, model/context/sequence settings, free baseline, startup/steady/peak per-process and total selected-device VRAM, long-session growth/OOM, disk/RAM | **NOT RUN / HELD** |
| Cold/warm readiness and TTFT | Separate ASR first token, microturn decision token, first content token; timestamps from controlled input frames and explicit token events, monotonic clocks | **NOT RUN / HELD** |
| First audio and perceived response | Word-end-to-first-generated-audio and word-end-to-audible-playback separately; client buffer/network and cache-hit state; p50/p95 plus failures on fixed consented utterances | **NOT RUN / HELD** |
| Breeze vs Kokoro vs Chatterbox cost | Same texts, authorized voices, segmentation and sample format; TTS TTFA/RTF, peak VRAM, clipping/underrun, prosody/intelligibility, blinded paired listening with declared sample count/raters | **NOT RUN / HELD** |
| Duplex quality and tools | Versioned FDB subset plus pauses/backchannels/overlap/barge-in; tool schema-selection and unauthorized/speculative-side-effect tests; compare all paths on identical audio/conditions | **NOT RUN / HELD** |
| Cancellation/reconnect and drain | Busy and pinned candidate tests, actual inference exit, delayed release, stale reconnect/revoked grant, unknown-exit recovery and exact original-state restoration | **NOT RUN / HELD** |
| Transport/privacy/supply chain | Unauthorized/oversized/slow clients, bounded queues, no audio/secret leak, disabled raw debug/shell routes, immutable cache reuse verification and complete dependency/license inventory | **NOT RUN / HELD** |

Publish measured values with raw non-sensitive methodology/results and failures,
not a green check derived from health endpoints, advertised SM support or an
upstream benchmark chart. Owner must ratify workloads, quality/latency/VRAM
thresholds and maximum session/drain policy before performance acceptance.

## 7. Decisions needed before an accepted DoR or build

Route these to automated-citadel / automated-aceteam for named owner decisions:

1. Accept or revise the conditional primary candidate and Kokoro-first swap;
   agree which PersonaPlex comparison and permissioned evaluation are needed.
2. Ratify provider/adapter session authorization, browser/phone ingress,
   bounded audio protocol, tool broker schemas/approval/idempotency and retention.
3. Resolve #1340 drain/custody and unsupported direct ingress before exclusive
   residency; settle long-lived busy-session wait versus separately authorized
   operator termination. No scheduling/preemption contract is changed here.
4. Assign named source/image/model/license reviewers, approved manifest and
   measurement thresholds; approve exact execution custody separately.

Only after the parent design is accepted may build children be proposed/filed
for packaging, realtime/tool integration, drain integration and native acceptance.
This draft itself authorizes none of those actions and does not close #1376.

[source]: https://github.com/speakrail/speakrail/tree/49798e8e72c8a8429cb4d005840a6af389ececb7
[readme]: https://github.com/speakrail/speakrail/blob/49798e8e72c8a8429cb4d005840a6af389ececb7/README.md
[compose]: https://github.com/speakrail/speakrail/blob/49798e8e72c8a8429cb4d005840a6af389ececb7/compose.yaml
[code-license]: https://github.com/speakrail/speakrail/blob/49798e8e72c8a8429cb4d005840a6af389ececb7/LICENSE
[notice]: https://github.com/speakrail/speakrail/blob/49798e8e72c8a8429cb4d005840a6af389ececb7/NOTICE
[model-init]: https://github.com/speakrail/speakrail/blob/49798e8e72c8a8429cb4d005840a6af389ececb7/services/model-init/init.py
[asr-build]: https://github.com/speakrail/speakrail/blob/49798e8e72c8a8429cb4d005840a6af389ececb7/services/asr/Dockerfile
[tts-build]: https://github.com/speakrail/speakrail/blob/49798e8e72c8a8429cb4d005840a6af389ececb7/services/tts/Dockerfile
[server]: https://github.com/speakrail/speakrail/blob/49798e8e72c8a8429cb4d005840a6af389ececb7/app/server.py
[tools]: https://github.com/speakrail/speakrail/blob/49798e8e72c8a8429cb4d005840a6af389ececb7/app/tools.py
[tts-client]: https://github.com/speakrail/speakrail/blob/49798e8e72c8a8429cb4d005840a6af389ececb7/app/tts.py
[gguf-card]: https://huggingface.co/audio-cpp/audio.cpp-gguf/blob/e36610ac69b5262e914a52635324050bee8f1ad2/README.md
[voxtral]: https://huggingface.co/mistralai/Voxtral-Mini-4B-Realtime-2602
[turn-card]: https://huggingface.co/speakrail/Voxtral-Mini-4B-Realtime-2602-TurnHead/tree/ba89de14c626a362d4c7bc35094b0972c534e9d7
[gemma-card]: https://huggingface.co/google/gemma-4-12B-it-qat-w4a16-ct/blob/1d2c2d7f2466070e69d6fb3fd5ce9a7d75f2f6ee/README.md
[adapter-card]: https://huggingface.co/speakrail/gemma-4-12B-it-qat-Speakrail/tree/f1004b2763dafee266f108bb2f5167554b984c48
[breeze-card]: https://huggingface.co/BreezeBlue/Breeze-TTS-2/tree/3e28c5151381a722f1d8661b4118c298caa77aa4
[breeze-license]: https://huggingface.co/BreezeBlue/Breeze-TTS-2/blob/3e28c5151381a722f1d8661b4118c298caa77aa4/LICENSE
[kokoro-card]: https://huggingface.co/hexgrad/Kokoro-82M/tree/f3ff3571791e39611d31c381e3a41a3af07b4987
[kokoro-license]: https://github.com/hexgrad/kokoro/blob/main/LICENSE
[chatterbox-card]: https://huggingface.co/ResembleAI/chatterbox/tree/5bb1f6ee58e50c3b8d408bc82a6d3740c2db6e18
[personaplex]: https://github.com/NVIDIA/personaplex/tree/3428dfd95309a7f3c84fd93259ded0f810d1ff91
[personaplex-card]: https://huggingface.co/nvidia/personaplex-7b-v1/tree/fdaf4090a61cb315c138a1faee287ffd6c716309
[personaplex-build]: https://github.com/NVIDIA/personaplex/blob/3428dfd95309a7f3c84fd93259ded0f810d1ff91/Dockerfile
