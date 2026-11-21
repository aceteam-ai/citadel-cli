# D: queue versus unsent human composer text

Version: **codex-cli 0.163.0**, inspected 2026-11-19.
Status: **unverified**. No TUI composer was opened, read or written.

Executed inspection argv:

```json
["<CODEX>", "queue", "--help"]
["<CODEX>", "app-server", "generate-json-schema", "--experimental", "--out", "<SCHEMA_DIR>"]
```

Neither this inspection nor an idle thread/read result proves that the human
composer is empty. The private stdio canary has no composer and cannot substitute
for this test. No PTY, tmux or keystroke mechanism was used to manufacture one.

An operator must enter harmless unsent text in a throwaway interactive canary,
then run this argv from a separate local process and record whether that draft
is preserved, submitted, displaced or blocks processing:

```json
["<CODEX>", "queue", "--thread", "<CANARY_THREAD>", "--message", "ping from probe"]
```

L3 consequence: idle is not evidence of no human draft. Unknown human-driving
state must not be treated as permission to wake a personal/undeclared session.
L4 consequence: keep the human-driving refusal/policy boundary and L0 gate;
never inspect terminal contents or inject a key to fill this evidence gap.

## What Jason would need to do

This needs an operator-owned interactive canary, not another headless daemon
probe. It is a proposed manual procedure, not a run or a new authorization:

1. Explicitly authorize the D TUI/composer observation. Use codex-cli 0.163.0,
   a newly isolated HOME, CODEX_HOME and XDG set, a new working directory, a
   credential-free fake provider, and only new canary threads. Keep the fake
   endpoint guard, reviewed fixture hooks, bounded observation and own-process
   cleanup from the reviewed native canary. Do not use a real lane, real model,
   auth file, personal draft, global daemon command or existing thread.
2. Open that new canary in the actual native TUI. Jason manually types a unique
   fake sentinel, for example `unsent composer sentinel from probe`, and leaves
   it unsent. Record the version and that the canary is idle with this draft
   visibly present. The agent must not read, scrape or write the terminal.
3. From a separate operator terminal using the same isolated environment, run
   the one-shot queue argv above against only that new canary UUID. Record CLI
   exit/acknowledgement separately from model-request, UserPromptSubmit, pending
   count and thread-status observations. Never retry an uncertain queue.
4. Jason visually checks whether the sentinel remains byte-for-byte editable,
   was cleared, replaced, merged with the queued text, or submitted. The fake
   provider can check exact sentinel presence in memory and emit booleans only,
   without logging request contents. Distinguish a preserved draft with a
   processed queue from a preserved draft that blocks the queue. Do not infer
   composer preservation from a hook, status read or absence of model traffic.
5. After the first bounded observation, Jason may manually submit the sentinel
   once to prove it remains his separate draft. Record that optional action's
   timestamp and additional request separately; it must not be attributed to
   the earlier queued wake. Stop and clean up only the created canary processes
   and private directory. Report observations and counts, not terminal contents,
   thread identifiers, paths or raw model bodies.

A busy-thread variant would require its own recorded initial state and receipt;
the idle-draft result alone cannot establish all races. If the isolated TUI
cannot be safely started, leave D unverified. No automated PTY, tmux or
keystroke fallback is an acceptable substitute.

C reconnect pause is a separate remaining interactive gap; completing D alone
would not clear it or complete L0. The literal Jason gate in #1818 remains for
the coordinator or Jason to update, not this lane.
