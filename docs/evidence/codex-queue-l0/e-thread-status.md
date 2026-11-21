# E: reading thread status from a local process

Version: **codex-cli 0.163.0**, observed 2026-11-19.
Method and isolation: [common receipt](README.md#common-method).

Initial positive-control server argv:

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

The initial run verified this transition on one private stdio connection.
The later [operator-owned r2 receipt](https://github.com/aceteam-ai/citadel-cli/pull/1818#issuecomment-7010738136)
verifies a **separate process** attaching to the new private Unix daemon outside
the sandbox. Server and observer argv were:

```json
["<CODEX>", "app-server", "--listen", "unix://"]
["/usr/bin/python3", "-I", "<CANARY_ROOT>/private-ws-client.py", "<PRIVATE_CONTROL_SOCKET>"]
```

The observer used the script's HTTP Upgrade/WebSocket adapter, initialized with
experimentalApi true, confirmed the private codexHome, then read only the newly
created canary thread with includeTurns false. It observed idle, active while
the fake response was held, then idle after release. No thread/resume, second
server, existing interactive thread, PTY or remote endpoint was used. The common
receipt records script identity and cleanup. This is not a live fleet observer
implementation or evidence about human composer contents.

L3 consequence: status is useful liveness evidence but not a lease/incarnation,
human-draft or processed-work fence. L4 consequence: E's independent daemon
reader gap is cleared for this native canary; C and D still retain the L0 gate.
