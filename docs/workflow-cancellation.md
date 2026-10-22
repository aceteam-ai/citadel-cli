# Workflow cancellation (#1317)

`Executor.Submit` creates and stores the per-run cancellation function before
publishing the run. `Cancel` requests cancellation without holding the execution
mutex while context callbacks run. The existing immediate cancelled status and
timestamp are retained as cancellation-request metadata; late node completion
or startup must not resurrect/overwrite a terminal state.

`Execution.Done` is the separate actual-exit signal: it closes only after the
executor goroutine has returned and released its executor run slot. The
`workflow.Handler.Execute` goroutine waits for it on cancellation and all
terminal observations. A node that ignores its context therefore retains that
executor slot and handler goroutine, not the runner's execution lane.
`worker.Runner.executeWithDeadline` waits for actual handler exit only for
heavy-lane handlers; WORKFLOW_RUN does not declare that lane. The runner can
publish terminal failure and acknowledge the job before this handler returns.
This change does not add runner-level exit fencing or alter scheduling lanes.

Linux/macOS Shell commands get a dedicated process group. The current
CommandContext cancellation hook targets that group, not just `/bin/sh`, while
the direct shell is still being waited on. There is an unresolved gap: once
the shell is reaped, an ordinary background child can retain the output pipe
while the CommandContext watcher has stopped. Cancellation and run timeout do
not then invoke the group hook. See the repair constraints below; dedicated
group creation alone is not proof that this case is fixed.

Context errors remain failures instead of becoming successful outputs with a
negative exit code. Normal nonzero command exits preserve their existing
result contract. Shell permission/passcode checks still apply before launch.
Normal completion does not kill descendants that have closed their output
descriptors, and the dedicated group is no longer the terminal's foreground
group. Interactive termination therefore relies on Citadel delivering context
cancellation, not direct terminal signal delivery to shell children.

This is not a sandbox, cgroup/job-object containment or a guarantee about
commands deliberately daemonizing into a different process group. Detached
privileged shell work and other callbacks that ignore cancellation require
separate containment/custody design; do not infer their exit from this signal.
Windows/other platforms retain the existing /bin/sh requirement and parent
CommandContext behavior; no native Windows process-tree guarantee is added.

## Retained-output cancellation repair constraints

Any repair must keep cancellation observable through the entire output drain,
including after the shell exits. An arbitrary pipe wait deadline is not an
exit proof and would truncate legitimate late background output on normal
completion. A watcher that signals a numeric process group after reaping its
leader also needs explicit protection against PID/group reuse.

The proposed Linux design retains the direct child without reaping it until
the output pipe has reached EOF, observes exit without reaping through
`waitid` with `WEXITED` and `WNOWAIT`, joins the cancellation watcher, and only
then calls the sole `Cmd.Wait`. This preserves the leader's PID while group
signalling is possible. It requires exclusive wait ownership of that child;
no global reaper may consume it. Interrupted observations must retry. Start,
pipe, observer and signal failures need explicit cleanup and custody behavior,
never a successful fallback that silently loses the identity anchor.

This is a proposal, not implemented behavior. Darwin cannot blindly reuse the
Linux observer: Go's `os/wait_waitid.go` records a Darwin waitid issue where a
stopped process can appear waitable. A platform-specific, non-reaping exit
observer and its registration/exit race need separate review. Neither native
macOS nor Windows acceptance follows from cross-compilation.

Regressions must include a shell that exits immediately while a same-group
child retains the output pipe, covering both explicit cancellation and run
timeout. Observe the original shell's unreaped exited state before triggering
cancellation; a fixed sleep is not that barrier. Preserve PID and birth-time
identity for cleanup. A successful uncancelled background writer must retain
its complete output. Fake ordering tests must verify watcher shutdown before
reaping, EOF and exit-observation ordering, interrupted observations, and
start/observer/signal error cleanup without claiming escaped descendants are
contained.
