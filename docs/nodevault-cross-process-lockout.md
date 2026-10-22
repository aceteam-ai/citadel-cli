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

M2 follow-up keeps a persistent inode rather than trying to delete a failed
publication. The source plan is:

1. Attempt parent uid/gid assignment on a newly root-created lock, then read
   actual descriptor metadata even when that assignment fails.
2. If the inode remains root-owned, only effective root may use it, including
   later opens. Emit one static warning for the process. Parent-owned locks keep
   the existing contract; other foreign owners remain refused. Existing supplied
   locks are never chmodded, chowned, removed or replaced by this fallback.
3. Cover new/reopened root fallback, non-root refusal, normal parent ownership,
   foreign ownership, reported-success verification and metadata-read failure
   through injected syscall/stat fixtures. Normal CI only; native acceptance is
   still held.

The separable mode correction explicitly sets mode 0600 on a newly created
descriptor regardless of umask; it never chmods an existing supplied file.
Prepared regressions use disposable child processes with umasks 000/077/0277,
preserve/refuse an existing bad-mode inode, and inject owner-preparation and
metadata-read failures. No local candidate execution is authorized.
The root fallback prevents that creator's subsequent opens from refusing solely
because parent assignment failed. It does **not** repair mixed-UID handover:
later non-root callers cannot use a root-owned 0600 inode and must fail closed.
This exception is only for the advisory lock. Lockout YAML temporary-file
ownership still requires successful parent assignment before publication; a
namespace that cannot perform that assignment still refuses state mutations.
The fallback is not a guarantee that every rootless-container PIN flow works.
Other setup failures can still leave a persistent refusal; no automatic cleanup
is claimed. Explicit local-authority ownership reconciliation must first stop
all participating processes. Never remove or replace a live advisory-lock inode
while another participant could hold or open it: that creates two independent
locks and loses serialization. This draft performs no live reconciliation.
Checking `Lstat`/`SameFile` and then unlinking a final pathname is not an atomic
compare-and-unlink operation: it can remove a replacement and is not acceptable
as a hostile-path guarantee. No unlink/staging publication algorithm is added.
All designs assume the existing node directory is trusted: a malicious owner of
that directory is not defeated by advisory locking or pathname identity checks.
First creation by root can transiently refuse a concurrent non-root opener until
ownership is assigned; a refusal never grants authentication authority.

## Trusted parent compatibility and upgrade limits

R1 follow-up plan: allow the trusted node/state parent directory to resolve
through a symlink, matching the existing sidecar parent-stat contract. Pin the
resolved directory descriptor with `O_DIRECTORY`, then retain descriptor-only
ownership validation/assignment for the new temporary state inode. Do not
relax final-sidecar no-follow, mode, link-count or owner checks. Prepare a real
legacy Attempt/history/Reset regression through a temporary symlinked node
directory, while retaining the temporary-name substitution-target protections.
The root-owner fallback warning uses the existing structured warning sink and
remains static and once per process. No live ownership recovery is performed.

An old root worker may have left lockout YAML root-owned and unreadable to a
non-root caller. That caller now reports unavailable and refuses authentication
rather than interpreting a failed read as a clean slate. A later successful
root state publication can assign the parent owner; explicit local-presence
reset can replace unreadable history where the caller has directory authority.
Neither is an automatic repair guarantee. A root-owned fallback **sidecar** is
different: non-root reset cannot use it, and ownership reconciliation remains
offline with all participants stopped. Native macOS/Windows, privileged real
cross-UID/root-fallback operation and mixed-UID handover remain acceptance gates;
unprivileged injected fixtures and cross-compilation do not establish them.

L3 status inspection is an existing-only diagnostic snapshot. On supported
OSes an absent sidecar is healthy without creating it or its parent. A present
sidecar must satisfy the same current type/link/owner/access contract (0600 on
Unix; existing ACL/reparse/handle rules on Windows), with descriptor/path
identity checks before/after opening. An unsupported mutation backend reports
unavailable even when the sidecar is absent. Inspection never creates,
chmods/chowns, repairs, acquires/waits for a lock, or emits a fallback warning.
An existing held lock is not an availability failure. A successful snapshot
does not prove future write/chown/rename/fsync success, lack of contention or
stable permissions; filesystem operations have no hard deadline. Status cannot
authorize access or promise that reset repairs an unusable sidecar.
