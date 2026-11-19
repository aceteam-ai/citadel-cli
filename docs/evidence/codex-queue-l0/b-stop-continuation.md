# B: Stop block and stop_hook_active

Version: **codex-cli 0.163.0**, observed 2026-11-19.
Launch argv and isolation: [common receipt](README.md#common-method), using
`["<CODEX>", "app-server", "--stdio"]`.

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
this passes only the private stdio Stop positive control, not daemon wake or
interactive pause acceptance.
