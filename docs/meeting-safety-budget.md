# Meeting safety timing and disclosure

Refs #1505 and Jason's [accepted values](https://github.com/aceteam-ai/citadel-cli/issues/1505#issuecomment-6525494676).
The node's source contract lives in `meetingpolicy.Default`, `Profile.Budget`
and `Budget.CheckAuthority`. The shared numeric [fixture](meeting-pipeline-budget-fixtures.json)
is a producer/node test contract, not new wire fields or live acceptance.

`config.Meeting.Safety` accepts optional canonical decimal second overrides in
`meeting.yaml`. Keys match `meetingpolicy.Config.Resolve`; absent keys use the
ratified defaults, while malformed, zero, fractional, unknown or expanded values
refuse readiness. For example, `safety: {admission_max_seconds: "600"}` retains the
accepted admission maximum. Profiles may tighten limits but must remain internally
compatible. Larger allowances need a new owner decision. Unreadable or malformed
configuration cannot silently restore meeting readiness.

`MeetingJoinHandler.MeetingProfile` freezes the resolved policy for its worker
registration. `LegacyHandlerAdapter.meetingReadiness` withholds the capability
when the node's explicit long-job/STUCK ceiling is incompatible or its recovery
monitor is disabled. The worker validates each payload and caller deadline before
native work. An omitted job timeout uses the finite meeting profile; a shorter
explicit ceiling is refused. Other job types keep their existing watchdog tiers.
An explicit disabled STUCK check is incompatible; a disabled generic long-tier
watchdog does not make meetings unbounded.

`resolveExecutionBudget` fixes execution start before publication of the start
message. `executeWithDeadline` retains an uncooperative meeting handler until its
actual return, with the existing recovery action as backstop. Execution tickets
span actual handler exit and terminal publication. Uncertain teardown retains
that ticket and source ownership without ACK/NACK/rejoin. The monitor checks each
execution independently, so a long meeting or short-job completions cannot hide
another stuck execution. Intake is fenced before the existing non-idle restart.
No agent-driven restart or native fault drill accompanies these changes.

`MeetingCleanup` shares one allowance across rolling shutdown, recorder stop,
media teardown, backup and publication. Cancellation does not replenish it and
cleanup remains possible after the execution context is cancelled. Operations
that ignore cancellation retain actual custody; a timeout is not evidence of
resource removal. Phase contexts bound supported I/O and reject late successes.
Native uncooperative behavior remains an acceptance gate, not a claim that Go
can forcibly stop an arbitrary goroutine.

`MeetingJoinHandler.Execute` closes owned media before final transcription. The
existing meetingd `max_duration_seconds` is a session lifetime from creation, so
the node includes setup, admission and cleanup in that lifetime without extending
the recording cap. Final transcription receives its separate phase ceiling, and
its mutex acquisition is cancellable. A transcription timeout preserves the
truthful partial-result/audio behavior; it never fabricates a transcript, changes
model quality or chooses paid hosted inference.

`announceOnAdmission` resolves only the canonical presentation fields, registers
the exact dynamic announcement for echo suppression before posting, and requires
new chat readback before `disclosure_posted=true`. Plain and interactive Meet
both attempt disclosure. A failed/uncertain post records false and continues
recording. Plain mode also honors a standalone typed `/ace leave` without enabling
rolling inference. Teams chat remains in S5. Missing owner names use the accepted
generic fallback and never cause a profile/email/ID lookup.

## Paired gates remain

The pure capacity kernel in #1515 is not installed in any JobSource. Actual
pre-claim intake, incarnation/credit reconciliation, old-reader/backlog handling
and native resource/startup proof remain separate item 1 work with
aceteam#12916/#12931. Aceteam#12965 owns the paired owner-name/producer timeout and
wait changes, plus authoritative queue/handoff expiry before claim/execution.
This source does not infer those expiry proofs from node wall time, invent wire
fields or claim that numeric configuration enforces source expiry. Existing
legacy Redis reclaim floors are not proof of safe long-meeting custody.

Keep this source paired/held until those integration contracts and implementation
are independently verified. Fake page/media/source/clock/model tests establish
source behavior only. Full #1505 stays open/WIP; native meeting/audio/model/bot
sign-in, maximum-duration acceptance, counsel/customer offering, deployment and
activation remain held. There are no new flags, schema changes, credentials,
funding gates or audio-retention changes.
