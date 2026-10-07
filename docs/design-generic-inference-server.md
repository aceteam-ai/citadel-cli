# ADR: Generic config-driven inference server, with OmniVoice as the first consumer

Status: **ACCEPTED AND IMPLEMENTED for the TTS/OmniVoice slice.**

Decision date: 2026-09-08. Last reconciled with citadel-cli v2.179.0 on
2026-10-04.

Implementation references:

- `aceteam-ai/citadel-inference-server` P0 at
  `faf0e19edaf8c4efd50526a8448c3b201ab7d8ec`.
- citadel-cli P1, PR #1008, merge commit
  `91f95565f6c051cfd5b5113ffdabf8922447dcfa`.
- AceTeam routing P2, PR #9347, merge commit
  `271f64a27595fcd3c05ac8eff2fb44e30170a336`.

This is an architecture decision record, not a promise that every future
modality will use the same runtime. The implemented scope is one reusable
runtime, one `tts` task, and an OmniVoice adapter. Later adapters remain
separate decisions.

## 1. Context

Adding an inference model to Citadel historically meant either:

- checking another Python wrapper into citadel-cli (`diffusers-service` and
  `whisper-service` are current examples);
- building a model-specific image in another repository, as Kokoro and
  extraction do; or
- building on the node, as Bonsai does.

That made citadel-cli own model-serving code even though its primary job is
orchestration. The requested direction was to stop adding model wrappers here:
citadel-cli should register a service and route a stable job contract, while a
separate runtime owns model loading and HTTP serving.

OmniVoice was the first concrete consumer. Its Python package exposes a model
API and demo, not the authenticated, bounded, loopback HTTP service the worker
needs. The published `k2-fsa/OmniVoice` checkpoint is CC-BY-NC; the code is
Apache-2.0. The implementation reports that checkpoint license rather than
hiding it, but reporting it does not remove the product/legal constraint.

## 2. Decision

### 2.1 Runtime ownership

Use the public `aceteam-ai/citadel-inference-server` repository (CIS), not an
in-repo Python build context and not a directory under one catalog service.

CIS owns:

- environment parsing and validation;
- Hugging Face cache placement and disk preflight;
- lazy, single-flight model loading;
- `/health` and `/info`;
- concurrency and input bounds;
- task-level HTTP routes and receipt headers; and
- thin model-family adapters.

citadel-cli owns:

- the embedded compose service and loopback port;
- cache, port, hash, and engine registration;
- the existing `SYNTHESIZE_SPEECH` job boundary; and
- selection of a configured TTS backend.

A new checkpoint within an implemented adapter family can be configuration.
A genuinely new model family still requires an adapter in CIS. “Config-only”
never meant that unrelated Python APIs could be served without glue.

### 2.2 Base runtime plus task/family adapters

Keep HTTP/task behavior separate from model-family behavior:

- the `tts` task owns `POST /v1/audio/speech`, request validation, WAV
  encoding, the semaphore, and `X-TTS-*` receipt headers;
- the `omnivoice` family owns model loading and the mapping to
  `OmniVoice.generate`; and
- a registry resolves `(CIS_TASK, CIS_ADAPTER)`.

Only `CIS_TASK=tts` is implemented today. STT, image/video, extraction, and
chat contracts differ materially and are not represented as implemented CIS
capabilities by this ADR.

### 2.3 Keep the existing job type

Do not add an OmniVoice-specific job. `SYNTHESIZE_SPEECH` has an optional
`backend` payload field:

- absent means `kokoro`, preserving every older dispatch;
- `omnivoice` selects the CIS-backed sidecar; and
- any unknown value fails before an HTTP request.

The handler forwards optional `speed` and `instructions` only when present and
returns an additive `backend` field in its result envelope. Backend-specific
defaults are load-bearing: Kokoro uses `am_michael`/`opus`; OmniVoice uses
`auto`/`wav`.

There is no new gateway `/v1/audio/speech` route. Product traffic uses the
existing job path. AceTeam's `/native/tts` route now selects and probes the
requested backend; its public narration endpoint remains Kokoro-only.

### 2.4 Network and device boundary

CIS has no service authentication. The OmniVoice compose therefore publishes
only on `127.0.0.1`; its sole consumer is the co-located worker.

