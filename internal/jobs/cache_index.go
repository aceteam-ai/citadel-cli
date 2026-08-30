// internal/jobs/cache_index.go
//
// Wires the durable cache index (citadel #682 P2a, internal/cacheindex,
// docs/design-cache-ownership.md §8) to this package's pull/evict handlers.
//
// cacheIndexFn is a package var (mirroring llamaCppCacheDirFn/
// hfCacheModelSizeFn's existing test seams in model_cache_pull.go), but its
// DEFAULT is nil -- deliberately, and unlike those two seams. This package's
// existing seams default to a real path (a plain string computation with no
// side effect), which is harmless for a test that never reaches the point of
// using it. A *cacheindex.Store is different: a lazily-constructed real
// singleton reachable from any successful pull/evict would mean ANY existing
// or future test in this package that exercises a real pull/evict success
// path (there are several: fake-binary ollama tests, SERVICE_START tests,
// ...) silently creates and writes to a REAL file under THIS machine's
// actual node config dir the moment `go test ./internal/jobs/...` runs --
// exactly what happened once during this feature's own development, before
// this file took its current shape (a real ~/citadel-node/cache-index.json
// was created and had to be manually removed). Requiring an explicit
// InitCacheIndexStore call (made once, by cmd/work.go's runWork -- the real
// `citadel work` process, never a test binary) means every write helper
// below, seeing a nil store, is a safe no-op by construction, and no test
// needs to remember to stub this seam to stay side-effect-free.
//
// Production wiring is a process-wide SINGLETON, not a fresh
// cacheindex.Open() per call: internal/cacheindex's package doc explains why
// -- two independent *cacheindex.Store instances against the SAME file in
// the same process would each flush a full snapshot on write and silently
// clobber the other's most recent change (no merge). One process, one live
// Store, for the whole lifetime of `citadel work`.
package jobs

import (
	"sync"

	"github.com/aceteam-ai/citadel-cli/internal/cacheindex"
)

var (
	cacheIndexMu   sync.Mutex
	cacheIndexReal *cacheindex.Store
)

// cacheIndexFn resolves the process-wide cache index store, or nil if
// InitCacheIndexStore has never been called in this process (the common
// case for every test, and for any CLI entry point other than `citadel
// work`). Overridden directly in tests that want to exercise the write
// wiring against a throwaway cacheindex.Open() on a t.TempDir() path.
var cacheIndexFn = func() *cacheindex.Store {
	cacheIndexMu.Lock()
	defer cacheIndexMu.Unlock()
	return cacheIndexReal
}

// InitCacheIndexStore opens (or, if already open, returns) the process-wide
// cache index store at path. The ONE production call site is cmd/work.go's
// runWork, early in startup -- before startManagedServices' boot-time
// service starts (which can reach ensureOllamaModel) and well before the
// job dispatch loop (which reaches MODEL_CACHE_PULL/MODEL_CACHE_EVICT) --
// so both write paths described in design doc §8.3 are covered for the
// lifetime of the process. See the package doc comment for why this is an
// explicit call rather than an implicit lazy-open default.
//
// Scope note: `citadel agent` (legacy Nexus HTTP polling, cmd/job_handlers.go)
// and `citadel test` register the SAME handler instances but never call
// this, so a pull/evict dispatched through those older entry points gets a
// nil store and skips the index write -- safely (see upsertCacheIndexEntry/
// removeCacheIndexEntry's nil checks), just without coverage. `citadel
// work` is the real production job-dispatch path this feature targets;
// widening this to the legacy entry points is a documented, low-priority
// follow-up, not a P2a requirement.
func InitCacheIndexStore(path string, logf func(format string, args ...any)) *cacheindex.Store {
	cacheIndexMu.Lock()
	defer cacheIndexMu.Unlock()
	if cacheIndexReal == nil {
		cacheIndexReal = cacheindex.Open(path, logf)
	}
	return cacheIndexReal
}

// upsertCacheIndexEntry is the shared call-site helper for every
// MODEL_CACHE_PULL/SERVICE_START pull-success path (design doc §8.3's
// write-site table): best-effort, never fails the pull that already
// succeeded, and a safe no-op when the cache index has not been initialized
// (see the package doc comment). A write failure is logged via ctx.Log at
// "warn" so it is visible in the job's own output, not just the process log
// InitCacheIndexStore's logf writes to. jobID is optional (some call sites,
// e.g. ensureOllamaModel, have no job ID in scope) and omitted from the log
// line when empty.
func upsertCacheIndexEntry(ctx JobContext, jobID string, e cacheindex.Entry) {
	store := cacheIndexFn()
	if store == nil {
		return
	}
	if err := store.Upsert(e); err != nil {
		if jobID != "" {
			ctx.Log("warn", "     - [Job %s] cache index update failed for %s/%s (pull still succeeded): %v", jobID, e.CacheDir, e.Model, err)
		} else {
			ctx.Log("warn", "     - cache index update failed for %s/%s (pull still succeeded): %v", e.CacheDir, e.Model, err)
		}
	}
}

// CacheIndexStore returns the process-wide cache index store, or nil if
// InitCacheIndexStore has not been called yet in this process. Exported so
// cmd/work.go's startup backfill scan (Store.ReconcileScan) and the
// resident-implies-used heartbeat reconciler (Store.MarkUsed, on the
// existing OnStatus fan-out) share the EXACT same in-memory Store the
// pull/evict handlers write through -- see the package doc comment above
// for why a second construction site pointed at the same file would be a
// same-process data-loss hazard, not just a cross-process one. Callers must
// handle a nil return (the index is not available yet, or this process
// never initialized it).
func CacheIndexStore() *cacheindex.Store {
	return cacheIndexFn()
}
