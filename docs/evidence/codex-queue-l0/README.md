# Codex queue L0: partial canary evidence

Issue: [citadel-cli #1816](https://github.com/aceteam-ai/citadel-cli/issues/1816).
Parent: [aceteam #13011](https://github.com/aceteam-ai/aceteam/issues/13011).
Related acceptance: [aceteam #13378 A6](https://github.com/aceteam-ai/aceteam/issues/13378).

Observed 2026-11-19 with **codex-cli 0.163.0**, Linux, against a new
credential-free canary. This is **partial evidence, not L0 completion or L4
activation approval**. No product code, production thread, PTY, tmux, keystroke
injection, or live model API was used.

| Question | Result |
| --- | --- |
| [A: queue and UserPromptSubmit](a-user-prompt-submit.md) | RPC positive control passes; actual CLI daemon path unverified |
| [B: Stop continuation](b-stop-continuation.md) | Private stdio positive control passes |
| [C: paused queue](c-paused-queue.md) | Unverified; requires operator canary |
| [D: human composer draft](d-human-draft.md) | Unverified; requires operator canary |
| [E: local daemon status](e-thread-status.md) | Stdio status reads pass; independent daemon reader unverified |

## Common method

All subprocesses used argv arrays with `shell=False`. Paths below are
placeholders, not real host identifiers or thread IDs:

```json
["<CODEX>", "--version"]
["<CODEX>", "queue", "--help"]
["<CODEX>", "app-server", "generate-json-schema", "--experimental", "--out", "<SCHEMA_DIR>"]
["<CODEX>", "app-server", "--listen", "unix://"]
["<CODEX>", "app-server", "--stdio"]
```

The child environment was constructed from scratch: PATH, private HOME,
XDG_CONFIG_HOME, XDG_CACHE_HOME, XDG_RUNTIME_DIR and LANG only. It did not
inherit API keys, real Codex configuration, auth files or AceTeam hooks. A new
working directory was outside any repository. The fake Responses endpoint
bound only to loopback, rejected Authorization headers and returned a fixed
assistant message, `pong from probe`. Inputs were fake `ping from ... probe`
strings. Landlock ABI 4 restricted child TCP connects to the fake endpoint's
port; a connect trace confirmed attempted background HTTPS connections were
denied. This is not a claim that all UDP/DNS traffic was disabled. An initial
unguarded stdio exploration showed background outbound connection attempts;
the results below were reproduced with the TCP guard installed.

The private config selected model `probe-model`, provider `probe`,
`wire_api = "responses"`, `requires_openai_auth = false`, read-only sandbox,
approval policy `never`, and `features.hooks = true`. The fake provider emitted
`response.created`, `response.output_item.done` with one assistant output-text
item, then `response.completed` with synthetic usage. No tool call was emitted.

The stdio client sent `initialize` with experimentalApi true, then `initialized`,
then `thread/start`. Two reviewed fixture hooks, UserPromptSubmit and Stop,
recorded only event name and stop_hook_active. They began untrusted and were
approved through native `config/batchWrite` using their exact current hashes
under `hooks.state`, followed by `hooks/list` confirming both trusted. No hook
trust bypass flag or managed-hook substitution was used. Native command hooks
accept command strings; these fixtures invoked only the reviewed Python
recorder. The no-shell argv rule above refers to the Codex launch/queue boundary.

The private Unix listener failed before initialization with:

```text
Error: app-server socket directory must be a user-owned directory with mode 0700
```

Native 0.163 uses a fixed protected `/tmp/codex-daemon-<UID>` rendezvous root,
independent of HOME, XDG or TMPDIR. No permissions were changed on that root,
and no connection to the live daemon was attempted. The private stdio process
was terminated and reaped after each run. No daemon service was installed,
restarted or stopped.

## What remains

An operator-owned throwaway interactive canary outside the agent sandbox is
needed for A's actual `codex queue` path and for C, D and E's independent
daemon-reader path. The coordinator has referred that decision to Jason.
Keep [L4 #1817](https://github.com/aceteam-ai/citadel-cli/issues/1817) gated.
Do not replace missing observations with a second writer, `--remote`, PTY reads,
terminal scraping or keystrokes.

Documentation expectations were checked against the official
[hooks reference](https://learn.chatgpt.com/docs/hooks) and
[developer commands reference](https://learn.chatgpt.com/docs/developer-commands?surface=cli).
They are not observations of this installed build. In particular, the installed
queue help has no `--resume` option. Protocol and socket interpretation also used
the official [0.163 source tag](https://github.com/openai/codex/tree/rust-v0.163.0).
