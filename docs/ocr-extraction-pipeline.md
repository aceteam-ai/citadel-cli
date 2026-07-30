# Sovereign Document-Extraction Pipeline (Unlimited-OCR)

How to turn the `unlimited-ocr` engine (PR #623) into a document
**routing + extraction** pipeline that runs on Citadel fabric, plus the
persistence schema for what to store. Companion to `services/compose/unlimited-ocr.yml`.

## Pipeline shape

```
document image ──▶ Unlimited-OCR (fabric)  ──▶ text + <|det|> regions + <table> HTML
                          │
                          ├──▶ embedding (fabric)  ──▶ vector  ──▶ ROUTE (classify doc_type)
                          │
                          └──▶ schema-constrained extraction (fabric) ──▶ fields + provenance ──▶ DB
```

This mirrors the prior-company (adanomad) extraction stack: **AdaExtract2**
(GLiNER2 entity/relation extraction, which is citadel's own `extraction`/gliner2
service), the **embedding** repo (image → 1024-dim vector → ANN for routing), and
**AdaIE/ie-datasets** (schema-constrained IE with typed, normalized output).

## OCR output grammar

Unlimited-OCR emits an ordered region sequence:

```
<|det|>TYPE [x1, y1, x2, y2]<|/det|>CONTENT
```

- `TYPE` ∈ `title | text | table | …` (treat as an open set — only `title/text/table`
  observed so far; do not add a DB CHECK constraint).
- bbox is pixel coords `[x1,y1,x2,y2]`.
- `CONTENT` is plain text, or reconstructed **HTML `<table>`** markup for tables.

Request contract (Baidu recipe): prompt `"<image>document parsing."` + a base64
`image_url`, with `skip_special_tokens:false` and
`vllm_xargs:{ngram_size:35,window_size:128}`. Known quirk: `ngram_size:35`
occasionally eats a repeated digit run (an invoice id `#2026-0729` OCR'd as
`#2026-07`) — relevant for serial numbers / invoice ids.

## Persistence schema

See `docs/ocr-extraction-schema.sql`. Tables: `documents` (+ routing `doc_type`),
`pages`, `blocks` (one row per OCR region: type + bbox + text/html), `doc_embeddings`
(`vector(1024)` + HNSW, the routing signal), `extraction_schemas` / `extractions`
/ `extraction_provenance` (field → source-region citations, the AdaExtract2 pattern).
pgvector is greenfield wherever this lands (pronghorn has zero vector infra; AceTeam
uses Supabase/pgvector).

## AceTeam Flow (runs on fabric)

A flow exists — **"Sovereign Document Extraction (OCR → Extract → Embed on Citadel
Fabric)"** (flow id `0d065a4a-c08f-4caa-be92-db063ccda8ea`) — built from
`APICall` (OCR + embed + extract, all hitting the fabric inference gateway) +
`Jinja`/`ExpandJSON` glue. Design decisions:

- All three model steps are `APICall` to the inference gateway (not `AgentMessage`),
  because no AceTeam agent is bound to a fabric model — `APICall` is what makes a
  node "run on fabric" with no invented `agent_id`.
- `api_base`, `ocr_model`, `extract_model`, `embed_model` are **flow inputs**
  (defaults: `https://api.aceteam.ai`, `baidu/Unlimited-OCR`, `bonsai-27b`,
  `baai/bge-small-en-v1.5`). Auth is a **runtime** input (`api_token`), never
  stored in the graph.
- `APICall.response` is delivered as the raw response **string** — parse it
  (`ExpandJSON` / a JSON step), don't feed it to a mapping-typed param directly.

### ⚠️ Known blocker: internal vs public gateway URL

Running the flow with `api_base=https://api.aceteam.ai` returns **HTTP 403 + a
Cloudflare bot-challenge** — the AceTeam flow backend's egress is challenged on the
public domain (a browser/trusted-IP client gets 200). `api.aceteam.ai/v1` is
*served by* the backend (`routes/gateway.py`) and fabric routing happens over an
**internal** Redis-dispatch / SOCKS5 path (`routes/inference.py`,
`routes/fabric_relay.py`). So the flow's `api_base` must be the backend's
**internal** inference URL, not the public one. This is a one-parameter fix once
that internal host is known.

Deeper fix (product gap): there is no first-class **Fabric Inference** flow node —
`APICall`-to-a-URL is a workaround that also re-exposes the auth/URL plumbing. A
native node that routes to fabric over the internal path (the same one chat uses)
would remove the Cloudflare/auth/base-url footguns entirely.

## Live status

- `unlimited-ocr` engine e2e-proven on an RTX 3090 (direct OCR + gotenberg→PDF→OCR).
- Gateway-routed OCR (`POST api.aceteam.ai/v1 {"model":"baidu/Unlimited-OCR"}`)
  lights up once #623 ships (release + node auto-update) AND the model is deployed
  on a fabric node — `managedProbeEngines` (this PR) is what makes the gateway
  resolve the model to the node.
