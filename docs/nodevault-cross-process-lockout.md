# Cross-process lockout transactions (#1395)

Source-only DRAFT plan; existing PIN thresholds, rolling window, backoff and
`ErrLockedOut` meaning remain unchanged. No live vault/config mutation.

1. Add a persistent advisory-lock sidecar next to each lockout state file.
   Use `flock` on Linux/macOS and `LockFileEx` on Windows, bounded acquisition
   and automatic release on descriptor/process exit. Never unlink the sidecar
   or lock the atomically replaced YAML inode. Refuse symlinks/nonregular lock
   files and do not expose a world-readable Unix lock. Preserve user ownership
   when the root worker creates the lock inside a user-owned node directory.
2. Serialize every failure/success/reset read-modify-write under that lock.
   Retain the existing per-path mutex. Acquire no OS lock across Argon2/bcrypt:
   admission reads and the expensive secret check remain outside the OS
   transaction. Propagate transaction errors instead of issuing an unlocked
   session or silently granting a successful credential check after failure.
   Unreadable/corrupt state must refuse admission and ordinary mutations, not
   become a clean slate under another user's UID. Read-only status explicitly
   reports unavailable state and creates no sidecar. The separate local-presence
   reset is the only recovery operation: under the same OS lock it may replace
   unreadable/corrupt history with an empty history and zero verification anchor.
3. Add synthetic subprocess contention/count regressions, success/history and
   reset ordering checks, lock failure/refusal cases, and proof that a separate
   descriptor can lock the state while the expensive-check callback runs.
   Use normal existing GitHub CI; local candidate execution remains held.

This does not create globally serialized PIN admission or preempt an already
running KDF. Concurrent processes admitted before a lockout may finish checks;
every resulting failure must still be recorded. Uncooperative older writers
do not use the sidecar and are outside the new guarantee. General vault/header
rotation concurrency, hostile directory owners and remote/NFS lock semantics
are not fixed by this slice. Root-created lockout state must inherit the parent
directory's uid/gid through the new temporary file's descriptor, before rename;
other vault/header/pepper writes do not gain an ownership-repair policy. Existing
unreadable/corrupt state still refuses even a correct secret; recovery requires
the explicit local-presence reset, never an authentication-triggered reset.
Unsupported OSes must refuse mutations, not pretend to hold a kernel lock.
Review the actual source/tests and exact CI head before claiming acceptance;
Linux CI and cross-compilation do not establish native Windows/macOS behavior.
No undraft/merge, credential change, service restart, deployment or SQL.

## Independent-review follow-up (#1408 review 5608201592)

Source-only corrections, with normal existing GitHub CI and no local execution:

- B1: preserve fail-closed admission/failure/success accounting. Only `Reset`
  may discard corrupt/unreadable history while holding the existing sidecar
  lock; it also discards an untrusted `LastVerifiedAt`. Healthy resets retain
  the verified anchor. Status reports `Unavailable`, not a clean counter.
- M1: apply parent uid/gid to a root-created lockout YAML temporary inode using
  descriptor-only `fchown` before atomic rename. Never chown an existing state
  pathname or a symlink target; no generic vault/header ownership migration.
  Refused preparation must prevent publication. Unix ownership branches and
  injected failures require source regressions; Windows keeps its existing
  parent-ACL authority rather than inventing Unix uid/gid semantics.
- N1/N2/N4: move the misplaced architectural paragraph; warn statically when a
  boolean gate denies unavailable accounting (never log the PIN, path or error
  text); test wrong-PIN plus failed failure-recording for unlock and rotation.

M2 remains an unresolved publication design, not a completed cleanup fix.
The separable mode correction explicitly sets mode 0600 on a newly created
descriptor regardless of umask; it never chmods an existing supplied file.
Prepared regressions use disposable child processes with umasks 000/077/0277,
preserve/refuse an existing bad-mode inode, and inject a root-owner preparation
failure and foreign-owner refusal. No local candidate execution is authorized.
These tests do not prove recovery from an orphaned failed-setup inode: failure
still closes the descriptor without unlinking, loosening ownership or granting
the creator an exception on this or subsequent opens. Such an exception would
not repair the parent-owner contract for later non-root callers.
Checking `Lstat`/`SameFile` and then unlinking a final pathname is not an atomic
compare-and-unlink operation: it can remove a replacement and is not acceptable
as a hostile-path guarantee. A proposed Unix staging approach initializes and
validates an owned inode before publishing without replacement. `linkat` can
provide no-replace publication on Linux/macOS, but exposes a temporary link count
of two; failed staging unlink can then leave a persistent refusal. Neither
accepting multiple links nor claiming that failure is harmless is approved.
Linux `O_TMPFILE`/`AT_EMPTY_PATH` is not a portable macOS fallback. An accepted
cross-platform publication/cleanup shape must resolve this before M2 is wired;
the current Unix create path remains known to leave an unusable new sidecar
after some setup failures. Existing or changed final paths must never be removed.
All designs assume the existing node directory is trusted: a malicious owner of
that directory is not defeated by advisory locking or pathname identity checks.
First creation by root can transiently refuse a concurrent non-root opener until
ownership is assigned; a refusal never grants authentication authority.
