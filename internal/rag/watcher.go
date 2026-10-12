package rag

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/fsnotify/fsnotify"
)

// debounceDelay is how long a burst of filesystem events is coalesced before a
// re-index runs. A single editor save fires many events (create tmp, write,
// rename); debouncing avoids re-indexing on each one.
const defaultDebounceDelay = 3 * time.Second

// maxWatchedDirs caps how many directories the watcher registers. Each watched
// directory consumes one inotify watch (Linux max_user_watches is commonly
// 8192); authorizing a huge tree like $HOME could otherwise exhaust the limit
// and fail with "no space left on device". Past the cap we stop adding and log
// a warning rather than failing.
const maxWatchedDirs = 4096

// watchSkipDirs are directory names never watched or indexed, mirroring the
// FILE_INDEX walk so the watcher and the indexer agree on what to ignore.
var watchSkipDirs = map[string]struct{}{
	".git":         {},
	"node_modules": {},
	"__pycache__":  {},
	".venv":        {},
	"vendor":       {},
}

// debouncer coalesces rapid trigger() calls into a single fn() invocation that
// runs debounceDelay after the LAST trigger. It is safe for concurrent use and
// is unit-tested independently of the filesystem.
type debouncer struct {
	mu      sync.Mutex
	delay   time.Duration
	timer   *time.Timer
	fn      func()
	stopped bool
}

func newDebouncer(delay time.Duration, fn func()) *debouncer {
	return &debouncer{delay: delay, fn: fn}
}

// trigger (re)starts the debounce timer. fn runs once, delay after the final
// trigger in a burst.
func (d *debouncer) trigger() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped {
		return
	}
	if d.timer != nil {
		d.timer.Stop()
	}
	d.timer = time.AfterFunc(d.delay, d.fn)
}

// stop cancels any pending invocation and prevents future ones.
func (d *debouncer) stop() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stopped = true
	if d.timer != nil {
		d.timer.Stop()
		d.timer = nil
	}
}

// UsageSink records one watcher-driven re-index of a root as a usage record
// (aceteam#10876 C6, origin=watcher). The cmd layer wires the production sink
// (writing to the node's usage.db); tests inject an in-memory capture. nil means
// usage recording is disabled — the re-index still runs. It is called once per
// re-indexed root, after a successful Index, with the elapsed time's start.
type UsageSink func(root string, res IndexResult, start time.Time)

// Watcher watches the authorized roots and triggers an incremental re-index of
// the node-local semantic index when files under them change. It is best-effort:
// construction and operation degrade to a logged warning rather than failing the
// caller (a watcher problem must never take down `citadel work`).
//
// Re-indexing is PER-ROOT: an event under root A re-indexes only A (not every
// root), both to avoid thrash and so the usage sink attributes work to the root
// that actually changed.
type Watcher struct {
	svc   *Service
	roots []string
	fsw   *fsnotify.Watcher
	deb   *debouncer
	logf  func(string, ...any)
	// dbPaths is the set of files (plus their -wal/-shm/-journal siblings) the
	// re-index itself writes — the index DB AND, once IgnorePath is called from
	// cmd, the usage.db. Events on these are ignored so a root that happens to
	// CONTAIN them (e.g. an operator authorizing ~ or ~/citadel-node) cannot
	// create a self-sustaining write -> event -> index -> write loop that never
	// idles.
	dbPaths map[string]struct{}

	// dirty is the set of roots with a pending (debounced) re-index; dirtyMu
	// guards it. loop() marks the specific root an event fell under; a fire
	// drains and re-indexes exactly those roots.
	dirtyMu sync.Mutex
	dirty   map[string]struct{}

	// usageSink, when set, records each re-index as an origin=watcher usage
	// record. nil disables recording (the re-index still runs).
	usageSink UsageSink

	// reindexMu serializes re-index runs so a burst can never run two Index calls
	// over the same DB concurrently.
	reindexMu sync.Mutex

	closeOnce sync.Once
}

