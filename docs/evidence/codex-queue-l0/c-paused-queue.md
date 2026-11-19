# C: paused queue, reconnect pause and stalled goal

Version: **codex-cli 0.163.0**, inspected 2026-11-19.
Status: **unverified**, not a passing or failing pause observation.

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

The 0.163 source's `app-server/tests/suite/v2/thread_goal_empty_responses.rs`
establishes a narrower non-interactive candidate: enable goals only in a new
fake-provider canary and let three empty final answers trip the native
no-activity breaker. An operator probe can require thread/goal/get to report
blocked before invoking the queue CLI once, then record queued acknowledgement,
model requests, UserPromptSubmit, pending queue count and the subsequent goal
status separately. This procedure has not been run natively by this lane and
is not yet evidence for C. Even a passing result would cover only that blocked
goal case, not the TUI reconnect pause or every stalled state.

```json
["<CODEX>", "queue", "--thread", "<CANARY_THREAD>", "--message", "ping from probe"]
```

L3 consequence: a queue acknowledgement alone must never become processed or
turn-started evidence; unknown pause state remains unobservable.
L4 consequence: no inferred resume behavior, no retry loop or terminal fallback;
retain the L0 gate until the operator records both cases.