OmniVoice is treated as GPU-only in the embedded service:

- the compose includes an NVIDIA device reservation;
- the compose sets `CIS_DEVICE=cuda` by default; and
- CIS fails loudly if explicit CUDA is unavailable.

Kokoro remains the CPU-capable TTS choice. `SYNTHESIZE_SPEECH` is deliberately
not part of the worker's chat-inference GPU-slot map; `CIS_SLOTS` bounds
OmniVoice concurrency. If measured contention requires a shared GPU admission
policy, that is a follow-up rather than an implicit expansion of the existing
chat-engine semaphore.

## 3. What shipped

| Phase | Repository | Implemented result |
|---|---|---|
| P0 | `aceteam-ai/citadel-inference-server` | Python runtime; `tts` task; OmniVoice and hermetic stub adapters; `:base` and `:tts` images; CI and publish workflows. Current main/published source is `faf0e19e`. |
| P1 | citadel-cli #1008 | Embedded `omnivoice` service, port/cache/hash/engine registration, backend-selectable `SYNTHESIZE_SPEECH`, per-backend defaults, request passthrough tests, and best-effort license reporting. |
| P2 | aceteam #9347 | `/native/tts` accepts `kokoro | omnivoice`, probes `SERVICE_STATUS` for the selected concrete service, forwards non-default `backend` and optional `instructions`, and preserves the old Kokoro payload when defaults are used. |

P0's current CI and publish workflows both pass for `faf0e19e`. P1 is present
in v2.179.0. P2 is present on AceTeam main. The source review behind this ADR
does not establish a recorded live GPU synthesis result; live node acceptance
is operational evidence, not a missing architecture decision.

### 3.1 CIS configuration actually implemented

`Config.from_env` is authoritative. The current contract is:

| Variable | Current behavior |
|---|---|
| `CIS_TASK` | Required; only `tts` is accepted. The `:tts` image sets it. |
| `CIS_ADAPTER` | Required; `omnivoice` and the test-only `stub` are registered. |
| `CIS_MODEL` | Required; the compose defaults to `k2-fsa/OmniVoice`. |
| `CIS_MODEL_REVISION` | Optional HF revision. |
| `CIS_DEVICE` | Server default `auto`; the Citadel compose defaults it to `cuda`. The intended values are `auto`, `cpu`, `cuda`, and `cuda:N`; the current parser actually accepts `auto`, `cpu`, or any string prefixed with `cuda`. |
| `CIS_DTYPE` | Default `auto`; the compose defaults to `float16`. |
| `CIS_SLOTS` | Default `2`; request semaphore size. |
| `CIS_MAX_INPUT_CHARS` | Default `5000`; longer synthesis input returns 413. |
| `CIS_PRELOAD` | Default `true`; loading begins after the socket is available. |
| `CIS_DISK_PREFLIGHT` | Default `true`; confirmed insufficient space fails closed. |
| `CIS_EXTRA` | Optional JSON object. OmniVoice reads `num_step` (default 32) and optional `model_license`. Malformed JSON refuses startup. |
| `PORT` | CIS default and image port `8000`. |
| `HF_HOME` | Default `/root/.cache/huggingface`; the compose mounts the shared node cache there. |
| `HF_TOKEN` | Optional token for gated repositories. |

The initial proposal described multiple modality values, adapter-specific
image names, port 8080, an audio cache, and ffmpeg formats. Those did not ship:
P0 intentionally narrowed the runtime to TTS, publishes `:base` and `:tts`,
listens on 8000, emits WAV only, and reports `X-TTS-Cache-Hit: 0`.

### 3.2 TTS HTTP contract actually implemented

`POST /v1/audio/speech` accepts:

| Field | Current behavior |
|---|---|
| `input` | Required bounded text. |
| `voice` | Default `auto`; enumerable preset or `auto`, never free text. |
| `response_format` | `wav` only. Anything else returns 400. |
| `instructions` | Optional voice design; overrides the preset. OmniVoice validates it against upstream's closed attribute vocabulary. |
| `language` | Optional OmniVoice language hint. |
| `speed` | Optional `0.5..2.0`. |
| `model` | Accepted and ignored for OpenAI-client compatibility. |

