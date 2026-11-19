# A: does codex queue fire UserPromptSubmit?

Version: **codex-cli 0.163.0**, observed 2026-11-19.
Method and isolation: [common receipt](README.md#common-method).

Actual launch argv:

```json
["<CODEX>", "app-server", "--stdio"]
```

After the direct `turn/start` positive control completed, the client submitted:

```json
{"method":"thread/queue/add","params":{"threadId":"<CANARY_THREAD>","clientUserMessageId":"<NEW_MESSAGE_ID>","input":[{"type":"text","text":"ping from queue RPC probe","text_elements":[]}]}}
```

Observed: one fake model request, one UserPromptSubmit hook, then one Stop hook
with stop_hook_active false; thread/read returned idle after completion.

This tests the RPC used by the installed CLI's queue implementation, **not**
the CLI's daemon discovery or end-to-end interactive behavior. The actual queue
argv remains **unexecuted and unverified** because the private Unix daemon could
not start inside the agent sandbox:

```json
["<CODEX>", "queue", "--thread", "<CANARY_THREAD>", "--message", "ping from probe"]
```

L3 consequence: RPC UserPromptSubmit is a useful positive control, not proof
that all queued fleet turns produce that signal. Retain authenticated MCP-call
fallback and unobservable classification when hook coverage is not known.
L4 consequence: retain the L0 gate pending the actual CLI canary.
