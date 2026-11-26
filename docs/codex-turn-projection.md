# Supplied item and turn projection

Refs #1734. `harnessdriver.CodexTurn` is an unwired, in-memory reducer for one
explicitly selected supplied stream. No command, feature flag, process launcher,
RPC client, credential reader, permission decision or runtime caller is added.
It does not depend on registry work or clear its identity/isolation holds.

The [official app-server documentation](https://learn.chatgpt.com/docs/app-server)
informed the item/turn mapping. The OpenAI Docs skill was used to verify those
shapes. Fixtures are synthetic, not generated schema or installed-version
compatibility evidence. The complete adapter and native acceptance remain open.

## What the checkpoint owns

`Apply` checks supplied thread/turn correlation and a matching start observation
before supported items or completion. It assigns bounded sequence numbers under
a mutex and refuses times before its last state-changing observation. Returned
event publication is caller-owned: a future concurrent caller must serialize
applying and publishing, not enqueue a later sequence before an earlier one.
Native command status and exit code
are projected independently of assistant text. Missing exit code stays null;
nonzero exit cannot become tool success. Required malformed evidence refuses
without a partial state change. Optional command/output/error detail is omitted,
including wrong-shaped optional output blocks. Tool previews remain empty until
a separately reviewed scrubbed-content projection exists.

The reducer counts observed unique command failures and denials separately.
Every projected completion explicitly says `counts_scope: observed_items`.
That is not a complete-tool-inventory claim, even when the model-turn outcome
is completed. A missing count scope on earlier candidate records also provides
no completeness guarantee. Source, identity and receipt time are caller
assertions, not authenticated native provenance.

These are candidate schema additions. Earlier fixtures still decode, but an old
closed-schema decoder rejects records containing the new fields. Cross-repository
adoption and version negotiation must precede any active transport.

Terminal item hashes suppress exact projected repeats without retaining native
bodies. Conflicting projected repeats and deltas after a terminal item refuse.
An item repeat is not an execution or delivery receipt. The item ledger is
bounded; refusal does not consume a sequence or counter. No sequence wraps.

Text events add an optional bounded `item_id`: a delta appends to that item,
while `assistant.text` is its final snapshot, not another append. Large text
that cannot fit the bounded contract refuses explicitly, never silently clips.
Content events are not safe logs or terminal rendering. Reducer/correlation
JSON and ordinary/debug formatting omit private state.

Idle alone emits a derived unknown completion and closes this reducer, never a
successful outcome. It cannot prove which native work finished. Late data
refuses, and a later authoritative outcome needs separate reconciliation. A
future caller must not blindly replay work on unknown completion.

## Boundaries and verification

Native input uses the line-size bound plus duplicate-key, UTF-8, nesting and
token guards. Only the documented projection subset is supported; RPC requests,
responses and other notifications refuse. A future full adapter must route
these deliberately and respect errors, not silently drop controls.

Tests use owned synthetic buffers, pipes and fixtures. They cover malformed
optional output, command failure beside a positive reply, null versus zero,
correlation, replays, concurrency, item/sequence bounds, idle, finite errors,
large omitted output, and the existing framing/writing contract. No vendor
binary, real session, credential, API, service or database is inspected.

Still outstanding: initialization/model discovery, isolated native launch and
pinned configuration, trusted stream continuity and incarnation ownership,
approval execution/correlation, usage and quota wiring, native schema/version
proof, a complete fake server, the local try command and native acceptance. No
production caller imports or invokes this reducer and no remote admission surface
is registered. The source checkpoint cannot establish those runtime properties.
