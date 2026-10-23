# Wake-on-inbox supervisor: source design for review

Refs #1442 and aceteam-ai/aceteam#12037. This document is the proposed design
of record, not an implementation or an accepted protocol change. One coding
author owns the slice; other agents performed read-only review.

## Verdict

Prepare the node-side design, but **hold executable integration** until the
remaining custody, presence and batching gates below are resolved. The initial
plain `session_wait` sketch is superseded by the backend owner's observational
listen plus existing v2 delivery choice. No command, daemon, adapter, installer
or backend change is added by this draft.

Standing authority permits source preparation and draft PRs only. It does not
permit a live wake, model turn, key mint/rotation, configuration change, unit
installation, SQL, deployment, undraft or merge. The permanent #10969 R3 STOP
remains intact: no retry, reconstruction, retargeting or native attempt.

## Settled requirements, not questions to reopen

- Citadel owns the node-side supervisor. AceTeam remains the source of inbox
  messages and lane identity; this does not add another platform agent loop.
- Each lane opts in. Maximum ten automatic turns per rolling twenty-four hours,
  thirty minutes per turn, one active turn per lane; a lane may lower those caps.
- At most one automatic native submission per prepared message. Ambiguous
  outcomes hold and are not retried, including via a different adapter.
- Messages arriving during a turn belong to the next single turn.
- Use the lane's configured existing credential, never mint one. A peer message
  is data and does not change permissions, approvals or standing instructions.
- Use only operator-declared dedicated unattended exact thread/resume IDs. Never
  adopt an ordinary human interactive session or lease merely because it is idle;
  unknown human/interactive custody refuses rather than accepting a prompt.
- First consumers are the already bootstrapped codex-rog and codex-hp lanes.
  This design makes no claim that either host was inspected or activated.
- Claude's native socket path needs pinned capability proof. Exact print-mode
  resume is the requested fallback; never choose a recent/arbitrary session.
- The future systemd user installer/uninstaller belongs in Citadel. No hand-written
  live unit or service installation is part of source preparation.

Automated-aceteam confirmed the protocol, credential, ambiguity, rolling-window
and unattended-thread choices on October 22, 2026, in coordination receipt
`sm_4c7acfb0f3c3408fba0ff744bcf76ba8`. That owner input settles the source design;
it grants no permission to execute, change a live lane or revive R3.

## Existing contracts that cannot be silently composed

Backend source reviewed at `34d1377b6970a7cdd831bc831c72728a71b9d021`.
The inspected local files were byte-identical to that backend main snapshot.
Paths below are relative to the Aceteam repository's `python-backend/`.

| Requirement | Current source fact | Design consequence |
| --- | --- | --- |
| Exact existing lane | Ordinary named tools can implicitly touch/create or rebind a same-machine lease before the handler. | A key plus lane name is not an attach-only incarnation fence. |
| Claim before native work | `session_wait` atomically records delivered-via-wait before returning the envelopes. | A lost response is not proof that a claim did not happen. |
| Prepared native-write fence | Channel v2 prepare requires its exact bridge consumer, incarnation and attempt in transport-received state. | A message already consumed by wait cannot then be prepared through that path. |
| Restart recovery | `session_inbox(include_delivered=true)` first claims new messages, then returns oldest retained history with a capped, unpaged result. | It is not an exact-message, read-only recovery operation. |
| Next-turn batching | Ordinary child MCP results can drain new inbox messages during an active turn. | Pausing supervisor polling alone does not enforce the requested batching. |
| Honest presence | Wait markers and call freshness have bounded expiry. | Presence is not instantaneous proof of native custody or absence of a human prompt. |

Source anchors:

- `routes/aceteam_mcp_sessions.py:246`: caller-carrier precedence.
- `middleware/mcp_audit.py:337`: channel-control exclusion from implicit lease touch.
- `utils/mcp_sessions.py:799`: implicit lease creation/refresh and binding logic.
- `routes/aceteam_mcp_sessions.py:1302`: inbox history still starts with a claim.
- `routes/aceteam_mcp_sessions.py:1466`: attach-only channel identity; v2 controls.
- `utils/mcp_session_inbox.py:1262`: atomic pending claim and delivery publication.
- `utils/mcp_session_inbox.py:1514`: irreversible bridge preparation.
- `utils/mcp_session_inbox.py:1771`: retained history ordering and limit.
- `middleware/mcp_session_inbox.py:41`: ordinary next-tool-result drain.
- `utils/mcp_session_wait_state.py:21`: five-minute waiter marker expiry.
- `utils/mcp_sessions.py:280`: derived reachability.
- `routes/aceteam_mcp_listen_shim.py:75`: raw authenticated listen interception.
- `utils/mcp_session_listen.py:505`: exact listen parameters, lease fence and slots.
- `utils/mcp_session_listen.py:958`: cursor/replay selection.
- `utils/mcp_session_listen.py:283`: defined listen-slot refresh helper.

These are source observations, not permission to call channel controls, revive R3,
change backend semantics or consume a live lane's inbox.

## Selected intake: existing observational listen and v2 delivery

Use the existing raw `subscriptions/listen` SSE path for observation, followed
by the existing channel-v2 claim, prepare, submit and unknown semantics. The
listen wire contract `session-events/1` is distinct from delivery protocol v2.
Do not build a consuming-wait/local-custody alternative or a parallel inbox API.

