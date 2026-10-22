# Declared session holder: S1 source boundary

Refs #1175 and the settled #1165 definition of ready. This is source preparation
for a **draft**, not native acceptance, activation or rollout permission.
The operator authorized source work and ordinary GitHub CI on 2026-10-22.
Local candidate execution, PTYs/sockets/systemd experiments, service changes,
fleet migration, undraft and merge remain held.

## What this slice owns

A hidden `citadel session-host NAME` process owns a harness PTY independently of
the worker. Local `citadel sessions list|status|start|stop|restart|logs` and
`citadel attach NAME` target that declared holder, not the worker's existing
instance shell. No-argument `citadel attach`, `--shell`, `citadel session attach`,
and existing tmux sessions retain their existing behavior. This implementation
does not replace or modify `internal/tmux`.

The new local-only commands deliberately bypass agent startup using their own
Cobra hook. They do not add a third exception to the root startup guard, log in,
mint a node key, repair credentials, install CA trust or auto-update. Configuration
is selected once at the CLI boundary; leaf packages receive an explicit path.
`--node-config-dir` selects an existing trusted node configuration directory.

Only canonical lowercase lease names are accepted:
`[a-z0-9][a-z0-9._-]{0,63}`. There is no trimming, case folding or lossy scope
slugging. The stable unit name is exactly `citadel-sess-NAME.scope`.

## Required isolation; no fallback

The first backend is non-root Linux with a functioning same-user systemd manager,
unified cgroup v2, and the invocation-addressed systemd D-Bus operations required
by the source implementation. Availability on any particular host is unproven.
Unsupported kernels, legacy cgroup v1, missing user buses or uncertain scope
identity fail closed as unavailable. Non-Linux commands refuse explicitly.
There is no fallback to direct execution, root/system scopes, tmux or a randomly
named unit. A weaker non-systemd foreground mode is **not implemented in S1**.

The holder verifies its own process birth and exact independent user scope before
creating session storage, binding a socket or launching a harness. A launcher's
`Start` returning successfully does not prove scope ownership or holder readiness.
Readiness requires authenticated local hello, exact reserved incarnation, process
birth, declaration revision and independently observed scope membership.

Stored PID files are observations, not signalling authority. Destructive cleanup
uses a pinned private user-bus manager and the saved nonzero invocation ID, never
a stable-name stop fallback. `Ref` is a GC pin, **not** invocation CAS. Unit stop
uses the invocation-addressed object returned by `GetUnitByInvocationID`; the
method reply is not job completion. Ambiguous replies or manager turnover never
authorize a destructive retry. Empty old scope and disappearance of the old
process birth are required before a new incarnation.

This boundary assumes cooperating Citadel processes and a non-malicious owning
UID. It is not a sandbox against that UID, root or hostile user-manager changes.

## Configuration and storage

`sessions.yaml` is a private 0600 regular file owned by the service UID in the
selected node configuration directory. The configured root and its ancestors
are trusted; aliases there are allowed. Beneath that pinned root, session
directories are 0700 and regular files are 0600, same UID, single-link and opened
without following final symlinks. Unsafe existing metadata is refused, not repaired.
Read-only list/status/log paths do not create session directories.

The whole YAML document is validated before use: duplicate/unknown fields,
aliases, wrong types, duplicate names and invalid bounds are refused. Defaults
match #1165. `command` is argv, not shell text; `cwd` is absolute. Missing
`command_restart` reuses the exact original argv, not guessed resume arguments.
`headless` can be represented but S1 refuses to launch it.

Harness environments are built from a closed essential set, checked `env_file`,
then explicit declaration `env`, followed by forced user/session/machine identity.
`path_prepend` is applied to the checked essential PATH. No ambient device
credential environment is copied. Identity/PATH/known node-credential overrides
are refused. `env_file` is a bounded private file using literal `KEY=VALUE` lines:
no shell evaluation, exports, expansion or quoted-value syntax. Diagnostics expose
resolved executable and environment hash, not argv, environment values or tokens.

### Durable clean-stop overlay

