# B: Stop block and stop_hook_active

Version: **codex-cli 0.163.0**, observed 2026-11-19.
Launch argv and isolation: [common receipt](README.md#common-method), using
`["<CODEX>", "app-server", "--stdio"]` initially, then
`["<CODEX>", "app-server", "--listen", "unix://"]` in the operator-owned r2 run.

The [native operator receipt](https://github.com/aceteam-ai/citadel-cli/pull/1818#issuecomment-7010738136)
independently reproduces the sequence below on the new private Unix daemon.
The lane did not execute that native script inside the sandbox.

The client started a turn with fake text `ping from stop probe`. A reviewed,
hash-trusted Stop fixture returned the following JSON once, then `{}` on its
next invocation:

```json
{"decision":"block","reason":"ping from probe continuation"}
```

Observed sequence for that request:

```text
UserPromptSubmit
fake model response
Stop(stop_hook_active=false) -> decision:block
fake model response
Stop(stop_hook_active=true) -> {}
thread/read: idle
```

Exactly two fake model requests occurred. The continuation did **not** produce
a second UserPromptSubmit event. The fixture was intentionally one-shot; this
does not establish a native bound on an always-blocking hook.

L3 consequence: do not count Stop continuations solely through
UserPromptSubmit. L7 must explicitly account for them, enforce Jason's two
consecutive-continuation cap and charge the lane turn budget. L4 consequence:
the Stop sequence is now observed on both private stdio and the native private
Unix daemon, but this does not clear interactive pause or composer acceptance.
