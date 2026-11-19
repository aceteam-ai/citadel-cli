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