The shipped presets are `auto`, `female`, `male`, `female-young`,
`male-young`, `female-mature`, `male-mature`, `female-high-pitch`,
`male-low-pitch`, `female-american`, `male-american`, `female-british`,
`male-british`, and `whisper`. `/info`.`voices` is the authority.

Success returns PCM-16 RIFF/WAV and the five headers consumed by Citadel:

- `X-TTS-Model-Version`;
- `X-TTS-Cache-Key`;
- `X-TTS-Chars`;
- `X-TTS-Duration-Seconds`; and
- `X-TTS-Cache-Hit`.

Errors use FastAPI's `{"detail":"..."}` shape. Citadel surfaces a non-2xx
body verbatim. `/health` always returns HTTP 200; readiness is the
`model_loaded` boolean, which is exactly what `synthesizeHealthReady` checks.
`/info` reports the task, adapter, model, formats, capacity, voices, adapter
details, and `model_license`.

The adapter calls `OmniVoice.from_pretrained` once and maps `input`, resolved
instructions, language, speed, and `CIS_EXTRA.num_step` into
`model.generate`. It does not pass cloning or ASR arguments.

### 3.3 Citadel registration actually implemented

| Item | Current value |
|---|---|
| Service/container | `omnivoice` / `citadel-omnivoice` |
| Image | `ghcr.io/aceteam-ai/citadel-inference-server:tts` |
| Host/container port | `127.0.0.1:${CITADEL_OMNIVOICE_HOST_PORT}:8000`; default host port 8214 |
| Cache | Shared `~/citadel-cache/huggingface` plus service-local `~/citadel-cache/omnivoice:/data` |
| Engine cache family | `CacheFamilyHFHub` |
| Provisioning | Self-provisioning; `MODEL_CACHE_PULL` is a no-op for this engine because the compose owns model download into the mounted cache |
| Load estimate | 60 seconds in the engine registry |
| Service type | `ServiceTypeOther` through the default mapping |
| GPU posture | NVIDIA reservation; no Darwin advertisement |

The registration is pinned by `TestOmniVoiceComposeRegistered`,
`TestOmniVoiceComposeContract`, `TestOmniVoiceHostPortRegistered`, compose hash
coverage, cache-table coverage, registry equivalence, and host-port collision
tests. There is no `services/omnivoice-service`, `build:` block, or
`ServiceAuxFiles` entry.

### 3.4 Citadel worker behavior actually implemented

`SynthesizeSpeechHandler` resolves `kokoro` and `omnivoice` through a
`BaseURLs` allowlist. Tests pin:

- byte-identical legacy Kokoro requests when new fields are absent;
- optional speed/instructions passthrough;
- routing to the second loopback URL;
- per-backend defaults; and
- rejection of an unknown backend without an HTTP call.

After successful synthesis, Citadel best-effort reads `/info` and records
`model_license`. The status collector then adds it to the running service's
heartbeat. Probe failure never fails an otherwise successful audio job.

Word timestamps were added after #1008. `word_timestamps=true` uses the
captioned endpoint and validates returned intervals; this is a Kokoro feature,
not part of the current CIS/OmniVoice contract.

## 4. Why the alternatives were rejected

### One server with no adapters

Rejected. Model APIs are different. Shared loading, validation, health, and
receipts belong in the runtime; model invocation belongs in a small family
adapter.

### One image containing every family

Rejected. Torch, transformers, diffusers, Whisper, and model-family pins are
large and conflict-prone. CIS therefore has a base image and task image. It
can add more task/family images later without changing the Citadel boundary.

### Put CIS under `citadel-services`

Rejected. A shared versioned runtime used by multiple catalog entries is not
owned by one catalog entry, and copied build directories would recreate the
drift this design avoids.

### Put the Python runtime in citadel-cli

Rejected. It would add another in-repo wrapper and make nodes build model code
locally. The chosen boundary keeps Citadel's OmniVoice contribution to compose
and orchestration code.

### Add a new job or expose the sidecar on the mesh

Rejected for this slice. The existing synthesis job already provides the
right text-in/audio-out boundary. The unauthenticated sidecar remains
loopback-only; routing and authorization stay at the existing fabric boundary.

## 5. Current gaps and follow-ups

