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
   Unreadable/corrupt state must refuse admission/mutations, not become a clean
   slate under another user's UID. Read-only status retains its best-effort
   atomic-file snapshot API and creates no sidecar.
3. Add synthetic subprocess contention/count regressions, success/history and
   reset ordering checks, lock failure/refusal cases, and proof that a separate
   descriptor can lock the state while the expensive-check callback runs.
   Use normal existing GitHub CI; local candidate execution remains held.

This does not create globally serialized PIN admission or preempt an already
running KDF. Concurrent processes admitted before a lockout may finish checks;
every resulting failure must still be recorded. Uncooperative older writers
do not use the sidecar and are outside the new guarantee. General vault/header
rotation concurrency, hostile directory owners, cross-UID state ownership
repair and remote/NFS lock semantics are not fixed by this slice. Existing
unreadable/corrupt state now refuses even a correct secret; recovery needs the
operator's separate local-authority path, not a silent counter reset.
Unsupported OSes must refuse mutations, not pretend to hold a kernel lock.
Review the actual source/tests and exact CI head before claiming acceptance;
Linux CI and cross-compilation do not establish native Windows/macOS behavior.
No undraft/merge, credential change, service restart, deployment or SQL.
