# Cross-process passcode lockout

`Lockout.Attempt`, vault failure/success accounting and explicit `Reset` retain
the per-path process mutex and serialize state read-modify-write through a
persistent sidecar: `flock` on Linux/macOS, `LockFileEx` on Windows. Acquisition
is bounded; descriptor/process exit releases the lock. The expensive secret
check runs outside the OS transaction. Concurrent processes admitted before a
lockout can finish their checks; every resulting failure must still be recorded.
This is not globally serialized PIN admission or KDF preemption. Existing
thresholds, rolling windows, backoff and `ErrLockedOut` meaning are unchanged.

Unreadable/corrupt state refuses authentication and ordinary accounting rather
than becoming an empty history. Only explicit local-presence `Reset`, under the
same sidecar lock, may replace that history and its untrusted verification
anchor. Healthy resets retain the verified anchor. `ResetIfPresent` leaves
absent state absent, but first inspects its existing sidecar: an unusable sidecar
must return an error, not a false "cleared" result. This absent-state path never
creates a parent, state or lock, repairs metadata, acquires a lock or mutates
history. It is a snapshot, not a future transaction guarantee.

`Status` is read-only. Its bounded unavailable reason distinguishes unsupported
backends, unusable sidecars and unreadable/corrupt state; sidecar failure takes
precedence over state failure. CLI guidance is static and never reflects raw
errors or paths. Existing-only inspection neither creates nor repairs a file,
waits for a held lock, nor emits the ownership-fallback warning. An absent
sidecar is healthy on supported platforms. A healthy snapshot does not prove
future write/chown/rename/fsync success, stable permissions or lack of contention;
filesystem operations have no hard deadline. Status grants no authority.

Boolean passcode gates deny unavailable accounting regardless of warning
frequency. The command boundary emits a static warning immediately for the
current node context in each master/legacy mode, then at most once per minute.
It stores only two current-context slots, not a history of node paths. Switching
context or a backward clock emits and resets that slot. Paths, PINs and wrapped
error text are never logged. No recovery is inferred or announced from a healthy
attempt or silence, and the error-only callback contract is unchanged.

Never remove or replace a live sidecar: another participant could hold/open the
old inode, creating independent locks and losing serialization. Final sidecars
must meet the type, identity, single-link, owner and mode contract (0600 on Unix;
existing ACL/reparse/handle rules on Windows). Existing supplied locks are never
chmodded, chowned or unlinked. A trusted parent may resolve through a symlink;
final-sidecar no-follow and descriptor identity checks remain intact. Advisory
locking assumes a trusted node directory and does not defeat its malicious owner.

Root-created Unix sidecars attempt parent uid/gid assignment and verify actual
descriptor metadata. A remaining root-owned inode may be used only by effective
root, with one static process warning. This fallback never repairs mixed-UID
handover: non-root callers must refuse a root-owned 0600 sidecar. New lockout YAML
temporary inodes separately require successful parent ownership assignment before
publication. Other vault/header/pepper files gain no ownership-repair policy.
Local ownership reconciliation must stop all participating processes first;
`reset-lockout` cannot recover an unusable sidecar or unsupported backend.

Older writers that ignore the sidecar, general vault/header rotation concurrency,
hostile directory owners and remote/NFS locking are outside this contract. Native
macOS/Windows, privileged real cross-UID/root fallback and mixed-UID handover
remain acceptance gates; injected fixtures, Linux CI and cross-compilation do not
establish them. This source change performs no live reconciliation or deployment.
