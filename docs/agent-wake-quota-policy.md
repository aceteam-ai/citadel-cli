# Inactive wake quota model

`internal/agentquota` is a pure source proposal for #1442. It accepts bounded
typed inputs and returns a hypothetical replacement snapshot with an expected
revision, or a static typed `Hold`. It has no runtime caller, configuration
installation, persistence schema, filesystem, clock, store, native process or
credential adapter. A `Plan` is neither a committed reservation nor permission
to start, retry, stop or otherwise operate a native process. This partial model
does not complete the governed-lane supervisor.

## One aggregate and explicit policy

All lanes share one host/pool-wide snapshot, revision and monotonic ID counter.
The future canonical storage key must be **host plus pool only**. Configuration,
lane, clock epoch and restart identity must never select a fresh ledger. Exact
`ConfigID` is a record binding: changing it holds pending separately reviewed
migration. Explicit cap reductions and opt-out may use the existing binding;
they do not erase stored debt. Supplied canonical-looking identifiers do not
prove physical host identity, alias exclusion or canonical storage keys.
`ConfigID` also does not prove that supplied policy values are current. The
future outer CAS must bind and recheck current policy version/content together
with quota and resource state, so a pre-opt-out plan cannot commit against stale
file-only policy assertions. Opt-out and cap edits must bump that shared
revision without discarding charges or custody; file-only edits cannot provide
this fence. No real policy-edit operation or adapter is supplied here.

Every configured lane declares its enabled state, a positive cap no greater
than ten turns per rolling 24 hours and a positive limit no greater than 30
minutes per turn. There are no activating defaults. A missing policy collection
holds; an explicitly empty collection disables admission for every lane. A
stored lane removed from policy remains in the aggregate and can still be
stopped and released with exact evidence. A new lane without existing state
holds; no genesis, migration, deletion, repair or reset API is supplied.

`Validate` bounds the aggregate to 64 lanes and eleven retained charges per
lane before copying, rejects incomplete/malformed collections and unknown
phases, checks globally distinct issued IDs and allows at most one occupied
custody slot per lane. Eleven recent charges are invalid even if none is
active. A lower current cap can leave more recent debt than that cap; this is
retained debt, not an empty ledger or corrupt-state repair opportunity.

## Clock and store are separate trust inputs

Every decision supplies a context with exact scope, expected revision and a
round strictly greater than the stored round. Both observations bind that same
context. The clock observation must explicitly assert a trusted logical epoch
and elapsed millisecond sample. Missing/unknown clock state, changed epoch or a
sample below the stored high water holds. Equal elapsed samples are allowed;
successful proposals retain the new round and high water. A large forward
number alone does not establish clock trust or permission to age debt.

Independently, the store observation must explicitly assert the same continuity
identity and continuous state. Unknown storage, rollback, changed continuity
or uncertain commit holds, even if the clock is valid or a supplied older
snapshot appears empty. The model does not produce, refresh or promote that
assertion. A future trusted adapter must establish freshness, durable
completeness, no rollback and resolution of ambiguous commits against a trusted
continuity witness. A caller changing a typed value from `commit_unknown` to
`continuous` is not that proof. No clock observation repairs store continuity.

The supplied `Complete` flag, identity, observation binding and inactivity state
are assertions, not actual decode validity, freshness, durability or native
proof furnished by this package. Historical snapshots cannot be trusted merely
because they pass structural validation.

## Charged custody transitions

| Operation | Proposed change |
| --- | --- |
| `PlanReserve` | Charge before attempting, mint reservation ID, occupy lane in `Reserved`. |
| `PlanAttempt` | Mint one attempt ID and enter `Attempting`; never reuse a prior attempt. |
| `PlanAttemptUncertain` | Enter `AttemptUnknown`, preserving charge, attempt and occupied custody. |
| `PlanRequestStop` | Mint an exact stop ID from `Reserved`, `Attempting` or `AttemptUnknown`; enter `Stopping`. |
| `PlanStopUncertain` | Enter `StopUnknown`, preserving the same stop ID and occupied custody. |
| `PlanConfirmInactive` | Require exact confirmed-inactivity evidence; mark inactive without refunding recent debt. |

