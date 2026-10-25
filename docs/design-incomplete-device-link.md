# Incomplete device link recovery proposal

Refs #1350. The durable state described here is an inactive, unratified design
requiring independent review. It is separate from this issue's bounded
stateless diagnostics. No marker, enrollment repair, credential extension,
database change or runtime activation is authorized by this document.

The source is stacked on #1462, commit
`018bd730784e6d1e4a09d8aac9f6b0e625aeb1bc`. Preserve that draft's current-grant
reclaim selection, force-new refusal and registration-ID reset when reviewing
or rebasing this follow-up. Neither draft is a deployment receipt.

## What can be established now

The current link operation uses the freshly approved device grant and the node
ID returned by its actual registration. An understood success acknowledgement
can establish that this attempt linked at that time. A lost response, transport
failure, 5xx or uncertain local write does not establish rejection or success.
An expired grant must never be saved for automatic replay on a later startup.

The inspected backend source provides no read-only endpoint that proves the
current bearer's device binding. At Aceteam source commit
`1b259763182221be81300b8c1e47e4c7c8207a07`, worker-config returns queue/org
information; by-node configuration checks organization ownership; the public
device-code configuration route is not bearer-bound. Heartbeat ingestion can
accept unlinked telemetry without minting node-identity proof. Device endpoint
allowlists do not themselves grant configuration lookup authority. These are
source observations, not claims about deployed versions.

Consequently, Ping, worker-config, configuration or heartbeat success must not
be displayed or stored as proof that this bearer is linked. A locally saved
token or a hostname match is also insufficient. A future authenticated lookup
would need an independently reviewed contract binding the requesting current
credential to its organization and exact node, with explicit missing, refused
and unavailable outcomes.

The bounded companion source slice reports saved enrollment as UNKNOWN when
no binding proof is available and gives the explicit recovery path
`citadel logout && citadel init`. Authentication/authorization refusal is
reported distinctly from completed linking. This is stateless diagnostic
behavior: it does not manufacture a confirmed marker, replay a grant, repair
permissions or assert that an HTTP 401/403 proves revocation. Fresh browser
approval is the recovery authority for an incomplete legacy enrollment.

## Proposed durable ownership

A future record must bind the exact captured private configuration location,
canonical API origin and relevant base path, organization, actual registered
node ID, credential digest, credential generation and local attempt identity.
Canonicalization must not merge distinct tenant endpoints. Use a digest of the
current bearer without storing or logging the bearer, registration authkey or
device code. A digest binds local observations; it does not prove remote state.

Every credential replacement creates a new generation under a shared private
configuration transaction. Its pending state must be published coherently with
that credential. Before registration, the pending record explicitly lacks a
node ID; it must not borrow one from an earlier attempt. Replacement
credentials inherit no earlier confirmation, grant or permissions, even if the
hostname and organization are unchanged.
Legacy files without a generation/record remain unknown; do not synthesize a
past success during upgrade. Missing, malformed or mismatched records cannot
select another credential or revive an earlier generation.

Confirmation is permitted only after this generation's current link operation
returns an understood success for the registered node. It records a historical
acknowledgement, not continuing authentication, membership, authorization or
server liveness. Later 401/403 responses remain explicit refusal observations;
they do not authorize local permission healing or key extension.

## Publication and failure rules

Before activating a record, all writers of credentials and related state must
participate in the same lock and generation/CAS protocol. This includes API
and Redis credential saves, clearing/logout, link outcome publication and
rollback. Existing map read/modify/write operations cannot be assumed to form
that transaction. A marker-only lock does not protect against an unlocked
credential writer overwriting its snapshot, or vice versa.

Re-read the captured configuration under the shared lock and require the exact
expected generation, digest, endpoint, organization and node before publishing
an outcome. An older attempt must not overwrite a replacement or its record.
Specify bounded schema/read limits, private ownership, alias/collision refusal,
atomic publication and file/directory durability before implementation. A
separate diagnostic file would need the same coherent-selection and bounded
history rules; it is not an automatic escape from writer coordination.

Keep unknown, pending, confirmed, definite refusal and uncertain commit
distinct. In particular, a response may be lost after the server linked the
node. Do not convert that uncertainty into a rollback or a success report.
Retain enough local ownership evidence to explain the incomplete state, with
no reusable expired grant. Recovery requiring a new approval starts a new
generation and does not claim that it revoked an older server credential.

Rollback may affect only fields or material proven to belong to that attempt
and still unchanged under the transaction. Do not restore an old bearer from
a snapshot, overwrite newer configuration, or use prior credentials to finish
a replacement. Enrolled CA trust, certificate/receipt identity and tsnet state
have independent authority. A link failure is not permission to delete,
regenerate or rotate them. Any future cleanup of newly created material needs
exact ownership and unchanged-byte proof; ambiguity retains it for review.

## Review and acceptance gates

First ratify the record schema, generation lifecycle, shared writer coverage,
server acknowledgement contract and uncertain-commit recovery policy. Then
prepare inactive source and fake fixtures for concurrent credential changes,
lost replies, stale generations, partial writes, restart, unknown legacy state,
401/403, changed endpoints and preserved CA/identity material. Fixtures must
prove that no failure replays an expired grant or inherits confirmation.

Actual authenticated server behavior, disk durability, service restarts and
legacy-upgrade recovery require separate acceptance. Until then, the explicit
UNKNOWN diagnostic and fresh logout/init guidance provide remediation without
claiming an automatic repair. No local parser, tests, browser approval, network
request, SQL, native execution or deployment accompanies this proposal.
