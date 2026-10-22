# Swap completion accounting

`SwapManager.completeSwap` owns terminal ledger publication and single-flight
handoff: it appends/prunes/snapshots the in-memory ledger before releasing the
matching slot in one `m.mu` critical section, then closes `op.done`. Optional
`ObserveSwap` runs after completion publication, outside the lock; a slow or
reentrant observer must not delay completed callers or later admission, and
observer order across operations is unspecified.

A released slot and completed `op.done` imply accounting is available to rate
admission. Warming, caller timeout/cancellation and the already-resident
`EnsureResident` fast path do not universally imply a completed swap. The ledger
remains in-process and supplies no restart/durability guarantee.
