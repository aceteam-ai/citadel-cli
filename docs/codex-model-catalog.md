# Offline supplied model catalog

Refs #1734. This is an unwired, synthetic-input checkpoint, not a complete
app-server adapter, native compatibility test or launch feature. No CLI,
configuration, registry, transport, process, file, credential or network access
is added. Existing identity, admission, security and isolation holds stay put.

## Supplied contract

`ParseCodexModelCatalog` accepts all response pages from one caller-asserted
discovery context in order. Each `CodexModelPage` supplies the request's string
ID, requested cursor (empty for the first page) and raw response bytes. Numeric
RPC IDs are outside this subset. No request is constructed or sent.

The parser requires matching response IDs, distinct request IDs, a matching
cursor chain and a final explicit null `nextCursor`. Missing final pages,
repeated cursors, trailing pages after completion, and duplicate native IDs or
model slugs fail atomically, without returning a partial catalog. Duplicate
rows, including visible/hidden aliases of the same slug, are conservatively
refused; this is not a claim that every vendor catalog excludes such aliases.

Each row requires a nonempty `id`, a contract-compatible `model` slug and a
boolean `hidden`. Missing or unknown visibility is not treated as visible.
Hidden rows count toward limits but are excluded from the returned models.
Native IDs and model slugs can differ. Only the model slug is selectable.
Output order follows visible rows in the supplied pages.

The total input is limited to 16 pages of at most 64 KiB each, and 128 rows
including hidden rows. The existing duplicate-key, nesting and token-budget
checks also apply. IDs, cursors and model slugs are bounded; unsupported or
malformed input returns finite errors without supplied diagnostics. The 128-row
and slug limits match the candidate `models.reported` payload, without a schema
change. This subset refuses catalogs it cannot represent rather than clipping
or silently omitting required rows.

Optional labels, defaults, effort options, modalities and upgrade information
are not projected or used. A valid JSON block with an unexpected type in one of
these unused fields does not discard required rows. Invalid JSON, duplicate
keys, or structural limits anywhere still invalidate the response. The parser
does not validate or promise support for unprojected capabilities.

## Explicit selection and content

`Models()` returns a copy of the visible slug list. A complete empty catalog
returns an empty array, never a fabricated model. `Select(requested)` requires
an exact, nonempty slug in that visible list. It does not choose the first row,
use `isDefault`, substitute a native ID or display label, trim/alias a request,
or read a configured model. The requested argument is itself caller-supplied;
the parser cannot establish that it came from a human choice.

The catalog is immutable after parsing; returned slices and supplied byte
buffers do not alias its retained selection state. Generic formatting and JSON
serialization omit catalog/page content. The explicit `Models()` payload and
selected slug are content, not generic safe diagnostics or secret-scanned text.
Do not log caller-selected model identifiers as private deployment metadata.

## Not native or entitlement evidence

The [official app-server reference](https://learn.chatgpt.com/docs/app-server)
describes paginated `model/list`, visible rows and selection using returned
values. The [official catalog caveat](https://developers.openai.com/siwc/token-sharing-open-source/codex-app-server)
also notes that some configurations return a bundled catalog rather than an
account entitlement check. This checkpoint adopts that distinction, not that
page's authentication setup. It never reads credentials or implements a key
or token flow.

Matching supplied IDs and cursors does not establish a real initialized
process, provider/account context, authenticated provenance, complete delivery,
freshness or model access. A future reviewed caller must establish those
boundaries and invalidate its catalog on context change. This parser provides
no persistent cache or freshness clock and cannot clear a probe/admission hold.
Catalog membership must never be used as evidence of capacity or account
identity. No inference or probe turn is used to test entitlement here.

The committed fixtures use synthetic model names and responses. They are not
vendor-generated schema fixtures or installed-version evidence. Full R1 still
requires initialization, discovery/capability integration, isolated launch,
approval handling, usage/quota mappings, the local command and end-to-end proof.
This checkpoint is Refs-only and must not close #1734.
