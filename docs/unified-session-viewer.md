# Read-only local session viewer (#1118, partial S3)

`citadel session attach` views an **already-running** S2 session. Interactive,
zero-argument bare `citadel` also selects that read-only viewer when checked
discovery returns valid existing-session status. This is not the complete unified
entrypoint. Legacy `citadel attach` (including its shell option), worker and
foreground commands, session stop and persistence installation are unchanged.
Confirmed no-owner bare invocations retain the existing enrollment/foreground
flow; non-interactive bare invocations retain their status-only dispatch.

This source child follows the founder correction in
[#1109 comment 5769548189](https://github.com/aceteam-ai/citadel-cli/issues/1109#issuecomment-5769548189):
the viewer is a client of the existing shared local control surface, not a new
socket, protocol, TUI relay or node owner. It is unrelated to the separate
declared-session epic #1165 and does not lift that epic's build gates.

## Source sequence and boundaries

1. Add an explicit expert CLI leaf. Its exact root pre-run exception skips the
   shared CA/bootstrap, interrupted-binary repair, invocation logging and update
   hooks. `Execute` still performs ordinary command registration/dispatch; this
   is not a claim that all imported package initialization is side-effect-free.
   The original offline stream exception stays first and this expert exception
   stays second. Only exact zero-argument interactive bare calls defer the same
   shared startup function into their invocation-local discovery path; expert
   and non-TTY commands do not receive a blanket startup exemption.
2. Read status only through a fixed GET of the existing session status route at
   `LocalAPISocketPath`. The new per-call status client refuses redirects. The
   existing generic session client's redirect behavior and stop/status commands
   are unchanged. State-directory resolution still reads its existing local
   pointer/config inputs. No loopback HTTP fallback, credential authentication
   flow or token use, new ACL,
   endpoint or wire field is introduced. Existing Windows pipe permissions
   remain owner, SYSTEM and Administrators; no new client-side admin check is
   implied. Permission denial or absence is not authority to launch anything.
3. Limit each request to three seconds and its body to 16 KiB. Require one UTF-8
   JSON object, no duplicate root members or trailing value, known mode, explicit
   connected/version/PID/uptime fields and bounded scalar values. Optional future
   fields remain compatible. Do not display peer bodies or arbitrary error text.
4. Render only mode, connection observation, node name, mesh IP, version, PID
   and uptime in a separate minimal BubbleTea model. It has no ControlCenter
   collectors or callbacks: no demo/terminal/VNC listener, job source, worker,
   heartbeat, telemetry, service action, permission/settings edit, chat, peer
   connect, auth modal or mesh reconnect/disconnect. Text is quoted to literal
   ASCII before rendering; no styling/color is used, including with NO_COLOR or
   the global no-color option. It does not load rendering preferences.
5. Poll sequentially, one request at a time, one second after each success.
   Error, observed PID/mode change or uptime regression clears all old status
   fields and stops polling. The screen reports unavailable/changed and asks for
   explicit detach/reattach. A valid disconnected mesh observation is displayed
   as offline, not mistaken for loss of the local session. Local request
   generations ignore late results; they are not a server-side identity fence.
6. q, Esc or Ctrl-C quits this viewer and cancels only its request context.
   Terminal exit also closes that context. No stop POST, session socket close,
   worklock cleanup, node signal or network disconnect callback is reachable.

## Checked bare discovery (partial S3 Phase A)

`network.ObserveStateDir` is a new observational resolver, not a change to legacy
`GetStateDir`, `HasState` or their callers. Valid-input precedence stays machine
pointer, the existing ordered config candidates, then owner-home legacy/fresh
fallback. A valid higher-priority path returns before lower credential config
or owner-home reads. Missing files or an absent config member may continue;
unreadable/malformed data, failed owner lookup, explicit invalid/empty/relative
paths and ambiguous filesystem observations refuse rather than choose another
directory. Windows unusable system-profile/ProgramData inputs also refuse in
this observer rather than silently guide enrollment elsewhere. The proposed
16 KiB pointer and 1 MiB config read caps are refusal bounds for this new bare
observer only, not ratified global legacy resolver limits.

One selected path is retained for status, lock observation, state inspection and
the attached viewer. Valid protected S2 status is enough to attach without
enumerating enrollment files. It is not proof of backend/job readiness. An
unreadable discovery input cannot reveal an otherwise undiscoverable hidden
Unix session socket; that invocation refuses rather than assuming absence.

Only a typed missing local endpoint (or Unix connection-refused dial), with no
HTTP response, proceeds to `worklock.ProbeContention`. HTTP errors including
404, redirects, malformed/TUN status, permission/busy/timeout and unknown errors
refuse without CA/bootstrap, repair, logs, updater, enrollment or owner startup.
The new existing-file-only lock probe observes kernel contention independent
of lock-record PID/liveness. It never creates, reclaims, writes or unlinks a
lock. Unexpected open/type/identity/probe/unlock/close errors are unknown, not
unheld; legacy `IsHeld` and `Acquire` remain unchanged.

Only endpoint absence plus an unheld probe plus a successful checked state
presence observation permits the existing startup function once. Presence uses
the legacy nonempty-directory predicate, not key validation. Its checked result
feeds enrollment classification directly, rather than reusing false-on-error
`HasState`. Readable missing/empty state keeps the existing guided enrollment;
present state keeps the existing persisted-mode foreground dispatch. The viewer
is seeded with its validated preflight snapshot so a different observed owner
at its first poll cannot be silently adopted. No cross-hook context, global
decision or cached startup authorization is retained between invocations.

These are snapshots, not an ownership lease or a new authentication mechanism.
A new owner may appear after the probe, and unchanged legacy startup/foreground
paths may resolve inputs again. The existing foreground `Acquire` remains the
ownership guard; this slice does not serialize all startup effects against such
races or promise that a successful observation proves future access/writes.
Filesystem operations also have no hard deadline guarantee.

## Prepared evidence, not execution

Hermetic source fixtures cover the actual fixed-route request builder with a
fake RoundTripper (including redirect refusal and unchanged generic-client
redirect behavior), injected response/request/terminal boundaries, strict
response parsing and body closure, exact root-leaf early return, and BubbleTea
messages for detach, sequential polling, late responses, observed owner changes,
offline observations and hostile terminal text. Bare-selection fixtures inject
resolver/request/probe/presence/startup/viewer boundaries, including all refusal
classes, checked presence, cancellation, exact startup deferral and repeated
invocation independence. Checked-discovery and platform probe fixtures record
their own source-only evidence separately. They start no terminal, socket,
listener, Redis process, service, mesh or model. No local candidate test,
formatter, vet or build has been run for this child. Normal exact-head CI and
independent source review must be recorded separately after publication.

## Still outside this child

PID plus observed uptime cannot prove an incarnation: a replacement reusing the
same PID and indistinguishable uptime can evade this observational check. No
generation nonce, authenticated process identity or stronger ownership guarantee
is invented. Local socket authority is the existing OS policy, not the displayed
fields. Native Unix/Windows ACL, signal, terminal and process behavior is not
proved by pure fixtures or cross-compilation.

Full #1118 still needs persistent detached start/ownership,
managed-service selection, activity events and update/reconnect behavior, plus
the detach/continuous-online and multiple-viewer acceptance. #1119 persistence
UX/Windows integration, #1120 mode switching and #1121 edge/native acceptance
remain separate. This source-only draft does not close #1118, start a node,
install persistence, mint credentials or authorize deployment/native execution.
