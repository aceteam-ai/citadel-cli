# Shared worker detail (#1441)

When a dedicated worker holds the existing node worklock, Control Center's
worker branch reads its status over the shared local control socket. It returns
before constructing another API job source, clock monitor, status collector,
heartbeat publisher or node-state reporter. The standalone worker branch and
unrelated host/service panels retain their existing behavior. This is one part
of #1118's shared-status work, not completion of the unified entrypoint.

In that monitor branch the early return also skips pending-update startup
reconciliation, loading device configuration or requiring a device token,
constructing an API source and WebSocket, starting the clock monitor and status
collector, configuring TUI activity telemetry, starting a duplicate ActualState
emitter, node-state reporting, heartbeat publication and a worker runner. Local
activity remains visible, including the static owner-change warning. Monitor
TUI-originated activity is no longer forwarded by a telemetry stream configured
in this branch. Automated-citadel accepted this engineering change on October
23, 2026: the lock-holding worker remains the node's single telemetry/state
publisher, while the viewer's local activity stays local. The PR lists this
behavior change explicitly. Native UI acceptance remains separate. The external
worker's own telemetry and heartbeat contract are unchanged.

The read-only `GET /citadel/session/v1/worker-detail` uses the same OS-protected
Unix socket or Windows pipe as existing session status. It adds no listener,
credential, enrollment or ownership mechanism. The default status GET and
explicit session viewer retain their existing small response and 16 KiB bound;
the viewer never requests this detail route. The optional HTTP status port is
neither enabled nor required.

The detail contains the existing session identity fields, `observed_at`, and
the worker's existing WorkerLiveness, SwapActivity and LaneActivity DTOs. The
worker creates an immutable provider bundle after selecting its transport and
before publishing the controller, then reuses those function values for
heartbeat and optional status collection. Existing atomic runner and swap
pointers supply initialization without replacing callbacks. Providers read
in-memory state; the detail route starts no collector, queue subscription or
GPU/model/service probe. Before the runner exists, these providers return a nil
worker block: the optional `/worker` and `/status` routes omit `worker` during
startup/handler construction. Heartbeat publication starts after runner
initialization. This is not an atomic cross-subsystem snapshot.

Missing providers and providers not yet initialized are unknown. A nil worker
or swap block, or nil lane list, is never interpreted as healthy idle work.
`Pulled` retains all three states: true, false and absent/unknown. The UI shows
consumption, last poll/job time, in-flight and cumulative counts, swap counters
and complete bounded history, and queued/executing/capacity/busy lane data. It
shows the selected identity and local receipt age, rather than treating the
monitor's own running goroutine as evidence of external consumption. Peer text
is quoted as literal ASCII and escaped for terminal-widget markup.

## Bounds and refusals

`status.WorkerDetailMaxBytes` caps the complete response at 256 KiB. Every string
is at most 256 UTF-8 bytes, Recent has at most 64 records, each record has at
most 64 evicted names, and Lanes has at most 16 entries. These bound only this
projection; they do not truncate or change the worker's underlying history or
heartbeat contract. The server validates lengths before cloning, copies nested
slices/timestamp/bool pointers, and serializes the whole reply before writing
success. An invalid field or overflow returns HTTP 503 with the static
`worker detail unavailable` error. JSON escaping also counts toward the total.

The client requests only the selected route on the protected socket, refuses
redirects, reads at most the body cap plus one byte, and closes every returned
body. Requests run sequentially with a three-second context deadline and a
five-second interval after each completed read. Decoding refuses duplicate
members, known-field case aliases, missing/null required members, malformed
known subsystem objects, excessive nesting, invalid UTF-8, oversized data and
trailing JSON. Bounded additive fields remain compatible.

A legacy owner without the detail route, denied request, malformed reply or
transport failure clears the monitor's retained observation and receipt time.
The jobs panel, copied jobs text and open detail modal show unavailable on the
next ordinary UI refresh; no old healthy worker snapshot is retained as a
fallback. A transport failure may recover only to the same observed owner.
The initially selected worklock PID and the first validated mode/version are
pinned; changed identity or decreasing observed uptime latches a distinct static
owner-changed message until an explicit monitor restart. The first latch adds
one local activity warning. Node name, mesh IP and connectivity are display
data and may change or be absent during reconnect without selecting a different
owner. Cancellation
clears observations and cancels only monitor-owned requests, never the worker.

PID and uptime remain observations, not authenticated incarnation evidence.
PID reuse with identical observable fields is indistinguishable. This child
does not invent an identity fence or silently take over a replacement worker.
Ordinary Control Center start/stop callbacks retain their existing semantics;
the shared-status display does not acquire a new worker-control operation.
The shared jobs modal ignores its start/stop key, so that key cannot cancel the
local monitor or imply control of the external worker.

## Source evidence and remaining gates

Prepared fixtures cover live provider changes and late initialization, nested
copy ownership, default status compatibility, handler refusal before writing
success, response and field bounds, strict/additive decoding, fake-transport
redirect refusal and body closure, sequential reads, cancellation, selected
owner replacement, reconnect and stale clearing, and literal rendering of
unknown/false/true states. Source wiring guards pin the held-worker return
before worker-owned constructors and shared heartbeat provider reuse.
Additional fixtures cover blank/restored/renamed display names, distinct
replacement reporting, valid detail JSON under failed HTTP statuses, observed
consumption overriding the local monitor flag, and ignored modal control keys.

These fixtures are source preparation; normal GitHub CI and independent source
review are recorded on the draft PR. Local candidate execution, native
Unix/Windows permission and reconnect testing, and live UI parity remain
separately gated. No deployment, service installation, SQL, credentials, bare
entrypoint preference or declared-session protocol changes are included.
