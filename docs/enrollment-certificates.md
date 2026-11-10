# Enrollment certificates

The added init, enroll and Control Center certificate enrollment is inactive by
default. It returns before opening or creating any identity state, submits no CSR,
and ignores unsolicited certificate bundles. Existing login's CSR path is
unchanged. Ordinary device authorization still completes with its current grant.

`CITADEL_ENROLLMENT_CERTIFICATES_ENABLED=true` (or `1`) opts the added paths in
for a separately approved testbed rollout. Do not set it until the native
acceptance and activation holds are cleared. It is sampled once for each grant,
not a new authorization or credential. With the opt-in, those paths request a
fabric CA leaf for the existing convergent signing key. They generate one CSR per
authorization attempt and retain its exact public key while polling.

`runDeviceAuthFlow` supplies the shared init/enroll path. Login retains its
existing CSR-aware flow. Control Center binds polling and acceptance callbacks to
the captured grant, retaining the existing cancellation, expiry and current-token
admission checks. A poll never persists the certificate; only an admitted token
can reach its identity persistence callback. Poll cleanup does not discard the
context needed by that later acceptance.

`nexus.NewCSRTokenPoller` refuses another grant code. It falls back to a poll
without CSR only for the same recognized certificate-enrollment refusals as the
existing CLI poller. Authorization denial, expiry and unrelated failures do not
silently drop the CSR. An older server that ignores the optional field can still
complete ordinary device authorization without returning a certificate.

`generateLoginCSR` uses the same convergent identity store as the receipt signer.
`persistLoginIdentityBundle` requires a complete leaf, chain and node UID, binding
the leaf to both the submitted CSR key and the signed UID. It validates current
validity and the issuer chain before persistence. A malformed, mismatched or
incomplete bundle does not replace an existing good leaf. The existing valid-leaf
idempotence and expired-leaf renewal behavior remain in that helper.

CSR or certificate failures are nonfatal to enrollment. Unavailable identity
state and rejected bundles use static diagnostics without key paths, PEMs or
credentials. An unvalidated response UID cannot select a serving name.

The operator's display hostname remains the mesh registration and reclaim name,
including when an opted-in grant returns a valid certificate UID. The UID stays
in the identity/certificate state and never renames these device-enrollment
paths. The new paths do not write `login_node_uid`, which legacy worker, rename
and logout readers otherwise use as a serving name. Init/enroll and Control
Center use the same display-name policy. The
post-provisioning init path completes its captured device grant in-process,
retaining the current authorization and link without an authkey-only child login.

## Local diagnostics

Whoami, including its JSON/cache output, and doctor report local certificate
presence and whether its SubjectPublicKeyInfo matches the signing key. Presence
and match are nullable when unavailable. Invalid, missing, unreadable, expired
and not-yet-valid leaves remain distinguishable. Inspection never generates a
new key; it preserves the convergent store's existing read-through adoption of an
already registered legacy identity.

These checks do not establish CA trust, revocation, enrollment membership or a
verified receipt. They perform no remote verification. Certificate diagnostics
are informational and do not change doctor's existing health exit policy.

## Existing leafless nodes

There is no automatic fleet backfill in this slice. The existing CA renewal
client requires a current leaf; the re-enrollment client also requires a client
certificate. The browser device-auth endpoint returns the full enrollment grant,
including network authorization and a device credential. It is not a
certificate-only bootstrap operation.

A separate request command must not silently replace credentials, re-enroll or
rename a running node, or obtain a credential that it then discards. The existing
node's non-reenrolling bootstrap remains the paired server-contract follow-up in
#1498. This source completes the supported enrollment paths and local diagnostics
but leaves that requirement open, as the issue permits when a new route is
needed. No certificate-only command is advertised here.

Native enrollment, mesh naming, credential lifecycle and end-to-end verified
receipt acceptance remain separate checks on the approved testbed. Fake fixtures
and compilation are not evidence that a live node obtained or used a certificate.
