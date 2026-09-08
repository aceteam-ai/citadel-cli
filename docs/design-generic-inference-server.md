# Design: a generic, config-driven inference server — and OmniVoice as its first consumer

Status: DRAFT for review. Design doc only — no engine, wrapper, or Go changes
ship with it. Everything below is grounded in the repo at v2.148.0 (2026-09-08);
where the brief that prompted this doc disagreed with the code, the code won and
the disagreement is called out inline (§1.4 collects them).

Per this repo's CLAUDE.md rule on restated facts: functions, tables, tests and
files are cited by name, never by line number. Treat every "today it does X" as
verified at doc-writing time and re-check the named symbol before building on it.

## Context

Jason's direction, verbatim in spirit: *"I don't really want citadel-cli repo to
carry things like bonsai wrappers, it should be purely citadel-cli."* And: *"we
CAN embed a generic http server that can be used for whatever that needs."* The
ask is one generic, config-driven inference server so onboarding a new model
becomes "point the server at an HF repo + modality via config," not "write
another bespoke wrapper." citadel-cli should only ever carry the compose file +
minimal Go registration.

The immediate consumer is **OmniVoice** (`k2-fsa/OmniVoice`): a 0.6B-parameter
zero-shot TTS diffusion-LM on a Qwen3-0.6B backbone, 600+ languages, 24 kHz
output. Verified against the upstream README and model card: code is
Apache-2.0; the **published checkpoint is CC-BY-NC** ("due to constraints from
its training data (e.g., Emilia)"). Jason has chosen to proceed toward
commercial use anyway — this doc treats the weights license as a tracked
caveat (§9), not a blocker, and does not re-litigate it. Upstream ships
`pip install omnivoice`, a Python API (`OmniVoice.from_pretrained(...)`,
`model.generate(text=, ref_audio=, ref_text=, instruct=, num_step=, speed=,
duration=)` plus a `language_id`), CLIs (`omnivoice-infer`,
`omnivoice-infer-batch`) and a Gradio demo (`omnivoice-demo`, port 8001) — and
**no HTTP API server**. Omitting `ref_text` when cloning triggers a Whisper ASR
side-download (`asr_model_name`/`asr_device`), which matters for §7.

## 1. Current state

### 1.1 Two ways a model becomes a citadel service today — actually three

The brief framed this as bonsai (build-based, in-repo Dockerfile) versus
kokoro/gliner2/extraction (prebuilt images built elsewhere). The repo is
messier, and the mess is the point:

| Service (`services.ServiceMap` key) | Image | Where the wrapper SOURCE lives | Publish pipeline |
|---|---|---|---|
| `bonsai` | `build:` from `services/compose/bonsai/Dockerfile` (compiles the PrismML llama.cpp fork on the node) | citadel-cli | none — built on every node (`ServiceAuxFiles` + `WriteAuxFiles`) |
| `diffusers` | `ghcr.io/aceteam-ai/diffusers-service` | **citadel-cli** `services/diffusers-service/` (`app.py`, 480 lines; `model_preflight.py`; tests) | `.github/workflows/build-diffusers-service.yml` |
| `transcribe` | `ghcr.io/aceteam-ai/whisper-service` | **citadel-cli** `services/whisper-service/` | `build-whisper-service.yml` |
| `kokoro` | `ghcr.io/aceteam-ai/kokoro-service` | `aceteam-ai/citadel-services` `services/kokoro/build/` (`server.py`, ~27 KB; Dockerfile) | citadel-services `publish-kokoro-image.yml` |
| `extraction` | `ghcr.io/aceteam-ai/gliner2-service` | private repo `aceteam-ai/gliner2-service` (+ a `build/` copy under citadel-services `services/gliner2/`) | that repo |
| `unlimited-ocr`, `vllm`, `sglang`, `llamacpp`, `tei`, `ollama`, `lmstudio` | upstream images | n/a — no AceTeam wrapper | n/a |

Plus four non-inference service sources also in-repo with their own
`build-*-service.yml` workflows: `claudecode-service`, `hermes-service`,
`meeting-service`, `nvr-service`.

**So citadel-cli is not pure today.** Bonsai's Dockerfile is not the first
wrapper the repo carries; it is the seventh, and only the only one that builds
on the node instead of in CI. The honest framing of the goal is therefore
**"stop adding in-repo wrappers now; migrate the existing ones out later"**
(§8), not "keep the repo clean." The v1 rule this doc proposes is: **no new
`services/<x>-service/` directory and no new `ServiceAuxFiles` entry in
citadel-cli, ever again.**

### 1.2 The contracts the generic server must speak (verified)

**`SYNTHESIZE_SPEECH`** — `internal/jobs/synthesize_speech.go`,
`SynthesizeSpeechHandler`:
- `synthesizeServiceURL()` always returns `http://localhost:<services.TTSHostPort>`.
  There is no backend-selection field; the handler is hardwired to kokoro.
- Payload read: `text` (alias `input`), `voice` (default `am_michael`),
  `response_format` (alias `format`, default `opus`). **`speed` is not
  forwarded**, even though kokoro's `SpeechRequest` accepts it (`speed: float
  = Field(1.0, ge=0.5, le=2.0)`) — a pre-existing passthrough gap.
- Sends `POST /v1/audio/speech` with `{"input","voice","response_format"}`;
  expects raw audio bytes on 200, a JSON error body otherwise.
- Readiness: `waitForReady` polls `GET /health` and gates on the BODY's
  `model_loaded: true` (`synthesizeHealthReady`), not the status code, because
  kokoro's `/health` always returns 200 with `{"status":"up"|"loading",
  "model_loaded":bool,...}`. Two budgets: `synthesizeUnreachableTimeout` (8s,
  connection refused) vs `synthesizeReadyTimeout` (120s, reachable-but-loading).
- Receipt: best-effort from `X-TTS-Model-Version`, `X-TTS-Cache-Key`,
  `X-TTS-Chars`, `X-TTS-Duration-Seconds`, `X-TTS-Cache-Hit` ("0"/"1")
  (`synthesizeReceiptFromHeaders`). Kokoro's `server.py` sets exactly these.
- Response envelope `{"encoding":"base64","content","format","voice","receipt"}`
  is what aceteam's `routes/native_tts.py` parses.

**`MEDIA_GENERATE`** — `internal/jobs/media_generate.go`,
`MediaGenerateHandler`: same hardwired shape (`mediaGenerateServiceURL()` →
`services.DiffusersHostPort`), but a different sidecar dialect: `POST
/generate` / `POST /generate/video` with a JSON body, JSON response carrying
`image_base64`/`video_base64`; `/health` means *reachable*, not loaded (the
diffusers sidecar deliberately lazy-loads inside the generation POST). Its
envelope is `{"encoding","content","format","model","receipt"}`.

**`llm_inference`** — `internal/worker/llm_inference.go`,
`NewLLMInferenceHandler`: the ONLY existing multi-backend router. A `baseURLs
map[string]string` keyed by the payload's `Backend`, built from
`services.*HostPort` constants, plus a `switch backend` in `Execute`
(vllm/sglang/ollama/llamacpp/bonsai/unlimited-ocr). This is the precedent for
making a job type engine-selectable without duplicating the handler.

**No gateway route for `/v1/audio/speech`.** `internal/gateway/chat_route.go`
registers `/v1/chat/completions`, `/v1/completions`, `/v1/models`; grep for
`audio/speech` in `internal/gateway` returns nothing. TTS is reachable only via
the job type. Confirmed.

### 1.3 How a new embedded service registers in citadel-cli today (verified, longer than the brief's list)

The commit that added kokoro (#584, `5ff63690`) touched 11 files. Several
tables and tests have grown since, so the checklist for a NEW `ServiceMap`
entry as of v2.148.0 is:

| # | File / symbol | Why it's mandatory (what fails otherwise) |
|---|---|---|
| 1 | `services/compose/<name>.yml` + `//go:embed` var + `ServiceMap["<name>"]` in `services/embed.go` | the service exists |
| 2 | `services/ports.go`: `Env<X>HostPort` const, `<X>HostPort` const, `ServiceHostPorts` + `serviceHostPortEnv` entries; a `Test<X>HostPortRegistered` in `services/ports_test.go` | `HostPortEnv()` won't inject the var; compose `${CITADEL_*_HOST_PORT}` won't resolve |
| 3 | `services/caches.go` `EngineCacheDirs["<name>"]` | `TestEngineCacheDirsCoverServiceMap` is **bijective** with `ServiceMap`; `TestEngineCacheDirsMatchComposeMounts` requires the literal `~/citadel-cache/<Dir>:` mount string in the compose |
| 4 | `services/known_hashes.go` | `TestKnownComposeHashesCoverCurrentTemplates` prints the missing sha256; you paste it. **There is no `genhashes` tool** — the file header's `go run ./services/compose/genhashes` refers to a directory that does not exist (the brief repeated this stale claim) |
| 5 | `services/embed_test.go` `composeHostPorts`'s `envVarHostPort` map, and `internal/apps/hostport_collision_test.go`'s equivalent map | the host-port collision guards resolve `${CITADEL_*_HOST_PORT}` tokens through these maps; an unmapped var is silently skipped, so the new port is unguarded |
| 6 | `internal/jobs/model_cache_pull.go` `selfProvisioningEngines["<name>"]` (with a reason string; `TestSelfProvisioningEnginesMatchTheirComposeFiles` checks the compose mounts a cache) | **load-bearing on the deploy path, not hygiene**: aceteam's `model_catalog.py`/`fabric_node_manifest.py` dispatch a `MODEL_CACHE_PULL` for whatever engine the catalog resolved, so a self-provisioning engine missing here logs `unsupported engine` on every deploy (#666) |
| 7 | `internal/engine/registry.go`: BOTH leaf-package mirrors — `loadEstimateByEngine["<name>"]` (60s, the `defaultLoadEstimate` fallthrough) and `selfProvisioningEngines["<name>"]` | `TestRegistryEquivalence` iterates **every** `ServiceMap` engine and checks `LoadEstimate` unconditionally (0 ≠ 60s fails) and `SelfProvisioning` against `jobs.IsSelfProvisioningEngine` |
| 8 | `internal/status/running_services.go` `embeddedServiceType` | optional — the `default: ServiceTypeOther` is already right for a TTS/STT/image sidecar; only a chat engine needs a case |

What is NOT needed (confirmed): `managedProbeEngines`, `idleCapableEngines`,
hotswap's `engineModelEnvVars`/`engineDefaultModel`, `engineReadyPath`,
`gpuBoundJobTypes`, `matchEngines` — those are for `llm_inference`-routed
chat engines. kokoro is in none of them. Heartbeat visibility comes free via
`collectRunningEmbeddedServices` (`internal/status/running_services.go`), which
reports any running `citadel-<name>` container whose name is a `ServiceMap`
key, with `Health=unknown` and the registry port.

### 1.4 Where the repo contradicted the brief (collected)

1. **citadel-cli already carries six wrapper sources + publish workflows** (§1.1).
   "Keeps citadel-cli clean" is true only of kokoro and gliner2.
2. **kokoro's wrapper lives in `citadel-services/services/kokoro/build/`**, with
   `publish-kokoro-image.yml` in that repo — not a bespoke repo. gliner2 is the
   only true bespoke-repo case. This changes the Q2 comparison (§3.2).
3. **`genhashes` does not exist** (§1.3 #4).
4. **`SYNTHESIZE_SPEECH` is routed by `target_node`, not by an `engine:tts`
   tag.** aceteam's `routes/fabric_dispatch.py` says so explicitly ("there is
   no `tts` capability engine to route by"); `internal/capabilities/detector.go`'s
   `matchEngines` only ever emits `engine:` tags for vllm/sglang/ollama/
   llamacpp/lmstudio; the `engine:tts`/`tts:kokoro` tags come from the
   citadel-services catalog module's `service.yaml` `node_tags`, which merge
   into the manifest only on a catalog install, never from the embedded
   compose. The comment in `internal/worker/handler_adapter.go` ("they only
   land on nodes carrying the engine:tts tag") is stale. **Sharper
   consequence:** aceteam's `routes/native_tts.py` `_find_tts_ready_node`
   probes `SERVICE_STATUS` with `_PROBE_SERVICE_NAME = "kokoro"` hardcoded
   before routing — an OmniVoice-only node fails that probe and never receives
   a job. A node-side `backend` field is necessary but **not sufficient** (§5.3).
5. **"gliner2" is the service `extraction`** (`ServiceMap["extraction"]`, image
   `gliner2-service`).
6. **`response_format` default is decided by the Go handler, not the server.**
   The handler always sends an explicit `response_format` (default `opus`;
   aceteam mirrors `_DEFAULT_FORMAT = "opus"`). The brief's "OmniVoice defaults
   to wav" is a server-side default that is never exercised on the job path;
   what matters is that the adapter accepts `opus`/`mp3`/`wav` and transcodes
   (§6.3).
7. **kokoro's contract has `speed`**; the Go handler drops it (§1.2).
8. **The per-service `deploy.resources` GPU block is a real fork in the road,
   not a nit**: kokoro deliberately omits it (CPU-capable, and a compose with
   an nvidia device reservation fails `up` on a daemon without the nvidia
   runtime); diffusers/unlimited-ocr/bonsai include it. §6.2 picks one for
   OmniVoice and says why.

## 2. Goals and non-goals

**Goals**
- G1. One reusable inference-server image/package, so a new model in an
  already-supported modality is onboarded with **config + the citadel-cli
  registration checklist (§1.3) and nothing else** — no Python in citadel-cli.
- G2. Byte-compatible with the contracts the Go handlers already speak
  (§1.2), so the handlers change only to become backend-selectable, never to
  learn a second dialect.
- G3. OmniVoice serves through it end-to-end as the proof consumer.
- G4. Existing bespoke sidecars (kokoro, diffusers, whisper, extraction) can
  migrate onto it later without a contract change (§8) — designed for, not
  built in v1.

**Non-goals (v1)**
- N1. Serving LLM chat. vLLM / llama.cpp / ollama / sglang ARE the generic
  server for that modality; re-implementing `/v1/chat/completions` in Python
  would be strictly worse. Bonsai is a compiled-CUDA-fork *build* problem
  (§8), not a wrapper problem.
- N2. Migrating any existing sidecar. §8 shows the path; nothing moves in v1.
- N3. OmniVoice voice cloning (`ref_audio`/`ref_text`) — needs binary INPUT the
  text-in/audio-out job contract cannot carry (§7).
- N4. A gateway HTTP route for TTS. Not needed for the job path; §10 Q6.
- N5. Per-request metering/billing changes. The receipt header contract is
  preserved as-is (advisory, per `native_tts.py`'s own note that TTS is not
  settled through `settle_inference_job`).

## 3. Design

### 3.1 Q1 — One server, or a base + adapters? **Base runtime + thin per-modality adapters.**

A single server that "spans all modalities" is not one thing; it is five wire
contracts with almost nothing in common at the request/response layer:

| Modality | Request | Response | Readiness semantics today |
|---|---|---|---|
| TTS (`/v1/audio/speech`) | JSON text | raw audio bytes + `X-TTS-*` headers | `/health` body `model_loaded` (kokoro) |
| STT (`/transcribe`) | workspace path | JSON segments | non-200 until loaded (whisper) |
| Image/video (`/generate`, `/generate/video`) | JSON prompt | JSON with base64 media | `/health` = reachable only; lazy load in the POST (diffusers) |
| Extraction (`/extract`-style) | JSON text+schema | JSON | (gliner2) |
| Chat | OpenAI messages | SSE/JSON | `/health` or `/v1/models` — **out of scope, N1** |

What IS common — and is 80% of every `app.py`/`server.py` this org has
written — is the runtime around the model call: env-driven config, HF cache
placement (`HF_HOME`), lazy single-flight model load, a `/health` that reports
`model_loaded`, a `/info` describing the served model, a concurrency semaphore
(`TTS_SLOTS`), input-size caps, device/dtype resolution with CPU fallback,
ffmpeg transcoding, receipt headers, and the disk preflight
(`model_preflight.py` in the diffusers sidecar). Compare kokoro's `server.py`
and diffusers' `app.py`: they re-implement the same scaffolding around
different 30-line model calls.

**Recommendation: a `citadel-inference-server` Python package/image with**
1. a **runtime core** (FastAPI app factory, config loader, HF cache/preflight,
   `/health` + `/info`, semaphore, ffmpeg transcode helpers, receipt headers,
   structured error bodies), and
2. **adapters**, one per *model family*, selected by config, each implementing
   a small interface for its **task**:
   ```python
   class TTSAdapter(Protocol):
       def load(self, cfg: ServerConfig) -> None            # weights -> device
       def voices(self) -> list[str]                        # for /info
       def synthesize(self, req: SpeechRequest) -> np.ndarray, int  # pcm, sample_rate
   ```
   with sibling protocols `STTAdapter`, `ImageAdapter`, `VideoAdapter`,
   `ExtractionAdapter`. The **task** decides which HTTP routes the core
   mounts (`/v1/audio/speech` for `tts`, `/generate`+`/generate/video` for
   `image`/`video`, ...); the **adapter** decides how the model is called.

Why not "one server, no adapters": every model family's Python API is
different (`OmniVoice.generate(text=, instruct=, num_step=)` vs kokoro's
`KPipeline(text, voice=, speed=)` vs `AutoPipelineForText2Image(...)`). Some
glue per family is unavoidable; the design goal is that the glue is (a) tiny,
(b) lives in the server repo, never in citadel-cli, and (c) never touches
HTTP, config, caching, or health — those are the core's job. **"Config-only
onboarding" holds for a new model inside an already-implemented adapter
family** (a different Kokoro voice pack, a different diffusers checkpoint, a
future OmniVoice-v2 checkpoint); a brand-new family costs one adapter file in
the server repo. That is still an order of magnitude less than today's
"new server + Dockerfile + CI + citadel registration," and it is honest.

Why not one image per adapter: one image with all adapters installed makes
the image enormous (diffusers + torch + kokoro + omnivoice + whisper deps
conflict on `transformers` pins — the diffusers `requirements.txt`'s own
comment history shows how brittle those pins are). **Ship one base image
(`citadel-inference-server:base`, runtime core + torch) and one thin layer
per adapter family (`citadel-inference-server:tts-omnivoice`,
`:tts-kokoro`, ...)** built from the same repo by a matrix workflow. Each
citadel compose pins the adapter-family tag. This is the base+adapter split
at the image layer too, and it keeps dependency isolation per family.

### 3.2 Q2 — Where does it live? **A new public repo, `aceteam-ai/citadel-inference-server`.**

Three real options, given §1.4 #2:

| Option | For | Against |
|---|---|---|
| **A. New repo `citadel-inference-server`** (recommended) | Jason's stated preference; the server is a versioned library + image with its own tests, not a catalog entry; adapters and the core version together; one publish workflow with a matrix over adapter families; public like `citadel-cli`/`citadel-services` so nodes pull without auth | one more repo; must add publish plumbing (copy of `publish-kokoro-image.yml`) |
| B. `citadel-services/services/<name>/build/` (the kokoro precedent) | publish workflow already exists there; catalog entry and build sit together | citadel-services is a *catalog of per-service compose files*; a shared runtime used by N catalog entries doesn't belong under one entry's `build/`, and the per-entry copy pattern (gliner2 already exists as both a private repo and a `build/` copy) is exactly the drift `kokoro.yml`'s own comment warns about ("the two copies can drift") |
| C. In-repo generic build context in citadel-cli (`services/compose/generic/`), reused via `ServiceAuxFiles` | no new repo | re-introduces Python into citadel-cli (the thing Jason ruled out), makes every consumer a *build-on-node* service like bonsai (first-start builds of minutes, Ampere-only kernels, `WriteAuxFiles` in both materialization sites), and grows the repo's `//go:embed` surface with model code. Rejected. |

Recommend **A**. Consequence to be explicit about: the six existing in-repo
`services/*-service/` sources should eventually move OUT (into this new repo
as adapters where they are inference — diffusers, whisper — or stay put where
they are not inference at all — claudecode, hermes, meeting, nvr). That is
§8's migration, sequenced after v1.

### 3.3 Q3 — The config contract

Env vars, not a mounted config file. Every existing sidecar is configured by
env (`DIFFUSERS_MODEL`, `WHISPER_MODEL`, `KOKORO_*`), the compose files
forward `${VAR:-default}` from the node's `<name>.env` (the `catalog`
config-persistence path), and citadel's `HostPortEnv()` injection is
env-shaped. A file would be a second mechanism for the same thing.

Prefix `CIS_` (citadel inference server). Core:

| Var | Required | Meaning |
|---|---|---|
| `CIS_TASK` | yes | `tts` \| `stt` \| `image` \| `video` \| `extraction`. Decides which routes mount and which adapter protocol is expected. |
| `CIS_ADAPTER` | yes | adapter family name, e.g. `omnivoice`, `kokoro`, `diffusers`, `faster-whisper`, `gliner2`. |
| `CIS_MODEL` | yes | HF repo id (or local path) handed to the adapter, e.g. `k2-fsa/OmniVoice`. |
| `CIS_MODEL_REVISION` | no | HF revision pin. |
| `CIS_DEVICE` | no (`auto`) | `auto` \| `cpu` \| `cuda` \| `cuda:N`. `auto` = CUDA when visible. |
| `CIS_DTYPE` | no (`auto`) | `float16` \| `bfloat16` \| `float32`; `auto` = fp16 on CUDA, fp32 on CPU (the diffusers/kokoro rule). |
| `PORT` | no (`8080`) | container listen port; the compose owns the host publish (§1.3 #2). Kept as bare `PORT` for parity with every existing sidecar. |
| `HF_HOME` | no (`/root/.cache/huggingface`) | HF cache root, bind-mounted from `~/citadel-cache/huggingface`. |
| `CIS_SLOTS` | no (`2`) | concurrency semaphore (kokoro's `TTS_SLOTS`). Advertised on `/health`/`/info`. |
| `CIS_MAX_INPUT_CHARS` | no (`5000`) | 413 above this (kokoro's `KOKORO_MAX_INPUT_CHARS`). |
| `CIS_PRELOAD` | no (`true`) | load at startup vs lazily on first request. TTS defaults `true` (the handler gates on `model_loaded`); image defaults `false` (the diffusers `/health`-is-reachable contract). |
| `CIS_DISK_PREFLIGHT` | no (`true`) | refuse to start a download that cannot fit (`model_preflight.py`, generalized). |
| `CIS_EXTRA` | no | JSON object of adapter-specific knobs, parsed by the adapter only. |

Task-specific (TTS): `CIS_DEFAULT_VOICE`, `CIS_DEFAULT_FORMAT` (`opus`),
`CIS_OPUS_BITRATE` (`32k`), `CIS_MP3_BITRATE` (`64k`), `CIS_CACHE_DIR`
(`/data/cache`, content-addressed audio cache), `CIS_CACHE_MAX_GB` (`5`).
These are kokoro's knobs with the prefix swapped, so a kokoro adapter is a
rename, not a redesign.

Self-provisioning is the default posture: the adapter downloads `CIS_MODEL`
into `HF_HOME` on load. There is no "off" switch in v1 — a node that wants
pre-fetched weights already has `MODEL_CACHE_PULL`, which writes the same HF
hub layout into the same mounted directory (`internal/jobs.canonicalHFCacheDir`),
so the container's own `from_pretrained` finds them and pulls nothing.

`/info` (new, from kokoro's shape) returns `{task, adapter, model, revision,
device, dtype, model_loaded, capacity:{slots,max_input_chars}, voices?}`.
`/health` returns kokoro's exact shape: `{"status":"up"|"loading",
"model_loaded":bool, "model_version":str, "slots":int}` with HTTP 200 always
— the Go handler's `synthesizeHealthReady` depends on the body, and a 503
during load would trip `isConnectionRefused`-adjacent paths for no gain.

### 3.4 Q4 — citadel-cli's minimal Go surface

**Per new model: the §1.3 checklist and nothing else.** No new handler, no
new job type, no new probe table, no aux files. Confirmed by reading every
kokoro reference in non-test Go (`grep -rln kokoro --include=*.go`): they are
the compose/registry/cache tables, the two hardwired handlers, and doc
comments.

**One handler change, to make TTS engine-selectable** — mirror
`NewLLMInferenceHandler`:

```go
// internal/jobs/synthesize_speech.go
type SynthesizeSpeechHandler struct {
    // BaseURLs maps a TTS backend name to its loopback base URL. Built from
    // the services registry so per-node host-port overrides are honored.
    BaseURLs   map[string]string
    HTTPClient *http.Client
}

func NewSynthesizeSpeechHandler() *SynthesizeSpeechHandler {
    return &SynthesizeSpeechHandler{BaseURLs: map[string]string{
        "kokoro":    fmt.Sprintf("http://localhost:%d", services.TTSHostPort),
        "omnivoice": fmt.Sprintf("http://localhost:%d", services.OmniVoiceHostPort),
    }}
}

const defaultSynthesizeBackend = "kokoro"
```

`Execute` reads `job.Payload["backend"]`, empty ⇒ `defaultSynthesizeBackend`
(**zero change for every existing dispatch**), unknown ⇒ a job FAILURE naming
the allowed set (explicit allowlist, like `selfProvisioningEngines`'s "a typo
must still fail loudly"). The existing `ServiceURL string` field becomes a
test-only override for the resolved backend, or is dropped in favor of
`BaseURLs` injection (the `llm_inference` tests inject the map).

Two passthroughs the same change should close, since the request body is
being touched anyway: forward `speed` (kokoro already accepts it; today it is
silently dropped) and forward `instructions` (§6.3) — both omitted from the
JSON when absent, mirroring `buildMediaGenerateRequest`'s omit-if-empty rule
so kokoro's pydantic defaults apply and a server that doesn't know
`instructions` ignores it. The response envelope gains `"backend"` (additive;
`native_tts.py` reads only `encoding`/`content`/`format`/`receipt`).

Per-backend readiness timeouts: keep one pair (`synthesizeReadyTimeout`
120s) for v1. OmniVoice's checkpoint is ~1.2 GB fp16 (0.6B params) — same
order as kokoro's warm-up budget assumes. Revisit if a heavier TTS family
lands.

**`MEDIA_GENERATE`**: does NOT need the same change in v1 — there is exactly
one image/video backend, and its dialect (`/generate`, JSON base64 out) is
diffusers-specific. When a second image family arrives it should get the
identical `BaseURLs`+`backend` treatment; the generic server's `image` task
mounts the same `/generate` routes precisely so that switch stays a URL
lookup. Documented, not built.

**Routing (cross-repo, hard dependency — §1.4 #4).** citadel-cli alone
cannot make an OmniVoice node receive a job:

| aceteam-side change | Why | Where |
|---|---|---|
| `SYNTHESIZE_SPEECH` payload gains optional `backend` | node reads it; absent ⇒ kokoro | `routes/native_tts.py`, `routes/public_tts.py` request models |
| `_find_tts_ready_node` probes the backend's **service name** (`omnivoice`), not the hardcoded `"kokoro"` | otherwise an OmniVoice node fails the `SERVICE_STATUS` readiness probe and is never selected | `routes/native_tts.py` `_PROBE_SERVICE_NAME` |
| (optional) `model_catalog.json` entry `engine: "omnivoice"` for `/fabric/models` deploys | makes `SERVICE_START`+`MODEL_CACHE_PULL` dispatch by catalog, the same follow-up bonsai and Wan2.1 needed | aceteam `data/model_catalog.json` |

Without the first two, the citadel side is inert exactly the way `vram_mb`
preemption and `FabricNodeID` are inert today — real, tested, unreachable.

### 3.5 Q5 — OmniVoice end-to-end (the proof consumer)

Detailed in §6.

### 3.6 Q6 — Migration path

Detailed in §8.

## 4. The wire contract the `tts` task serves (kokoro-byte-identical)

`POST /v1/audio/speech`, request (OpenAI-shaped; kokoro's `SpeechRequest` +
one field):

| Field | Default | Notes |
|---|---|---|
| `input` | required | text; > `CIS_MAX_INPUT_CHARS` ⇒ 413 |
| `voice` | `CIS_DEFAULT_VOICE` | adapter-interpreted (§6.3) |
| `response_format` | `CIS_DEFAULT_FORMAT` (`opus`) | `opus` \| `mp3` \| `wav`; PCM → ffmpeg for opus/mp3, raw RIFF for wav |
| `speed` | `1.0` | `0.5..2.0`; adapters that cannot honor it ignore it and say so in `/info` |
| `instructions` | absent | free-text voice design. Named after OpenAI's own `instructions` field on `gpt-4o-mini-tts` so the shape stays OpenAI-compatible; kokoro ignores it |
| `language` | absent | BCP-47-ish hint; OmniVoice's `language_id`; kokoro derives from the voice prefix |

Response: 200 with the audio bytes, `Content-Type` per format, and the five
`X-TTS-*` headers exactly as kokoro emits them (`X-TTS-Model-Version` =
`"<adapter>-<pkg-version>+<CIS_MODEL>"`, e.g.
`omnivoice-0.1.0+k2-fsa/OmniVoice`, mirroring
`kokoro-0.9.4+hexgrad/Kokoro-82M`). Errors: JSON `{"error":...}` with 4xx/5xx
— the handler surfaces the body verbatim.

`GET /health`, `GET /info`: §3.3. `/v1/audio/speech/batch` and the cache
routes kokoro has are **not** part of the v1 contract (no Go caller uses
them); the kokoro adapter can keep them when it migrates (§8).

## 5. What citadel-cli must and must not change

### 5.1 Must (v1)
- §1.3 checklist for `omnivoice` (§6.1 gives the values).
- `SynthesizeSpeechHandler` backend selection + `speed`/`instructions`
  passthrough (§3.4). One PR, with tests mirroring
  `TestLLMInferenceHandler_*`'s injected-map pattern: default-backend
  byte-identical request (assert key ABSENCE of `speed`/`instructions`/
  `backend` when unset — the #603 rule), explicit `backend:"omnivoice"` hits
  the second URL, unknown backend fails without an HTTP call.
- Fix the stale `handler_adapter.go` comment about `engine:tts` routing while
  in that file.

### 5.2 Must not
- No `services/omnivoice-service/`, no `build:` in `omnivoice.yml`, no
  `ServiceAuxFiles` entry. `TestKokoroComposeContract`'s "should NOT declare a
  build: section" assertion is the template for `TestOmniVoiceComposeContract`.
- No new job type. `SYNTHESIZE_SPEECH` is the job; `backend` is the selector.
- No gateway route (N4).

### 5.3 Depends on (other repos)
- `aceteam-ai/citadel-inference-server`: core + `tts` task + `omnivoice`
  adapter, image `ghcr.io/aceteam-ai/citadel-inference-server:tts-omnivoice`
  published (§3.2).
- aceteam backend: the two routing changes in §3.4's table. Until they land,
  an OmniVoice node can be exercised only by an explicit `target_node`
  dispatch that includes `backend:"omnivoice"` (the `fabric_dispatch_job` MCP
  tool path, which already accepts arbitrary payload keys).

## 6. OmniVoice onboarding, concretely

### 6.1 citadel-cli registration values

| Item | Value | Pinned by |
|---|---|---|
| `ServiceMap` key / container | `omnivoice` / `citadel-omnivoice` | `TestOmniVoiceComposeRegistered` |
| Host port | **8214** — next free after `UnlimitedOCRHostPort` 8213; nothing in `ReservedCitadelPorts`, `fixedComposeHostPorts`, or the apps range claims it | `OmniVoiceHostPort` const, `TestOmniVoiceHostPortRegistered` |
| Env var | `CITADEL_OMNIVOICE_HOST_PORT` (`EnvOmniVoiceHostPort`); registry key = implementation name, like `kokoro` (the `tts` generic name is already spent on `CITADEL_TTS_HOST_PORT`) | `serviceHostPortEnv`, both collision-test env maps |
| Container port | `8080` (`PORT=8080`, the generic server default) | compose |
| Publish | `127.0.0.1:${CITADEL_OMNIVOICE_HOST_PORT}:8080` — **bare token, no `:?` guard** (the loopback prefix breaks the port parser's colon split; kokoro.yml's comment explains) | `TestOmniVoiceComposeContract` |
| Cache | `EngineCacheDirs["omnivoice"] = {Dir: HFHubCacheDirName, Family: CacheFamilyHFHub}`; compose mounts `~/citadel-cache/huggingface:/root/.cache/huggingface` (shared hub cache; `k2-fsa/OmniVoice` lands as `models--k2-fsa--OmniVoice`) + `~/citadel-cache/omnivoice:/data` for the audio cache (not modeled in the table, same as kokoro's `/data`) | `TestEngineCacheDirsMatchComposeMounts` |
| Self-provisioning | `selfProvisioningEngines["omnivoice"] = "the omnivoice compose pins CIS_MODEL and the generic server downloads it into the shared HuggingFace cache"`, mirrored in `internal/engine/registry.go` | `TestSelfProvisioningEnginesMatchTheirComposeFiles`, `TestRegistryEquivalence` |
| Load estimate | `loadEstimateByEngine["omnivoice"] = 60s` (fallthrough) | `TestRegistryEquivalence` |
| Known hash | paste from `TestKnownComposeHashesCoverCurrentTemplates`'s failure message | that test |
| Service type | `ServiceTypeOther` (default) | — |

### 6.2 `services/compose/omnivoice.yml` (shape)

```yaml
services:
  omnivoice:
    image: ghcr.io/aceteam-ai/citadel-inference-server:tts-omnivoice
    container_name: citadel-omnivoice
    ports:
      - "127.0.0.1:${CITADEL_OMNIVOICE_HOST_PORT}:8080"
    environment:
      - PORT=8080
      - CIS_TASK=tts
      - CIS_ADAPTER=omnivoice
      - CIS_MODEL=${OMNIVOICE_MODEL:-k2-fsa/OmniVoice}
      - CIS_DEVICE=${OMNIVOICE_DEVICE:-auto}
      - CIS_DTYPE=${OMNIVOICE_DTYPE:-float16}
      - CIS_SLOTS=${TTS_SLOTS:-2}
      - CIS_MAX_INPUT_CHARS=${OMNIVOICE_MAX_INPUT_CHARS:-5000}
      - CIS_DEFAULT_VOICE=${OMNIVOICE_DEFAULT_VOICE:-auto}
      - CIS_DEFAULT_FORMAT=${OMNIVOICE_DEFAULT_FORMAT:-opus}
      - CIS_EXTRA=${OMNIVOICE_EXTRA:-{"num_step":32}}
      - HF_TOKEN=${HF_TOKEN:-}
    volumes:
      - ~/citadel-cache/huggingface:/root/.cache/huggingface
      - ~/citadel-cache/omnivoice:/data
    restart: unless-stopped
    deploy:
      resources:
        reservations:
          devices:
            - driver: nvidia
              count: all
              capabilities: [gpu]
```

Loopback-only, like kokoro: no auth of its own, sole consumer is the
co-located worker. **GPU block included** (unlike kokoro, like diffusers):
OmniVoice is a 32-step diffusion-LM; CPU inference is possible but not
real-time, and a service that "starts fine and times out every request" is
worse than one that fails `up` loudly on a GPU-less daemon. The tradeoff is
that this compose cannot come up on the org's M1 node; if a CPU-capable TTS is
wanted there, kokoro remains the answer. The `CIS_EXTRA` default is a
compose-literal JSON; if the `${VAR:-{...}}` braces prove fragile in compose
substitution, fall back to a dedicated `OMNIVOICE_NUM_STEP` var read by the
adapter.

### 6.3 The `omnivoice` adapter's mapping

| Contract field | OmniVoice call |
|---|---|
| `input` | `text=` |
| `voice: "auto"` (default) | no `instruct`, no `ref_audio` — the model's own default speaker |
| `voice: "<anything else>"` | v1: treated as a **preset name** the adapter resolves from a small built-in table of `instruct` strings (e.g. `female-warm`, `male-deep`); unknown ⇒ 400 listing `/info`'s `voices`. Keeps `voice` closed-vocabulary like kokoro's, so the aceteam UI's voice picker stays a list |
| `instructions` | `instruct=` verbatim (voice design: "gender, age, pitch, style, accent, dialect"); overrides the preset |
| `language` | `language_id=` |
| `speed` | `speed=` |
| `CIS_EXTRA.num_step` | `num_step=` (default 32) |
| output | `np.ndarray` @ 24 kHz → wav (raw), or ffmpeg → opus/mp3 |

Receipt: `X-TTS-Chars` = `len(input)`, `X-TTS-Duration-Seconds` from the
sample count / 24000, `X-TTS-Cache-Key` = sha256 over
`(model_version, voice, instructions, language, format, bitrate, speed, text)`
— OmniVoice is non-deterministic per call, so the content-addressed cache is
what makes a repeated request cheap and stable, same as kokoro's design.

**The adapter must never construct the ASR model in v1.** `ref_text`
auto-transcription (Whisper) only fires on the cloning path, which is
deferred (§7); the adapter passes neither `ref_audio` nor `ref_text`, so no
Whisper download occurs and the self-provisioning claim in §6.1 ("downloads
`CIS_MODEL`") stays exact.

### 6.4 Exercising it before the aceteam side lands

1. `citadel run omnivoice` (or `SERVICE_START {"service":"omnivoice"}`), wait
   for `curl localhost:8214/health` → `model_loaded:true`.
2. Dispatch `SYNTHESIZE_SPEECH` with `target_node` + `backend:"omnivoice"` via
   `fabric_dispatch_job`; expect the base64 envelope with `receipt.model_version`
   starting `omnivoice-`.
3. Dispatch the same job WITHOUT `backend`; expect kokoro's `model_version`
   (or the kokoro-unreachable error if kokoro is not running) — proves the
   default is unchanged.

## 7. Deferred: voice cloning (v2)

`ref_audio`/`ref_text` need binary INPUT. `nexus.Job.Payload` is
`map[string]string`; the `SYNTHESIZE_SPEECH` contract is text-in/audio-out.
Two options with repo precedent, either of which is a v2 PR:

- **Inline base64 in the payload** (`ref_audio_b64`, `ref_text`). Simplest;
  a 5–15 s 24 kHz reference clip is ~250–700 KB base64. It rides Redis
  Streams/the API proxy like the response envelope already does in the other
  direction. Needs a size cap and a `ref_audio_sha256` for the cache key.
- **Workspace path**, the `TRANSCRIBE_AUDIO` pattern: `FILE_WRITE_BYTES` puts
  the clip under the node workspace, the compose mounts
  `${CITADEL_WORKSPACE}:/workspace:ro` (transcribe.yml), and the payload
  carries a workspace-relative path. Handles long references and reuse
  across many requests, but couples TTS to workspace-having nodes (the
  handler is deliberately registered unconditionally today for exactly the
  opposite reason — see `handler_adapter.go`).

Both require the adapter to construct the Whisper ASR path (or require
`ref_text`), which changes the self-provisioning reason string and the cache
footprint. Also v2: a `voice:` value that names a stored reference profile on
the node (`/data/voices/<name>.wav`), which would make cloning usable from the
existing closed-vocabulary `voice` field without any payload change.

## 8. Migration path for existing sidecars (non-goal for v1; must not be precluded)

| Service | Path onto the generic server | Contract change? | Purity win |
|---|---|---|---|
| `kokoro` | `tts` task + `kokoro` adapter (`KPipeline`); ports `server.py`'s cache/receipt logic INTO the core, which is where it belongs anyway | none — the contract in §4 IS kokoro's | citadel-services `kokoro/build/` retires; `kokoro.yml` swaps the image tag |
| `diffusers` | `image`/`video` tasks + `diffusers` adapter; core absorbs `model_preflight.py` | none (`/generate`, `/generate/video`, lazy-load `/health` are the task's routes) | **deletes `services/diffusers-service/` and `build-diffusers-service.yml` from citadel-cli** — the largest single purity win available |
| `transcribe` | `stt` task + `faster-whisper` adapter; the `/workspace:ro` mount becomes a task-level option | none | deletes `services/whisper-service/` |
| `extraction` | `extraction` task + `gliner2` adapter | none | retires the private `gliner2-service` repo |
| `bonsai` | **not a generic-server candidate.** It is a compiled CUDA fork of llama.cpp serving a `Q1_0` GGUF; a Python runtime cannot serve it. Its purity fix is orthogonal: publish `citadel-bonsai` as a prebuilt image from citadel-services (a `publish-bonsai-image.yml` with a `CUDA_ARCHITECTURES` build matrix), switch `bonsai.yml` from `build:` to `image:`, and delete `ServiceAuxFiles`/`WriteAuxFiles` — the only build-based service is gone | none | deletes `services/compose/bonsai/Dockerfile` |
| `unlimited-ocr`, `vllm`, `sglang`, `llamacpp`, `tei`, `ollama` | none needed — upstream images already are the generic server for their modality (N1) | — | — |

Nothing in v1 precludes these: each is "swap the `image:` tag, keep the
compose's env names as `${OLD:-default}` → `CIS_*` aliases for one release,
delete the source dir." The core's env schema deliberately keeps `PORT`,
`HF_HOME`, `HF_TOKEN` unprefixed for that reason. The kokoro migration is
the natural second consumer because it also proves the "byte-identical"
claim against a live aceteam caller.

## 9. Phasing, testing, risks

### 9.1 Phases

1. **P0 — `citadel-inference-server` MVP** (new repo): core + `tts` task +
   `omnivoice` adapter; `/health`, `/info`, `/v1/audio/speech`; unit tests
   with a fake adapter (no GPU in CI) + one opt-in GPU smoke test; matrix
   publish workflow producing `:base` and `:tts-omnivoice`. Deliverable: a
   `curl` against the container reproduces kokoro's headers byte-for-byte.
2. **P1 — citadel-cli: OmniVoice registration + backend-selectable
   `SYNTHESIZE_SPEECH`** (this repo, one PR): §5.1. Ships inert for
   platform-routed traffic; verifiable via §6.4.
3. **P2 — aceteam routing** (other repo): §3.4's table. This is what makes P1
   reachable from the product.
4. **P3 — second consumer: kokoro adapter** (server repo + a one-line image
   swap in `kokoro.yml`). Proves migration is a tag swap and retires the
   citadel-services build.
5. **P4 — bonsai prebuilt image** (citadel-services + a `bonsai.yml` swap;
   deletes `ServiceAuxFiles`). Independent of P0–P3.
6. **P5 — diffusers/whisper/extraction adapters**, each deleting an in-repo
   source dir. Independent PRs, lowest urgency.
7. **v2 — cloning** (§7), and a `MEDIA_GENERATE` backend selector when a
   second image family exists.

### 9.2 What the existing tests catch for free (P1)

- Forgetting `EngineCacheDirs`: `TestEngineCacheDirsCoverServiceMap` (bijective).
- A cache mount that doesn't match the table: `TestEngineCacheDirsMatchComposeMounts`.
- Forgetting either `internal/engine/registry.go` mirror: `TestRegistryEquivalence`.
- Forgetting `known_hashes.go`: `TestKnownComposeHashesCoverCurrentTemplates`.
- A port collision with any compose, app range, or reserved listener:
  `TestHostPortNoCollisions` + `TestReservedCitadelPortsPairwiseDistinct` —
  **only if** the new env var is added to both collision-test maps (§1.3 #5);
  that omission is the one silent gap in the checklist, so
  `TestOmniVoiceHostPortRegistered` should also assert `HostPortEnv()` emits
  `CITADEL_OMNIVOICE_HOST_PORT=8214` (the `TestTTSHostPortRegistered` pattern).
- A self-provisioning entry naming a compose with no cache mount:
  `TestSelfProvisioningEnginesMatchTheirComposeFiles`.
- Compose YAML validity: `TestComposeFilesAreValidYAML`.

New tests P1 must add: `TestOmniVoiceComposeRegistered`/`Contract` (no
`build:`, loopback, bare token, prebuilt image), and the handler tests in §5.1.

### 9.3 Risks

| Risk | Mitigation |
|---|---|
| **New HTTP surface = new unaudited code.** The core is loopback-only and unauthenticated, like every sidecar; but it is now ONE codebase whose bug is every consumer's bug. | Keep the core small and dependency-light; no file-path inputs in the `tts` task (the `stt` task's `/workspace:ro` mount is the only path-taking route and stays opt-in); input-size caps default on; the compose never publishes off-loopback. Security review of the core once, not per model. |
| **Dependency pin conflicts across adapters** (the diffusers `requirements.txt` history is a warning). | Per-adapter-family image layers (§3.1); the base pins torch only; each family pins its own `transformers`/`diffusers`. |
| **First-start cold pull** (`k2-fsa/OmniVoice` ~1.2 GB fp16 + tokenizer) on the deploy path. | `SERVICE_START` is in the watchdog's unbounded tier; `CIS_PRELOAD=true` + the `model_loaded` gate keep the handler honest; `MODEL_CACHE_PULL` pre-fetch works unchanged because the hub layout is shared. Disk preflight in the core refuses rather than fills the disk. |
| **OmniVoice weights are CC-BY-NC.** Jason's call to proceed; the risk is a later need to swap the checkpoint. | `CIS_MODEL` is config: a relicensed or fine-tuned checkpoint is a `.env` change. Record the license in `/info` (`model_license` field) so the heartbeat/`citadel services` can surface it later. Tracked caveat, not a gate. |
| **Concurrency/VRAM.** OmniVoice is GPU-class; `gpuBoundJobTypes` deliberately excludes `SYNTHESIZE_SPEECH`, so nothing throttles TTS against chat engines on the same card. | Same posture as kokoro/diffusers today (`CIS_SLOTS` is the only bound). If contention shows up in practice, add `JobTypeSynthesizeSpeech` to `gpuBoundJobTypes` — that file's comment already invites exactly this, explicitly rather than by broadening the predicate. |
| **Metering.** `X-TTS-*` receipts remain advisory and unsigned. | Unchanged from kokoro; the AEP receipt signing path (`internal/aep`) is chat-only today. Out of scope. |
| **The aceteam routing dependency (§3.4) does not land** and P1 ships inert. | Acceptable and precedented (`vram_mb`, `FabricNodeID`); §6.4's `target_node` path keeps it testable. Named as a hard dependency in the P1 PR body. |
| **Non-determinism** makes cache hits rare unless keyed on the full request. | Cache key includes every generation input (§6.3). |

## 10. Open questions for Jason

1. **Repo**: confirm `aceteam-ai/citadel-inference-server` (public) over
   `citadel-services/<x>/build/` (§3.2). Public matters: nodes pull GHCR
   images without credentials today.
2. **`voice` semantics for OmniVoice**: closed preset vocabulary + separate
   `instructions` (recommended, keeps the UI's picker a list), or let `voice`
   carry free-text instruct directly (fewer fields, but `voice` stops being
   enumerable in `/info`)?
3. **GPU block on `omnivoice.yml`** (§6.2): include it (fails `up` on the M1
   node, loud) or omit it like kokoro (comes up everywhere, times out on CPU)?
4. **Should P1 also add `JobTypeSynthesizeSpeech` to `gpuBoundJobTypes`** now
   that a GPU-class TTS exists, or wait for observed contention?
5. **Bonsai**: agree that its fix is a prebuilt image from citadel-services
   (§8), and that this is independent of the generic server? If yes, P4 can
   be dispatched today.
6. **Gateway route**: is a mesh-reachable `/v1/audio/speech` on the gateway
   (routing by `backend`/model the way `chat_route.go` routes chat) wanted at
   all, or is the job path the only TTS entry point for the foreseeable
   future?
7. **Migration priority**: diffusers first (biggest purity win, deletes 500+
   lines of Python from this repo) or kokoro first (proves byte-identity
   against a live caller)?
8. **License surfacing**: is a `model_license` field on `/info` and the
   heartbeat worth wiring now, given the CC-BY-NC decision, or is a doc note
   enough?
