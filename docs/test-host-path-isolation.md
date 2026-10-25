# Command test path fixtures

Refs #1468 and #1356. This slice redirects default host-state discovery; it
does not make the entire command suite a native-free or OS-sandboxed suite.

`network.OverrideStateDirectoriesForTesting` installs immutable node and
machine-config directories before `GetStateDir` constructs any production
inputs. `GetNodeConfigDir` still derives the parent of that state directory.
Machine-pointer operations use the fixture machine directory; reconnect
manifest discovery uses only the fixture node manifest. Fixture state creation
does not look up or chown to a real service account.

`platform.OverrideConfigDirectoryForTesting` similarly redirects `ConfigDir`
and its candidate enumeration to one fixture, before root/sudo/Windows discovery.
`platform.OverrideHomeDirectoryForTesting` redirects `HomeDir`, including named
and sudo-user lookups, before consulting environment variables or the account
database. The command package's former direct `os.UserHomeDir` callers and the
CLI log sink use this seam. Platform home fallbacks (including desktop paths)
also use it. A source audit test prevents reintroducing direct OS-home calls in
the command/default-log files. Cache and workspace path selection is separated
from directory creation so tests can check the destination before writing.

The podman provenance marker has its own scoped directory input. Production
still uses `/etc/citadel/rootless-worker-user`; fixtures choose an existing owned
directory before marker reads or writes. Root ownership, directory mode,
exclusive creation and marker-content checks remain unchanged. The fixture
writer test supplies a verifier for its temporary directory; production always
uses the original root/mode verifier. Provenance tests use a read-only core
separate from the native account/group/sudo audit and never provision users.

There is no environment switch. With no explicit override, existing production
resolution, pointer precedence and ownership behavior are unchanged.

Installers require absolute, existing, non-symlink leaf directories. The caller
must own the fixtures: validation does not prove ancestor-symlink absence, UID
ownership or resistance to hostile writers. Explicit arbitrary paths passed to
other APIs are not rewritten. This is dependency injection, not access control.

## Lifetime

Install providers before path-dependent work. Serialize scope changes; readers
use atomic immutable snapshots. Join all users and close open handles before
restoring a scope or deleting its directories. Nested scopes restore in reverse
order. Restoration is idempotent; an out-of-order restore fails and leaves the
current provider installed. A failed installation leaves the previous provider.

Command `TestMain` creates private fixtures, installs state/config/home/marker
providers and checks their selected paths before any default-path effect, then
wires credential/passcode hooks and constructs the diagnostic registry. On exit it
closes the CLI log handle, restores providers and removes the root. If provider
restoration fails, it retains the directories and exits unsuccessfully.
It first refuses a non-nil diagnostic registry, so a direct `init` assignment
cannot be hidden by later once-only construction.

The network package's `TestMain` independently installs owned node/machine
directories for its existing default-getter/state tests. Before running tests it
checks pointer and state selection, before any test can write through them.
Positive-control tests likewise check computed paths before invoking pointer
writes or state creation/deletion. The command tripwire seeds the pointer file
directly in its known fixture directory rather than trusting the writer under
test to choose that directory.

The legacy diagnostic registry no longer reads permissions or captures an index
path in package `init`. Its once-only construction happens at diagnostic job
execution, or explicitly after fixture setup in `TestMain`. Handler construction
takes permission/index inputs explicitly; live shell permission and strict
passcode callbacks retain their per-job behavior.

Tests that need a different HOME use `useCmdFixtureHome` to install all four
scoped paths.
Changing HOME alone cannot disable the process guard. The helper deliberately
does not create `~/citadel-node`: manifest tests must retain their absence oracle
for an explicitly selected alternate node. Logs close before scope restoration
and before the owning `TempDir` cleanup.

## Verification and remaining scope

Regression fixtures include discovery tripwires, actual credential clearing and
network-state deletion with unchanged foreign temporary sentinels, reconnect
manifest selection, invalid installs, ordered restoration and concurrent path
readers. They use synthetic temporary data, never a real enrolled node.

Optional Chromium/ffmpeg/bash/process tests still exist. A child process does not
inherit Go's in-memory overrides. Whole-command local execution therefore remains
held pending independent review and the separate explicit native opt-in and
host-reader inventory work in #1468. Other packages' direct OS/environment/account
lookups and explicit paths are not intercepted by the home seam; this bounded
command/default-log slice does not qualify every transitive reader. Native OS
roots and optional processes remain outside this slice. This draft does not
authorize real node configuration inspection/restoration, service operation,
model work or activation.
