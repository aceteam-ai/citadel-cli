// Package cacheindex implements the durable model-cache index (citadel #682
// P2a, docs/design-cache-ownership.md §8). It gives a node a persisted answer
// to "which files on disk belong to which pulled model" -- the record §1.3 of
// that design doc established does not exist anywhere pre-P2a.
//
// LEAF PACKAGE: imports only stdlib plus services (for EngineCacheDirs /
// CacheFamily). Callers resolve the index file's path themselves --
// <network.GetNodeConfigDir()>/cache-index.json, the SAME machine-convergent
// directory swap-lru.json (internal/worker/swap_persist.go, #688) and
// aepSigningStoreDir (internal/worker/llm_inference.go) already use, and for
// the identical reason: the writer is the pull/evict handler inside a
// (frequently systemd-root) `citadel work`, while readers include an
// interactive non-root `citadel status` -- platform.ConfigDir() resolves
// those to different directories and a reader would see nothing forever (the
// #726/#845/CLAUDE.md ConfigDir() bug class). This package never imports
// internal/network to stay a leaf.
//
// ONE Store per process, not one per call site. cacheindex.Open constructs an
// in-memory Store from whatever is on disk; each Upsert/Remove/MarkUsed
// mutates that in-memory copy and flushes a FULL snapshot back to disk (never
// a merge). Two independent Store instances pointed at the same file inside
// the SAME process would each flush their own (possibly stale) view and
// silently clobber the other's most recent write -- there is no reconciliation
// between two live Stores. internal/jobs.CacheIndexStore() is the exported
// accessor that keeps this to exactly one live Store per `citadel work`
// process; see its doc comment before adding a second construction site.
//
// Reads are lenient at every level (missing file, corrupt JSON, or a single
// malformed entry all degrade to "absent" rather than failing the caller) --
// mirroring TokenHashEntry.UnmarshalJSON (citadel #815) and
// swap_persist.go's loadLastUsedFile (citadel #688) reasoning. Writes are
// atomic (temp file + rename in the same directory) and best-effort: a
// caller that fails to persist logs it and proceeds -- see each write
// method's doc comment for the exact direction-of-error this protects
// (a missing entry is a redundant, idempotent re-pull at worst; a stale
// entry is guarded by verify-before-act at the consumer, not by write
// reliability here).
package cacheindex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/aceteam-ai/citadel-cli/services"
)

// FileName is the index file's name under whatever directory the caller
// resolves (network.GetNodeConfigDir(), per the package doc above).
const FileName = "cache-index.json"

// FormatVersion is the on-disk schema version (the top-level "version" key).
// Bump and add a migration path here if the shape below ever changes
// incompatibly; nothing reads it yet (P2a has exactly one version), but its
// presence from day one avoids an ambiguous "no version key" case for a
// hypothetical future migration.
const FormatVersion = 1

// Cache-index entry provenance markers (Entry.Source).
const (
	// SourcePull means a MODEL_CACHE_PULL (or the SERVICE_START native-ollama
	// pull, #543) actually wrote this entry -- the richer, authoritative
	// provenance.
	SourcePull = "pull"
	// SourceBackfill means Store.ReconcileScan discovered this artifact
	// already on disk, with no pull recorded on this node (pre-existing
	// cache, a self-provisioning engine's own container download, or a node
	// that predates the index). Never overwrites a SourcePull entry -- see
	// ReconcileScan's doc comment. Exempt from GC in a future phase (P5);
	// P2a only sets the marker, no GC logic reads it yet.
	SourceBackfill = "backfill"
)

// nativeAggregateModel is the synthetic Model key used for a native-family
// engine with no Go-side pull path (lmstudio, tei) -- one aggregate
// size-only row per store, per design doc §8.1. Ollama gets real per-model
// entries via its own pull/evict call sites instead (see cache_index.go in
// internal/jobs); this constant is not used for ollama entries.
const nativeAggregateModel = "_store"

