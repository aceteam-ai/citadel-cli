# E: reading thread status from a local process

Version: **codex-cli 0.163.0**, observed 2026-11-19.
Method and isolation: [common receipt](README.md#common-method).

Executed server argv:

```json
["<CODEX>", "app-server", "--stdio"]
```

The client sent read-only status requests over its own pipe:

```json
{"method":"thread/read","params":{"threadId":"<CANARY_THREAD>","includeTurns":false}}
```

Observed statuses before a fake request, while the fake model response was
held, and after releasing it:

```json
{"type":"idle"}
{"type":"active","activeFlags":[]}
{"type":"idle"}
```

This verifies the status shape and live transition on **one private stdio
connection**. It does not verify an independent process attaching to an already
running daemon. That path remains **unverified**: the private Unix listener
refused its protected rendezvous directory inside the agent sandbox.

The operator canary should attach a separate initialized reader to its own
daemon and repeat thread/read across idle and active states. It must not create
a second server/writer for an existing interactive thread or use `--remote`.
No observer argv is claimed as executed for that missing test.

L3 consequence: status is useful liveness evidence but not a lease/incarnation,
human-draft or processed-work fence. L4 consequence: do not ship a daemon-status
integration on schema/stdio evidence alone; retain the L0 gate.
