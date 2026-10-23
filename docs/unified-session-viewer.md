# Explicit read-only local session viewer (#1118, partial S3)

`citadel session attach` views an **already-running** S2 session. This is not the
complete unified entrypoint. Bare `citadel`, legacy `citadel attach` (including
its shell option), Control Center, worker and foreground commands, session stop
and persistence installation retain their existing behavior. In particular,
bare interactive use does not select this viewer or require its status socket.
Whether a viewer should replace monitor-only Control Center is a separate
product decision, not implied by this source child.

This follows the founder correction in
[#1109 comment 5769548189](https://github.com/aceteam-ai/citadel-cli/issues/1109#issuecomment-5769548189):
the viewer is a client of the existing shared local control surface, not a new
socket, protocol, TUI relay or node owner. It is unrelated to the separate
declared-session epic #1165 and does not lift that epic's build gates.

## Source sequence and boundaries

1. Add only the explicit expert CLI leaf. Its exact root pre-run exception
   skips shared CA/bootstrap, interrupted-binary repair, invocation logging and
   update hooks. The original offline stream exception remains first and this
   explicit exception second; bare commands keep main's ordinary startup.
   Skipping those writes is deliberate for this read-only client, not a claim
   that imported package initialization is universally side-effect-free.
2. Resolve the destination through the same existing `network.GetStateDir`
   used by legacy local session commands. There is no separate HOME/SUDO_USER
   discovery algorithm, state observer, kernel-lock probe or bare startup
   authorization. Existing pointer/config reads and their legacy fallback
   semantics are unchanged; this child does not repair those semantics.
3. Preflight a fixed GET of the existing session status route at
   `LocalAPISocketPath` before opening a terminal. The per-call status client
   refuses redirects. The generic session client's redirect behavior and
   stop/status commands are unchanged. Permission, missing socket, malformed
   status, HTTP refusal or unknown transport error returns a static error and
   a next step through Cobra RunE; it never starts or stops a node.
4. Limit each request to three seconds and its body to 16 KiB. Require one UTF-8
   JSON object, no duplicate root members or trailing value, known mode,
   explicit connected/version/PID/uptime fields and bounded scalar values.
   Optional future fields remain compatible. Peer bodies, paths, parser
   diagnostics and arbitrary transport or terminal errors are not reflected.
5. Seed the separate minimal BubbleTea model with the preflight observation.
   Render only mode, connection observation, node name, mesh IP, version, PID
   and uptime. No ControlCenter collectors or callbacks are reused: no demo,
   terminal/VNC listener, job source, worker, heartbeat, telemetry, service
   action, settings edit, chat, peer connect, auth modal or mesh
   reconnect/disconnect. Text is quoted to literal ASCII before rendering.
   No styling/color or rendering-preference reads are used.
6. Poll sequentially, one request at a time, one second after each success.
   Error, observed PID/mode change or uptime regression clears old status and
   stops polling until explicit reattachment. A valid disconnected observation
   displays offline, not loss of the local session. Local request generations
   ignore late results; they are not a server-side identity fence. The command
   checks the model's current final unavailable state after terminal exit and
   returns a static status error, not a separate historical error latch.
7. q, Esc or Ctrl-C quits this viewer and cancels only its request context.
   Terminal exit also closes that context. Terminal-program initialization or
   runtime errors return a distinct static viewer error with a status-command
   fallback. No stop POST, session socket close, worklock cleanup, node signal
   or network disconnect callback is reachable.

## Legacy behavior and acceptance limits

A held worker with no usable controller still reaches the old enrolled-session
dispatch. Control Center's existing lock policy chooses monitor-only on
contention; its existing instance check retains the `citadel attach --shell`
guidance for a Control Center already open in another terminal. This change
does not put permission/unknown/controller errors in front of those paths.
Presence-mode dispatch is also unchanged.

Existing Windows pipe permissions remain owner, SYSTEM and Administrators. No
client-side admin check is added or claimed. Whether a non-admin owner should
be permitted under the historical admin-only-v1 decision remains an owner
policy question; this source child neither broadens nor narrows the shared
transport's authority.

PID plus observed uptime cannot prove an incarnation: a replacement reusing the
same PID and indistinguishable uptime can evade this observational check.
There is no generation nonce, authenticated process identity or stronger
ownership guarantee. Local socket authority is the existing OS policy, not
the displayed fields.

## Prepared evidence, not execution

Authored fixtures cover the actual fixed-route request builder with a fake
RoundTripper, redirect refusal and unchanged generic-client redirect behavior,
strict bounded response parsing and body closure, initial refusal before
terminal startup, distinct terminal errors, fixed selected target and seeded
baseline, exact root-leaf guards, and BubbleTea cancellation/poll/status
transitions. The actual command's source wiring pins the legacy state resolver,
fixed status requester and returned RunE error.

Bare-dispatch fixtures retain the original classification/session seam. A
separate fixture combines that seam with the actual Control Center contention
helper and a held temporary lock beside absent or unusable Unix controller
placeholders. It starts no listener or TUI and does not prove real EACCES,
cross-UID, Windows named-pipe ACL or live Control Center behavior. Source
comparison preserves bare dispatch and Control Center implementation from
main; source-shape guards are not claimed as full integration acceptance.
The root logger fixture stubs terminal detection deterministically.

No local candidate tests, formatter, vet, build, terminal, service, mesh or
model have been run for this correction. Existing exact-head CI and
independent source review must be recorded separately after publication.
Native Unix/Windows ACL, signal, terminal and process behavior remains gated.

Full #1118 still needs an accepted bare-entry preference, persistent detached
ownership/readiness, managed-service selection, activity events and
update/reconnect behavior, plus detach/continuous-online and multiple-viewer
acceptance. #1119 persistence UX/Windows integration, #1120 mode switching and
#1121 edge/native acceptance remain separate. This source-only draft does not
close #1118, start a node, install persistence, mint credentials or authorize
deployment/native execution.

The proposed mode-preserving `session serve` path remains held on the
[B1 restore-only dependency design](session-serve-restore-policy-design.md).
That comparison preserves the owner no-login/no-node-key invariant; it adds
neither the command nor a dependency patch.
