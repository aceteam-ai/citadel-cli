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

Command `TestMain` creates private fixtures, installs both providers, then wires
credential/passcode hooks and constructs the diagnostic registry. On exit it
closes the CLI log handle, restores providers and removes the root. If provider
restoration fails, it retains the directories and exits unsuccessfully.

The legacy diagnostic registry no longer reads permissions or captures an index
path in package `init`. Its once-only construction happens at diagnostic job
execution, or explicitly after fixture setup in `TestMain`. Handler construction
takes permission/index inputs explicitly; live shell permission and strict
passcode callbacks retain their per-job behavior.

Tests that need a different HOME use `useCmdFixtureHome` to install scoped paths.
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
host-reader inventory work in #1468. This draft does not authorize real node
configuration inspection/restoration, service operation, model work or activation.
