# Local session search

`citadel sessions find violet compass --since 2026-01-01 --harness claude`
searches the current OS account's local harness logs. Add `--json` for a bounded
report. Words are case-insensitive and must all occur in one selected text block
or title. Dates are UTC midnight, inclusive; RFC3339 timestamps allow finer
selection. The command bypasses agent startup, enrollment, updates and backend
access. It never installs hooks or executes an open command.

`sessionsearch.Search` owns record selection and publication. Prompt text comes
only from the two harness-native history files. Claude replies come from primary
project transcripts; Codex replies come from dated session rollouts. Only
assistant text blocks are selected. Tool content, thinking, sidechains, injected
user/system transcript records, pasted-content attachments and event mirrors
are not selected. These files are parsed locally, not uploaded or indexed.

`sessionsearch.Native` selects the home from the OS account rather than HOME.
Linux and macOS require the current real and effective UID to agree. The root,
source directories and opened regular files must belong to that UID. Confined
root resolution, no-follow leaf opens and nonblocking opens keep other file
types out of content parsing. Unsupported platforms refuse the operation.
Custom harness roots are not searched.

Matches are grouped by harness and validated session UUID. Each result contains
at most three recent matching snippets, each at most 240 characters, and the
report contains at most 20 sessions. The shared `trust.CheckSecrets` guard scans
the complete selected value before clipping and the final snippet after clipping;
possible secrets are withheld. Titles use the same guard. Control characters are
removed before terminal output. Detection is not a guarantee that all sensitive
personal text is removed: this is an explicit owner-local content search.

Traversal is capped at 4,096 entries, 4,096 grouped sessions, 16 MiB per file,
128 MiB of content and 1 MiB per JSONL record. Optional hook name records are
limited to 4 KiB for each of the final 20 results. The CLI requests an eight-second scan
deadline; regular filesystem operations and OS account lookup may themselves
wait on the operating system. Missing sources are empty; refused, malformed or
bounded sources produce incomplete coverage. No result does not prove that an
unsearched or truncated source has no match. The oldest timestamp covers observed
selected records, not all historical activity.

Content search finishes before optional bounded `laneinventory` placement.
An unambiguous live thread observation adds a tmux target or a declared-session
attach command. The hook state file can supply a display name but is not proof
of a live holder. A present transcript otherwise offers the harness resume
command; a history-only result says pruned, or unknown if coverage is incomplete.
An available project/cwd is printed separately as guarded display text, never
interpolated into a shell command or used to change directories.
Commands are printed only and carry validated identifiers, never text from a
transcript as shell syntax. No cwd switch is executed or inferred as authority.

There is no remote search job, central persistence, semantic index, TUI surface
or query logging in this slice. Tests use owned temporary synthetic logs and
injected placement observations; they never inspect real session content.
