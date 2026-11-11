# Engine, model and module licensing: a design

Status: DESIGN ONLY (2026-10-19). Nothing here is implemented. Owner
decisions on section 3 were recorded on 2026-10-20 (see section 3 and #1377):
the first build slice surfaces licence labels only and enforces nothing. Follows up
`docs/design-engine-adapter.md` section "Prior art: LocalAI", which proposed a
single `EngineSpec.License` field. This doc argues that one field is the wrong
shape and proposes what to build instead.

Legal disclaimer, stated once and meant everywhere below: AceTeam **labels**
licences; it does not interpret them and does not give legal advice. A label of
`commercial_use: allowed` is a transcription of a model card, not a warranty.
Whether an org may use a given output is that org's decision, with its own
counsel. Jason signs off on which licences are carried in the curated catalog;
nobody else in the pipeline is asserting anything legal.

Facts below are marked **[verified]** (read from code at the cited line, or from
the Hugging Face API on 2026-10-19) or **[inferred]**.

## 1. Inventory: where licence information lives today

Repo heads read: citadel-cli `a4c91052`, citadel-services `9ddb090`,
citadel-inference-server `faf0e19`, aceteam `781cc5833` (all `origin/main`).

| # | Surface | Where | Shape and vocabulary | Label or gate |
|---|---------|-------|----------------------|---------------|
| 1 | Module manifest `license:` | citadel-services `services/*/service.yaml` (e.g. `voice-clone/service.yaml:31`, `vllm/service.yaml:6`); parsed into `ServiceManifest.License` at citadel-cli `internal/catalog/catalog.go:73`; printed by `citadel catalog info` (`cmd/catalog.go:316`) | Free text: `MIT`, `Apache-2.0`. Describes the **module code/image only**. voice-clone says `MIT` while serving CC-BY-NC weights; paw-compile says `MIT` while its runtime-downloaded weights have **no licence at all** (`paw-compile/service.yaml:33-40`) | Label (display only). Not in the lockfile (`internal/catalog/lockfile.go:19` `LockEntry` has no licence), not in `nodestate`/reconcile reports |
| 2 | voice-clone engine metadata | citadel-services `services/voice-clone/build/engines.py:68-72` (`weights_license`, `code_license`, `commercial_use`, `watermark`); Chatterbox `:135-141` MIT/MIT/true; OmniVoice `:189-200` Apache-2.0 code, `CC-BY-NC` weights, false; MMS_FA aligner `:290-299` `CC-BY-NC`, false | Separate code vs weights fields already exist here, the only place in the ecosystem | **Gate**: `server.py:97-128` refuses OmniVoice with 403 unless `OMNIVOICE_ACCEPT_NONCOMMERCIAL_LICENSE=true` (node-owner env, read once at startup). Aligner is label only (`server.py:491-499`, "disclose, never refuse") |
| 3 | voice-clone receipt + headers | `server.py:463-500` receipt `{model_license, commercial_use, watermark, consent, aligner_model, aligner_license}`; headers `X-TTS-Model-License`, `X-TTS-Aligner-License`, `X-TTS-Receipt` (README `:164-173`) | `commercial_use = engine AND aligner` (`server.py:499`); undeclared aligner licence treated as non-commercial | Label |
| 4 | CIS `/info.model_license` | citadel-inference-server `cis/app.py:79`; adapter contract `cis/adapters/base.py:48` ("the served CHECKPOINT's license as the model card states it"); `cis/adapters/omnivoice.py:77-83` hardcodes `k2-fsa/OmniVoice -> "CC-BY-NC"`, any other model `"unknown"`, `CIS_EXTRA.model_license` overrides | Literal card prose; HF API has **no** `license` tag for OmniVoice at all [verified], so the string is hand-transcribed | Label |
| 5 | citadel-cli SYNTHESIZE_SPEECH | `internal/jobs/synthesize_speech.go:562-636` copies headers into the job `receipt`; `:524-555` best-effort `GET /info` after a synthesis, stored via `status.RecordModelLicense` | Pass-through strings; `:631-634` defaults `commercial_use=false` when an aligner licence is present and the flag is missing (good: never permissive by omission) | Label |
| 6 | Heartbeat `ServiceInfo.model_license` | `internal/status/types.go:664`; attached by `applyModelLicenseSignal` `collector.go:257-264`; store `model_license.go:18-73` | One string **per engine**, process-local, unpersisted, populated only after a TTS dispatch. Nothing on the platform reads it (aceteam#11455 section 5.1 confirms) | Label |
| 7 | Diffusers sidecar catalog | citadel-cli `services/diffusers-service/image_models.py:126-222` `ImageModelSpec.license` + `permissive`; `/health.allowed_models[].license/permissive_license` (`image_swap.py:273-274`) | `apache-2.0` (FLUX.2-klein-4B, Z-Image-Turbo, Qwen-Image), `sai-nc-community (Stability AI Community License)` for sdxl-turbo, `unverified (operator-supplied DIFFUSERS_MODEL)` for custom | **Soft gate**: `DEFAULT_ALLOWLIST` `:233-237` excludes sdxl-turbo unless the operator names it. The licence is **not** carried to the MEDIA_GENERATE result (`internal/jobs/media_generate.go:301-345` receipt has task/size/seed only) |
| 8 | Platform model catalogs | `python-backend/data/model_catalog.json` (47 entries: `Apache 2.0`, `MIT`, `Llama 3.1 Community License`, `Qwen License`, `LFM Open License v1.0`, `Gemma License`, `DeepSeek License`, `Stability AI Community License`); DB `fabric_catalog_models.license TEXT NOT NULL DEFAULT 'unknown'` (`supabase/migrations/20260701_fabric_catalog.sql:76`); HF ingest `models/hf_model_metadata.py:109,142` copies `cardData.license` else `"unknown"` | Free text, inconsistent casing (`Apache 2.0` vs `apache-2.0` vs `Apache-2.0`); community licences named, terms not encoded | Label: `fabric/models` page (`app/(protected)/fabric/models/[modelId]/page.tsx:341`), library card/table, `DatacenterModels.tsx:67` licence filter, MCP `fabric_catalog_models` (`aceteam_mcp_fabric_catalog.py:145`). The only **gate** is HF `gated` (`fabric_provision.py:871-884`), which is about download tokens, not licence terms |
| 9 | Platform TTS receipt | `routes/native_tts.py:376-385` optional fields; `workflows/synthesize_speech.py:107-114` always-present keys, `:762-800` `_merge_license_fields` (first `commercial_use=False` wins); `utils/voice_license.py:41-57` and `utils/voiceLicense.ts:62-82` (`/\bNC\b/i` regex) | "most restrictive" = any non-commercial wins, else first. Unknown is **not** NC (`voiceLicense.ts:62`) | Label, explicitly: `EnrollVoiceDialog.tsx:477` "This is a label only and does not block anything"; Jason's 2026-10-09 decision in aceteam#11455/#11459 |
| 10 | Drive / pages | `document_info.metadata.license = {model_license, commercial_use, engine, source}` copied at attach/upload (aceteam#11459); `FileRow.tsx:238` chip; hosted-page footer notice (`utils/hostedPage.ts:852`) | Same subset as #9; aligner fields are dropped at this boundary | Label |
| 11 | Signed receipts | `internal/aep/receipt_v2.go:54-83` canonical fields: version, node, job, time, engine, model, input/output/policy hashes, action, verdict hash, grounding; work receipts `:376`; app-deploy `:258` | **No licence field anywhere**. `policy_hash` is `EmptyPolicyHash` | n/a |
| 12 | Usage records | `internal/usage/record.go:6-60` (`Backend`, `Model`, units, receipt binding) | No licence | n/a |
| 13 | Desktop bootstrap | `internal/desktopmodel/bootstrap_manifest.go:113` `License: "descriptor_metadata_only_not_reviewed"` | An honest evidence-limit sentinel, not a licence | Label |
| 14 | LocalAI (reference only) | citadel-cli#1360: per-backend licence in `backend/index.yaml`; mixes MIT, BSD-4 (whisperx), non-commercial (insightface), custom (liquid-audio) | Free text per backend | Label |

Model-card spot check [verified 2026-10-19 via `huggingface.co/api/models`]:
Qwen/Qwen-Image `apache-2.0` (an older memory note calling its weights
non-commercial is superseded by the card); FLUX.2-klein-4B `apache-2.0`;
sdxl-turbo `other` + `license_name: sai-nc-community`; k2-fsa/OmniVoice **no
licence tag** (prose only); ResembleAI/chatterbox `mit`; hexgrad/Kokoro-82M
`apache-2.0`; prism-ml/Bonsai-27B-gguf `apache-2.0`;
Systran/faster-whisper-large-v3 `mit`; meta-llama/Llama-3.1-8B-Instruct
`llama3.1`, `gated: manual`.

### Inconsistencies found

1. **Four vocabularies for one idea.** `Apache 2.0` (platform JSON), `apache-2.0`
   (HF, diffusers), `Apache-2.0` (service.yaml, voice-clone), `MIT`/`mit`. The UI
   licence filter (`DatacenterModels.tsx:138`) treats these as distinct values.
2. **`license` means different things per surface.** service.yaml = module code.
   CIS `/info` = checkpoint. voice-clone receipt = engine weights, with the
   aligner in a second field. The platform's `document_info.metadata.license` =
   whichever string survived the merge. A reader cannot tell which component a
   string describes.
3. **`unknown` is spelled three ways and means two things.** `"unknown"` (CIS,
   DB default, HF ingest), `"unverified (operator-supplied ...)"` (diffusers),
   `"descriptor_metadata_only_not_reviewed"` (desktop). Sometimes it means "we
   did not look", sometimes "we looked and the source declares nothing" (OmniVoice,
   paw-compile weights). Those are different facts.
4. **Non-commercial detection is a regex on a free string** (`\bNC\b`). It
   matches `CC-BY-NC` and misses `sai-nc-community`, `Llama 3.1 Community
   License`, `Qwen License` and every custom licence with conditions. Those read
   as plain chips today.
5. **The heartbeat signal is per engine, not per model**, and only TTS engines
   feed it. vllm/ollama/llamacpp serve many models (`DiscoverLocalEngines`,
   `internal/status/local_engines.go:42`) and report no licence at all.
6. **Diffusers knows the licence and drops it.** The sidecar catalog is the
   best-curated licence source in the stack (verification record at
   `image_models.py:13-26`), but neither the image response nor
   `media_generate.go` carries it, so image outputs never get a chip.
7. **One real gate, one soft gate, everything else a label.** OmniVoice (403,
   node-owner env), diffusers allowlist (operator env). Neither records who
   accepted or when, and neither is visible to the platform or the org.

## 2. Design

### 2a. What is licensed: components, not a string

A single string cannot express the cases already in production:

- voice-clone `word_timestamps`: Chatterbox MIT code and MIT weights, MMS_FA
  aligner CC-BY-NC. Audio is commercial-safe; the timestamped bundle is not.
  The module already needed two fields and a derived flag (`server.py:491-499`).
- OmniVoice: Apache-2.0 code, CC-BY-NC weights with no machine-readable tag.
- sdxl-turbo: `other` on HF; the actual terms are registration, attribution
  ("Powered by Stability AI") and a USD 1M revenue cap. That is "conditional",
  not "forbidden", and not expressible as a boolean.
- Llama 3.x: `llama3.1` on HF, a 700M-MAU threshold, an acceptable-use policy,
  "Built with Llama" attribution, and `gated: manual`.
- paw-compile: MIT server, weights with no declared licence. The manifest says
  MIT and that is true of the code.

Recommended shape (Go names illustrative; YAML/JSON keys are the contract):

```yaml
license:
  code:    { spdx: Apache-2.0 }                        # runtime/image/module source
  weights: { spdx: LicenseRef-CC-BY-NC-unversioned,    # the checkpoint actually served
             source: model_card, ref: k2-fsa/OmniVoice@<revision> }
  components:                                          # auxiliary models, zero or more
    - { role: aligner, model: torchaudio.pipelines.MMS_FA, spdx: CC-BY-NC-4.0 }
    - { role: vae, ... }   # tokenizer, text_encoder, safety_checker, lora, embeddings
  output_terms: { }        # optional: output-specific clauses (Stability's, Llama's naming rule)
  flags:                   # DERIVED, never hand-written (section 2b)
    commercial_use: forbidden
    attribution_required: true
    share_alike: false
    research_only: false
    acceptable_use_policy: false
    thresholds: []
    confidence: declared   # declared | card | unverified
```

`code` and `weights` are both required on every engine/model entry; a missing
`weights` block is an error in the catalog test (section 2i), never a silent
`unknown`. `components` is where the aligner, a LoRA, the safety checker and a
VAE go. `output_terms` is a place for clauses that attach to generated content
rather than to the artifact (Stability's attribution, Llama's "Built with"
naming); it is advisory text plus flags, not a parser.

### 2b. Vocabulary

- Identifiers: SPDX ids where one exists (`Apache-2.0`, `MIT`, `CC-BY-NC-4.0`,
  `BSD-4-Clause`). Custom terms get a `LicenseRef-` id we own and pin to a URL:
  `LicenseRef-Llama-3.1-Community`, `LicenseRef-SAI-Community-1.0`,
  `LicenseRef-LFM-Open-1.0`, `LicenseRef-Qwen`, `LicenseRef-Gemma`,
  `LicenseRef-DeepSeek`, `LicenseRef-CC-BY-NC-unversioned` (OmniVoice: the card
  says CC-BY-NC with no version; do not invent `4.0`, which is exactly the rule
  CIS already follows at `omnivoice.py:77-80`). `LicenseRef-None-Declared`
  for paw-compile's weights. `unknown` is reserved for "nobody has looked".
- A small, versioned **licence table** (one YAML file, owned in citadel-cli,
  mirrored to the platform) maps each id to its derived flags. Flags are
  computed from the table, never typed per model, so a flag change is one
  diff with one review.
- `commercial_use` is a four-valued enum: `allowed | conditional | forbidden |
  unknown`. The existing boolean is kept as a projection (section 2h).
- **Unknown is never permissive.** Rendering: `unknown` and `LicenseRef-None-
  Declared` show an "unverified" chip; any gate that ever exists treats them
  like `forbidden`. This is the same posture the voice-clone receipt took for an
  undeclared aligner (`server.py:494-496`) and `synthesize_speech.go:631`.
- Normalization at every ingest boundary: `Apache 2.0`, `apache-2.0`,
  `Apache-2.0` all map to `Apache-2.0`; HF `other` + `license_name` maps through
  a small alias list (`sai-nc-community -> LicenseRef-SAI-Community-1.0`), and
  an unmapped `other` is `unknown`, not the raw string.

### 2c. Granularity and authority

**Licence attaches to the model actually served, resolved at request time.**
Engines are containers for many models (vllm/ollama/llamacpp/diffusers); an
engine-level licence is only correct for single-model engines (kokoro, bonsai).
So:

| Component | Authority | Why |
|-----------|-----------|-----|
| Module/engine **code** | citadel-services `service.yaml` `license.code`; embedded engines: a new `services/licenses.go` table keyed like `EngineCacheDirs` and pinned by a test | The module author knows the image's licence; it changes with the image tag |
| **Weights** of a managed, catalog-deployed model | aceteam `fabric_catalog_models` row, pinned to `source_ref@revision` | Curated once, reviewed by Jason, versioned with the row |
| Weights of an operator-chosen model (`DIFFUSERS_MODEL`, `LLAMACPP_MODEL`, a bare `ollama pull`) | The serving sidecar's own report (`/info`, diffusers `/health`), else `unknown` | The node is the only party that knows what is loaded |
| Auxiliary components | The module that bundles them (voice-clone's aligner) | Only the module knows which aligner/VAE it loaded |
| HF model card | **Evidence, not authority.** Recorded with the pull as `card_license@revision` | Cards change; OmniVoice has no tag; `other` is not a licence |

Consistency: `MODEL_CACHE_PULL` (`internal/jobs/model_cache_pull.go`) records
`{repo, revision, card_license, pulled_at}` next to the cache (the P2 durable
cache index in `docs/design-cache-ownership.md` is the natural home). On a
re-pull or a heartbeat probe, a card licence that differs from the catalog's
pinned value is reported as `license_drift` on the heartbeat and in `citadel
doctor`: a warning, never a stop, because the card is evidence. A catalog entry
is pinned to a revision/digest; bumping the revision re-requires a licence
review (the catalog test fails until the row is touched).

### 2d. Composition

A job's effective licence is the fold of its components. Rules, per flag:

- `commercial_use`: `forbidden > unknown > conditional > allowed`. Any
  forbidden wins; otherwise any unknown yields unknown (not permissive);
  otherwise conditional with the **union** of conditions (attribution lists
  concatenate, the strictest threshold applies); otherwise allowed. This
  generalizes today's "first `commercial_use=False` wins"
  (`synthesize_speech.py:773`) and fixes the gap where a conditional component
  reads as plain.
- Boolean obligations (`attribution_required`, `share_alike`,
  `acceptable_use_policy`): OR.
- `research_only`: OR, and it forces `commercial_use: forbidden`.
- Provenance is kept: the merged object carries `components[]` with each
  input's id and role, so a chip can say "non-commercial because of the
  aligner" instead of only "non-commercial". The platform's multi-paragraph
  merge and the Film node's audio merge are instances of the same fold.
- The fold is a pure function with one implementation per language
  (`internal/license` in Go, `utils/license.py` + `utils/license.ts` on the
  platform) and a shared JSON table test so the three cannot drift.

### 2e. Surfaces

| Surface | Today | Proposed | Notes |
|---------|-------|----------|-------|
| Heartbeat | `ServiceInfo.model_license` string per engine, TTS only | `ServiceInfo.license` (the 2a object, engine code + default weights) and `ServiceInfo.ModelLicenses map[model]LicenseSet` for multi-model engines; keep `model_license` as a projection of the default model's weights id | Fed from the per-engine code table, the sidecar's report and the catalog pin; `license_drift` flag |
| Job output | TTS receipt has it; MEDIA_GENERATE, llm_inference, TRANSCRIBE have nothing | Every content-producing handler attaches `license` (merged object) to its result next to `receipt`; TTS keeps the flat legacy keys too | Diffusers: read `/health.allowed_models[model].license`; llm_inference: catalog pin by model name, else `unknown` |
| **Signed receipts** | none | **Not in the v2 canonical bytes.** Attach the merged object as an unsigned sibling `license` next to `aep_receipt`/`trust_verdict`, plus a `license_hash` (sha256 of the canonical object) in the same unsigned map. Fold `license_hash` into the signed canon only at the v3 JCS cutover (CLAUDE.md "v3 ... is NOT emitted"), together with the aceteam#9287 verifier and goldens | Changing v2's fifteen fields breaks the deployed verifier and `internal/aep/testdata/v2`; v3 is already the planned coordinated flip, so licence rides it rather than forcing a v2.1 |
| Usage records | none | `LicenseID` (weights id) + `CommercialUse` enum on `UsageRecord`, schema v3 `ADD COLUMN`; stream payload bumps | Lets earnings/marketplace reporting (2g) filter by licence without re-deriving |
| UI chips | voice-only, regex-driven | One `LicenseChip` reading the object: plain / "conditional" (amber) / "non-commercial" (warning) / "unverified" (muted). Hover shows components | Keep `VoiceLicenseChip` as a thin wrapper |
| MCP | `fabric_catalog_models` prints a licence column; `local_list_models` none | Both return the object; `fabric_catalog_models` adds `commercial_use` so an agent can filter | |
| `citadel status` / `doctor` | none | `status`: licence column per service/model. `doctor`: warn on `unknown` weights for a running engine, on `license_drift`, and on a non-commercial model running on a marketplace-shared node (2g) | Warn, never exit non-zero, matching the current doctor contract for advisory signals |

### 2f. Policy: label vs gate

Default posture stays **label only**, per Jason's 2026-10-09 decision
(aceteam#11455/#11459) and the consent dialog's own wording. The design adds
the data so a gate *could* exist, and defines where one would sit, without
turning any on.

Three layers, each with a different actor:

1. **Node-owner acceptance** (exists: `OMNIVOICE_ACCEPT_NONCOMMERCIAL_LICENSE`).
   Generalize to a per-node file, `<node config dir>/license-acceptances.yaml`,
   entries `{license_id, accepted_by, accepted_at, scope: node}`, written by
   `citadel license accept <id>` and by `APPLY_DEVICE_CONFIG`. The env var stays
   as a compatibility input that materializes an entry. A module declares
   `requires_acceptance: [LicenseRef-...]` in `service.yaml`; the node refuses to
   start (or the sidecar keeps its 403) until the id is accepted. This is the
   only hard gate the node owner has to opt into, because the node owner is the
   party that downloads and runs the weights.
2. **Org policy** (new, platform): `org.license_policy = {noncommercial:
   label|warn|block, unknown: label|warn|block, conditional: label|warn}`
   default `label`. Evaluated at dispatch (`fabric_deploy_model`, SynthesizeSpeech
   and MediaGenerate nodes) using the merged object. `warn` surfaces in the run
   record; `block` fails the node with the component that caused it.
3. **Per-publish** was explicitly dropped (C2 in aceteam#11459). Not revived.

Acceptance is recorded, not just set: who, when, which id, which scope. Per-org
acceptance is a separate record from per-node acceptance; a node owner
accepting CC-BY-NC for their own box does not accept it on behalf of a
consuming org (2g).

**Consent is orthogonal.** A voice consent record (speaker, attester, time) is
about the reference clip's subject; the licence is about the weights. Both ride
the receipt, neither derives from the other, and the merge never touches
consent. Keep `voice_manage.go`'s "the module is the authority on consent" as is.

Org-level repo licensing (ELv2 on the product repos, per the 2026-07-29 audit)
is a different question from model licensing and is not changed by this design;
it only means the licence *table* file itself ships under ELv2 like the rest of
citadel-cli.

### 2g. Marketplace and fabric sharing

[inferred; needs counsel] Serving CC-BY-NC weights to a paying customer on a
shared or marketplace node is the hardest case: the node owner's acceptance
covers *their* use, but earning from inference on those weights is plausibly
commercial use by the node owner, and the consumer's use is the consumer's.
Recommended posture:

- Marketplace listing excludes models whose weights are `forbidden` or
  `unknown` by default; a node owner can opt a model in only by accepting an
  explicit "I am responsible for this listing" record (scope: marketplace).
  This is the one place a default **exclusion** is proposed, because the
  alternative is the platform collecting fees on non-commercial weights.
- Cross-org sharing (`fabric_share_node`): the consuming org sees the licence
  object on every served model and in every receipt; the org's own policy
  (2f.2) applies to its dispatches. No acceptance transfers across orgs.
- Earnings reports carry the licence id per job (2e usage records) so a
  payout can be reviewed by licence class.

### 2h. Backward compatibility and migration

Existing keys are kept as projections of the object: `model_license` (weights
id), `commercial_use` (bool: `allowed` only; `conditional`, `forbidden` and
`unknown` all project to `false`, which is strictly more conservative than
today), `aligner_license`/`aligner_model` (the `aligner` component). CIS
`/info.model_license` stays a string. The heartbeat field stays. Old nodes
simply omit the new object and the platform's "absent means None" rule holds.

| Phase | Scope | Gate to start |
|-------|-------|---------------|
| P0 | Licence table + normalizer + fold, in all three languages; normalize `fabric_catalog_models.license` and `model_catalog.json` (data migration, Jason sign-off); every citadel-services `service.yaml` and every `ServiceMap` engine gets a `license.{code,weights}` block; catalog tests fail on a missing block | Jason approves the table and the `LicenseRef-` list |
| P1 | Per-model resolution: diffusers licence into `MEDIA_GENERATE` output, catalog pin into `llm_inference` output, `ModelLicenses` on the heartbeat, `license_drift` from the pull record, chips read the object | P0 merged |
| P2 | Receipts and usage: unsigned `license` + `license_hash` beside `aep_receipt`; `UsageRecord` columns; `fabric_run_work` ingest | P1; aceteam#9287 verifier told to ignore the sibling |
| P3 | Policy: node acceptance file + `citadel license accept`; org policy enum; marketplace default exclusion; signed `license_hash` rides the v3 JCS cutover | Section 3 decided 2026-10-20; the enforcement parts (org `block`, marketplace exclusion) wait for a new decision under the surface-only override |

### 2i. Testing

- `TestLicenseTableComplete`: every `LicenseRef-`/SPDX id used anywhere in
  `services/`, `citadel-services`, the diffusers catalog and the platform
  catalogs resolves in the table; the table has no id with all-unknown flags.
- `TestEveryEngineDeclaresLicense`: each `ServiceMap` entry and each
  `service.yaml` has `license.code` and `license.weights`; `unknown` is allowed
  only with a `reason` string (paw-compile: `none_declared_upstream`).
- Fold table tests shared as JSON across Go/Python/TS: the voice-clone aligner
  case, sdxl-turbo conditional, Llama threshold union, unknown dominance.
- Golden job outputs for TTS (legacy keys unchanged byte-for-byte when the
  object is absent), MEDIA_GENERATE, llm_inference.
- Receipt goldens: v2 canonical bytes **unchanged** by the sibling `license`
  object (a test that marshals with and without it and compares the signed
  digest); a v3 golden with `license_hash` added when that cutover lands.
- Drift test: catalog revision bump without a licence review fails.

### 2j. Non-goals

Legal interpretation of any licence; automated reading of licence text; a
content filter; DRM; retroactive relabeling of already-published artifacts
(they keep whatever label they carried, per aceteam#11459's "no network read at
render time"). The product labels and records; Jason, with counsel, decides what
any label means for the business.

## 3. Owner decisions (2026-10-20)

Jason accepted all eight recommendations, with one override: **surface only,
no enforcement for now.** Nothing refuses, gates or excludes until a later
decision. The held build slice is #1377.

1. **Hard-block at the org layer.** The org policy enum defaults to `label`.
   `block` is designed (2f) but not built in the first slice.
2. **Unknown weights on a running engine.** Labelled "unverified" only.
3. **Marketplace default exclusion** of `forbidden`/`unknown` weights (2g).
   Accepted in principle, but **not built now**, because it is enforcement.
4. **Licence in receipts.** An unsigned licence sibling now; a signed
   `license_hash` at the v3 JCS cutover.
5. **Acceptance UX.** Per-node CLI plus device config first; a per-org
   acceptance screen later. A node owner's acceptance never satisfies a
   consuming org's policy.
6. **Catalog licence pins.** A `license-review` label on the catalog PR plus a
   named second reviewer (Jason by default).
7. **paw-compile weights.** Keep shipping with `LicenseRef-None-Declared` and
   an "unverified" chip.
8. **Data migration** of `fabric_catalog_models.license` and
   `model_catalog.json` to normalized ids. Approved in principle; the exact SQL
   needs Jason's sign-off before it runs (aceteam side).
