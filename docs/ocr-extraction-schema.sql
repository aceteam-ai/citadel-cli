-- Proposed persistence schema for the OCR → route → extract pipeline.
-- Postgres/pgvector flavor (AceTeam + pronghorn are Supabase/Postgres).
-- adanomad `embedding` used ClickHouse for image-vector ANN; pgvector is the
-- AceTeam-native equivalent. Reconcile table/column names with pronghorn's
-- existing schema (docs/database-schema.md) before applying.

-- 1. One row per ingested document.
create table documents (
  id            uuid primary key default gen_random_uuid(),
  source_uri    text,                 -- where the original came from
  sha256        text unique,          -- dedupe identical uploads
  mime          text,                 -- application/pdf, image/png, ...
  page_count    int,
  node_id       text,                 -- which fabric node did the OCR (sovereignty)
  ocr_model     text default 'baidu/Unlimited-OCR',
  doc_type      text,                 -- ROUTING result: invoice | contract | report | ...
  routing_score real,                 -- confidence of the routing decision
  created_at    timestamptz default now()
);

-- 2. One row per page (Unlimited-OCR is single-image; PDFs rasterize per page).
create table pages (
  id           uuid primary key default gen_random_uuid(),
  document_id  uuid references documents(id) on delete cascade,
  page_no      int,
  width_px     int,
  height_px    int,
  image_uri    text,                  -- rasterized page image (object store)
  unique (document_id, page_no)
);

-- 3. The heart of it: one row per OCR-detected REGION.
--    Mirrors the model's output grammar:  <|det|>TYPE [x1,y1,x2,y2]<|/det|>CONTENT
create table blocks (
  id           uuid primary key default gen_random_uuid(),
  page_id      uuid references pages(id) on delete cascade,
  seq          int,                   -- reading order within the page
  block_type   text,                  -- title | text | table | list | figure | formula | ...
  bbox         int[4],                -- [x1, y1, x2, y2] pixel coords (as emitted)
  text         text,                  -- plain text content
  html         text,                  -- table/list markup when block_type='table' (model emits <table>)
  char_len     int
);
create index on blocks (page_id, seq);

-- 4. Embeddings for ROUTING + similarity (the "embedding node").
--    Doc-level for routing/classification; block-level optional for fine retrieval.
create table doc_embeddings (
  document_id  uuid references documents(id) on delete cascade,
  model        text,                  -- image or text embedding model
  embedding    vector(1024),          -- adanomad `embedding` produced 1024-dim
  primary key (document_id, model)
);
create index on doc_embeddings using hnsw (embedding vector_cosine_ops);

-- 5. Schema-constrained extraction (GLiNER2 / AdaExtract2 lineage).
create table extraction_schemas (
  id           uuid primary key default gen_random_uuid(),
  name         text unique,           -- 'invoice_v1', 'contract_v1', ...
  labels       jsonb                  -- entity/relation label set + field spec
);

create table extractions (
  id           uuid primary key default gen_random_uuid(),
  document_id  uuid references documents(id) on delete cascade,
  schema_id    uuid references extraction_schemas(id),
  fields       jsonb,                 -- the extracted structured record
  created_at   timestamptz default now()
);

-- 6. Provenance: link each extracted field back to the source region + span.
--    (AdaExtract2's "citations linking extracted entities back to source text".)
create table extraction_provenance (
  extraction_id uuid references extractions(id) on delete cascade,
  field_path    text,                 -- e.g. 'total_due' or 'line_items[2].price'
  block_id      uuid references blocks(id),
  char_start    int,
  char_end      int
);
