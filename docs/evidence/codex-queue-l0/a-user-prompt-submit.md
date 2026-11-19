# A: does codex queue fire UserPromptSubmit?

Version: **codex-cli 0.163.0**, observed 2026-11-19.
Method and isolation: [common receipt](README.md#common-method).

Initial private stdio positive-control launch argv:

```json
["<CODEX>", "app-server", "--stdio"]
```

After the direct `turn/start` positive control completed, the client submitted:

```json
{"method":"thread/queue/add","params":{"threadId":"<CANARY_THREAD>","clientUserMessageId":"<NEW_MESSAGE_ID>","input":[{"type":"text","text":"ping from queue RPC probe","text_elements":[]}]}}
```

Observed: one fake model request, one UserPromptSubmit hook, then one Stop hook
with stop_hook_active false; thread/read returned idle after completion.

That initial run tested the RPC, not CLI discovery. The subsequent
[operator-owned r2 canary](https://github.com/aceteam-ai/citadel-cli/pull/1818#issuecomment-7010738136)
started a private Unix daemon outside the sandbox and executed the actual CLI:

```json
["<CODEX>", "queue", "--thread", "<CANARY_THREAD>", "--message", "ping from probe"]
```

Observed natively: CLI exit 0 and queued acknowledgement, exactly one fake
model request, UserPromptSubmit then Stop with stop_hook_active false, and
thread/read idle after completion. Only the new canary UUID was targeted. No
interactive TUI was started. Script identity and isolation are in the common
receipt.

L3 consequence: UserPromptSubmit is verified for this native CLI/private-daemon
path, not every possible paused or interactive fleet state. Retain authenticated
MCP-call fallback and unobservable classification when hook coverage is unknown.
L4 consequence: A's actual CLI gap is cleared; C and D still retain the L0 gate.