// Entry is one cached artifact: a model/repo pulled into a cache_dir, or
// (for CacheFamilyNative) an aggregate row for a store with no per-model
// tracking. The primary key is (CacheDir, Model) -- deliberately NOT
// (Engine, Model): several engines share the "huggingface" cache_dir
// (services.EngineCacheDirs), so keying by engine would either duplicate a
// shared repo per engine or force a fake "owning engine" onto a directory
// the layout says is shared. Engine is recorded as provenance (who pulled
// it), not identity.
type Entry struct {
	// CacheDir is the cache subdirectory name (services.EngineCacheDirs'
	// Dir values -- "huggingface", "llamacpp", "bonsai", "ollama", ...),
	// relative to the resolved ~/citadel-cache root (DefaultCacheRoot).
	CacheDir string `json:"cache_dir"`
	// Family names the on-disk layout at CacheDir (services.CacheFamily).
	Family services.CacheFamily `json:"family"`
	// Model is the model/repo id (e.g. "meta-llama/Llama-3.1-8B-Instruct"),
	// a bare GGUF filename for a backfilled gguf-dir entry with no known
	// repo, or nativeAggregateModel for an aggregate native-store row.
	Model string `json:"model"`
	// Engine is the engine that pulled this (provenance only -- see the
	// type doc comment on why it is not part of the key).
	Engine string `json:"engine"`
	// Files holds family-specific provenance (see the package doc / design
	// doc §8.1 for the exact per-family semantics):
	//   - hf-hub:   exactly one entry, the "models--org--repo" directory
	//               name, relative to the resolved hub dir.
	//   - gguf-dir: the repo-relative file path(s) this pull's own files
	//               landed at, relative to CacheDir.
	//   - native:   empty. The native store is its own provenance.
	Files []string `json:"files,omitempty"`
	// SizeBytes is the total on-disk size attributed to this entry.
	SizeBytes int64 `json:"size_bytes"`
	// PulledAt is when this entry was created: the pull's own completion
	// time for a SourcePull entry, or the on-disk file's mtime for a
	// SourceBackfill entry (a real signal for a downloaded file, unlike
	// atime -- see the package doc's "Reads are lenient" note and design
	// doc §2.2 for why atime itself was rejected). Zero means unknown.
	PulledAt time.Time `json:"-"`
	// LastUsed is the most precise "used" signal recorded for this entry:
	// set to PulledAt at pull time, then advanced by MarkUsed's
	// resident-implies-used heartbeat reconciler (design doc §8.1). Zero
	// means unknown -- NOT "never used"; see LeastRecentlyUsed's doc
	// comment for why an unknown must not sort as automatically coldest.
	LastUsed time.Time `json:"-"`
	// Pinned is RESERVED for P5 (GC); P2a never sets it. Present in the
	// schema now so a future GC-writing binary does not need a schema
	// migration to add it.
	Pinned bool `json:"pinned,omitempty"`
	// Source names how this entry was created: SourcePull or
	// SourceBackfill.
	Source string `json:"source,omitempty"`
}

// entryJSON is Entry's on-the-wire shape: timestamps as RFC3339 strings
// (omitted when zero) rather than encoding/json's default time.Time
// marshaling, so a malformed timestamp degrades that ONE field to "unknown"
// during UnmarshalJSON instead of failing to parse the entire entry (and, if
// entries were decoded as part of one slice, the entire file -- see Load's
// per-entry decoding for why entries are unmarshaled individually).
type entryJSON struct {
	CacheDir  string   `json:"cache_dir"`
	Family    string   `json:"family"`
	Model     string   `json:"model"`
	Engine    string   `json:"engine"`
	Files     []string `json:"files,omitempty"`
	SizeBytes int64    `json:"size_bytes"`
	PulledAt  string   `json:"pulled_at,omitempty"`
	LastUsed  string   `json:"last_used,omitempty"`
	Pinned    bool     `json:"pinned,omitempty"`
	Source    string   `json:"source,omitempty"`
}

// MarshalJSON implements json.Marshaler, formatting PulledAt/LastUsed as
// RFC3339 (UTC) when set, omitting them (via entryJSON's omitempty) when
// zero.
func (e Entry) MarshalJSON() ([]byte, error) {
	ej := entryJSON{
		CacheDir:  e.CacheDir,
		Family:    string(e.Family),
		Model:     e.Model,
		Engine:    e.Engine,
		Files:     e.Files,
		SizeBytes: e.SizeBytes,
		Pinned:    e.Pinned,
		Source:    e.Source,
	}
	if !e.PulledAt.IsZero() {
		ej.PulledAt = e.PulledAt.UTC().Format(time.RFC3339)
	}
	if !e.LastUsed.IsZero() {
		ej.LastUsed = e.LastUsed.UTC().Format(time.RFC3339)
	}
	return json.Marshal(ej)
}