// NewWatcher builds a watcher over the given authorized roots, indexing into the
// same node-local index the Service uses. workspaceForDB locates index.db (see
// NewWithRoots). Returns an error only if the underlying fsnotify watcher cannot
// be created.
func NewWatcher(roots []string, workspaceForDB, modelOverride string) (*Watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	svc := NewWithRoots(roots, workspaceForDB, modelOverride)
	// Watcher-driven re-indexing is attributed as origin=watcher on the usage
	// record (aceteam#10876 C6), distinct from a `citadel rag index` (local_cli).
	svc.SetOrigin(jobs.OriginWatcher)
	db := svc.DBPath()
	dbPaths := map[string]struct{}{
		db:              {},
		db + "-wal":     {},
		db + "-shm":     {},
		db + "-journal": {},
	}
	w := &Watcher{
		svc:     svc,
		roots:   roots,
		fsw:     fsw,
		logf:    log.Printf,
		dbPaths: dbPaths,
		dirty:   make(map[string]struct{}),
	}
	w.deb = newDebouncer(defaultDebounceDelay, w.reindexDirty)
	return w, nil
}

// SetUsageSink wires the usage-record sink invoked once per re-indexed root
// (origin=watcher). nil disables usage recording. Set before Start.
func (w *Watcher) SetUsageSink(sink UsageSink) { w.usageSink = sink }

// IgnorePath adds p (and its -wal/-shm/-journal siblings) to the self-trigger
// ignore set, so a file the re-index itself writes under a watched root — the
// usage.db in particular — cannot create a write -> event -> re-index loop. Call
// before Start.
func (w *Watcher) IgnorePath(p string) {
	if p == "" {
		return
	}
	for _, s := range []string{p, p + "-wal", p + "-shm", p + "-journal"} {
		w.dbPaths[s] = struct{}{}
	}
}

// SetLogf overrides the log function (tests inject a capturing logger).
func (w *Watcher) SetLogf(f func(string, ...any)) { w.logf = f }

// SetDebounceDelay overrides the debounce window (tests use a short delay).
func (w *Watcher) SetDebounceDelay(d time.Duration) {
	w.deb.stop()
	w.deb = newDebouncer(d, w.reindexDirty)
}

// Start registers watches for every root (recursively), kicks off an initial
// index-on-startup, and runs the event loop until ctx is cancelled or Close is
// called. It returns immediately; the event loop runs in a goroutine.
func (w *Watcher) Start(ctx context.Context) {
	// Index-on-startup: bring the index current with whatever is on disk now,
	// before we begin reacting to changes. Best-effort (TEI may be down). This
	// legitimately re-indexes every root, and each emits an origin=watcher record.
	go w.reindexRoots(w.roots)

	for _, root := range w.roots {
		w.addWatchRecursive(root)
	}

	go w.loop(ctx)
}

// addWatchRecursive walks dir and adds a watch for it and every subdirectory,
// skipping noise dirs and respecting maxWatchedDirs.
func (w *Watcher) addWatchRecursive(dir string) {
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entry: skip, don't abort the walk
		}
		if !d.IsDir() {
			return nil
		}
		if _, skip := watchSkipDirs[d.Name()]; skip && p != dir {
			return filepath.SkipDir
		}
		if len(w.fsw.WatchList()) >= maxWatchedDirs {
			w.logf("citadel search watcher: reached %d watched dirs, not watching more under %s", maxWatchedDirs, dir)
			return filepath.SkipDir
		}
		if err := w.fsw.Add(p); err != nil {
			w.logf("citadel search watcher: cannot watch %s: %v", p, err)
		}
		return nil
	})
}