S1 never rewrites human-edited YAML. A clean on-failure exit writes and syncs a
per-name `stop.json` marker **before** publishing `stopped_clean`. A changed
declaration revision turns that veto into `config_conflict`; it never silently
clears the veto. Only an explicit local start/restart may clear it after proving
the old holder and exact scope gone, under the operator lock.

This overlay provides S1's manual-manager stop behavior. It is not a claim that
literal `enabled: false` YAML editing or S2 reboot reconciliation is complete.
Any later reconciler must honor the per-name veto before spawning. Human-editor
compare-and-swap and automatic YAML rewriting remain outside this slice.

Operator, holder-lifetime and short record locks are distinct. A holder does not
take the operator lock while a manager waits for its stop/readiness response.
Record writes sync a private staging file, rename, then sync the directory. A
post-rename sync failure is uncertainty, not rollback or durable success.

## Attachment, custody and logs

The local 0600 Unix socket requires same-UID peer credentials in both directions.
The client corroborates the holder's saved identity; manager attachment additionally
checks the same live client against process birth, scope and declaration policy.
There is one controller. A second interactive client is a viewer until an explicit
eligible `--take-control`; `--readonly` is an immutable input ceiling.
The holder enforces policy, not just CLI flags.

The default detach chord is Ctrl+] followed by `d`; `--detach-keys 'C-a z'` is an
example override. A mismatched prefix is forwarded unchanged. The detach chord
and following bytes in the same read are not forwarded. Read-only clients still
recognize detach. Client EOF/cancellation closes that viewer only, never the
harness. Terminal state/status flags and reader lifetime are restored/joined.

Replay snapshot and live registration share one actor-owned boundary. Whole
output chunks are retained within the ring budget. Each viewer has a bounded
queue; a slow viewer is disconnected without blocking the harness. Controller
transfer fences queued/in-flight old input before a new grant. Resize requests
and input are checked against current controller and child generation.
Child cleanup revokes every viewer to read-only. S1 does not implicitly restore
a controller after a child restart; an eligible operator can detach and explicitly
reattach with `--take-control` to obtain a fresh-generation grant.

The holder retains process-group identity until descendant cleanup and scope
proof, reaps the leader once, and does not close/reuse PTY descriptors while I/O
is in flight. Unknown survivors or uncertain cleanup quarantine the holder;
they never authorize another writer or an automatic replacement.

Raw `console.log` is sensitive local history. `events.jsonl` accepts finite
lifecycle fields and static reasons, not console text or arbitrary errors.
Both rotate under configured private bounds. `sessions logs NAME --events`
selects events; default output is sensitive console. `--bytes` reads a bounded
tail of the current file only. There is no support-bundle upload or remote log API.

## Source ceilings and acceptance still required

New defensive ceilings are implementation proposals, not hardware evidence:

- Config 1 MiB / 64 sessions; argv 64 items, 8 KiB each / 64 KiB total.
- Environment 128 keys / 64 KiB; env file 64 KiB; 32 prepend paths / 4 KiB each.
- Viewers 1–32; replay 0–4 MiB; logs 4 KiB–64 MiB, 1–16 archives.
- Frames 64 KiB; input 8 KiB; output chunks 16 KiB; 32 live chunks per viewer.
- Replay also retains at most 1,024 whole chunks; JSON nesting is limited to 16.
- Five-second protocol operations; 15-second heartbeat / 45-second read bound.
- CLI lifecycle wait 30 seconds; slow terminal output detaches after five seconds.

Prepared fixtures and ordinary GitHub CI do not prove native scope containment,
reboot/worker-restart survival, multi-client terminal behavior, signal/process-group
cleanup, real storage-failure durability or host compatibility. Those acceptance
gates remain open. S2 worker/reboot reconciliation, S3 attestation/leases, remote
and web attachment, headless execution, fleet migration and service activation
are not included. The existing console/passcode/remote gates are unchanged.

The invocation fence is based on the reviewed
[systemd v255 manager API](https://github.com/systemd/systemd/blob/v255/src/core/dbus-manager.c),
[invocation path generation](https://github.com/systemd/systemd/blob/v255/src/core/unit.c),
and [object-label escaping](https://github.com/systemd/systemd/blob/v255/src/basic/bus-label.c).
These references establish source semantics, not the installed fleet baseline.
