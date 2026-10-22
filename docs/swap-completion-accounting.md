# Swap completion and rate accounting (#1422)

Source-only DRAFT plan, based on main `dc139d1d`. The in-memory rate ledger
currently receives a completed swap after the single-flight slot is released.
Another swap can observe the old count in that gap and exceed the configured
eviction bound. The test-only refusal waits in #1420 do not fix this race.

Ordering reviewed with automated-citadel on 2026-10-22:

1. Build the terminal record while the original operation still owns its slot.
2. Under one existing `m.mu` critical section, append/prune/snapshot the ledger
   and clear only the matching in-flight operation.
3. Unlock, then publish `op.done`. Only after completion publication call the
   optional controller observer, outside the mutex. No extra observer goroutine.

A released single-flight slot and a completed `op.done` must imply that the
terminal record is available to rate admission. This does not mean that every
`EnsureResident` return implies a completed operation: warming, caller timeout
or cancellation and the already-resident fast path have different semantics.

Preserve thresholds, window/pruning/retention, outcome classification, pull
evidence, preflight, resident policy and the independent background context.
Observer delivery remains best effort and may be reordered across operations;
a blocked/reentrant observer must not hold the mutex or delay completed callers
or later admission. This adds no durable storage or restart guarantee.

Prepare deterministic fake-clock/barrier regressions for the completion gap and
the next admission at the ceiling, plus blocked/reentrant observer coverage.
Use fake controllers only; no GPU, models, processes or service calls. Existing
normal GitHub CI is allowed. Local tests, formatter, vet, standalone builds,
native operations, live config/credentials, SQL/deployment, undraft and PR merge
remain unauthorized. Independent source review is required before publication
of the implementation, and source CI is not native eviction acceptance.
