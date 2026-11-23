# Seat registry foundations (incomplete)

This package is the offline identity/environment increment for #1748. It has no
production caller. It neither reads vendor credentials nor executes a vendor
command, logs in, selects an account, changes a live process, or rotates quota.

The child-environment plan retains a small locale/platform allowlist and sets
only the chosen harness's configuration home from a validated local identity.
Provider credentials, shared configuration homes and executable-loader or shell
startup overrides are not inherited. Local identities are excluded from JSON;
the reporting projection has a domain-separated opaque reference and bounded
seat metadata, never local owner/configuration/work paths or account digests.
Salted account digests support local duplicate-account refusal.

These are structural, fake-only proofs, not proof of native login isolation or
permission to launch. A caller must still establish the actual owner, private
directory modes, registry integrity, entitlement/scope/data-class policy,
enabled state, and a trusted vendor probe result. Unknown account identity is
not uniqueness or authentication evidence.

Remaining P1 work: safe default-home adoption; owner-bound seat
add/list/probe/enable/disable/remove; vendor status adapters that never open
credential files; observed metadata and duplicate-account reconciliation;
two-seat native isolation proof including keychain/secret-store/daemon behavior. The later
driver/permission slices must bind this identity at start and every turn. No
HARNESS job, remote enable path, allowance multiplier or routing policy is added
by this foundation.

The salt storage increment is also offline/unwired. An existing 0700 directory
chosen by a trusted local owner resolver can be pinned; the store refuses a
symlink root, foreign ownership, permissive modes, linked/non-regular files and
damaged salt instead of repairing them. Reads never initialize identity. An
explicit initialization publishes one 32-byte random salt through a synced
temporary file and a non-overwriting link, under a bounded kernel sidecar lock.
Concurrent cooperating stores share the same value; cancellation while waiting
for the lock does not publish a salt. Paths are anchored to the opened directory
and errors omit them.
The owning user/root are trusted, not sandboxed by this store.

Private salt storage currently supports Linux and macOS; other platforms refuse
until native privacy and locking checks exist. Linux temporary-file tests are
not native macOS/Windows acceptance. No production caller invokes initialization.

The offline harness.yaml codec preserves other root sections, rejects ambiguous
YAML and unknown seat fields, and re-resolves owner homes through an injected
trusted resolver. A bounded, private atomic writer shares the salt lock and uses
snapshot compare-and-swap to refuse stale updates. Removing a seat removes only
its declaration, never its vendor directory. Registry initialization refuses a
missing salt whenever any existing seat is present; generic salt initialization
also refuses an existing registry. Neither reads nor updates create identity.
The data-ceiling field is only a bounded policy selector here, not an invented
classification ordering or permission decision. Local status fields are
declarations, not trusted evidence of login, scopes or native isolation.

Local configuration-directory preparation is also offline/unwired and limited
to the process owner. Its identity/home must come from the trusted owner
resolver, not remote input. It opens directories only, refuses symlinked or
foreign/writable ancestors, requires a 0700 leaf and creates missing components
0700 only on an explicit create. An existing leaf is never silently adopted
by create. Read-only validation creates nothing. Neither operation repairs
permissions or deletes anything; a failed create may leave private parents for
the owner to inspect. Revalidation checks the pinned inode and the current
no-symlink path, refusing a replacement or lost privacy. This is not custody for
a later path-based subprocess or proof of native vendor isolation. Unsupported
platforms refuse without creating state. The owner/root remain trusted.

Offline local-edit helpers use the same snapshot fence before any add creates
directories and again before publishing metadata. Add accepts only declared
fields and starts disabled with unknown auth; it never invents observed plan,
model or account data. Enable rechecks directory privacy but changes only the
local flag, not authentication evidence or permission. Disable and remove remain
available with an unsafe vendor directory, which neither operation opens,
repairs or deletes. A concurrent add losing publication may leave its new
private directory for the owner to inspect; retries do not silently adopt it.
These helpers do not create a CLI, remote route, login command or launch path.
