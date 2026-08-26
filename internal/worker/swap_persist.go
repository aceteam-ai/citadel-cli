// internal/worker/swap_persist.go
//
// Durable LRU recency for the swap manager (citadel-cli#688).
//
// m.lastUsed (swap.go) is the primary input to sortByLRU, but until now it was
// purely in-process: a worker restart zeroed it, so every engine looked equally
// "never used" and the idle/largest-VRAM tiebreak silently decided instead.
// This file adds an opt-in (WithPersistence) best-effort disk mirror of that map
// so recency survives a restart.
//
// Two things this deliberately does NOT do, because #688 draws the line there:
//   - It does not touch forget()'s behavior. forget() (swap.go) has never
//     deleted lastUsed -- verified against git history before writing this --
//     so an evicted engine already re-enters as "recently used", not "never
//     used" (the issue's suggested fix #2). Adding a delete(m.lastUsed, name)
//     to forget() would be the exact regression #688 exists to prevent; see the
//     comment on forget() in swap.go.
//   - It does not promote LRU to the primary preemption sort key (#688's
//     suggested fix #3). That is out of scope here -- see swap.go's package
//     comment and the #687 ordering note in CLAUDE.md.
//
// What #688 DOES ask for beyond persistence is a "fixed forget": once lastUsed
// is durable, an entry for an engine that is long gone (uninstalled, renamed,
// never coming back) would otherwise survive on disk forever. It is harmless to
// eviction ordering -- PreemptInputs only ever lists currently-running engines
// as candidates, so a stale entry for an absent engine is never read by
// sortByLRU -- but it is unbounded growth with no correctness role, which is
// exactly the kind of state that "becomes a real defect the moment you make it
// durable" (a bit-rotted config file, more entries than any node will ever
// realistically serve). pruneStaleLastUsedLocked bounds that.
package worker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// LastUsedFileName is the persisted file's name under whatever directory
// WithPersistence is given (the caller resolves the directory; see
// cmd/hotswap.go, which uses network.GetNodeConfigDir() -- the machine-
// convergent node config dir, not the invoker-scoped platform.ConfigDir()).
// Exported so the single call site that builds the full path doesn't need a
// second, hand-copied constant that could drift from this one.
const LastUsedFileName = "swap-lru.json"

// lastUsedRetention bounds how long a persisted lastUsed entry survives with no
// further touch/markReady before a flush prunes it. It is deliberately generous
// (weeks, not hours): the failure mode being guarded against is unbounded growth
// from engines that are gone for good, not a live engine that is merely idle for
// a while -- an idle-but-installed engine is exactly what LRU exists to still
// remember correctly.
const lastUsedRetention = 30 * 24 * time.Hour

// swapPersistMinGap is the default debounce interval between disk writes.
// touch() fires on EVERY EnsureResident call, including the already-resident
// fast path, so persisting synchronously there would be one write per inference
// request; this bounds it to roughly one write per gap regardless of request
// rate. Tests override this directly (same package) for determinism.
const swapPersistMinGap = 5 * time.Second

// persistedLastUsed is the on-disk shape. A named field (not a bare map) so the
// format can grow without an ambiguous top-level-array-vs-object migration.
type persistedLastUsed struct {
	LastUsed map[string]time.Time `json:"last_used"`
}