The raw ASGI shim authenticates outside ordinary FastMCP tool middleware. Its
resource URI and incarnation identify one existing lane under the authenticated
org/user. Opening a stream checks that incarnation and every protected frame is
fenced against lease/key changes. It emits paged replay without claiming inbox
messages or creating/rebinding a lease. It does seal the coordination name and
reserve listen-registry slots; observational does not mean entirely write-free.

Therefore an observational server primitive already exists in reviewed source;
do not file a generic missing-wake endpoint. `session_list` is not a safe fallback:
although its handler renders pending counts, an ordinary MCP result can trigger
the caller's inbox drain. `session_channel_lookup` only recovers one already-known
prepared message, not an unknown pending inbox.

Initial listen without a cursor starts at the current head, not the old backlog.
Use the exact-incarnation attach replay cursor and persist progress; never
synthesize or copy a different lane's cursor. Replay may include already claimed
entries, so seeing an envelope is not permission to execute it. Cursor gaps,
identity changes and lost claim responses hold for explicit reconciliation, not
silent fast-forward or automatic resubmission. Short prepare/write windows are
not thirty-minute execution leases.

SSE message bodies can be truncated by both body and serialized-frame bounds.
Treat them as identity/wake hints, never as the complete native prompt. Only a
successful exact v2 claim supplies the complete message and attempt. Do not claim
or prepare the next batch while the current long turn still owns native custody.

The source defines a listen-slot refresh helper but has no caller for it in the
inspected backend. Slots age out after thirty seconds while streams can last
540 seconds, and listening alone does not renew the underlying session lease.
A healthy stream must not be advertised as fresh indefinitely from a stale slot;
an accepted non-creating, exact-incarnation presence/renewal path remains a gate.

This protocol does not prove exactly-once execution. The intended claim is bounded
at-most-one automatic submission, with visible uncertainty rather than replay.
No second message store or parallel remote dispatch surface should be invented.

Backend review must settle how child MCP drains are suppressed or fenced
for a supervised lane without changing ordinary interactive behavior. That is a
backend protocol decision, not a harmless local polling adjustment.

## Proposed supervisor state machine, after contract acceptance

An operator-owned private declaration pins authenticated org/user, canonical lane
name, existing lease incarnation and harness binding, exact native thread/resume
ID, absolute repository path, standing prompt, adapter revision, opt-in and lowered
caps. A replacement credential or lease inherits no prepared execution authority.
The supervisor never changes the user's normal harness configuration or login.

An exclusive local lane guard covers intake through terminal reconciliation.
Before any native write, persist the exact message identifiers, immutable
envelope/prompt digest, configuration/adapter revision, cap reservation and
prepared/submitting state. Crash recovery revalidates those facts; it never uses
an unfenced name, a bare PID, retained history or a timeout as execution authority.

Lifecycle: disabled, observing, prepared, submitting, running, completed; failures
become backoff, approval/input hold, or outcome-unknown hold. No unknown state
automatically returns to observing for the same prepared messages. Requests
received while running remain for the next batch, never parallel or mid-turn.
An explicit operator resolution is required where identity or outcome cannot be
proved. Prompt contents and credentials stay out of status and diagnostic logs.

Quota reservation happens durably before an attempt and is conservatively
consumed on uncertainty, including attempted submissions. The owner's selected
window is rolling twenty-four hours, never a midnight/reset-on-restart counter.
Clock rollback,
unreadable quota state or unconfirmed prior custody holds rather than resetting
the budget. Failure backoff must be bounded, persistent and non-spinning.

The thirty-minute bound includes a separately defined cancellation and process
custody protocol. Sending an interrupt or timing out is not proof the old turn
or descendants stopped. No new turn starts while termination is unconfirmed.
Peer messages cannot approve a tool request or weaken sandbox policy.

## Native adapter source proof

The [official Codex App Server documentation](https://learn.chatgpt.com/docs/app-server)
describes Unix-socket transport as WebSocket over UDS with HTTP Upgrade, not raw
JSONL. Initialization precedes an exact thread resume and a new turn. Existing
thread policies should be preserved, not overwritten by a wake. Documentation
does not establish the installed versions or exclusive interactive custody on
the two target hosts; those remain capability-proof gates.

For Codex, fallback to exact exec resume is allowed only before any native
submission could have occurred and only after accepted idle/exclusive-custody
proof. The [CLI reference](https://learn.chatgpt.com/docs/cli/reference) documents
exact exec resume and stdin prompts. Never use a most-recent-session selector,
approval/sandbox bypass, or fallback after an ambiguous write. The configured
flake wrapper is a fixed argv/environment boundary, not shell-interpolated data.

Claude remains exact print-mode resume only until its separate native proof is
accepted. Runtime syntax and flags need pinned source/capability fixtures before
implementation; this draft does not claim those proofs exist.

## Gates and eventual verification

Source review must still settle child-drain batching, durable presence/lease
liveness, exact interactive-custody proof and cancellation authority. Installer
ownership, v2 intake, rolling quotas and dedicated-thread policy are recorded
owner decisions, not permission to activate. Further questions route through
automated-aceteam or automated-citadel.

After those decisions, prepare deterministic fake-transport/clock/storage fixtures
for every transition, cap boundary, restart, lost reply, partial write, identity
replacement, busy/approval/input state, cancellation failure and backoff. Normal
GitHub CI may verify source; local candidate execution remains held. No fixture
may claim native process or interactive safety from a fake transport.

The live wake-within-a-minute, batching, real harness failure, honest presence and
systemd install/uninstall acceptance remain gated. This draft does not satisfy
those acceptance criteria or activate a lane. Receipt/AEP display follows the
existing #8993 posture and must not claim containment or vendor-token metering.
