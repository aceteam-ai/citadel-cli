# Offline harness protocol checkpoint

Refs #1734. This is a **candidate contract**, not a working vendor adapter or
completed acceptance. `internal/harnessdriver` supplies typed envelopes and
payloads, an embedded JSON Schema and shared synthetic NDJSON fixtures. The
vocabulary follows the requested command/event families; it is not a claim of
wire compatibility with another repository's current adapter types.

## Contract and unknown values

`protocol.schema.json` owns required keys, closed payload fields, vocabulary,
length limits and JSON-safe integer bounds. `DecodeEvent` and `DecodeCommand`
also refuse duplicate keys, invalid UTF-8, excessive depth/token count and
envelopes over 64 KiB. They return finite errors without peer contents. Golden
fixtures cover all six commands and ten event types and round-trip through the
same embedded schema. Changing this candidate requires updating its fixtures;
cross-repository adoption and version negotiation are not implemented here.

Every event carries session name, incarnation, native and turn IDs, sequence,
timestamp, source and fidelity. Native and turn IDs use an empty string until
known, never an invented vendor identity. Sequences are positive integers no
larger than 2^53-1; allocation, monotonicity and incarnation fencing belong to
the eventual adapter. Source, timestamp and fidelity are supplied assertions,
not authenticated provenance. Parsing a command never authorizes it.

Unknown token counts, tool exit/error state and quota/reset values are null,
distinct from explicit zero or false. Usage represents supplied per-turn
counts, not inferred remaining plan capacity. Model discovery is not proof of
entitlement. A tool's failed state carries `is_error: true`; started tools do
not assert an exit code. A completed model turn can still contain tool errors
and denials. Assistant text is never a tool-success signal. An idle observation
without authoritative completion must not become a successful turn outcome.

### Additive seat reports

Refs #1749. `SeatQuota` adds a complete seat metadata block to `quota.reported`:
`seat_ref`, `scope`, nullable `reached`, `overage`, `reset_credits_available`,
`source`, `confidence`, `adapter_version` and `observed_at`. Optional bounded
`window_minutes`, model-scoped `model` and machine-readable `reason` complete
the observation. Account-scoped rows cannot claim a model. The candidate also
names weekly, session, plan-period and credit windows for future collectors;
this does not add collectors or establish those vendors' wire schemas.

Any extension field requires the complete metadata block. Unknown and
consumption-only observations cannot carry a known quota percentage. Known
percentages require exact or derived confidence and a known window. Five-hour
and seven-day durations, when supplied, must agree with their named window.
Reset counts remain report-only. Legacy minimal `Quota` and `Usage` payloads
and earlier golden fixtures remain valid and serialize unchanged.

`ProjectSeatQuota` copies the normalized supplied observation into that
validated payload; absent windows become explicitly unknown. `ProjectSeatUsage`
adds a seat reference and an optional actual model, leaving the legacy `model`
empty rather than substituting a requested model. Unknown counts remain null,
not zero. Both return owned pointers and finite errors without partial reports.
These functions do not produce an authenticated event envelope, prove identity,
advance freshness, replenish quota after reset or enable admission. A future
caller still owns trustworthy binding, receipt time, sequence and isolation.

The [app-server documentation](https://learn.chatgpt.com/docs/app-server)
informs the separate supplied quota parser. This candidate normalized schema is
not the native schema or a version-pinned compatibility check. Tests use only
synthetic observations; native collection, schema-diff CI and the seat probe
remain unimplemented. No runtime caller is wired by this checkpoint.

Input previews, output excerpts and assistant text are bounded **content**, not
safe diagnostics. The package does not scrub arbitrary secrets. In a future
adapter, preview/excerpt fields must remain empty until a reviewed projection
scrubs them, and content must never be logged as protocol diagnostics. The
fixture keeps tool previews empty. No arbitrary native detail object or raw
error message is carried by the error payload.

## Supplied-stream helpers

`LineReader.Next` uses one persistent buffered reader and `ReadBytes` with a
per-frame read budget. It preserves buffered lines, accepts a 5 MiB line and
refuses lines over 8 MiB without reading an unbounded delimiter-free stream.
The limit counts content bytes before LF, including an optional CR. Final
frames require LF; CRLF is accepted. Terminal read/size errors poison the
reader, so a partial suffix is never reused as another frame. The native line
bound is separate from the smaller envelope bound: an eventual adapter must
project/chunk large native text explicitly, not silently discard or truncate it.

`FrameWriter` serializes complete single-line JSON writes, retries short writes
and refuses reuse after an error or zero-progress write. Such a failure has an
unknown delivery extent, not automatic retry permission. It accepts trusted
caller values; it does not bound allocation performed inside a caller-supplied
custom marshaler.

`RunStreams` owns caller-supplied stdout/stderr read-closers. It reads them in
separate goroutines, discards stderr without retaining it, closes both readers
on cancellation or terminal failure and waits for both goroutines. A clean
stdout EOF ends the read pump, not a successful model turn. Reader `Close`
must interrupt blocked reads and handlers must cooperate with their context.
The helper has no process lifetime or descendant-management authority.

`EventQueue` holds at most 64 validated envelopes, each at most 64 KiB. It copies
payload bytes and backpressures producers instead of dropping or coalescing
any event. Closing refuses new pushes and lets consumers drain queued controls
in insertion order. Producers must use cancellation and the future adapter
must bound producer concurrency. The queue does not allocate sequence numbers,
bind identities or offer durable delivery.

## Remaining acceptance

An unwired supplied-item/turn reducer is documented in
`codex-turn-projection.md`. Its optional text item identity and explicit observed
count scope extend this candidate without changing earlier fixture decoding.

No CLI, remote job, process launcher, credential reader, active native transport adapter,
approval executor, storage or routing caller is added. Tests use synthetic
memory buffers and owned pipes only. No vendor binary or real user session is
inspected or invoked. This is not a registered remote tool and adds no remote
admission surface or admission-ledger entry.

The remaining R1 work includes the Codex initialize/model-discovery adapter,
versioned schema evidence, strict isolated launch and pinned configuration,
trusted per-session/turn and approval correlation, reviewed content projections,
native per-turn usage conversion and quota integration, the fake vendor server, the
local try command and separately authorized end-to-end/native acceptance. The
registry's existing identity/isolation holds are unchanged. No supplied protocol
field clears them. Source tests do not establish any of these runtime properties.
