# Local harness permission checkpoint

Refs #1735. This is a local saved-setting checkpoint, not a working harness
relay or completed issue acceptance.

`permissions.yaml` gains an independent `harness` boolean, default off for a
new, missing, older or malformed configuration. `citadel permission harness
enable|disable` changes only this setting and writes a fixed audit log entry.
The command uses the enrolled node's existing machine-convergent permissions
directory. It refuses unreadable or malformed policy rather than rewriting it
from defaults, and saved policy retains its existing private-file behavior.

`citadel permission` reports the saved setting and explicitly says execution
is not wired. Enabling it does not start a process, register a job, grant Shell,
open a listener or clear owner, identity, profile or isolation requirements.
Shell, console, desktop, files and every other permission remain independent.

The current whole-policy Control Center writer cannot edit this setting and
preserves its current value instead of restoring an older snapshot. Existing
generic toggles do not gain a harness option. Applying the restrictive setup
posture clears it along with the other interactive access settings. No remote
enable field or remote configuration path is added. Heartbeat advertisement and
the Control Center control remain future integration, not evidence of a live
capability.

## Remaining acceptance

The typed job dispatcher, per-job local-disable checks, first-party remote
enable with pairing and step-up, owner/seat binding, profiles, declared sessions,
pending human approval correlation and process lifetime are not implemented
here. They must be implemented and reviewed with the unfinished driver before
any live operation. The permission bit is not an authorization decision on its
own, and the checkpoint creates no remote tool or admission surface.

Tests use only synthetic temporary policies and an injected directory resolver.
No actual node settings, vendor process, credential store, live session or
service is read or changed. Full live acceptance remains held.
