# PersonaPlex full-duplex voice: source-first feasibility proposal

Status: **UNACCEPTED DESIGN OF RECORD PROPOSAL**, partial source feasibility for
#1216 / epic #1214, inspected 2026-10-22 against Citadel
`dc139d1de01239fc4d432990bef415d59f6b4e33`. No implementation, service, catalog,
model or image is supplied. No native execution, model/image/package download
or benchmark was run.
**BUILD requires a separate Jason green-light**, even after source/design review.
Implementation children below are proposals in this document only; none are
filed or authorized. #1216 remains open/WIP while feasibility gates are unmet.

## 1. Recommendation consistent with #1376

Retain PersonaPlex as the unified speech-to-speech comparison candidate, not a
second default resident voice engine. [Speakrail draft #1423][speakrail-design]
conditionally proposes Speakrail plus a validated Kokoro adapter first, because
its text/tool seam fits explicit AceTeam tool authority. This document does not
reverse that recommendation or claim either candidate is production-ready.

PersonaPlex's reason to exist is continuous audio-conditioned conversation, not
just a different TTS checkpoint. If measured turn-taking, quality, fit or simpler
audio behavior outweigh the missing native tool seam, owners can revise the
primary choice using comparable results. Keep existing discrete transcription,
LLM and synthesis handlers unchanged. Do not route existing agents to a new
provider without separately approved integration and acceptance.

## 2. Pinned primary evidence and feasibility status

NVIDIA source examined: [`3428dfd95309a7f3c84fd93259ded0f810d1ff91`][source].
Public model metadata examined:
[`nvidia/personaplex-7b-v1@fdaf4090a61cb315c138a1faee287ffd6c716309`][model-files].
These are **evidence pins**, not approved executable artifacts or compatibility
pairs. The model metadata lists a language model, Mimi checkpoint, text
tokenizer, voice archive and static-client archive. No artifact bytes/checksums,
archive contents, tensor shapes or runtime behavior were verified.

| Required #1216 gate | Source evidence | Remaining acceptance |
| --- | --- | --- |
| Headless/non-GUI mode | [`moshi/moshi/server.py`][server] defines an aiohttp `/api/chat` WS service; `--static none` omits UI serving. [`offline.py`][offline] is a separate WAV-to-WAV/text evaluation path | Source-level headless entrypoint exists; successful startup/readiness and offline operation **NOT RUN** |
| Real model access | Public metadata exposes exact repo revision and filenames; access is gated (`auto`) | Named-account terms approval and actual authorized immutable artifact retrieval/verification **HELD**, not proven pullable by this draft |
| Pullable image | [`Dockerfile`][dockerfile] and [`docker-compose.yaml`][compose] are public build recipes; Compose uses `build`, not a published image digest. Repository releases API returned no entries at inspection | No reviewed/pullable serving-image digest or provenance established; absence of releases does not prove no third-party image exists |
| Duplex transport | Server and [`client/src/protocol/encoder.ts`][protocol] provide a binary WS protocol, not OpenAI JSON chat/completions | Hardened bounded adapter, codec interoperability and authenticated realtime integration **UNACCEPTED / NOT RUN** |
| VRAM and latency | [`loaders.py`][loaders] defaults to BF16 LM loading; runtime has two Mimi instances and warm streaming state. [Model card][model-card] names A100 80 GB test hardware | 24 GB fit, selected-device peak/steady memory, startup cost and end-to-end latency **UNKNOWN / NOT RUN** |

The issue's approximately 170 ms is not a measured Citadel promise. NVIDIA's
card reports a smooth-turn-taking latency of 0.170 and interruption latency of
0.240 in its benchmark, with different definitions; its inference hardware is
an A100 80 GB. Neither result establishes a 3090/4090 end-to-ear budget. The
[project page][project] describes learned interruptions/backchannels and combined
role/voice control; these are upstream capability claims pending our acceptance.

## 3. Actual media and conditioning contract

The inspected model configuration fixes mono audio at 24 kHz and 12.5 model
frames/s: 1,920 samples per model frame (80 ms), **not measured response latency**.
The browser [`useUserAudio`][user-audio] configures `opus-recorder` for 24 kHz,
mono, 20 ms encoder frames and streaming pages. The server consumes those Opus
stream bytes through `sphn`, encodes/steps/decodes continuously and returns Opus
stream bytes; a WS message is not necessarily one codec/model frame or RTP packet.
[`useServerAudio`][server-audio] decodes before browser playback. Agree exact
container/page, resampling, jitter and codec state behavior in tests before use.

| Actual server wire tag | Meaning at this source pin |
| --- | --- |
| `0x00` server to client | One-byte readiness handshake after conditioning; not authentication, version negotiation or an AceTeam session grant |
| `0x01` both directions | Opus stream bytes following the first byte; not raw PCM16, base64 JSON or WebRTC media |
| `0x02` server to client | UTF-8 generated text pieces; not an authenticated user transcript or a structured tool request |
| Other client protocol tags | Client types also name control/metadata/error/ping, but this server receive loop only handles audio tag 1. Do not infer working end-turn/pause/restart/tool controls from the client enum |

Role text and a voice selection are query parameters in the bundled client.
The server wraps role text in system tags and loads voice audio or precomputed
`.pt` conditioning. [`lm.py:load_voice_prompt_embeddings`][conditioning] calls `torch.load`;
these are trusted executable-adjacent artifacts, not safe arbitrary uploads.
[`Conversation.tsx`][conversation] also builds/logs a URL carrying role text and
optional worker/email fields. Those fields are not enforced by this server.

Future adapter proposal: take a bounded approved voice ID plus reviewed role
configuration revision from authorized control state, never a caller pathname,
checkpoint URL or raw Python object. Resolve to immutable verified assets owned
by the service; bound any separately approved voice import and establish consent
and artifact identity first. Custom conditioning is not an automatically granted
voice-cloning product or a bypass of tool/session authorization.

### Source hardening prerequisites, not fixes made here

The raw server is not a tenant boundary: it has no general session authorization;
voice path joining and shared conditioning mutation precede the session lock.
It logs prompts/paths. The parsed `--host` is not passed to `web.run_app`, so the
flag alone cannot prove listener confinement. These source observations require
separate review/remediation before any raw endpoint is reachable by callers.

Propose admission **before** any prompt, codec or shared LM mutation, exactly
one accepted session per model instance until isolation is proven, and explicit
bounded wait/refusal rather than unlimited upgraded sockets waiting on the lock.
Put configuration and streaming reset under the same session ownership fence;
an arriving second session must not alter a running conversation's persona/voice.
Use an isolated private runtime network and a trusted adapter, with no raw app/UI
exposure. An external auth proxy alone does not fix unsafe shared-state ordering,
arbitrary voice selection or decode-buffer growth.

## 4. License and supply-chain inventory: labels, not clearance

Apply the accepted #1368 policy in `docs/design-engine-licensing.md` and held
#1377: **surface only, no new enforcement**. Code, language weights, codec,
tokenizer, voice embeddings/reference rights, client assets and image dependencies
need separate pinned provenance. A single `MIT` engine label is insufficient.

| Component | Primary evidence and limits |
| --- | --- |
| NVIDIA server/source | [`LICENSE-MIT`][code-license], SPDX headers and Python manifest declare MIT. Preserve NVIDIA/Kyutai notices; this does not relicense models |
| Inherited Moshi / AudioCraft / browser client | Pinned `moshi/LICENSE.moshi`, `moshi/LICENSE.audiocraft` and `client/LICENSE` contain MIT notices. Include third-party package/assets/SBOM inventory separately |
| PersonaPlex language-model weights | Public HF metadata reports `other` / `nvidia-open-model-license`. Public card names NVIDIA Open Model License governing terms, and identifies additional CC-BY-4.0 information |
| Mimi codec, tokenizer and voices | Separate named assets in the same gated repo, not assumed MIT because loader code is MIT. Exact artifact notices, voice provenance/consent and underlying model attribution remain reviewed inventory gates |
| Underlying Moshi provenance | NVIDIA project page identifies the base Moshi weights as CC-BY-4.0. Preserve attribution provenance; do not assume that one label fully describes each repackaged artifact |
| Serving/base/client images | No accepted digest. CUDA/NVIDIA runtime terms, Python/Torch/Opus/SentencePiece/sphn and browser/WASM/npm layers need individual license/notices and vulnerability/provenance review |

The public [NVIDIA Open Model License Agreement][model-license], last modified
2025-10-24 when inspected, permits commercial use subject to its conditions; it
also addresses guardrails, Trustworthy AI terms, redistribution notices and
separate components. NVIDIA claims no ownership in generated outputs, but that
is not clearance of voice likeness, privacy, third-party IP or a particular
resale offering. Do not transpose Cosmos-specific attribution to PersonaPlex
without an applicable term. The exact accepted terms and accompanying notices
for retrieved pinned artifacts remain unverified: no gated artifacts were fetched.

This corrects the issue's shorthand “clean for resale” into attributed license
evidence, not a legal/business determination. Jason's named catalog license
review and any required legal review remain gates; no credentials are requested
or passed through a conversation. Label unknowns as unverified; build no new
org block/exclusion mode. Existing signed v2 receipt bytes stay unchanged; any
proposed license sibling is unsigned until separately coordinated v3 work.

### Build proposal held behind a separate green-light

A future catalog module would be a distinct duplex engine/service, following
the inference-server lifecycle precedent, not a `DIFFUSERS_MODEL` entry or a
claim that the existing TTS `/v1/audio/speech` shape handles continuous audio.
Propose AceTeam-owned source build into GHCR, source and platform image digests,
locked/hash-verified dependencies, SBOM/provenance and an offline verified model
store. No new port is allocated now; use `services/ports.go` and its collision
guard if a loopback host listener is approved later.

The upstream serving Dockerfile uses a CUDA `12.4.1-runtime-ubuntu22.04` tag,
`ghcr.io/astral-sh/uv:latest`, apt and `uv sync`; its dependency ranges and
build success are not reviewed pins. The separate client Dockerfile builds on
`node:20`, runs `npm install`, creates a self-signed certificate and launches a
development server. Neither recipe establishes a production image, security
baseline, host confinement or trusted TLS distribution. Do not copy Compose's
unqualified host-port publish or broad root cache mount as accepted defaults.

Serving startup currently calls unrevisioned HF downloads, including config,
voices/static archives; local weight flags alone do not prove offline startup.
Propose a reviewed import-only stage with explicit revision/digest manifest and
safe bounded archive extraction (no links/traversal/expansion surprises), followed
by read-only serving mounts and denied runtime installation/downloads. Runtime
`torch.load` and optional checkpoint paths must never receive caller artifacts.
This draft supplies no Dockerfile, image digest, approved checkpoint manifest,
catalog row, runtime patch, download token or native compatibility result.

## 5. Authenticated integration and bounded lifecycle proposal

Current `internal/jobs/huddle_realtime_conn.go:dialRealtime` waits for authenticated
and media-ready AceTeam events; `huddle_join.go` binds org/channel/call/agent/node/
job/attempt and owns cleanup debt. `huddle_converse.go` carries 24 kHz PCM16 using
OpenAI-style JSON. PersonaPlex's Opus WS protocol is not compatible unchanged.
`internal/gateway/gateway.go:proxyWebSocket` tunnels bytes; it is not semantic
session admission, a bounded decoder, or proof of ongoing tenant authorization.

Propose the same cross-repo provider/adapter boundary as #1423: authenticate
browser/phone/huddle participants at the trusted AceTeam media boundary, use an
authenticated node-side WSS control/audio leg, and keep raw PersonaPlex private.
PersonaPlex is not a WebRTC/ICE/SIP/PSTN provider; those transports need separate
session/consent/codec adapters. No public raw node listener, global service secret
or borrowed human identity is introduced by this proposal.

Before capture/admission, bind short-lived audience-limited authority to org,
node, agent, call/session, attempt, provider/model/voice/config revision and expiry;
check live grants/membership, readiness and Origin, then recheck revocation.
The exact session auth exchange, browser credential transport and policy numbers
remain **UNACCEPTED**, not a protocol created by this doc. Upstream `worker_auth_id`
or prompt query fields are not a substitute for that exchange. Never log them.

Bound both compressed bytes and decoded samples/duration, per-frame/control
messages, queued input/output, waiting sessions, context/prompt length and total
session time. Ratify finite heartbeat, admission, idle and slow-client budgets.
Validate Opus/page sequencing and malformed inputs; use explicit backpressure
and observable failure rather than unbounded buffering or silently dropping
speech while reporting uninterrupted success. Observe client playback/jitter
separately from model generation. Audio, transcript and persona content stay out
of generic logs; org-scoped retention/audit requires its own approved destination.

There is **no native typed AceTeam MCP tool-call protocol in the inspected WS
handler**. Generated text is not a trusted tool command. Propose no autonomous
side effects for the initial PersonaPlex comparison. A later approved tool bridge
would require typed allowlisted schemas and trusted broker authorization per
live org/agent/call grant, required human confirmation, stable idempotency IDs
and uncertain-outcome reconciliation. Do not scrape spoken/generated text into
shell commands or treat role prompting as tool authorization. This missing seam
is a reason to keep Speakrail as the conditional primary candidate.

### Residency, drain, cancellation and restart custody

Propose one selected-GPU residency owner for the complete model/codec/session
lifecycle, including warmup and retained CUDA caches. `GPUTracker`'s chat-job
slots do not establish resident voice exclusivity; current `Reserve` and
`ReserveExclusive` do not prove all-ingress drain. Default BF16 and “7B” naming
do not establish sufficient 24 GB headroom. CPU offload is an upstream option,
not proof of interactive fit, safety or equivalent latency.

Depend on unaccepted #1340 per-service drain design: pinned services never
evicted; non-pinned services including Ollama/vLLM/OmniVoice may be candidates,
but busy work drains naturally and is never cancelled to obtain capacity.
Fence worker/gateway/direct admission, keep active leases to actual exit, and
resolve unmanaged ingress before claiming coverage. Never wait on global active
jobs including the reserving job, discard waiters, or change #1271 queue rules.

Persist owner/incarnation/epoch, selected GPU and exact original service state.
After separately authorized voice-session shutdown, join model/codec/adapter
tasks and verify actual GPU/residency release before restoring only owned evicted
services. A socket close or `task.cancel()` is not a CUDA-exit certificate.
Unknown exit retains visible custody/recovery debt, never overlapping residency.
Long-lived busy calls may delay eviction: timeout cannot turn natural drain into
forced cancellation; operator termination policy requires separate owner approval.

Learned barge-in is conversation behavior, not an authenticated session-cancel
operation. Specify distinct playback suppression/utterance fences, continued
listening, and session-end cleanup; the client enum does not prove server cancel.
On reconnect, freshly authorize and fence the exact attempt; do not replay old
audio or silently transfer authority/state to replacement credentials. Existing
shared streaming reset is not a durable multi-tenant session checkpoint.

Use P0 #1222/follow-up receipt and cleanup-debt authorization contracts where
applicable, without claiming durable live audio, exactly-once arbitrary tools or
rerun permission. A lost live conversation must be reported/reconciled honestly;
no fabricated recovery from missing model/audio state. Exact new session recovery
is cross-repo design work, not implemented here.

## 6. Missing acceptance and owner decisions

| Evidence required after separate authorization | Method to ratify | Status |
| --- | --- | --- |
| Exact artifact and image availability | Named terms approval, revision/digest/signature/notice manifest, reviewed import and successful pull/start | **HELD / NOT RUN** |
| Headless offline readiness | Reviewed no-static/no-tunnel/private listener configuration, no runtime network fetch; authenticated ready only after model/codec conditioning | **HELD / NOT RUN** |
| 24 GB fit | Otherwise idle selected 3090, exact driver/CUDA/source/image/model/voice settings; startup/steady/peak per-process and selected-device VRAM, host RAM, long-session growth/OOM, offload separately | **HELD / NOT RUN** |
| Latency and audio quality | Cold/warm conditioning, first generated text/audio and client-audible response separately; p50/p95 and failures, interruption-stop latency, jitter/underruns, authorized voice intelligibility/similarity | **HELD / NOT RUN** |
| Comparable provider choice | Same consented audio, benchmark version/definitions and hardware for PersonaPlex, Speakrail plus permitted TTS and current cascade; no cherry-picked cross-benchmark score comparison | **HELD / NOT RUN** |
| Isolation and bounded media | Second-session conditioning race, path/asset refusal, malformed/oversized compressed and decoded audio, slow consumer, revoked grants, no prompt/token/path leaks | **HELD / NOT RUN** |
| Actual lifecycle custody | Disconnect/barge-in/session-end distinctions, actual task/GPU exit, stale reconnect, pinned/busy drain, crash recovery and exact prior-state restore | **HELD / NOT RUN** |

Route decisions to automated-citadel / automated-aceteam for named owners:

1. Keep the conditional Speakrail-first comparison or revise after agreed
   measurements; settle PersonaPlex's initial no-autonomous-tools scope.
2. Ratify provider/codec/session authorization, bounded media/privacy policy and
   voice provenance/import scope. A prompt or raw query field never grants access.
3. Resolve #1340 all-ingress custody and long-call policy, then approve exact
   artifact/license/image reviewers and performance/quality thresholds.
4. Explicitly accept a DoR, then separately obtain Jason's build and execution
   green-light. Source review alone satisfies neither approval.

Potential later children, **documented only**: reviewed packaging/offline import;
authenticated codec/provider adapter; lifecycle/drain integration; hermetic
protocol/privacy fixtures; native fit/audio acceptance. No runtime/catalog work,
child filing, activation, schema/SQL, undraft, merge, deploy or release occurs here.

[source]: https://github.com/NVIDIA/personaplex/tree/3428dfd95309a7f3c84fd93259ded0f810d1ff91
[server]: https://github.com/NVIDIA/personaplex/blob/3428dfd95309a7f3c84fd93259ded0f810d1ff91/moshi/moshi/server.py
[offline]: https://github.com/NVIDIA/personaplex/blob/3428dfd95309a7f3c84fd93259ded0f810d1ff91/moshi/moshi/offline.py
[loaders]: https://github.com/NVIDIA/personaplex/blob/3428dfd95309a7f3c84fd93259ded0f810d1ff91/moshi/moshi/models/loaders.py
[conditioning]: https://github.com/NVIDIA/personaplex/blob/3428dfd95309a7f3c84fd93259ded0f810d1ff91/moshi/moshi/models/lm.py
[protocol]: https://github.com/NVIDIA/personaplex/blob/3428dfd95309a7f3c84fd93259ded0f810d1ff91/client/src/protocol/encoder.ts
[user-audio]: https://github.com/NVIDIA/personaplex/blob/3428dfd95309a7f3c84fd93259ded0f810d1ff91/client/src/pages/Conversation/hooks/useUserAudio.ts
[server-audio]: https://github.com/NVIDIA/personaplex/blob/3428dfd95309a7f3c84fd93259ded0f810d1ff91/client/src/pages/Conversation/hooks/useServerAudio.ts
[conversation]: https://github.com/NVIDIA/personaplex/blob/3428dfd95309a7f3c84fd93259ded0f810d1ff91/client/src/pages/Conversation/Conversation.tsx
[dockerfile]: https://github.com/NVIDIA/personaplex/blob/3428dfd95309a7f3c84fd93259ded0f810d1ff91/Dockerfile
[compose]: https://github.com/NVIDIA/personaplex/blob/3428dfd95309a7f3c84fd93259ded0f810d1ff91/docker-compose.yaml
[code-license]: https://github.com/NVIDIA/personaplex/blob/3428dfd95309a7f3c84fd93259ded0f810d1ff91/LICENSE-MIT
[model-files]: https://huggingface.co/nvidia/personaplex-7b-v1/tree/fdaf4090a61cb315c138a1faee287ffd6c716309
[model-card]: https://huggingface.co/nvidia/personaplex-7b-v1
[model-license]: https://www.nvidia.com/en-us/agreements/enterprise-software/nvidia-open-model-license/
[project]: https://research.nvidia.com/labs/adlr/personaplex/
[speakrail-design]: https://github.com/aceteam-ai/citadel-cli/pull/1423
