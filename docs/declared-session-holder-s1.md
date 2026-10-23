# Declared session holder: S1 source boundary

Refs #1175 and the settled #1165 definition of ready. This is source preparation
for a **draft**, not native acceptance, activation or rollout permission.
The operator authorized source work and ordinary GitHub CI on 2026-10-22.
This preparation lane does not perform local candidate execution,
PTY/socket/systemd experiments, service changes, fleet migration, undraft or
merge. Source verification does not satisfy the separate native acceptance gate.

## What this slice owns

A hidden `citadel session-host NAME` process owns a harness PTY independently of
the worker. Local `citadel sessions list|status|start|stop|restart|logs` and
`citadel attach NAME` target that declared holder, not the worker's existing
instance shell. No-argument `citadel attach`, `--shell`, `citadel session attach`,
and existing tmux sessions retain their existing behavior. This implementation
does not replace or modify `internal/tmux`.

`citadel attach NAME` now assigns meaning to the formerly ignored positional
argument: it selects an exact declared session. Omit NAME for the legacy worker
status view, or use `attach --shell` for the existing instance shell.

The new local-only commands deliberately bypass agent startup using their own
Cobra hook. They do not add a third exception to the root startup guard, log in,
mint a node key, repair credentials, install CA trust or auto-update. Configuration
is selected once at the CLI boundary; leaf packages receive an explicit path.
`--node-config-dir` selects an existing trusted node configuration directory.
The new named-session commands explicitly refuse the legacy `--node-dir` and
nonempty `CITADEL_NODE_DIR` overrides, including when `--node-config-dir` is also
given. They never silently fall back to the real node after an unsupported
override. The no-name legacy attach path keeps its existing selection behavior.

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

The launcher uses the fixed, metadata-validated system executable
`/usr/bin/systemd-run`, never the caller's PATH. Hosts with systemd-run only in
another location, including NixOS-style layouts, report `isolation_unavailable`;
this slice supplies no alternate-path lookup or fallback. A pre-exec failure retires its
pending reservation only after proving the same untouched reservation has no
captured holder and the exact named scope is empty. Unknown observations,
changed records or a positive launcher PID remain held as `starting`; a timeout
alone never authorizes a second launch. A proven unlaunched
`isolation_unavailable` record permits another explicit start/restart attempt.

If the CLI or launcher exits while a reservation is still `starting`, Start
does not automatically launch another holder. Recover explicitly with
`citadel sessions stop NAME`, then `citadel sessions start NAME`; do not delete
`state.json` by hand. For a holderless reservation, Stop must first own the
existing operation and holder guards and prove the exact named scope entirely
absent. It rechecks the unchanged reservation under the record lock, persists
the stop veto, then retires that reservation without claiming log flush. A late
holder refuses the veto or terminal phase. Unknown scope observations, a live
owner or a changed reservation still refuse. If persistence is interrupted after
the veto but before retirement, repeat Stop; Start cannot clear that partially
completed holderless-start recovery. Status labels the known intermediate record
`recovery_incomplete`; status/start/restart give the explicit Stop-then-Start
command hint. A captured holder instead uses the existing
exact process-birth/scope cleanup, with the same Stop-then-Start recovery.

An old live holder can still be attached after a CLI update when its protocol is
compatible. The saved record, authenticated hello and live state must corroborate
the same holder binary identity; it need not equal the new CLI's release.
`holder_outdated` remains an informational status, not a restart instruction.
This slice uses protocol version 2 for the new failure phases. Version 1 and 2
clients/holders do not attach across versions; a downgrade cannot interpret the
new phases as a compatible live session. Prefer an explicit stop with the matching
CLI before changing versions. A new CLI may explicitly retire only the known
holderless `starting` record shapes from versions 1 and 2 under the same exact
empty-scope/owner-lock/veto checks; unknown protocol records remain held.

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
Record-lock contention gets a bounded retry rather than being mistaken for
corrupt storage. Initial holder publication retains the five-second acquisition
budget because it is fatal on failure and may race the manager's launch record.
Only after that publication, before sharing the handle, actor record writes use
at most 50 ms to acquire the lock and retain exact pending data for retry on busy.
Ordinary manager record operations retain the five-second budget. This bounds only
lock contention, not filesystem open, write or fsync latency. Console writes are batched within fixed memory/time ceilings;
normal terminal completion still waits for pending logs/events and final state
to be durably published. An uncertain append is not automatically replayed.

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
The client opens validated independent terminal file descriptions rather than
changing shared `O_NONBLOCK` flags. Normal exits restore raw terminal state;
SIGKILL, power loss and other uncatchable termination cannot restore shared
termios state, so native interruption acceptance remains required.

Output is event-driven with a bounded ready-frame burst, not limited to one frame
per stdin poll. A holder-side EOF, fatal operation refusal or slow-viewer disconnect is
a nonzero client result. Diagnostic reasons come from a fixed allowlist and never
reflect arbitrary peer text or terminal bytes. Explicit local detach remains a
successful viewer-only action.

Replay snapshot and live registration share one actor-owned boundary. Whole
output chunks are retained within the ring budget. Each viewer has a bounded
queue; a slow viewer is disconnected without blocking the harness. Controller
transfer fences queued/in-flight old input before a new grant. Resize requests
and input are checked against current controller and child generation.
If a valid input or resize frame loses its controller grant, arrives during
restart wait, only that role-refused frame is
dropped. The attachment remains connected for output, role changes and heartbeat;
the frame is not acknowledged, queued for replay or automatically retried. A
busy input/resize queue instead disconnects with the static `busy` reason: the
same connection cannot forward later paste chunks after dropping a middle chunk.
This does not add delivery acknowledgements or make prior queued bytes atomic.
A read-only role transition displays a static local notice that input is no
longer forwarded. A refused take-control operation, malformed frame, stopped holder or slow viewer
keeps the existing disconnect behavior. Real role-transfer and saturated-paste
behavior still require native acceptance.
Child cleanup revokes every viewer to read-only. S1 does not implicitly restore
a controller after a child restart; an eligible operator can detach and explicitly
reattach with `--take-control` to obtain a fresh-generation grant.

The holder retains process-group identity until descendant cleanup and scope
proof, reaps the leader once, and does not close/reuse PTY descriptors while I/O
is in flight. Unknown survivors or uncertain cleanup quarantine the holder;
they never authorize another writer or an automatic replacement.
An explicit local stop/restart can recover an authenticated quarantined holder
through its saved exact invocation scope. This forced path is labelled
`forced_cleanup_no_flush`, not a graceful completion or proof of log durability.
Status uses `cleanup_unconfirmed` for uncertain child cleanup, `spawn_failed`
for a failed automatic respawn with no child, and `event_queue_overflow` for
exhausted pending event capacity. All three retain quarantine and custody; none
authorizes automatic relaunch or a graceful flush acknowledgement. The explicit
manager recovery path remains exact-scope cleanup with no flush claim.

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
