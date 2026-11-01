# Enrollment recovery and service refresh

This describes the source behavior behind #1356. Refreshing a managed unit
repairs selected configuration on disk. It does not establish that a stopped
worker has recovered or that a new device may replace an old registration.

## What service refresh reaches

[`candidateManagedUnits`](../internal/service/rematerialize.go) owns the fixed
candidate set. On Linux it includes the fleet system worker, the CLI-installed
system unit, the fleet user-unit file and the CLI user unit under the invoking
process's resolved home. It does not enumerate every home or arbitrary custom
unit names. A user unit found from a root invocation is still a user-unit
candidate; discovering its file does not establish access to its user's manager.

[`isCitadelManagedUnit`](../internal/service/rematerialize.go) checks the
recognized file's Citadel description and worker command before normalization.
An unrelated file at a candidate name is left alone. This is a base-file check;
it does not resolve manager search precedence, drop-ins, generators, aliases or
transient units into an effective configuration.

`hardenUnitContent` owns the restart-policy normalization. The additional
`rootConfigWritePathContent` repair grants the configuration-directory path
only to a recognized system-unit base file with a known root identity,
`ProtectSystem=strict` and understood path syntax. It preserves operator
directives and declines this extra grant when the identity or syntax is
uncertain. It does not convert an unprivileged unit into a root unit.

`rematerializeManagedUnits` backs up and writes eligible changed files, then
requests the appropriate system or caller-user `daemon-reload`. An unchanged
file needs no write or reload. An unprivileged system-file repair reports the
root remediation instead of writing. Read, backup and write failures are
reported per candidate; reload failures are reported per affected manager after
the candidate loop. The returned list of rewritten files is not proof that
every candidate was repaired or that every manager reload succeeded.

The CLI entry point is [`runSvcRefreshUnit`](../cmd/service_cmd.go), exposed as
`citadel service refresh-unit`. Manual update and worker startup also call the
managed-unit refresh. The refresh itself does not issue a worker restart or
`reset-failed`; there is no explicit start-counter reset in this source path.
A change that affects the worker's process takes effect when the service is
subsequently started or restarted.

## A repaired unit can still be start-limited

Systemd treats reloading unit configuration and starting a service as distinct
operations. Start-rate limiting also applies to manual starts. Once the limit
has been reached, automatic restarts stop; a later manual, timer or socket
activation can start the unit after its interval permits it. `reset-failed`
clears a unit's failed state and start/restart counters; it does not itself
start that service. See the upstream [systemctl manual](https://www.freedesktop.org/software/systemd/man/latest/systemctl.html)
and [unit start-limit documentation](https://www.freedesktop.org/software/systemd/man/latest/systemd.unit.html).

Consequently, successful file repair is not evidence that an already failed
worker resumed. An operator must assess the original failure and the actual
unit and manager before choosing a separate reset or start. Any such operation
uses the real unit name and its system or user manager, rather than assuming
that a root shell controls another user's manager. This documentation does not
record a live reset, restart or acceptance test.

The existing root configuration-directory exception also does not relocate
runtime CA, TLS or pointer-lock state. Those paths and their ownership,
conflict, offline and locking rules remain the separate unratified
[runtime-state migration design](design-enrollment-runtime-state.md).

## Enrollment reclaim selects a credential and sends a hostname

[`reclaimEnrollmentNodesWith`](../cmd/enrollment_reclaim.go) owns enrollment's
credential selection and force-new behavior:

| Enrollment case | Reclaim behavior |
| --- | --- |
| Force-new device | Return before loading saved credentials or issuing reclaim. |
| Current device grant exists | Use only that grant's device token; an absent token does not fall back to saved credentials. |
| Legacy flow with no current grant | May load the existing saved token through the explicit legacy path. |

Within this enrollment helper, remaining reclaim requests use unique, nonempty
hostnames.
[`ReclaimStaleNode`](../internal/nexus/reclaim.go) still sends `NodeName` to the
device-auth deregistration route. It does not send a saved node ID. The
[`reclaimStaleNodeByHostname`](../cmd/init.go) wrapper is best-effort: an error
is logged at debug level and does not itself stop enrollment. A reclaim attempt
or a continuing enrollment is not proof that an old registration was removed.

The original #1356 request to include a saved Headscale ID remains a selector
proposal, not permission to delete a predecessor. For a node with durable
configuration, the paired backend anchors deregistration to the configured
device credential and organization. An ID or matching hostname cannot replace
that binding. A freshly approved replacement credential does not inherit a
durably linked predecessor's delete authority.

The backend retains a distinct legacy path for nodes without durable
configuration. It can authorize reclaim using the exact authenticated credential
in the node's organization and that credential's original hostname; a hostname
alone is insufficient.
During that legacy reclaim, a new unlinked enrollment credential is preserved
so it can link afterward. This describes the existing backend compatibility
path, not a new replacement grant or a proposed expansion of its authority.

The CLI's force-new case still skips reclaim unconditionally, independently of
that backend legacy exception. Any later predecessor removal is a separate
platform operation requiring its own authority. Do not interpret force-new as
that authority or use it to recover an old bearer token. No node-ID reclaim
implementation or platform replacement protocol is introduced here. The current
selection tests are in
[`enrollment_reclaim_test.go`](../cmd/enrollment_reclaim_test.go).

Backend source reference for this audit: Aceteam main
`5981274b1557c952bdd676bee9818993f0dc8737`,
[`device-auth/deregister`](https://github.com/aceteam-ai/aceteam/blob/5981274b1557c952bdd676bee9818993f0dc8737/app/api/fabric/device-auth/deregister/route.ts)
and its [credential-revocation helper](https://github.com/aceteam-ai/aceteam/blob/5981274b1557c952bdd676bee9818993f0dc8737/utils/deviceApiKeys.ts).
This is source evidence, not proof of a deployed version or successful live
replacement. #1356 remains open for its separate migration and acceptance gates.
