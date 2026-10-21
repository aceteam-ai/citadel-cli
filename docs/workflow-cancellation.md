# Workflow cancellation (#1317)

Source-only draft. No live workflow/process was cancelled or executed locally.

`Executor.Submit` creates and stores the per-run cancellation function before
publishing the run. `Cancel` requests cancellation without holding the execution
mutex while context callbacks run. The existing immediate cancelled status and
timestamp are retained as cancellation-request metadata; late node completion
or startup must not resurrect/overwrite a terminal state.

`Execution.Done` is the separate actual-exit signal: it closes only after the
executor goroutine has returned and released its run slot. The worker handler
waits for it on cancellation and all terminal observations. A node that ignores
its context therefore retains worker execution custody rather than being
reported as cleaned up merely because its status changed. There is no new
timeout that fabricates exit or releases the slot while a handler still runs.

Linux/macOS Shell commands get a dedicated process group; context cancellation
kills that group, not just `/bin/sh`. Ordinary shell children cannot continue
sleeping/holding the output pipe after their shell is killed. Context errors
remain failures instead of becoming successful outputs with a negative exit
code. Normal nonzero command exits preserve their existing result contract.
Shell permission/passcode checks are unchanged and still apply before launch.

This is not a sandbox, cgroup/job-object containment or a guarantee about
commands deliberately daemonizing into a different process group. Detached
privileged shell work and other callbacks that ignore cancellation require
separate containment/custody design; do not infer their exit from this signal.
Windows/other platforms retain the existing /bin/sh requirement and parent
CommandContext behavior; no native Windows process-tree guarantee is added.

Prepared regressions cover actual context delivery, pre-start cancellation,
late completion, capacity rejection/cleanup, and worker-handler exit custody.
Linux-owned temporary Shell fixtures test a real sleeping executable and an
ordinary sleeping child/process group. Normal GitHub CI only; local candidate
tests/build/formatter, real host services, deployment, live credentials/schema,
SQL application, undraft and merge remain held. Native macOS/Windows acceptance
is not established by cross-compilation.