// UnmarshalJSON implements json.Unmarshaler. A malformed timestamp string
// degrades that one field to the zero value (never an error) -- the
// TokenHashEntry (citadel #815) lenient-parse pattern applied per-field.
func (e *Entry) UnmarshalJSON(data []byte) error {
	var ej entryJSON
	if err := json.Unmarshal(data, &ej); err != nil {
		return err
	}
	e.CacheDir = ej.CacheDir
	e.Family = services.CacheFamily(ej.Family)
	e.Model = ej.Model
	e.Engine = ej.Engine
	e.Files = ej.Files
	e.SizeBytes = ej.SizeBytes
	e.Pinned = ej.Pinned
	e.Source = ej.Source
	e.PulledAt = time.Time{}
	if ej.PulledAt != "" {
		if t, err := time.Parse(time.RFC3339, ej.PulledAt); err == nil {
			e.PulledAt = t
		}
	}
	e.LastUsed = time.Time{}
	if ej.LastUsed != "" {
		if t, err := time.Parse(time.RFC3339, ej.LastUsed); err == nil {
			e.LastUsed = t
		}
	}
	return nil
}

// fileFormat is the top-level on-disk shape.
type fileFormat struct {
	Version int     `json:"version"`
	Entries []Entry `json:"entries"`
}

// entryKey builds the (cache_dir, model) primary key. NUL-joined so a
// cache_dir/model pair can never collide with a different split of the same
// concatenated string (neither field is expected to contain a NUL byte).
func entryKey(cacheDir, model string) string {
	return cacheDir + "\x00" + model
}

// Index is a read-only, in-memory snapshot of the cache index. Returned by
// Load and Store.Snapshot; safe for concurrent reads (it is never mutated
// after construction).
type Index struct {
	entries map[string]Entry
}

func newIndex() *Index {
	return &Index{entries: map[string]Entry{}}
}

func (ix *Index) snapshot() *Index {
	out := newIndex()
	for k, v := range ix.entries {
		out.entries[k] = v
	}
	return out
}

