# Inactive supervisor resource model

`internal/agentresources` is a pure source proposal for the disk-floor and owned
worktree-pool portion of #1442. It has no runtime caller and performs no I/O,
filesystem inspection, clock read, storage write, CAS, deletion, cancellation,
harness submission, enrollment or service change. A successful `Plan` contains
only an expected revision and an independently cloned hypothetical snapshot.
It is not a committed reservation or permission to perform a native action.

## Explicit policy and observations

`Policy` requires exact, bounded host/pool/configuration IDs, a positive finite
pool cap and all three repository/worktree/state filesystem IDs. IDs have one
case-sensitive ASCII representation; they are opaque references, not paths.
The model does not establish physical identity, canonical filesystem aliases,
ownership or the truth of a supplied configuration. Those need separate adapters.

`FloorBytes` requires an amount of at least 40 and an explicit `GB` or `GiB` unit.
GB means 1,000,000,000 bytes; GiB means 1,073,741,824 bytes. Checked multiplication
refuses overflow. Neither unit is selected by default and there is no raw-byte
configuration that can turn the owner's floor into forty bytes. This package
does not ratify or activate either unit choice, install configuration or select
a pool cap.

`EvaluateDisk` requires every distinct configured filesystem to have known,
fresh free-byte evidence bound to the exact identity, snapshot revision and
decision round. Shared filesystem IDs need only one observation; duplicate or
foreign observations refuse. Equality at the configured threshold passes.
Missing, unknown or stale observations hold rather than inventing free space.
A known below-floor observation holds and carries copied allocation IDs whose
custody is active or unknown, indicating that controlled cancellation is required.
Known pressure still reports that requirement when another filesystem is unknown.
This neither sends cancellation nor establishes that any process stopped.

`Complete`, `Fresh`, `Owned` and the other positive evidence flags are supplied
input assertions for deterministic fixtures. They do not prove real completeness,
ownership or freshness. The package defines Go values, not a persisted schema or
decoder, so its validation does not establish external schema/decode validity.
There is no elapsed-time threshold or clock port here. Real freshness, inventory
completeness and trustworthy observations remain unimplemented gates.

## Conservative pool accounting

`Validate` refuses missing/default/incomplete snapshots, identity mismatch,
over-capacity state, unknown phases/custody, inconsistent idle/in-use custody,
duplicate or unsorted allocation IDs and duplicate exact worktree references.
It does not repair, truncate or reinterpret invalid data as an empty pool.
A known empty inventory must be explicitly complete with a non-nil empty list.

Every accepted allocation phase consumes a slot: `Reserved`, `Creating`,
`CreateUnknown`, `Idle`, `InUse`, `CleanupPending` and `CleanupUnknown`.
The package observes/counts creation and use phases but supplies no native
creation/use transition API. It does not enforce one active turn per lane.

`PlanReserve` requires valid disk evidence and capacity. It chooses the next
monotonically increasing allocation ID; removal never decrements that counter.
Overflow holds rather than wrapping or reusing an ID. The worktree reference
must be distinct from every occupied allocation. A reserved entry remains
occupied even when creation never starts: no implicit absence or refund exists.

`PlanBeginCleanup` accepts only an idle, inactive allocation with fresh positive
owned/clean/published/inactive evidence matching its allocation, exact worktree
reference and current context. It records a cleanup attempt identified by the
next snapshot revision. The slot stays occupied. `PlanCleanupUncertain` retains
that same allocation and attempt in `CleanupUnknown`; it neither retries deletion
nor releases capacity.

`PlanConfirmRemoval` requires a fresh confirmed-removal receipt bound to that
allocation, exact worktree/filesystem reference, cleanup attempt and current
context. Only a pending/unknown cleanup with inactive custody can be removed.
An old receipt cannot release a later allocation even if it uses the same
worktree reference. Unknown custody continues to hold. All of these inputs model
evidence; they do not authenticate an actual filesystem or native cleanup proof.

## Outstanding integration and verification

Two plans prepared from the same snapshot return the same expected revision.
They are conflicting proposals, not evidence that both reservations are safe.
Future host-wide CAS must choose at most one, durably commit it and reconcile
uncertain outcomes. This package has no application or persistence adapter.
The counter does not detect a rolled-back store; no durability, atomicity,
crash recovery, rollback resistance or real filesystem race guarantee is claimed.

Rolling turn quotas, clock/store continuity, one-active-turn-per-lane enforcement,
real custody/cancellation proof, checkpoint authority, inbox batching, presence,
native harness enforcement and installer acceptance remain separate #1442 gates.
No full supervisor completion or activation follows from this partial proposal.

Source fixtures cover unit conversion and threshold boundaries, every occupied
phase, literal identity collisions, stale/unknown evidence, cleanup/removal
fences, counter exhaustion and independent plan ownership. They use supplied
values only. Their eventual execution cannot prove real filesystem, storage,
clock or native safety. Local candidate execution remains held; normal protected
GitHub CI can verify the source proposal under the existing session authorization.
