# Wake policies: proposed atomic reservation boundary

Refs #1442. This is a design for independent review, not an implemented
transaction, storage format or permission to wake a lane. The existing
[supervisor design](agents-wake-design.md) and
[quota contract](agent-wake-quota-policy.md) retain their execution gates.
The next proposed source slice is an inactive, pure combined-reservation
planner with fake fixtures only. It must be separately accepted before code.

## Current source and the missing boundary

`agentresources.PlanReserve` proposes a worktree allocation and increments its
snapshot revision. `agentquota.PlanReserve` proposes a charged lane reservation
and increments a different snapshot revision. Neither function commits state.
Both packages are now merged, including #1512's harness limits, but neither has
a production caller. Calling the two functions or committing two records in
sequence does not make an atomic admission decision.

In particular, the successful resource proposal must not survive a quota hold,
and a quota proposal must not be charged independently of a refused resource
reservation. A lost commit response must never lead to a second reservation,
even when a stale copy appears free. The quota charge must be committed before
any later native attempt, and uncertainty must never refund it.

## Proposed input and replacement

Use one complete host/pool aggregate containing current policy, both child
snapshots and explicit links between occupied lane reservations and worktree
allocations. These are internal concepts for a later pure model, not proposed
wire fields, a file layout or a new backend API.

The canonical storage namespace is host plus pool only. Config, lane, harness,
clock epoch, process incarnation and policy changes must not select a new empty
ledger. A change to the existing `ConfigID` binding holds for separately reviewed
migration; ordinary cap reductions must preserve charges and custody.

One outer revision fences the entire aggregate. At an accepted boundary both
child revisions must equal it. A planning round derives both child contexts
from that same immutable input and the same trusted observations. Both child
proposals must name the same expected revision and produce its checked
successor. There is no sequential commit or independently usable child result.
An unchanged child in a later operation must still be fenced by the outer
revision; how those later operations advance it is outside the first slice.

Compare host, pool and config identities exactly in both models. Their current
identifier grammars differ: resource identifiers permit characters quota scope
identifiers reject. Reject an identity that does not validate in both; do not
lowercase, normalize or silently translate it. Resource worktree references
remain opaque references and must never be joined into paths.

The aggregate owns its current policy content. A future writer must fence policy
changes through the same outer revision, retaining debt and allocation custody.
A config filename, unchanged `ConfigID`, supplied version label or caller's
assertion alone cannot establish current policy. File-only opt-out or cap edits
must not leave a previously prepared admission committable. This proposal adds
no policy-edit operation or live configuration installation.

## First proposed source slice: reserve only

The future pure planner would validate the complete input and its cross-model
links before copying it. Each occupied quota reservation must reference exactly
one existing allocation, and that allocation must not be lent to another
occupied lane reservation. Numeric IDs from the two models are separate
namespaces, not interchangeable evidence. Historical inactive quota charges
need not retain an allocation after independently proved worktree removal.
Unlinked idle allocations may remain counted in the resource pool; they are
not implicitly available for a new turn.

For a new reservation, evaluate the existing resource and quota reserve
functions against that one input. Only when both succeed may the planner
return one hypothetical replacement containing both changes and their exact
link. A hold returns no replacement and changes neither input. Do not expose a
resource-only or quota-only admission path. Preserve typed deadline and disk
pressure cancellation requirements as diagnostic data; they do not send a stop
or prove inactive custody.

This first slice reserves a new worktree reference only. It does not reuse an
idle worktree, create a directory, mark a worktree created, attempt a turn,
accept a native receipt, edit policy or perform cleanup. The resource model
currently has reserve and cleanup proposals but no complete creation/native
attachment lifecycle. Those transitions need their own source contract before
attempt integration; do not synthesize a successful creation receipt.

Use the approved per-harness defaults: Codex 48 and Claude Code 10 reservations
per lane per rolling 24 hours, at most 30 minutes per turn and one occupied
custody record per lane. Explicit harness/lane limits may lower these values.
This adds no host-wide turn serialization or additional money/product cap.
Pool capacity and disk-floor units remain explicit reviewed policy inputs, not
new activating defaults. The existing resource floor validation still applies.

## Observations, commit ambiguity and later lifecycle

Clock, store continuity and disk evidence remain separate trusted inputs.
Unknown, changed or backwards logical time; rollback or uncertain commit; and
unknown/stale disk evidence all hold. A successful structural validation,
`Complete` flag or caller-provided "continuous" value proves none of those
facts. The first pure slice must not acquire, persist, refresh or upgrade them.

A future store adapter must atomically compare the complete outer revision and
current policy, then durably replace the aggregate. A proposal, matching number,
write attempt or lost reply is not a confirmed commit. Unknown outcome blocks
further admission until a separately reviewed trusted continuity/reconciliation
path resolves that exact operation. No retry under a fresh lane, config, key or
incarnation may manufacture a replacement reservation. The pure planner adds
neither that store adapter nor a commit-acknowledgement API.

Plans remain mutable hypothetical data, not capabilities. A future adapter must
validate the exact candidate against trusted current state; it must not accept
caller-edited child results, links or policy merely because their revision
numbers match. No returned object authorizes native work.

Later source work must settle policy edits and draining before it can compose
all transitions. For example, current `agentresources.Validate` rejects a
snapshot whose allocation count exceeds `PoolCap`. Simply lowering that cap
below existing occupancy would block cleanup as well as admission. A separately
reviewed distinction between retained custody and new-admission eligibility is
needed; do not drop allocations, raise the requested cap or pretend the current
model already supports that edit. Existing quota debt after harness/cap changes
must likewise remain intact.

Exact actual-inactivity evidence and resource cleanup receipts remain distinct.
A quota stop request or terminal result cannot prove a worktree removable; a
removed directory cannot prove a native turn stopped. No timeout, restart or
age boundary releases unknown custody. Reuse, cleanup and lifecycle composition
remain held rather than being inferred from combined reservation.

## Proposed fake proofs and implementation gates

Before an implementation review, require deterministic fixtures for:

- Resource success plus quota refusal, and quota success plus resource refusal:
  no replacement, no input mutation or partial reservation.
- Two lane proposals from the same aggregate: one fake revision comparison may
  win; the other cannot commit either child or rebase its old observations.
- Exact identity, revision and policy binding, including a policy edit between
  planning and fake comparison. No fresh ledger on config/harness changes.
- Wrong, duplicate, missing or cross-lane links; unrelated ID namespace
  collisions; occupied worktrees; and malformed/incomplete input rejected before
  a replacement is constructed.
- Clock/store/disk uncertainty independently holding, including a lost fake
  write response that does not permit another reservation or refund.
- The approved harness/lane caps and one occupied lane, checked successor
  revisions/counters, deep-copy isolation and caller-edited proposal refusal at
  the fake commit boundary.

A fake comparison tests only the proposed invariants. It proves no real CAS,
durability, trusted observations, native custody or deployment safety. Structural
bounds for any new aggregate/link collections must be explicit and reviewed
before copying; existing per-model validation is not a bounded wire decoder.

Independent acceptance of this source design is the next gate. A later pure
source DRAFT must keep store/clock/resource/native adapters, v2 intake,
checkpoint and presence, native tool enforcement, install/uninstall and live
wake acceptance absent or held. #1442 stays open and inactive.