// Load reads and parses the index file at path. It is LENIENT at every
// level, per the package doc: a missing file, a corrupt/unparsable file, or
// any single malformed entry all degrade toward "absent" rather than
// failing -- an empty *Index is always returned, never nil. A non-nil error
// is returned for a caller that wants to log it, but the returned Index is
// always usable regardless of err (mirrors loadLastUsedFile's identical
// contract, internal/worker/swap_persist.go).
func Load(path string) (*Index, error) {
	idx := newIndex()
	if path == "" {
		return idx, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return idx, nil
		}
		return idx, err
	}

	// Decode entries as raw JSON messages first, then unmarshal each
	// individually into an Entry -- this is what lets ONE malformed entry
	// be skipped without discarding every other (valid) entry in the same
	// array. A single json.Unmarshal into []Entry would fail the whole
	// slice on the first bad element.
	var raw struct {
		Version int               `json:"version"`
		Entries []json.RawMessage `json:"entries"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return idx, err
	}
	for _, rm := range raw.Entries {
		var e Entry
		if err := json.Unmarshal(rm, &e); err != nil {
			continue // malformed entry: skip it, keep the rest
		}
		if e.CacheDir == "" || e.Model == "" {
			continue // missing key fields: not identifiable, skip
		}
		idx.entries[entryKey(e.CacheDir, e.Model)] = e
	}
	return idx, nil
}

// writeIndexFile atomically persists idx's entries to path: write to a temp
// file in the same directory, then rename over the target. Mirrors
// writeLastUsedFile's identical shape (internal/worker/swap_persist.go).
// Entries are written in a stable (cache_dir, model) sorted order so the
// file diffs cleanly between writes.
func writeIndexFile(path string, idx *Index) error {
	if path == "" {
		return nil
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	entries := make([]Entry, 0, len(idx.entries))
	for _, e := range idx.entries {
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].CacheDir != entries[j].CacheDir {
			return entries[i].CacheDir < entries[j].CacheDir
		}
		return entries[i].Model < entries[j].Model
	})

	data, err := json.MarshalIndent(fileFormat{Version: FormatVersion, Entries: entries}, "", "  ")
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, ".cache-index-*.tmp")
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

// --- Read API (on *Index) -- designed so P3/P5/P6/P7 are thin consumers ---

// Lookup returns the entry for the exact (cacheDir, model) key.
func (ix *Index) Lookup(cacheDir, model string) (Entry, bool) {
	e, ok := ix.entries[entryKey(cacheDir, model)]
	return e, ok
}

// LookupForEngine resolves engine -> cache dir via services.EngineCacheDirs,
// then Lookup. False when the engine is unknown to that table (a catalog
// module, not a services.ServiceMap engine) or has no entry for model.
func (ix *Index) LookupForEngine(engine, model string) (Entry, bool) {
	ec, ok := services.EngineCacheDirs[engine]
	if !ok {
		return Entry{}, false
	}
	return ix.Lookup(ec.Dir, model)
}

// FilesFor returns a copy of the entry's recorded files (exact-provenance
// eviction, P6), or nil if no entry exists.
func (ix *Index) FilesFor(cacheDir, model string) []string {
	e, ok := ix.Lookup(cacheDir, model)
	if !ok || len(e.Files) == 0 {
		return nil
	}
	out := make([]string, len(e.Files))
	copy(out, e.Files)
	return out
}

// EntriesByDir groups every entry by CacheDir (reporting, P3), each group
// sorted by Model for a stable, diffable rendering.
func (ix *Index) EntriesByDir() map[string][]Entry {
	out := map[string][]Entry{}
	for _, e := range ix.entries {
		out[e.CacheDir] = append(out[e.CacheDir], e)
	}
	for k := range out {
		group := out[k]
		sort.Slice(group, func(i, j int) bool { return group[i].Model < group[j].Model })
		out[k] = group
	}
	return out
}

// All returns every entry, in no particular order. A thin convenience for a
// caller that wants the full set without grouping (e.g. a size total).
func (ix *Index) All() []Entry {
	out := make([]Entry, 0, len(ix.entries))
	for _, e := range ix.entries {
		out = append(out, e)
	}
	return out
}

// effectiveRecency is the sort key LeastRecentlyUsed uses: LastUsed when
// known, else PulledAt. See LeastRecentlyUsed's doc comment for why an
// entry with NO LastUsed must fall back to its PulledAt rather than sort as
// automatically coldest.
func effectiveRecency(e Entry) time.Time {
	if !e.LastUsed.IsZero() {
		return e.LastUsed
	}
	return e.PulledAt
}

// LeastRecentlyUsed returns every entry ordered oldest-effective-recency
// first (GC ordering, P5). An entry with a real LastUsed sorts on that; an
// entry with none (a SourceBackfill entry the resident-implies-used
// reconciler has not yet touched) sorts on its PulledAt (file mtime) instead
// -- deliberately NOT treated as automatically the coldest (the design
// doc §8.5 rule, the inverse of citadel #632's "no record must not read as
// recently loaded"): a zero-time LastUsed must not make every pre-index
// model look like the best eviction candidate just because it predates the
// index. An entry with BOTH timestamps unknown (should not occur for a
// well-formed entry; a defensive case) sorts last -- unknown-unknown is the
// one case this function cannot order meaningfully, so it is treated as the
// LEAST evictable rather than guessed at.
func (ix *Index) LeastRecentlyUsed() []Entry {
	all := ix.All()
	sort.SliceStable(all, func(i, j int) bool {
		ti, tj := effectiveRecency(all[i]), effectiveRecency(all[j])
		iZero, jZero := ti.IsZero(), tj.IsZero()
		if iZero && jZero {
			return false
		}
		if iZero != jZero {
			return jZero // the entry with a real timestamp sorts first
		}
		return ti.Before(tj)
	})
	return all
}

// Verify re-stats e's recorded file(s) under cacheRoot (the resolved
// ~/citadel-cache directory -- see DefaultCacheRoot) and reports whether
// they still exist on disk. false = stale: at least one recorded path is
// missing (an operator `rm`, a container's own cache management). A caller
// about to delete or report on an entry must call this first rather than
// trust a possibly-stale record -- see the package doc's direction-of-error
// note (a false negative here is a missed reclaim or a redundant re-pull; a
// false positive would be a wrong delete, which this exists to prevent).
//
// CacheFamilyNative entries have no files to verify (the native store is
// its own provenance) and always report true.
func (ix *Index) Verify(e Entry, cacheRoot string) (Entry, bool) {
	switch e.Family {
	case services.CacheFamilyNative:
		return e, true
	case services.CacheFamilyHFHub:
		return e, verifyFilesUnder(filepath.Join(cacheRoot, e.CacheDir, "hub"), e.Files)
	case services.CacheFamilyGGUFDir:
		return e, verifyFilesUnder(filepath.Join(cacheRoot, e.CacheDir), e.Files)
	default:
		return e, true
	}
}

func verifyFilesUnder(base string, files []string) bool {
	if len(files) == 0 {
		return false
	}
	for _, f := range files {
		if _, err := os.Stat(filepath.Join(base, filepath.FromSlash(f))); err != nil {
			return false
		}
	}
	return true
}

// DefaultCacheRoot resolves ~/citadel-cache, the parent directory of every
// services.EngineCacheDirs subdirectory (the SAME root
// internal/jobs.canonicalHFCacheDir/bonsaiCacheDir/llamaCppCacheDir each
// append their own subdirectory name to, and internal/resmon.hostDiskPath
// reports disk headroom on). Degrades to a relative "citadel-cache" on a
// UserHomeDir failure rather than erroring, mirroring those functions'
// identical fallback, so ReconcileScan always has a concrete path to walk.
func DefaultCacheRoot() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "citadel-cache"
	}
	return filepath.Join(home, "citadel-cache")
}

// --- Write API (on *Store) ---

// markUsedFlushMinGap debounces MarkUsed's disk flush: MarkUsed fires on
// every ~30s heartbeat tick per resident engine/model (potentially several
// times a tick on a multi-model node), so this bounds writes to roughly once
// per gap regardless of tick rate -- mirrors swapPersistMinGap's identical
// reasoning (internal/worker/swap_persist.go).
const markUsedFlushMinGap = 5 * time.Second

// Store is the write handle: an in-memory Index plus the machinery to flush
// it back to disk. See the package doc for why exactly one Store should be
// live per process for a given path.
type Store struct {
	mu   sync.Mutex
	path string
	idx  *Index
	logf func(format string, args ...any)
	now  func() time.Time

	flushMu       sync.Mutex
	lastFlushedAt time.Time
}

// Open constructs a Store, seeding its in-memory Index from path (leniently
// -- see Load). logf receives write/load failures for visibility; it may be
// nil (no-op). Open never fails: a missing or corrupt file degrades to an
// empty index, matching WithPersistence's identical contract
// (internal/worker/swap_persist.go).
func Open(path string, logf func(format string, args ...any)) *Store {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	idx, err := Load(path)
	if err != nil {
		logf("[cacheindex] load %s failed (degrading to empty index): %v", path, err)
	}
	return &Store{
		path: path,
		idx:  idx,
		logf: logf,
		now:  time.Now,
	}
}

// Snapshot returns a read-only copy of the Store's current in-memory Index,
// safe to read concurrently with further Store mutations.
func (s *Store) Snapshot() *Index {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.idx.snapshot()
}

// flushLocked writes the current in-memory index to disk. Callers must hold
// s.mu. Used by the immediate-flush write paths (Upsert/Remove/RemoveFile);
// MarkUsed's debounced path snapshots and flushes OUTSIDE the lock instead
// (flushSnapshot below), matching swap_persist.go's flushPersist shape.
func (s *Store) flushLocked() error {
	err := writeIndexFile(s.path, s.idx)
	if err != nil {
		s.logf("[cacheindex] failed to persist %s: %v", s.path, err)
	}
	return err
}

// Upsert records or replaces the entry for (e.CacheDir, e.Model), flushing
// immediately (pull/evict mutations are rare and each one matters, per the
// design doc §8.2). PulledAt/LastUsed default to now when zero; Source
// defaults to SourcePull when empty (the common case: every direct call
// site is a pull). Returns an error on a failed flush -- callers should log
// and otherwise ignore it (the pull/evict itself must still succeed; see
// the package doc's direction-of-error note).
func (s *Store) Upsert(e Entry) error {
	if e.CacheDir == "" || e.Model == "" {
		return errEmptyKey
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if e.PulledAt.IsZero() {
		e.PulledAt = now
	}
	if e.LastUsed.IsZero() {
		e.LastUsed = e.PulledAt
	}
	if e.Source == "" {
		e.Source = SourcePull
	}
	s.idx.entries[entryKey(e.CacheDir, e.Model)] = e
	return s.flushLocked()
}

// Remove deletes the entry for (cacheDir, model), flushing immediately. A
// no-op (nil error) if no such entry exists.
func (s *Store) Remove(cacheDir, model string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := entryKey(cacheDir, model)
	if _, ok := s.idx.entries[key]; !ok {
		return nil
	}
	delete(s.idx.entries, key)
	return s.flushLocked()
}

// RemoveFile removes a single file from the entry's Files list (gguf-dir
// eviction of one file out of a multi-file repo entry), flushing
// immediately. When the removal empties Files, the entry itself is dropped
// -- an entry the caller can no longer attribute any file to is not worth
// keeping. A no-op (nil error) if no entry exists or file was not recorded
// on it.
func (s *Store) RemoveFile(cacheDir, model, file string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := entryKey(cacheDir, model)
	e, ok := s.idx.entries[key]
	if !ok {
		return nil
	}
	kept := make([]string, 0, len(e.Files))
	found := false
	for _, f := range e.Files {
		if f == file {
			found = true
			continue
		}
		kept = append(kept, f)
	}
	if !found {
		return nil
	}
	if len(kept) == 0 {
		delete(s.idx.entries, key)
	} else {
		e.Files = kept
		s.idx.entries[key] = e
	}
	return s.flushLocked()
}

// MarkUsed advances the entry's LastUsed to at, debounced (markUsedFlushMinGap)
// so the resident-implies-used heartbeat reconciler (design doc §8.1, run on
// every ~30s OnStatus tick) does not write on every single tick. A no-op if
// no entry exists for (cacheDir, model) -- MarkUsed only refreshes recency
// for something already known to the index; it never fabricates an entry
// (that is Upsert's/ReconcileScan's job, which know the size/files/family a
// real entry needs).
func (s *Store) MarkUsed(cacheDir, model string, at time.Time) {
	s.mu.Lock()
	key := entryKey(cacheDir, model)
	e, ok := s.idx.entries[key]
	if !ok {
		s.mu.Unlock()
		return
	}
	e.LastUsed = at
	s.idx.entries[key] = e
	s.mu.Unlock()
	s.flushIfDue()
}

// flushIfDue debounces MarkUsed's disk write, mirroring
// swap_persist.go's persistIfDue shape exactly (a separate flushMu so the
// debounce check/update never blocks a MarkUsed caller on I/O, and the
// snapshot-then-write happens outside s.mu).
func (s *Store) flushIfDue() {
	now := s.now()
	s.flushMu.Lock()
	due := now.Sub(s.lastFlushedAt) >= markUsedFlushMinGap
	if due {
		s.lastFlushedAt = now
	}
	s.flushMu.Unlock()
	if !due {
		return
	}
	s.mu.Lock()
	snapshot := s.idx.snapshot()
	s.mu.Unlock()
	if err := writeIndexFile(s.path, snapshot); err != nil {
		s.logf("[cacheindex] failed to persist %s: %v", s.path, err)
	}
}

// errEmptyKey is Upsert's validation error for a caller that forgot to set
// CacheDir/Model.
var errEmptyKey = emptyKeyError{}

type emptyKeyError struct{}

func (emptyKeyError) Error() string {
	return "cacheindex: Upsert requires a non-empty CacheDir and Model"
}
