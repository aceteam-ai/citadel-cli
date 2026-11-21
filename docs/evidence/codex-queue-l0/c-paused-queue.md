# C: paused queue, reconnect pause and stalled goal

Version: **codex-cli 0.163.0**, inspected 2026-11-19.
Status: **partially observed**. The native no-activity-breaker blocked-goal case
is recorded below. TUI reconnect pause and other stalled states are **unverified**.

Executed argv:

```json
["<CODEX>", "queue", "--help"]
["<CODEX>", "app-server", "generate-json-schema", "--experimental", "--out", "<SCHEMA_DIR>"]
```

Installed queue help accepts thread and message, but exposes no `--resume`.
The experimental schema exposes thread/queue/add, list, update, delete,
reorder and start; method availability does not prove reconnect-pause or stalled
goal behavior. The separate thread/goal/get schema is likewise not proof that
it identifies the interactive queue's pause state.

The operator canary must establish each paused state first, queue fake text
using the exact argv below, then distinguish persisted acknowledgement from
actual turn start. No paused interactive state was created here.

The native r2 receipt clears only A/B/E, not this question. Source inspection
of 0.163's `tui/src/chatwidget/reconnect.rs` shows that `ChatWidget` owns the
reconnect pause: `pause_for_disconnect` sets input-queue autosend suppression,
and recovery reconciles exact confirmed message IDs before handling retained
input. A headless daemon restart creates no ChatWidget and therefore does not
establish this TUI pause. Queue persistence or an unloaded-thread test would be
additional evidence about another state, not a replacement for C. The stalled
goal case separately requires establishing and recording that state first;
neither an ordinary completed turn nor an interrupted turn is sufficient.

## Native r3 blocked-goal observation

The operator ran the [r3 script](https://github.com/aceteam-ai/citadel-cli/pull/1818#issuecomment-7011199590)
with SHA-256
`53cc83d4d2a6528cb9861688cd0323936f6e352c3e4608964de096e6bbb81955`.
The [native receipt](https://github.com/aceteam-ai/citadel-cli/pull/1818#issuecomment-7012245641)
records codex-cli 0.163.0, exit 0 and clean cleanup. This lane did not run it.
The procedure follows the installed source's
`app-server/tests/suite/v2/thread_goal_empty_responses.rs`: goals are enabled
only in the new private configuration, on a second new canary thread. Three
empty fake final answers trip the native no-activity breaker. The script reads
`thread/goal/get` as blocked and `thread/read` as idle before the one-shot queue,
and rechecks blocked immediately before that queue. It never writes blocked
directly or resumes the goal.

```json
["<CODEX>", "queue", "--thread", "<CANARY_THREAD>", "--message", "ping from probe"]
```

The r3 invocation uses the same argv shape with fake text
`ping from blocked goal probe`. Its content-free observation is:

```json
{"empty_response_calls":3,"goal_status":"blocked","probe":"C_goal_setup"}
{"goal_status_after":"blocked","model_calls_since_queue_attempt":1,"pending_queue_items":0,"poll_window_seconds":6,"probe":"C_blocked_goal_queue","queued_ack":true,"user_prompt_submit_count":1}
```

The same empty-final-answer fixture remained enabled for the queued turn.
The queue was acknowledged and delivered during the bounded observation:
one model request, one UserPromptSubmit and no remaining queued item. The goal
was still blocked at the final read. No exact delivery latency is recorded, so
this is not a millisecond-latency claim. It is evidence that this blocked-goal
state does not park an explicitly queued message, not evidence of goal resume
or subsequent automatic goal continuation. The six-second poll-window field is
a configured observation budget, not a measured delivery latency or end-to-end
deadline. The controller aborts if an observation sees more than 12
fake requests; it is not an atomic admission limit on the thirteenth request.

## Consequence for #13011

The design's section 8 row grouping reconnect pause and stalled goal as
`delivered`, then `ignored` is too broad for this observed blocked-goal case.
For an otherwise authorized wake, L3 must correlate actual same-incarnation
turn evidence; it must not force `ignored` merely because the goal stays blocked.
A queue acknowledgement alone is still only delivery, and the fixture hook is
not an authenticated AceTeam heartbeat or inbox claim. This canary does not
prove `consumed`, production correlation, or clearing a blocker.

L4 must keep the existing eligibility and human-driving refusals. This result
does not authorize waking a lane already classified blocked/paused/limited,
change Jason's D1 second-observation decision, or justify auto-resuming a goal.
If a permitted queue starts a turn, account for that turn under the normal
budgets; do not retry it because the goal still says blocked. D6 ignored-wake
backoff applies only when the receipt actually meets its ignored criteria.
No message resend, alternate thread, inferred resume or terminal fallback.

The TUI reconnect pause is still unverified, as are other stalled states. Do not
generalize this one no-activity-breaker result to them. L0 remains incomplete
and L4 stays gated pending the remaining evidence and authorization.