These are implementation or product follow-ups, not unresolved choices that
should keep this ADR in draft.

1. **AceTeam's default OmniVoice format is inconsistent.** Current
   `/native/tts` defaults `response_format` to `opus` for both backends and
   always sends it, while CIS accepts only `wav`. A caller selecting
   `backend:"omnivoice"` must currently send `response_format:"wav"`; the
   otherwise-default request reaches the correct node and then fails at the
   sidecar. AceTeam should use a backend-specific format default and add a
   route test that asserts the complete OmniVoice payload.
2. **Some AceTeam examples are not CIS presets.** The route accepts any voice
   string and one unit test uses `female-warm`, but the shipped CIS preset list
   does not include that value. Product clients should obtain or pin values
   from `/info`.`voices`, and cross-repo tests should use an implemented preset.
3. **Two compose variables are dead configuration.** The current compose still
   sets `CIS_DEFAULT_VOICE` and `CIS_DEFAULT_FORMAT`, including an `opus`
   fallback, but `Config.from_env` does not read either variable and the TTS
   route is WAV-only. Citadel's worker always sends its own per-backend
   `auto`/`wav` defaults, so this does not break the current job path; the dead
   variables should be removed rather than presented as supported CIS knobs.
4. **Device validation is looser than its error message.** CIS currently
   accepts any `CIS_DEVICE` string beginning with `cuda`, not only `cuda` or
   `cuda:N` as the startup error says. Tightening that parser is a CIS
   validation follow-up; Citadel's compose uses the valid value `cuda`.
5. **Floating image refresh is manual.** The compose pins floating `:tts`; a
   normal embedded-service `compose up` does not first pull it. The compose
   records that redeploy must pull before up, but the general service-start
   path has not adopted that behavior.
6. **Live acceptance remains separate evidence.** CI is hermetic and the CIS
   image smoke uses the stub adapter. A real NVIDIA-node test should record
   model load, one WAV synthesis, license reporting, and the full
   AceTeam-to-node route before describing the product path as live-validated.
7. **Voice cloning is deferred.** No `ref_audio`, `ref_text`,
   `voice_clone_prompt`, or ASR-loading argument is passed. Adding binary
   reference input requires a separately bounded fabric contract.
8. **Other sidecars have not migrated.** Diffusers and Whisper still live in
   citadel-cli; Kokoro and extraction still use their existing images; Bonsai
   remains a local build. Migration order and whether each family belongs in
   CIS are future decisions, not part of #1008.
9. **GPU coexistence is observational.** `CIS_SLOTS` protects the sidecar from
   its own concurrent requests, but no common admission mechanism arbitrates
   OmniVoice against chat engines on the same card.
10. **The checkpoint license remains a constraint.** Surfacing `CC-BY-NC` in
   `/info` and heartbeat makes it visible; it does not grant commercial rights.

## 6. Consequences

Positive:

- Citadel gained a second TTS engine without importing model Python.
- Existing Kokoro dispatches remain compatible by construction.
- The model-serving boundary is reusable and independently tested/published.
- The sidecar is loopback-only, bounded, and explicit about checkpoint
  licensing.
- A new model in the same family can change configuration rather than the
  worker protocol.

Costs:

- Three repositories must keep a small cross-repo contract aligned.
- The current CIS task is intentionally narrow: WAV-only TTS, no cloning, no
  shared GPU scheduler, and no content cache.
- Floating task images need an explicit pull policy.
- Cross-repo unit tests can still drift unless they assert backend defaults and
  real adapter vocabulary, as the current format/preset gaps demonstrate.

## 7. Authoritative sources

When this ADR and code differ, code wins. Re-check:

- `services/compose/omnivoice.yml`;
- `services/embed.go`, `services/ports.go`, `services/caches.go`, and
  `services/known_hashes.go`;
- `internal/jobs/synthesize_speech.go` and its tests;
- `internal/jobs/model_cache_pull.go` and `internal/engine/registry.go`;
- `internal/status/model_license.go` and the status collector;
- CIS `cis/config.py`, `cis/app.py`, `cis/adapters/tts.py`, and
  `cis/adapters/omnivoice.py`; and
- AceTeam `python-backend/routes/native_tts.py`.