Reservation, attempt and stop IDs share the one monotonic counter. Already
attempting or uncertain attempts cannot issue another native attempt. Their
first stop ID is permitted; stopping and stop-unknown phases cannot mint a
second stop ID. Repeated plans are not independently executable: future commit
and native-custody integration must prevent replay of a committed attempt.
Neither an attempt failure, timeout, cancellation request nor uncertain outcome
refunds a charge or releases custody. Even a never-attempted reservation needs
a stop ID and confirmed inactivity before releasing its lane.

Admission checks only the target lane's quota and custody; another lane being
overdue does not freeze independent lane admission. Clock or store uncertainty
holds the whole aggregate. At or beyond the target turn deadline, admission
returns `HoldDeadline` and bounded `StopRequired` custody references. That is a
stop requirement, not a sent stop, terminal result, release or receipt.
Reservation time starts the budget, conservatively including any wait before
attempt. Attempt admission uses the smaller of stored and current turn limits
and stores that tightened limit; subsequent increases cannot extend it.

Rolling eligibility uses `now - reservedAt < WindowMillis`; exactly 24 hours
ages a charge, one millisecond earlier does not. Only proven-aged **inactive**
charges are pruned on a valid proposal. Active, reserved, stopping and unknown
charges remain occupied even after 24 hours. Nothing is pruned on a hold.
Elapsed comparisons use subtraction rather than overflow-prone deadline sums.
Admission also refuses an unrepresentable future deadline with `HoldCounter`;
an imported such reservation can still use exact stop/cleanup transitions.
Exhausted IDs, revisions or decision rounds hold without wrap or reset. For
example, an attempt that consumes the last issuable ID can leave no stop ID;
a proposal reaching the last revision or decision round cannot advance again,
including for confirmed-inactivity cleanup. ID-only exhaustion need not block
cleanup that mints no ID. This does not promise every supplied or proposed state
remains drainable through exhaustion; exceptional counter/epoch recovery remains
separately reviewed work. Nothing silently releases custody to restore liveness.

## Exact release and future commit boundary

An inactivity receipt binds current scope/revision/round and exact lane,
reservation, attempt and stop IDs, plus explicit confirmed actual inactivity.
Wrong IDs, active/unknown status and stale observations hold. An unrelated
lane's aggregate commit changes observation freshness, not the native identity:
the same actual inactivity fact may be freshly re-observed against the new
revision without repeating native work. Merely rewriting binding fields is not
proof that a future adapter made that observation. An old fact must never be
reassigned to later same-lane custody. Inactive history may age out, but the
monotonic counter and continuity state never reset or recycle its IDs.

The future adapter must resolve the **single host/pool CAS** and confirm its
commit before any native admission. Unknown commit must retain uncertainty and
block further admissions until trusted resolution. Competing lane plans from
the same revision are alternatives, not two reservations. This package has no
CAS or persistence implementation and cannot prove process-exit custody,
clock continuity, storage rollback resistance or exactly-once launch.

The resource model proposed in DRAFT #1453 is not a dependency or import here.
Future quota/resource composition must use one outer host/pool envelope and
one atomic commit of both decisions, not separate per-lane stores or sequential
authorization commits. Disk/worktree receipts, quota charges and confirmed
native custody must agree under that later reviewed integration. Enrollment,
checkpoint/presence, intake, harness/hooks, service control and activation are
outside this source proposal.

## Source fixtures and remaining acceptance

Fixtures use only typed in-memory inputs and an explicit fake revision
comparison. They cover rolling boundaries/caps, one-shot attempts, uncertainty,
deadline retention, policy reduction/opt-out, independent clock/store holds,
exact release/re-observation/replay, malformed state, checked numeric limits,
cross-lane competition and nested-copy isolation. The fake comparison proves
only that two proposals name the same expected revision; it proves nothing
about real CAS races, durable commits or native launch. No local candidate
formatter, parser, tests, build or native execution accompanied this proposal.
Normal protected GitHub CI and independent source review remain necessary;
real adapters, continuity proof, runtime integration and native acceptance
remain held.
