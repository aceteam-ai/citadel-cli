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
| [A: queue and UserPromptSubmit](a-user-prompt-submit.md) | Actual CLI/private Unix daemon operator canary passes |
| [B: Stop continuation](b-stop-continuation.md) | Private stdio and native Unix daemon canaries pass |
| [C: paused queue](c-paused-queue.md) | Native breaker-blocked goal accepts one queued turn; reconnect pause and other stalled states unverified |
| [D: human composer draft](d-human-draft.md) | Unverified; requires operator canary |
| [E: local daemon status](e-thread-status.md) | Separate-process native Unix daemon reader passes |

## Common method

### Initial private stdio positive controls

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

### Operator-owned native Unix daemon receipt

On 2026-11-19, the operator ran the reviewed r2 script outside the agent
sandbox. The [raw redacted receipt](https://github.com/aceteam-ai/citadel-cli/pull/1818#issuecomment-7010738136)
records codex-cli 0.163.0, successful initialization over the private Unix
socket, A/B/E passing, exit 0, every created process reaped and removal of the
temporary directory. No lane ran this native script from the sandbox.

The [exact operator script](https://github.com/aceteam-ai/citadel-cli/pull/1818#issuecomment-7010181248)
has SHA-256
`0b1fb7daddb145c612941473e0653cb301fc3cbae85a68aa7d2e0a62976bf6f9`.
Its environment starts empty, with new HOME, CODEX_HOME and XDG directories,
a private working directory and no credentials. The same fake-provider and
hash-reviewed hook fixtures apply. Landlock's TCP-port guard is installed
before all Codex children; it is not a UDP/DNS isolation claim.

The controller performs the HTTP Upgrade and masked WebSocket framing on the
new private socket, then validates `initialize.codexHome` and config/hook
origins. The separate observer launches with:

```json
["/usr/bin/python3", "-I", "<CANARY_ROOT>/private-ws-client.py", "<PRIVATE_CONTROL_SOCKET>"]
```

That observer uses the same framed transport source and only initializes and
reads the canary's status. The actual `codex queue` CLI targets only the newly
created canary UUID. Cleanup signals only process groups created by the script;
no global daemon lifecycle command or existing lane session is used.

The earlier r1 operator script failed at initialize because it mistakenly
treated `app-server proxy` as a framing adapter. In 0.163 that proxy relays raw
bytes. Its raw JSONL did not perform the listener's required HTTP Upgrade.
The r2 script fixes that transport mismatch without weakening the TCP guard.
The failed r1 run is not acceptance evidence.

## What remains

The operator receipts clear A, B and E for this private, fake-provider native
daemon canary and establish one narrower C case. In the
[native r3 receipt](https://github.com/aceteam-ai/citadel-cli/pull/1818#issuecomment-7012245641),
the [reviewed r3 script](https://github.com/aceteam-ai/citadel-cli/pull/1818#issuecomment-7011199590)
with SHA-256
`53cc83d4d2a6528cb9861688cd0323936f6e352c3e4608964de096e6bbb81955`
first observes the goal blocked by three empty final answers, then queues once.
During its configured six-second polling observation the receipt records one model call,
one UserPromptSubmit, zero pending items and a goal that remains blocked.
The queued turn uses the same empty-final-answer fixture. The poll-window field
is not a measured latency or end-to-end deadline. Exit 0 and clean cleanup are
recorded; A/B/E reproduced. No lane ran r3.

A blocked goal therefore does not imply a parked queued wake in this case;
turn delivery and goal continuation are distinct. L3 must use actual correlated
turn/consumption evidence, not infer ignored or consumed from goal status or
queue acknowledgement. Existing wake eligibility refusals and caps remain;
this is not permission to wake blocked lanes or resume their goals.

This does not prove all interactive lifecycle states or clear L0. C's TUI
reconnect pause and other stalled states and D's unsent human composer remain
unverified. Reconnect pause and the composer need operator-owned interactive
observations. The D note gives a manual fake-sentinel procedure for Owner; it
does not authorize a lane to inspect or manipulate his terminal.
Keep [L4 #1817](https://github.com/aceteam-ai/citadel-cli/issues/1817) gated.
Do not replace missing observations with a second writer, `--remote`, PTY reads,
terminal scraping or keystrokes.

Documentation expectations were checked against the official
[hooks reference](https://learn.chatgpt.com/docs/hooks) and
[developer commands reference](https://learn.chatgpt.com/docs/developer-commands?surface=cli).
They are not observations of this installed build. In particular, the installed
queue help has no `--resume` option. Protocol and socket interpretation also used
the official [0.163 source tag](https://github.com/openai/codex/tree/rust-v0.163.0).
