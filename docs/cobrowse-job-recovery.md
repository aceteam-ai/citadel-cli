# Recovering a co-browse start by job ID

This is the Citadel companion to aceteam-ai/aceteam#13895 and its scoped
`COBROWSE_SESSION` recovery contract. It does not activate the Free-tier billing
policy or claim native acceptance. Unscoped legacy paid starts retain their
existing handler path.

The authenticated producer chooses a canonical UUID `jobId` before quota
reservation. A scoped start carries server-derived `caller_org_id` and
`owner_user_id`. The target is the dispatcher's existing exact-node routing, not
a new payload field. It must match this worker's configured control-node ID and the exact `jobs:v1:shell:org_<node-org>:node:<id>` transport
stream. A payload cannot supply stream evidence. The routing node org may differ
from the authorized billing caller org on a shared node. Scope authentication and
owned-or-shared access remain the producer's responsibility.

## Durable execution boundary

`cobrowserecovery.Engine` reserves the job, scope, hashed request and preassigned
session handle before the browser call. `FileStore` requires a private, owned Unix
directory and regular, owned, single-link files. It refuses links and public
permissions. A snapshot file is fsynced, renamed and its directory fsynced before
launch. Creation of the journal directory is synced in its parent too. Any
failure or uncertain publication refuses launch. An empty snapshot is initialized
only by the process that creates a fresh journal directory; a missing snapshot
in an existing directory is a continuity failure and refuses every recovery
operation. External deletion/replacement of the entire journal directory is not
a supported recovery procedure. The journal contains neither
PINs, profile names, raw requests, URL queries nor page content. The request hash
uses an ordered JSON tuple of canonical URL and profile; PIN is excluded, so a
failed PIN attempt is terminal under that job ID.

An OS advisory lock serializes journal operations **including** browser launch
and stop. Contention returns a retryable journal-unavailable error. In particular,
a stop racing a launch cannot claim success while that launch can still finish;
the recovery caller keeps its reservation and retries the same job ID after the
launch finishes. This intentionally trades concurrent journal access for a simple
cross-process execution fence. It does not introduce an unbounded wait for a lock.

A duplicate scoped start returns its existing running or terminal identity. It
never invokes another launch. A changed scope or request conflicts. A stop before
start writes a durable terminal tombstone, preventing any late delivery from
launching. Records are bounded at 8,192 per node and the encoded journal at 8 MiB.
One reserved staging name bounds crash leftovers to one additional journal-sized
file; lock descriptors are close-on-exec so a browser cannot inherit custody.
Active and unknown records are never evicted. Terminal tombstones remain at least
48 hours; wall-clock rollback does not expire them. Clock correction far into the
future requires the same operator care as other wall-time retention policies.

## Recovery actions and honest uncertainty

`job_status` and `stop_by_job_id` accept `start_job_id` and the same server-derived
scope. Their receipts contain only `start_job_id`, `state` and, when known,
`session_id`. They never enumerate other sessions or disclose page data.

States are `not_found`, `launching`, `running`, `stop_requested`, `stopped`,
`ended`, `failed` and `unknown`. Neither `not_found` nor `unknown` proves that an
execution never happened. On restart, the preassigned session handle allows the
existing manager/registry to reconcile a surviving browser. Missing or
unverifiable manager/registry state remains `unknown`, without a fresh launch.
Only an observed browser-exit channel followed by confirmed display/profile
cleanup supports `ended`. The older best-effort
registry's expired label is not sufficient evidence.

A stop intent is synced first. `stopped` requires successful teardown and observed
browser exit, or a pre-delivery tombstone that never authorized launch. A missing
session, unresolved launch, failed cleanup or unobserved exit does not confirm a
stop. A lost or unverifiable registry can therefore need operator reconciliation;
recovery must retain the charge and must not invent a successful stop. Persistent
profile cleanup still belongs to the existing session manager.

## Capability, paired rollout and acceptance

`capabilities.cobrowse_job_recovery` is a boolean emitted from the actual live
configured handler, not inferred from a version string. Disabled, absent or
unbound adapters do not advertise support. The Unix adapter supports the durable
journal. Windows refuses until a protected-DACL and durable-publication adapter
exists. The backend must ingest this additive heartbeat field, fail closed on an
absent/false value, and probe scoped `job_status` on the exact node before Free
quota reservation. The backend's matched reservation/reaper implementation is a
required companion; this source alone enables no billing policy.

Offline tests use fake browsers and owned private storage, with isolated HOME and
XDG and external networking blocked. Native acceptance remains separate: lose a
start reply and its result reader, confirm one browser and one charge, then run
the quota sweep and confirm browser exit before releasing the reservation. Also
exercise a node restart with a live registry, unknown registry state, a persistent
profile PIN failure, and Unix storage failure. No service restart or live browser
acceptance is authorized by this document.
