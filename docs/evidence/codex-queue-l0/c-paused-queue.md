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

```json
["<CODEX>", "queue", "--thread", "<CANARY_THREAD>", "--message", "ping from probe"]
```

L3 consequence: a queue acknowledgement alone must never become processed or
turn-started evidence; unknown pause state remains unobservable.
L4 consequence: no inferred resume behavior, no retry loop or terminal fallback;
retain the L0 gate until the operator records both cases.
