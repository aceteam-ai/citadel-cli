# Read-only lane inventory

`internal/laneinventory.Collector` owns the #1699 node-side inventory projection.
`citadel sessions inventory` emits the same JSON document as `LANE_INVENTORY`.
The local command inherits the sessions command's local-only startup hook. It
does not enroll, start, stop, attach to, adopt or restart anything.

## Admission and rollout

Remote inventory is **default-off**. An operator must explicitly set
`CITADEL_LANE_INVENTORY=1` on the node process to opt in. Every other value,
including absent, `true`, `yes`, and whitespace variants, stays off. While off,
`LANE_INVENTORY` is absent from `SupportedJobTypes` (the heartbeat's
`capabilities.job_types` provider); a direct handler call returns the typed
`lane_inventory_disabled` refusal without observing anything. Advertisement
and execution both recheck the same gate. The explicit local CLI command does
not enable remote inventory or bypass its gate.

The worker handler matches its **complete configured org and node stream**:
`jobs:v1:shell:org_<org>:node:<node>`. Empty identity, shared queues, a foreign
org/node, or a queue containing only a misleading `:node:` marker refuse before
any observation. Caller payload fields never choose a path, process, socket,
command or queue. Both worker startup paths register the same handler.
Only `redis` and `redis-api` delivery sources are supported; websocket or other
source labels refuse even when they carry the same queue text.

This is not an authentication replacement. The paired platform slice
[aceteam#14139](https://github.com/aceteam-ai/aceteam/issues/14139) must enforce
the live key-bound session-read grant and own/shared-node organization policy
before dispatch. This source is inert by default, not permission to opt in:
source review does not authorize enabling remote inventory before that paired
boundary is reviewed. Do not infer a session grant from a pane name or process
argument.
Direct Redis deployments must independently restrict stream writers to trusted,
authorized producers. Placement in a stream is not proof of a caller's API-key
grants; the paired API boundary does not authorize arbitrary Redis writers.

Inventory must not expose another AceTeam user's web-terminal sessions. The
current job envelope has no verified caller user identity, so v1 conservatively
omits all recognized web terminals, including the caller's own. It excludes
sessions with a Citadel management marker or default/configured terminal name
prefix, and their process subtrees, before argv/worktree projection. This also
conservatively omits other managed tmux sessions. `terminal_scope` reports that
the view is incomplete. Ordinary unmarked node-local lanes remain eligible.
Names alone never establish caller ownership; legacy custom unmarked sessions
cannot prove it. Showing a caller's own terminal requires a separately reviewed
paired caller-identity contract, not an arbitrary payload user ID. The paired
backend must also restrict non-terminal lane results to the node owner.

## Result

- `panes`: session name, window ID, pane target ID, current executable basename,
  cwd, initial pane process start time in Unix milliseconds when observed,
  optional declared session name, harness family and optional thread ID.
- `worktree` on each pane, when observed: repository root, remote **names**,
  branch or detached status, HEAD, optional ahead count and dirty-entry count.
  It is the worktree containing the pane cwd, not a scan of other directories.
- `capacity`: load1/load5, logical CPU count, available memory bytes and free
  bytes on the selected node configuration filesystem. Unavailable numbers are
  omitted, not made up as zero.
- `unavailable`: finite component codes marking incomplete observation. Probe
  diagnostics are never reflected. No tmux observation is an explicit failure,
  not proof that the host has no lanes.
- `truncated`: rows or process coverage reached a bound.

Only same-real/effective-UID pane processes and their same-UID descendants are
eligible for argv reads. Detection recognizes an invoked executable or a known
Node launcher entrypoint, not prompt text mentioning a harness. Only UUID-shaped
Codex resume/fork or Claude resume/session identifiers are emitted. Other argv
is discarded. There is no process environment, prompt, conversation, rollout,
credential file, working-file content or console/scrollback read.

Declared-session names are observation hints only. The native adapter reads the
existing private sessions declaration/state metadata without creating anything;
it does not load env files or publish command/env fields. It binds a stored
holder/child to observed PID, start ticks, UID and boot ID before mapping its
name through the pane's process ancestry. A mapping grants no control rights.
Unavailable declarations stay explicitly unknown. The process may exit or
change after this non-atomic snapshot, so it is not a custody or placement proof.

## Privacy and bounds

Both CLI and job publication use `Encode`: one valid JSON document including
its CLI newline fits 65,536 bytes. The largest encoded pane row is removed first
until it fits, so an oversized early row cannot erase healthy later panes;
JSON is never byte-truncated. The collector caps panes at 128, processes at
4,096, argv at 256 entries and 32 KiB per process, aggregate argv at 8 MiB, and
known secret replacement values at 64 KiB. Oversized sensitive input refuses
instead of dropping a secret from the redaction set. Text fields over 4 KiB are
omitted. Subprocess stdout is capped at 1 MiB, with a two-second command timeout
and a fifteen-second collection context. Existing tmux binary resolution also
has its own bounded version probe.
Publication retains at most 32 remote names per worktree, omits names longer
than 128 bytes, and marks either limit as truncated. HEAD must be a full 40- or
64-character hexadecimal object ID; invalid values become unknown. Escaped
JSON size, not unescaped text size, determines which row is shed.

Git runs fixed read-only argv with optional locks, fsmonitor and untracked cache
disabled. Inherited Git overrides/config and credentials are not forwarded;
global/system configuration is disabled for these readers. No fetch, shell,
hook, pager, external diff or URL query is requested. Status filenames are
counted internally, including NUL-delimited rename pairs, and never returned.

The existing `memory.RedactSensitiveText` and `trust.CheckSecrets` mechanisms
scrub projected text before it leaves the collector and again at publication.
Known credential-shaped argv values are removed from every pane's metadata,
including a secret discovered in a later pane. Control characters are removed,
credential shapes are rechecked after normalization, and URL-shaped metadata is
omitted. Raw arbitrary secrets cannot be recognized without a shape or a known
value; this is why the result excludes raw argv, filenames, URLs and env values
rather than relying solely on pattern matching. Treat cwd and branch metadata
as private organization data even after redaction.

## Platforms and verification

The initial native path observes the worker user's default tmux server on Unix.
It never launches a server (`tmux -N list-panes`) or probes custom sockets from a
payload. Windows refuses because this slice cannot prove the same-user tmux
process boundary there. Declared-state verification uses the existing Linux
process identity boundary; another platform reports that component unavailable.

Tests use injected pane/process/argv/git/capacity/declaration fixtures, including
planted credentials and dirty filenames, foreign users, shared and foreign-node
queues, detached/no-upstream worktrees, malformed inputs, and bounded output.
Native reader functions are not executed by these fixtures. Source proofs do
not establish native acceptance or approve a live node restart or deployment.
