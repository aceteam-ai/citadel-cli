# Report-only seat quota observations

This checkpoint introduces pure functions over supplied observations. There is
no process, filesystem, network, timer, admission or routing caller. It does not
enable a seat probe or relax registry identity and isolation holds.

`seatquota.Codex` projects the result/params object of a quota read or push into
two bounded window observations. The receipt time, seat reference and source
are caller assertions, not authenticated evidence. The parser handles only the
default `codex` metered bucket and does not combine buckets or infer a model from
their identifiers. A supplied multi-bucket view takes precedence; an unsupported
bucket cannot fall back to an apparently free legacy view.

The [official app-server documentation](https://learn.chatgpt.com/docs/app-server)
describes `account/rateLimits/read`, the update notification, the compatibility
and multi-bucket views, minute-based durations, second-based reset timestamps,
and the authoritative available reset count. This projection follows those
documented field shapes; its synthetic examples are not a generated or
installed-version-verified schema. Unknown fields, labels, account data and
credit detail records are not published.

Only five-hour and seven-day windows are recognized in this increment. Missing,
malformed or unsupported windows remain unknown with null percentages. A bad
optional window or credit block does not discard a valid independent window.
Unknown vendor-wide limit states withhold the allowance projection. Reset
credits are counts only; they never replenish quota or authorize consumption.
Overage remains unknown rather than inferred. Duplicate JSON keys, deep input,
unbounded labels and oversized records are refused with a finite error.

`Freshness` marks known observations fresh through 120 seconds, aging through
30 minutes and stale thereafter. A past reset is unconfirmed, never evidence of
replenishment. `Due` is only a pure proposal with a 60-second attempt floor and
a jittered 15-minute idle interval. A future collector must independently prove
idle state, serialize reads, bind a trusted seat, preserve every attempt including
failures and provide a trustworthy clock. This function does none of those.

`ReadCadence` adds an in-memory model for one supplied seat/provider binding.
Concurrent `Begin` calls issue at most one opaque local attempt. Failed or
canceled attempts complete with no observation, but retain their attempt time
and its 60-second floor. A still-active attempt stays held until its matching
completion; no timer, automatic retry or expiry launches another read. Idle
reads follow the jittered 15-minute cadence, while on-demand proposals require
an observation older than 120 seconds. Turn-only providers cannot reserve reads.
Successful completions accept a receipt within that attempt and never replace
a newer push receipt. Duplicate, stale, foreign and invalid completions cannot
release another attempt. Clock rollback is refused, and snapshots own no mutable
internal pointers. These timestamps track supplied observations, not allowance.

This model has no production caller, transport, scheduler, persistent state or
native process. It is not a seat-identity proof or permission to probe. A future
collector must retain exactly one instance for each trusted seat binding, prove
idle state, durably record every attempt before I/O, and arrange completion on
failure/cancellation. Creating a new instance loses the in-memory floor; it is
not restart-safe cadence ownership. Native integration and its existing holds
remain unchanged.

`Consumption` copies bounded caller-supplied counts and the actual reported
model. Unknown remains null; consumption is not a plan percentage or entitlement.
`EstimateTurns` accepts at most sixteen supplied turn brackets for the same seat,
model, window and reset epoch. It requires asserted exclusive use and fresh
bracketing observations; ambiguous activity or a changed epoch yields unknown.
Its derived estimate is not an admission decision or verified work-size history.

`CollectionMethodAllowed` allows only the read method as future collection
policy. It is not yet applied by a transport, and therefore does not prove a
live method boundary. No reset consumption, login, logout, email or model-turn
method is enabled here.

Remaining acceptance for #1749 includes trusted native collectors and isolation,
registry/CLI and live protocol integration, vendor-version schema-diff CI, verified
formats for the other harnesses and durable bounded history/cadence ownership.
Unknown vendor identity stays held. Tests use supplied synthetic data only.

The additive candidate payload and pure projection helpers are described in
`harness-driver-protocol.md`. They accept these normalized observations without
adding a native reader or a live event publisher. This is partial protocol work,
not completion of the probe acceptance.