// loadLastUsedFile reads a persisted lastUsed snapshot. Any failure to read or
// parse -- file absent (first run, or persistence just enabled), truncated,
// corrupt JSON, wrong shape -- degrades to "no persisted recency" rather than an
// error: a node that has never had a good file must still start up and serve,
// and a bad file must not cost more than the recency it would have seeded. This
// mirrors TokenHashEntry.UnmarshalJSON's per-entry-lenient reasoning (citadel
// #815) at the whole-file granularity, since there is nothing partial to salvage
// here the way there is with a token list.
func loadLastUsedFile(path string) map[string]time.Time {
	out := map[string]time.Time{}
	if path == "" {
		return out
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	var payload persistedLastUsed
	if err := json.Unmarshal(data, &payload); err != nil {
		return out
	}
	if payload.LastUsed == nil {
		return out
	}
	return payload.LastUsed
}

// writeLastUsedFile atomically persists a snapshot: write to a temp file in the
// same directory, then rename over the target. The rename is what makes this
// atomic from a reader's perspective -- a crash mid-write leaves the OLD file
// (or nothing, on first write), never a half-written one that loadLastUsedFile
// would then have to degrade past. Callers must pass a snapshot, not the live
// map, so this never needs the manager's lock.
func writeLastUsedFile(path string, snapshot map[string]time.Time) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(persistedLastUsed{LastUsed: snapshot})
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".swap-lru-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	_, writeErr := tmp.Write(data)
	closeErr := tmp.Close()
	if writeErr != nil {
		os.Remove(tmpPath)
		return writeErr
	}
	if closeErr != nil {
		os.Remove(tmpPath)
		return closeErr
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}

// SwapManagerOption configures optional SwapManager behavior at construction.
type SwapManagerOption func(*SwapManager)

// WithPersistence enables durable LRU recency: lastUsed is seeded from `path`
// (if present and readable) at construction, and mutating events
// (touch/markReady/eviction) opportunistically flush a fresh snapshot back to
// it, debounced to at most once per swapPersistMinGap. logf receives write
// failures for visibility; it may be nil (no-op). A failed persist is always
// best-effort: it is logged and otherwise ignored, never surfaced as a swap
// error -- losing the on-disk recency mirror must never block a swap decision
// that would otherwise have succeeded purely in memory.
func WithPersistence(path string, logf func(format string, args ...any)) SwapManagerOption {
	return func(m *SwapManager) {
		if path == "" {
			return
		}
		if logf == nil {
			logf = func(string, ...any) {}
		}
		m.persistPath = path
		m.persistLogf = logf
		m.persistMinGap = swapPersistMinGap
		for name, ts := range loadLastUsedFile(path) {
			m.lastUsed[name] = ts
		}
	}
}

// persistIfDue debounces disk writes: it flushes at most once per
// m.persistMinGap, tracked under the dedicated persistMu (deliberately NOT
// m.mu, so the debounce check/update never blocks a swap decision on I/O and a
// flush's snapshot phase never needs to hold m.mu across the write). A no-op
// when persistence was never enabled.
func (m *SwapManager) persistIfDue() {
	if m.persistPath == "" {
		return
	}
	now := m.now()
	m.persistMu.Lock()
	due := now.Sub(m.lastPersistAt) >= m.persistMinGap
	if due {
		m.lastPersistAt = now
	}
	m.persistMu.Unlock()
	if !due {
		return
	}
	m.flushPersist()
}

// flushPersist snapshots lastUsed (pruning stale entries in the same critical
// section) and writes it to disk OUTSIDE the lock -- the "snapshot-then-write"
// shape that keeps a debounced background persist from ever stalling a swap
// decision waiting on m.mu.
func (m *SwapManager) flushPersist() {
	m.mu.Lock()
	m.pruneStaleLastUsedLocked(m.now())
	snapshot := make(map[string]time.Time, len(m.lastUsed))
	for name, ts := range m.lastUsed {
		snapshot[name] = ts
	}
	m.mu.Unlock()

	if err := writeLastUsedFile(m.persistPath, snapshot); err != nil {
		m.persistLogf("[swap] failed to persist lastUsed to %s: %v", m.persistPath, err)
	}
}

// pruneStaleLastUsedLocked drops lastUsed entries older than lastUsedRetention.
// Callers hold m.mu. See the package comment for why this exists only once
// lastUsed is durable, and why it is safe: a pruned engine simply reverts to
// "no lastUsed entry" for sortByLRU, the SAME state it would be in if it had
// never been swapped in on this node at all (or on a node running without
// persistence, today) -- not a new code path, just bounding an old one.
func (m *SwapManager) pruneStaleLastUsedLocked(now time.Time) {
	for name, ts := range m.lastUsed {
		if now.Sub(ts) > lastUsedRetention {
			delete(m.lastUsed, name)
		}
	}
}
