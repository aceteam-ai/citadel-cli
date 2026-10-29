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

This is the first source slice for #1487. The follow-up still needs ExecStart-based
service discovery, a failing restart request when no unit is found, doctor checks
against the actual unit user, and installer/unit preservation for both intentional
privilege separation and self-updating layouts. Native elevated owner transitions,
Windows ACL/share behavior and live fleet acceptance are separate gates. This
source work performs no installation, service change or deployment.

A read-only filesystem refusal also reports `requires_privileged_install`, including
a systemd `ProtectSystem=strict` sandbox. It does not authorize changing the
mount, service protection or installed binary ownership.
