package rag

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

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

// Watcher watches the authorized roots and triggers an incremental re-index of
// the node-local semantic index when files under them change. It is best-effort:
// construction and operation degrade to a logged warning rather than failing the
// caller (a watcher problem must never take down `citadel work`).
type Watcher struct {
	svc   *Service
	roots []string
	fsw   *fsnotify.Watcher
	deb   *debouncer
	logf  func(string, ...any)
	// dbPaths is the set of index.db files (plus their -wal/-shm/-journal
	// siblings) the re-index itself writes. Events on these are ignored so a root
	// that happens to CONTAIN the index (e.g. an operator authorizing ~ or
	// ~/citadel-node) cannot create a self-sustaining index -> DB-write -> event
	// -> index loop that never idles.
	dbPaths map[string]struct{}

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
	}
	w.deb = newDebouncer(defaultDebounceDelay, w.reindexAll)
	return w, nil
}

// SetLogf overrides the log function (tests inject a capturing logger).
func (w *Watcher) SetLogf(f func(string, ...any)) { w.logf = f }

// SetDebounceDelay overrides the debounce window (tests use a short delay).
func (w *Watcher) SetDebounceDelay(d time.Duration) {
	w.deb.stop()
	w.deb = newDebouncer(d, w.reindexAll)
}

// Start registers watches for every root (recursively), kicks off an initial
// index-on-startup, and runs the event loop until ctx is cancelled or Close is
// called. It returns immediately; the event loop runs in a goroutine.
func (w *Watcher) Start(ctx context.Context) {
	// Index-on-startup: bring the index current with whatever is on disk now,
	// before we begin reacting to changes. Best-effort (TEI may be down).
	go w.reindexAll()

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
			// Any create/write/remove/rename under a root warrants a re-index.
			// Chmod-only events are ignored (no content change).
			if ev.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Remove|fsnotify.Rename) != 0 {
				w.deb.trigger()
			}
		case err, ok := <-w.fsw.Errors:
			if !ok {
				return
			}
			w.logf("citadel search watcher error: %v", err)
		}
	}
}

// reindexAll re-indexes every authorized root. It is serialized so overlapping
// triggers cannot run concurrent Index calls, and NEVER passes a file_pattern
// (prune compares against files visited, so a narrowed run would delete siblings
// from the index). Errors are logged, not fatal.
func (w *Watcher) reindexAll() {
	w.reindexMu.Lock()
	defer w.reindexMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	for _, root := range w.roots {
		if _, err := w.svc.Index(ctx, root, ""); err != nil {
			w.logf("citadel search watcher: reindex %s failed: %v", root, err)
		}
	}
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
