# Durable HTTP delivery (S3a)

A legacy queue still publishes a terminal event and then acknowledges the job.
If the subscriber is disconnected, that sequence can lose a finished result.
The S3a path for #1227 makes server completion receipt the prerequisite for
publishing a terminal event on a registered durable queue.

**Production loss is unchanged until a canary queue is registered for durable
dispatch, activation proof is supplied, and the platform waiter falls back to
reading the committed result on timeout.** This source change does not register
a queue, activate a protocol, migrate storage or deploy a worker.

`redisapi.Client.ConsumeJob` negotiates per queue only after the consume route's
exact protocol-mismatch response. A durable request uses the protocol's fixed
consumer group; legacy queues retain their configured group. No generic error
allows a downgrade. HTTP metadata capture verifies the original tuple, original
payload digest and unique original scope. The generic parser, passive delivery
observations and handler payload cannot mint a claim. Claims remain opaque to
formatting and JSON output.

`APISource.adoptDelivery` immediately renews the HTTP candidate before admitting
it. `durableDelivery.renew` samples the local monotonic clock before the request
and uses only the server's `remainingLeaseMs` budget. Network latency consumes
that budget; `expiresAt` is advisory. The delivery renews at half-life while
queued and executing. An independent expiry timer cancels custody even if a
renewal request ignores cancellation. Failure loses the original claim; it
never reacquires an epoch for an old result.

The runner gives handlers a writer that suppresses terminal calls. Its own
terminal writes stage an immutable snapshot; source `Ack` or `Fail` uploads that
snapshot and validates the commit receipt before the raw publisher is allowed
to send it. Durable completion already acknowledges atomically, so no legacy
status, raw ACK, terminal lookup or repair path follows. An ambiguous completion
response publishes nothing and lets server disposition handle the record.
Terminal wake publication is bounded and retried only after the receipt; a
failed wake cannot cause another execution.

Cancellation commits an error with code `cancelled`. Permanent failures retain
a reason code. An unresolved local node ID commits `target_unresolved`. Retry,
GPU/lane admission saturation and `model_warming` stop renewal without a
terminal. Lease loss cancels the handler and retains its lane/executing bracket
until actual exit, including handlers that ignore cancellation. Their late
results and terminal writer calls cannot escape the lost epoch.

Unsupported sources refuse top-level durable metadata before execution or
legacy disposition. Direct Redis keeps the entry pending and never ACKs or
moves it to a dead-letter queue. WebSocket delivery also refuses; WebSocket
wake publication does not confer execution authority. Malformed or partial
metadata cannot silently become a legacy execution.

WORKER_CONTROL remains refused under v1 pending S3b's commit/restart gate and
restart persistence. HUDDLE_JOIN also remains refused under v1: its current
coordinator-owned lifecycle publishes outside StreamWriter and requires paired
integration before this barrier can cover it. Legacy control and huddle behavior
are unchanged. These refusals must remain visible when selecting a canary.

The focused tests use synthetic keys, fake HTTP, Go in-memory Redis and virtual
time. They cover claim isolation, strict receipts, original bytes, refused
redirects, cancellation, waiting renewal, backpressure and loss with an
uncooperative handler. Native, storage, restart and activation acceptance remain
separate gates; no node reads terminal result bytes.
