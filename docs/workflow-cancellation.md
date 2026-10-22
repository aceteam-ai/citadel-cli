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

Linux/macOS Shell commands get a dedicated process group.
`workflowShellCombinedOutput` owns cancellation through the complete `Cmd.Wait`,
including the output drain after the direct shell has been reaped. Cancellation
targets that group, not just `/bin/sh`, so an ordinary background child holding
the output pipe remains reachable. The built-in CommandContext watcher is
disabled deliberately; its pre-start cancelled-context check is retained.

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

## Retained-output cancellation and its limits

The helper starts exactly one `Cmd.Wait` goroutine and watches that result and
the context in the calling goroutine. A completed Wait is preferred when both
signals are ready. Otherwise cancellation sends one process-group SIGKILL and
then waits for the sole Wait result, even if signalling fails. No watcher can
send a later group signal after the helper returns. A start failure creates no
Wait goroutine. The shared output buffer is read only after Wait has finished
its output copying.

There is no pipe wait deadline, forced descriptor close or `WaitDelay` that
would truncate legitimate late output from an uncancelled background writer.
Nonzero command exits preserve their existing contract. A failed group signal
does not fabricate completion: the helper can remain blocked in Wait until the
owned command and its output copying actually finish.

This is bounded process-group cancellation, not identity-pinned containment.
A live ordinary group member retains the process-group ID after the shell is
reaped. Once all original group members are gone, however, that numeric ID can
be reused. Cancellation racing natural completion can therefore signal a new
group with the same ID. If a descendant escapes the original group while
retaining the output pipe, this reuse window can extend for the remaining
drain. The helper's completed-result preference reduces unnecessary signals
but does not eliminate this race. No zero-reuse-risk or escaped-child exit
guarantee is claimed. Stronger containment requires a separate design.

Regression fixtures include an original shell observed actually reaped while a
same-group child retains the output pipe, covering explicit cancellation and
run timeout. PID and birth-time identity constrain fixture cleanup. A normal
background writer covers complete delayed output. Fake ordering fixtures cover
completed-result preference, signal failure and waiting for actual Wait
completion. These source fixtures are not native macOS or Windows acceptance;
cross-compilation cannot establish either platform's process behavior.
