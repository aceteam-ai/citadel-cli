# Seat registry foundations (incomplete)

This package is the offline identity/environment increment for #1748. It has no
production caller. It neither reads vendor credentials nor executes a vendor
command, logs in, selects an account, changes a live process, or rotates quota.

The child-environment plan retains a small locale/platform allowlist and sets
only the chosen harness's configuration home from a validated local identity.
Provider credentials, shared configuration homes and executable-loader or shell
startup overrides are not inherited. Local identities are excluded from JSON;
the reporting projection has only a domain-separated opaque reference and the
harness name. Salted account digests support local duplicate-account refusal.

These are structural, fake-only proofs, not proof of native login isolation or
permission to launch. A caller must still establish the actual owner, private
directory modes, registry integrity, entitlement/scope/data-class policy,
enabled state, and a trusted vendor probe result. Unknown account identity is
not uniqueness or authentication evidence.

Remaining P1 work: private atomic harness.yaml storage and salt lifecycle;
safe default-home adoption; owner-bound seat add/list/probe/enable/disable/remove;
vendor status adapters that never open credential files; full metadata and
sanitized projections; duplicate-account reconciliation; two-seat native
isolation proof including keychain/secret-store/daemon behavior. The later
driver/permission slices must bind this identity at start and every turn. No
HARNESS job, remote enable path, allowance multiplier or routing policy is added
by this foundation.
