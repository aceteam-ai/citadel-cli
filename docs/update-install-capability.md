# Self-update install capability

Citadel #1487 separates install authority from auto-update preference. A root-owned
binary with an unprivileged worker and automatic updates disabled is a supported
deployment. Citadel does not change ownership or enable self-update to repair it.

`InspectSelfUpdateCapability` resolves the installed executable and checks existing
lock authority without creating a lock. It creates, owner-checks and removes a
private probe in the executable's own directory. The same-directory rule also
applies to the actual swap stage; moving a stage to a different filesystem does
not make atomic replacement possible. Probing never changes the installed bytes,
owner, group or permissions. Availability is an observation, not a grant: apply
retains its final lock, owner, version and file-identity checks.

The heartbeat `auto_update` block adds `self_update`, `install_directory`,
`binary_owner` and `worker_user`. Capability states are `available`,
`requires_privileged_install`, `homebrew_managed`, `desktop_managed` or `unknown`.
Unix user labels include the effective UID; Windows uses the actual token SID.
The worker and control center observe capability before publishing their initial
report and on periodic refresh, independently of the configured update preference.
The heartbeat serialization path still reads only the cached snapshot.

An opted-out worker retains its existing result and reports the capability, rather
than a failed update. When an enabled periodic updater cannot establish authority,
it records `apply_not_permitted` and the last refusal's directory/identities in its
existing state file, without release lookup or download. Its notice appears once
per changed refusal, and is reset after capability recovers. No extra host file is
created. Corrupt updater state remains preserved and reported as `state_corrupt`.

Remote `AGENT_UPDATE` refuses a privileged-install layout before exact-target lock
provisioning, lookup or download. It returns a structured, non-updated result with
the capability, both identities and remediation. Unknown capability fails closed.
The per-node stream requirement is unchanged. Manual installation also checks
authority before allocating a download attempt. The owner-preserving swap path
remains authoritative; no ownership policy is relaxed.

The Aceteam result consumer needs the paired change in
aceteam-ai/aceteam#12595 and its implementation aceteam-ai/aceteam#12606 to render this new refusal as manual-update guidance.
An older consumer calls the unfamiliar reason unsupported. This is an integration
requirement, not a reason to disguise the result as Homebrew or claim an update.

Replacement failure does not promise that the old binary survived every error.
If the installed path still names the original file, no fictitious rollback is
attempted. A changed, absent or uninspectable path invokes recovery and reports its
actual outcome. Neither failure text nor a rollback failure is proof of success.

Both heartbeat and node-info output require a strictly newer valid staged SemVer
before reporting `pending_version` or `restart_required`. An older state file from
another installation cannot demand a restart. Startup reconciliation clears stale
restart results only with valid version metadata, preserving other outcomes and
preferences.

On Linux, manual update restart detection uses `DiscoverActiveWorkers` instead of
a fixed unit-name list. It inspects the system manager and the caller's user
manager, accepting a literal `work` invocation of the same resolved executable.
Every inspected active unit's backing files must agree with loaded state
(`NeedDaemonReload=no`) before its contents can exclude a worker. Literal worker
candidates with an unresolved account, dynamic identity or unverified root/mount
mapping refuse discovery rather than disappearing from the candidate set. Empty
root/path mapping assignments reset the corresponding setting. Empty boolean or
enum assignments remain unverified; they are not resets. Read-only hardening remains
supported; discovery does not interpret service filesystem namespaces. Shell wrappers,
unknown expansion syntax, incomplete inspection and ambiguous matches are left
for manual inspection. It does not enter another user's service manager. Discovery
has bounded output, candidate count and elapsed time, and never exports unit
environment values. The user bus is pinned to the inspecting process's effective
UID; a user unit without `User=` carries that manager UID into diagnostics.
Unsupported ExecStart prefixes, continuations and unparsed flags before `work`
refuse incomplete discovery instead of hiding a second candidate. Existing unit
rematerialization keeps its narrower file list.

An explicit `update install --restart` fails nonzero when no single verified active
target is found, including on already-latest and superseded-update paths. After the
existing bounded job drain it rechecks the unit, binary and configured account;
changes refuse restart. Discovery failure stays distinct from verified worker
absence: after a binary swap it warns that the running worker may still use the
old binary, and a requested restart fails with the cause. An unrequested restart
remains unrequested.

`doctor` and successful returning manual installs report read-only install-layout
evidence for each observed worker, using its configured account rather than the
CLI invoker. They flag binary-owner mismatches and Unix directory mode bits that
lack write/search access. Unknown identity/metadata stays unverified. A matching
owner and permissive mode are not proof of effective authority: ACLs, capabilities,
mounts and the service namespace may differ. The worker's own probe remains the
authority. Deliberate privilege separation is advisory and does not fail doctor's
health exit code. A literal unit opt-out (`CITADEL_NO_AUTO_UPDATE=1` or a verified
`--no-auto-update` argument) is informational and retains the observed ownership
limitations. Environment resets and unsets are respected; an unread environment
file never proves opt-out policy. Ordinary installs suppress platform/no-worker
notes unless restart was requested; doctor retains explicit unknown evidence.
Neither diagnostic creates a probe or changes ownership.

Installer ownership and executable-directory write grants still need the explicit
self-update-intent decision for new installs; this slice does not migrate existing
privilege-separated layouts. The refresh regression pins `User=`, `Group=`, the
auto-update opt-out and existing environment/write paths. Native elevated owner
transitions, Windows ACL/share behavior and live fleet acceptance are separate
gates. This source work performs no installation, service change or deployment.

A read-only filesystem refusal also reports `requires_privileged_install`, including
a systemd `ProtectSystem=strict` sandbox. It does not authorize changing the
mount, service protection or installed binary ownership.