// loop drains fsnotify events, extends watches to newly-created directories, and
// debounces change events into re-index runs.
func (w *Watcher) loop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			w.Close()
			return
		case ev, ok := <-w.fsw.Events:
			if !ok {
				return
			}
			// Ignore events on the index DB itself (and its WAL/SHM/journal
			// siblings): the re-index writes them, so reacting would loop forever
			// when a root contains the index.
			if _, isDB := w.dbPaths[ev.Name]; isDB {
				continue
			}
			// A newly-created directory must be watched too (fsnotify is not
			// recursive). Its files will be picked up by the debounced re-index.
			if ev.Op&fsnotify.Create != 0 {
				if info, err := os.Stat(ev.Name); err == nil && info.IsDir() {
					if _, skip := watchSkipDirs[filepath.Base(ev.Name)]; !skip {
						w.addWatchRecursive(ev.Name)
					}
				}
			}
			// Any create/write/remove/rename UNDER a bound root warrants a
			// re-index of THAT root. Chmod-only events are ignored (no content
			// change). An event outside every root (which can happen for a watch
			// that lingered after a root was de-authorized) is dropped — it marks
			// no root dirty, so it produces no record.
			if ev.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Remove|fsnotify.Rename) != 0 {
				if root := rootForPath(w.roots, ev.Name); root != "" {
					w.markDirty(root)
				}
			}
		case err, ok := <-w.fsw.Errors:
			if !ok {
				return
			}
			w.logf("citadel search watcher error: %v", err)
		}
	}
}

// markDirty records that root has a pending change and (re)arms the debouncer.
// The debounced fire re-indexes exactly the roots marked dirty since the last
// fire, not every root.
func (w *Watcher) markDirty(root string) {
	w.dirtyMu.Lock()
	w.dirty[root] = struct{}{}
	w.dirtyMu.Unlock()
	w.deb.trigger()
}

// reindexDirty drains the pending dirty-root set and re-indexes exactly those
// roots. It is the debouncer's fire callback.
func (w *Watcher) reindexDirty() {
	w.dirtyMu.Lock()
	roots := make([]string, 0, len(w.dirty))
	for r := range w.dirty {
		roots = append(roots, r)
	}
	w.dirty = make(map[string]struct{})
	w.dirtyMu.Unlock()
	if len(roots) == 0 {
		return
	}
	w.reindexRoots(roots)
}

// reindexRoots re-indexes the given roots. It is serialized so overlapping
// triggers cannot run concurrent Index calls over the same DB, and NEVER passes
// a file_pattern (prune compares against files visited, so a narrowed run would
// delete siblings from the index). Each successfully re-indexed root is recorded
// via the usage sink (origin=watcher). Errors are logged, not fatal.
func (w *Watcher) reindexRoots(roots []string) {
	w.reindexMu.Lock()
	defer w.reindexMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	for _, root := range roots {
		start := time.Now()
		res, err := w.svc.Index(ctx, root, "")
		if err != nil {
			w.logf("citadel search watcher: reindex %s failed: %v", root, err)
			continue
		}
		// Only record meterable work. A re-index that indexed and pruned nothing
		// (the common case on an index-on-startup over an already-warm index, and
		// on a debounced fire for a no-content change) did no billable work, so it
		// writes no ledger row — otherwise an hourly worker restart would spam the
		// usage ledger with zero-unit rows, one per root.
		if w.usageSink != nil && (res.FilesIndexed > 0 || res.FilesRemoved > 0) {
			w.usageSink(root, res, start)
		}
	}
}

// rootForPath returns the authorized root that p resolves under, or "" when p is
// under none (so an event outside every root produces no record). The check is
// lexical containment via withinDir (filepath.Rel), not a string prefix, so a
// sibling like /data/rootEVIL is never matched by /data/root. Watched paths are
// derived from the already-normalized roots, so a lexical check is sufficient.
func rootForPath(roots []string, p string) string {
	for _, r := range roots {
		if r != "" && withinDir(r, p) {
			return r
		}
	}
	return ""
}

// withinDir reports whether target is dir itself or lexically beneath it, using
// filepath.Rel so /data/root does not match /data/rootEVIL. An un-relatable pair
// (different volumes on Windows) is treated as outside — failing closed. This
// mirrors jobs.withinDir (unexported there).
func withinDir(dir, target string) bool {
	rel, err := filepath.Rel(dir, target)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Close stops the debouncer and the underlying fsnotify watcher. Safe to call
// multiple times.
func (w *Watcher) Close() {
	w.closeOnce.Do(func() {
		w.deb.stop()
		if err := w.fsw.Close(); err != nil {
			w.logf("citadel search watcher: close error: %v", err)
		}
	})
}
