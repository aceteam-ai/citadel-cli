# Update state persistence and corrupt-state pauses

Citadel #1481 protects `update/state.json` against concurrent CLI and worker
writers. All production consumers use `MutateState` or `SetAutoUpdate`; loading a
snapshot and later passing it to `SaveState` can still lose intervening fields.
A source audit rejects that pattern in repository consumers. `SaveState` remains
an atomic full-snapshot replacement for explicit snapshots and test fixtures.

## Transaction and publication

Each transaction captures its state path once and holds an in-process mutex and
a cross-process kernel lock while loading, modifying and saving. The persistent
sibling `state.json.lock` is never truncated or removed. Locking the destination
inode would fail after the first replacement. Unix uses nonblocking flock;
Windows uses LockFileEx. Kernel-lock contention waits at most ten seconds after
opening the sibling; callback work must be short and must not nest state writes.
The mutex queue and callback execution are outside that contention deadline.

Publication writes a random temporary file in the same directory, preserves the
existing owner and restrictive mode, syncs and closes the file, then replaces the
destination. New state files retain the existing 0644 policy. Unix then syncs the
parent directory. A directory-sync failure after replacement returns an error
that explicitly says durability is unknown: the new snapshot is already visible
and is not rolled back. The transaction has no automatic retry. A caller must
not interpret a post-replacement error as proof that nothing was written.

Windows replacement uses MoveFileEx with REPLACE_EXISTING and WRITE_THROUGH;
it refuses an owner change. It does not claim Unix directory-fsync semantics.
Native Windows sharing, ownership transitions and power-loss acceptance remain
separate from Linux CI and cross-compilation. Unsupported platforms refuse writes.
These locks coordinate cooperating new-version writers on the same machine;
legacy writers that ignore the sibling lock can still overwrite state. No
network-filesystem or distributed-store guarantee is made.

## Corruption and explicit repair

Missing state keeps the existing default-off policy. Malformed JSON, including a
trailing extra object delimiter or null, returns ErrStateCorrupt. Unreadable or
non-regular state paths also remain unverified. Ordinary telemetry and lifecycle
writes preserve that evidence rather than replacing it with default preferences.
There is no automatic salvage of an initial JSON object.

Corruption replaces only the file-sourced preference with unknown. The effective
policy still follows opt-out/dev veto, then flag, then environment, then the
persisted preference. An explicit flag or environment enable therefore keeps
running release checks and updates. Heartbeat reports configured_enabled=null
and last_result=state_corrupt while retaining that effective policy and source.
Pending versions and timestamps are not guessed from the broken file. This result
is derived for the report; it is not written into the corrupt JSON.

When the file is the only policy source, each corrupt tick logs a paused repair
notice. An explicitly enabled tick logs that it is continuing under explicit
policy instead. Explicit opt-out, environment-off and development-build vetoes
do not emit a misleading repair/enable notice. Both repair commands are named in
the notices and in `citadel update status` error output.

An explicit `citadel update enable` or `citadel update disable` repairs malformed
JSON under the same lock using fresh defaults and the requested preference.
Before replacement it attempts a 0600, owner-preserving sibling copy named
`state.json.corrupt-<UTC timestamp>-<random suffix>`. Backup failures do not block
the requested repair; incomplete copies are removed, and prior copies are never
overwritten. No backup is created for an ordinary telemetry write or valid state.
Unreadable files are refused rather than replaced. Valid state retains its
other fields. No source change repairs a live node or enables updates by itself.

## Verification

Fixtures keep HOME, XDG and Windows profile selectors under private temporary
roots. Concurrent goroutines and an isolated helper subprocess exercise kernel
contention, committed increments and JSON readers. An already-open reader must
retain its old snapshot while new readers see the replacement. Injected sync,
rename and directory-sync failures check the publication boundaries and temporary
file cleanup. Corrupt-tick fixtures use an in-memory checker and manual ticks;
heartbeat fixtures check the pause and explicit repair. No real updater, network
release lookup, binary apply, restart or live host-state operation is involved.

Focused offline fixtures use isolated HOME/XDG, with no release service, binary
apply or restart. Normal GitHub CI supplies broader suite and cross-build evidence;
source review and CI do not establish crash or native fleet acceptance.
