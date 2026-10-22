# Read-only local session viewer (#1118, partial S3)

`citadel session attach` views an **already-running** S2 session. It does not
implement the complete unified entrypoint. Bare `citadel`, legacy
`citadel attach` (including its shell option), worker and foreground commands,
session stop, persistence installation and enrollment keep their existing paths.

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

## Prepared evidence, not execution

Hermetic source fixtures cover the actual fixed-route request builder with a
fake RoundTripper (including redirect refusal and unchanged generic-client
redirect behavior), injected response/request/terminal boundaries, strict
response parsing and body closure, exact root-leaf early return, and BubbleTea
messages for detach, sequential polling, late responses, observed owner changes,
offline observations and hostile terminal text. They start no terminal, socket,
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

Full #1118 still needs bare start-or-attach, persistent detached ownership,
managed-service selection, activity events and update/reconnect behavior, plus
the detach/continuous-online and multiple-viewer acceptance. #1119 persistence
UX/Windows integration, #1120 mode switching and #1121 edge/native acceptance
remain separate. This source-only draft does not close #1118, start a node,
install persistence, mint credentials or authorize deployment/native execution.
