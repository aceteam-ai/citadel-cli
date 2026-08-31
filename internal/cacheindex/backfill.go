package cacheindex

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/services"
)

// scanResult accumulates ReconcileScan's directory walk before any Store
// mutation happens, so the add/prune decisions below can be pure map
// lookups against a stable snapshot of "what's on disk right now".
type scanResult struct {
	// discovered holds candidate backfill entries, keyed exactly like
	// Store.idx.entries (entryKey(cacheDir, model)).
	discovered map[string]Entry
	// scannedDirs marks a cache_dir name as SUCCESSFULLY read this pass
	// (os.ReadDir succeeded, even if it returned zero entries). A cache_dir
	// NOT in this set was unreadable or absent -- e.g. an operator's
	// HF_HUB_CACHE/HUGGINGFACE_HUB_CACHE/HF_HOME override means the real hub
	// dir isn't at cacheRoot/huggingface/hub at all. The staleness cleanup
	// below must NEVER prune an entry whose cache_dir wasn't scanned: doing
	// so would read "we didn't look" as "it's gone", which is exactly
	// backwards (the #632/§8.5 "absence must not read as a negative signal"
	// rule, applied here to files instead of timestamps).
	scannedDirs map[string]bool
	// foundFiles is cache_dir -> the set of relative file/dir names actually
	// seen on disk this pass (a models--org--repo directory name for hf-hub,
	// a bare filename for gguf-dir). This is what staleness verification
	// checks against -- an entry's OWN Files, never its key -- because a
	// gguf-dir entry keyed by repo id (a real MODEL_CACHE_PULL, see
	// pullLlamaCppGGUF) has no key a directory walk could ever reconstruct;
	// only its Files are verifiable this way.
	foundFiles map[string]map[string]bool
}

func newScanResult() *scanResult {
	return &scanResult{
		discovered:  map[string]Entry{},
		scannedDirs: map[string]bool{},
		foundFiles:  map[string]map[string]bool{},
	}
}

func (r *scanResult) markScanned(cacheDir string) {
	r.scannedDirs[cacheDir] = true
	if r.foundFiles[cacheDir] == nil {
		r.foundFiles[cacheDir] = map[string]bool{}
	}
}

func (r *scanResult) markFound(cacheDir, file string) {
	r.markScanned(cacheDir)
	r.foundFiles[cacheDir][file] = true
}

// ScanOptions carries the scan-time inputs ReconcileScan needs but cannot
// resolve itself, per design doc §9.3. internal/cacheindex is a LEAF package
// (imports only stdlib + services) and must not import internal/jobs, so a
// caller that wants the legacy-HF-duplicate probe (see LegacyHFHubDir below)
// resolves it externally (jobs.LegacyHFHubDirForScan) and passes it in.
type ScanOptions struct {
	// LegacyHFHubDir, when non-empty, is the resolved pre-#682 legacy
	// HuggingFace hub-cache directory to size for the durable "legacy
	// duplicate" scan-metadata record (design doc §9.3's LegacyHFCache).
	// Empty means skip the probe entirely -- Meta.LegacyHF stays nil. The
	// caller is expected to have already applied the "at least one real
	// models--* entry" gate (jobs.LegacyHFHubDirForScan's doc comment
	// explains why): ReconcileScan does not re-derive or re-validate this
	// path, it only sizes whatever is passed.
	LegacyHFHubDir string
}

// ReconcileScan reconciles what is actually on disk under cacheRoot (see
// DefaultCacheRoot) into the index, per design doc §8.5. It is idempotent
// and safe to call repeatedly (every `citadel work` startup, per
// cmd/work.go's wiring, beside ReconcileOrphanedReservations -- same
// single-live-worker precondition: this Store must be the only writer for
// the staleness cleanup below to be correct, since it deletes entries this
// scan does not rediscover):
//
//   - A discovered artifact whose files are not already claimed by some
//     OTHER existing entry (see "double-counting", below) and has no entry
//     of its own gets a new one, tagged Source: SourceBackfill.
//   - A discovered artifact whose existing entry (at its OWN key) is ALSO
//     SourceBackfill gets its size/files refreshed (a size may have changed
//     since the last scan) with LastUsed left untouched.
//   - A discovered artifact whose existing entry is SourcePull is left
//     COMPLETELY untouched -- a real pull record is richer provenance than
//     anything a directory walk can reconstruct, and must never be
//     downgraded.
//   - An existing hf-hub/gguf-dir entry is dropped, logged, ONLY when its
//     cache_dir WAS successfully scanned this pass AND NONE of its own
//     recorded Files were found on disk. Both conditions matter: the first
//     avoids treating an unreadable/overridden directory as "everything in
//     it is gone" (see scanResult.scannedDirs); the second verifies by
//     FILES, never by the entry's key, because a repo-id-keyed gguf pull
//     entry's key can never be reconstructed from a bare directory walk --
//     only its files can. Native entries are never dropped this way: a
//     "not rediscovered" native aggregate row would just mean the store's
//     directory could not be walked this pass, not that the store is gone.
//
// Double-counting guard: a gguf-dir file already claimed by some entry's
// Files (typically a REPO-keyed MODEL_CACHE_PULL entry, e.g.
// "TheBloke/X-GGUF" -> ["a.gguf","b.gguf"]) never gets a SECOND,
// FILENAME-keyed backfill entry created for the same bytes -- scanGGUFDir
// has no way to know a bare "a.gguf" belongs to a repo id already tracked
// elsewhere, so this scan checks against every existing entry's Files
// before creating a new key, not just an exact key match.
//
// Scoped to services.EngineCacheDirs ONLY -- catalog/third-party module
// caches are out of scope, same boundary services.EngineCacheDirs itself
// documents. The scan also does not honor an operator's
// HF_HUB_CACHE/HUGGINGFACE_HUB_CACHE/HF_HOME override the way
// internal/jobs.hfCacheBaseDir() does (a P2a scope limitation, not a design
// doc requirement) -- it always looks under cacheRoot/huggingface/hub. Under
// such an override this degrades safely to "no backfill for that node"
// (scannedDirs never gets set for "huggingface", so nothing is pruned
// either) rather than wiping the index, but it will not discover an
// override-location's models. Widening this to thread the real resolver in
// is a documented follow-up.
func (s *Store) ReconcileScan(cacheRoot string, opts ScanOptions) error {
	res := newScanResult()

	for dir, engines := range enginesByCacheDir(services.CacheFamilyHFHub) {
		scanHFHubDir(cacheRoot, dir, representative(engines), res)
	}
	for dir, engines := range enginesByCacheDir(services.CacheFamilyGGUFDir) {
		scanGGUFDir(cacheRoot, dir, representative(engines), res)
	}
	for dir, engines := range enginesByCacheDir(services.CacheFamilyNative) {
		scanNativeDir(cacheRoot, dir, representative(engines), res)
	}

	// Scan metadata (design doc §9.3): a per-dir measured total for every
	// cache_dir this pass actually found on disk, plus the legacy-duplicate
	// probe. Computed from res/disk BEFORE any Store mutation below -- both
	// are pure reads independent of s.idx.entries.
	dirScans := buildDirScans(cacheRoot, res)
	legacyHF := probeLegacyHFCache(opts.LegacyHFHubDir)

	s.mu.Lock()
	defer s.mu.Unlock()

	s.idx.meta = Meta{ScannedAt: s.now(), Dirs: dirScans, LegacyHF: legacyHF}

	// Every file already claimed by an EXISTING entry, per cache_dir --
	// computed BEFORE any mutation below, so a candidate's own about-to-be-
	// refreshed entry does not (and need not) exclude itself: the add loop
	// only consults this map for a candidate whose KEY is brand new, and a
	// refresh always goes through the key-exists branch instead (which never
	// consults this map at all).
	claimed := map[string]map[string]bool{}
	for _, e := range s.idx.entries {
		if e.Family != services.CacheFamilyHFHub && e.Family != services.CacheFamilyGGUFDir {
			continue
		}
		m := claimed[e.CacheDir]
		if m == nil {
			m = map[string]bool{}
			claimed[e.CacheDir] = m
		}
		for _, f := range e.Files {
			m[f] = true
		}
	}

	for key, disc := range res.discovered {
		existing, ok := s.idx.entries[key]
		if !ok {
			if allClaimed(claimed[disc.CacheDir], disc.Files) {
				// Every file this candidate would introduce already belongs
				// to a DIFFERENT existing entry (the repo-keyed-pull vs
				// filename-keyed-backfill collision) -- skip, don't
				// double-count the same bytes under a second key.
				continue
			}
			s.idx.entries[key] = disc
			continue
		}
		if existing.Source == SourceBackfill {
			disc.LastUsed = existing.LastUsed
			disc.Pinned = existing.Pinned
			s.idx.entries[key] = disc
		}
		// else existing.Source == SourcePull (or any other/future
		// provenance): richer entry wins, leave it untouched.
	}

	// Staleness cleanup: verify by FILES against a SCANNED directory, never
	// by key membership -- see the doc comment above for why key-based
	// pruning silently destroyed every repo-keyed gguf pull entry, and why
	// an unscanned (overridden/unreadable) directory must never be read as
	// "empty".
	for key, e := range s.idx.entries {
		if e.Family != services.CacheFamilyHFHub && e.Family != services.CacheFamilyGGUFDir {
			continue
		}
		if !res.scannedDirs[e.CacheDir] {
			continue
		}
		if anyFileFound(res.foundFiles[e.CacheDir], e.Files) {
			continue
		}
		delete(s.idx.entries, key)
		s.logf("[cacheindex] dropped stale entry %s/%s (no longer found on disk)", e.CacheDir, e.Model)
	}

	return s.flushLocked()
}

// allClaimed reports whether every entry in files is present in claimed. A
// nil/empty files list is never "all claimed" (there is nothing to claim,
// so this must not short-circuit true and suppress a legitimate new entry).
func allClaimed(claimed map[string]bool, files []string) bool {
	if len(files) == 0 {
		return false
	}
	for _, f := range files {
		if !claimed[f] {
			return false
		}
	}
	return true
}

// anyFileFound reports whether at least one of files is present in found.
func anyFileFound(found map[string]bool, files []string) bool {
	for _, f := range files {
		if found[f] {
			return true
		}
	}
	return false
}

// enginesByCacheDir groups services.EngineCacheDirs by (Dir, Family),
// returning cache-dir-name -> sorted engine names sharing it. Sorted so
// picking a single "representative" engine for a shared directory (the
// hf-hub bucket especially) is deterministic across runs, not dependent on
// Go's randomized map iteration order.
func enginesByCacheDir(family services.CacheFamily) map[string][]string {
	out := map[string][]string{}
	for engine, ec := range services.EngineCacheDirs {
		if ec.Family != family {
			continue
		}
		out[ec.Dir] = append(out[ec.Dir], engine)
	}
	for dir := range out {
		sort.Strings(out[dir])
	}
	return out
}

func representative(engines []string) string {
	if len(engines) == 0 {
		return ""
	}
	return engines[0]
}

// scanHFHubDir discovers models--org--repo directories under
// cacheRoot/cacheDirName/hub and records one backfill Entry candidate per
// directory. Marks cacheDirName scanned only when the directory was
// actually readable.
func scanHFHubDir(cacheRoot, cacheDirName, engine string, res *scanResult) {
	hubDir := filepath.Join(cacheRoot, cacheDirName, "hub")
	entries, err := os.ReadDir(hubDir)
	if err != nil {
		return
	}
	res.markScanned(cacheDirName)
	for _, de := range entries {
		if !de.IsDir() || !strings.HasPrefix(de.Name(), "models--") {
			continue
		}
		res.markFound(cacheDirName, de.Name())
		modelID, ok := hfDirToModelID(de.Name())
		if !ok {
			continue
		}
		full := filepath.Join(hubDir, de.Name())
		key := entryKey(cacheDirName, modelID)
		res.discovered[key] = Entry{
			CacheDir:  cacheDirName,
			Family:    services.CacheFamilyHFHub,
			Model:     modelID,
			Engine:    engine,
			Files:     []string{de.Name()},
			SizeBytes: dirSize(full),
			PulledAt:  dirModTime(full),
			Source:    SourceBackfill,
		}
	}
}

// hfDirToModelID reverses the HF hub-cache "models--org--repo" directory
// naming back into "org/repo". ok=false when name does not carry the
// "models--" prefix at all (should not happen given the caller's own
// HasPrefix filter, but kept as an explicit contract rather than an assumed
// invariant).
func hfDirToModelID(name string) (string, bool) {
	trimmed := strings.TrimPrefix(name, "models--")
	if trimmed == name {
		return "", false
	}
	parts := strings.SplitN(trimmed, "--", 2)
	if len(parts) != 2 {
		return trimmed, true
	}
	return parts[0] + "/" + parts[1], true
}

// scanGGUFDir discovers GGUF (or any) files under cacheRoot/cacheDirName,
// RECURSIVELY -- a llamacpp pull of a repo whose GGUF lives in a
// subdirectory (pullLlamaCppGGUF/recordLlamaCppCacheIndexEntry) records that
// file's Files entry keyed by its repo-relative path (e.g. "sub/x.gguf",
// straight from the HF repo tree's own Path, forward-slash separated -- see
// recordLlamaCppCacheIndexEntry and listFilesRelative, both of which walk
// the SAME way and join relative to llamaCppCacheDir()'s root). A top-level-
// only scan never rediscovers that path, so ReconcileScan's staleness
// cleanup below would drop the repo-keyed pull entry on the very next
// restart even though the bytes are still on disk (citadel #937).
//
// Records one Entry candidate per file, keyed by its path RELATIVE TO dir
// (filepath.Rel + ToSlash) -- for a top-level file this is unchanged from
// before (bare filename == its own relative path), so existing filename
// keys are stable. The repo id itself is still not recoverable from a bare
// file on disk (design doc §8.5); ReconcileScan's caller-side "claimed"
// check is what stops this from double-counting a file a repo-keyed
// MODEL_CACHE_PULL entry already tracks, matched by relative path just like
// a top-level match. Marks cacheDirName scanned only when the root
// directory was actually readable; an error partway through the walk (e.g.
// a permission-denied subdirectory) skips just that entry rather than
// aborting the whole scan.
//
// A dot-prefixed path component (e.g. ".cache/huggingface/download/x.gguf.metadata",
// the bookkeeping tree `hf download --local-dir` writes INSIDE its target
// directory as of huggingface_hub's local_dir tracking; older versions used
// ".huggingface/") never becomes a discovered candidate -- recursing without
// this exclusion would mint a SourceBackfill Entry per metadata file on
// every pull, permanently growing the index with entries no P3/P5 consumer
// should ever see. markFound is still called for it, though: it can only
// ever SUPPRESS a staleness-cleanup deletion (the safe direction), and a
// real repo-keyed pull entry's Files CAN legitimately include such a path --
// recordLlamaCppCacheIndexEntry's before/after-diff fallback
// (hfRepoTreeFn failure) walks the directory with listFilesRelative, which
// does not exclude dotfiles, so a first-ever pull hitting that fallback
// records them as part of the pull's own provenance.
func scanGGUFDir(cacheRoot, cacheDirName, engine string, res *scanResult) {
	dir := filepath.Join(cacheRoot, cacheDirName)
	if _, err := os.ReadDir(dir); err != nil {
		return
	}
	res.markScanned(cacheDirName)
	_ = filepath.WalkDir(dir, func(path string, de fs.DirEntry, err error) error {
		if err != nil || de.IsDir() {
			return nil
		}
		info, infoErr := de.Info()
		if infoErr != nil {
			return nil
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		res.markFound(cacheDirName, rel)
		if hasHiddenPathComponent(rel) {
			return nil
		}
		key := entryKey(cacheDirName, rel)
		res.discovered[key] = Entry{
			CacheDir:  cacheDirName,
			Family:    services.CacheFamilyGGUFDir,
			Model:     rel,
			Engine:    engine,
			Files:     []string{rel},
			SizeBytes: info.Size(),
			PulledAt:  info.ModTime(),
			Source:    SourceBackfill,
		}
		return nil
	})
}

// hasHiddenPathComponent reports whether any slash-separated component of a
// relative path is dot-prefixed (".cache", ".huggingface", ".git", ...).
// Used to keep scanGGUFDir's recursive walk from minting backfill entries
// for a downloader's own bookkeeping files -- see scanGGUFDir's doc comment.
func hasHiddenPathComponent(rel string) bool {
	for _, part := range strings.Split(rel, "/") {
		if strings.HasPrefix(part, ".") {
			return true
		}
	}
	return false
}

// scanNativeDir records ONE aggregate row (nativeAggregateModel) for a
// native-family cache directory's total size, per design doc §8.1's "other
// native (lmstudio, tei)" case. Native entries are never subject to the
// scannedDirs/foundFiles staleness machinery (ReconcileScan's staleness
// cleanup only iterates hf-hub/gguf-dir families), so this does not call
// res.markScanned/markFound -- only res.discovered.
//
// Deliberately SKIPS creating a discovered ENTRY for ollama (no aggregate row),
// even though ollama is also CacheFamilyNative and design doc §8.1/§8.5
// describe per-model backfill for it: ollama's own pull/evict call sites
// (pullOllama, ensureOllamaModel, evictOllama -- internal/jobs) already write
// real per-model entries with an accurate size via `ollama list`, so a real
// gap only exists for models pulled OUTSIDE citadel's tracking before the
// index existed -- narrower, lower-value than duplicating an `ollama list`
// subprocess parser into this leaf package (which would also make
// ReconcileScan's tests depend on an `ollama` binary being present). Writing
// a synthetic "_store" aggregate row for ollama here as well would
// double-count against its real per-model entries the moment any P3
// reporting consumer sums a directory's entries. Documented as a deliberate
// P2a scope decision, not an oversight.
//
// res.markScanned IS still called for ollama's cache_dir (unlike P2a's
// original behavior, which skipped this function entirely for ollama before
// ANY disk check) -- P3 needs to know the directory was found so its measured
// total (buildDirScans) can report a real byte count even though this
// function records no per-model entry for it. Safe: the staleness cleanup
// this res feeds only iterates hf-hub/gguf-dir families (never native), so
// marking a native dir scanned has no effect on entry pruning.
func scanNativeDir(cacheRoot, cacheDirName, engine string, res *scanResult) {
	dir := filepath.Join(cacheRoot, cacheDirName)
	if _, err := os.Stat(dir); err != nil {
		return
	}
	res.markScanned(cacheDirName)
	if engine == "ollama" {
		return
	}
	key := entryKey(cacheDirName, nativeAggregateModel)
	res.discovered[key] = Entry{
		CacheDir:  cacheDirName,
		Family:    services.CacheFamilyNative,
		Model:     nativeAggregateModel,
		Engine:    engine,
		SizeBytes: dirSize(dir),
		PulledAt:  dirModTime(dir),
		Source:    SourceBackfill,
	}
}

// buildDirScans computes design doc §9.3's per-dir measured totals: one
// DirScan per services.EngineCacheDirs cache_dir that THIS pass successfully
// found on disk (res.scannedDirs -- set by scanHFHubDir/scanGGUFDir/
// scanNativeDir only on a successful directory read, per family). A dir NOT
// in res.scannedDirs is omitted entirely (unreadable, or an operator
// HF_HUB_CACHE-style override pointing the real layout elsewhere) --
// omission means "not measured", never "zero bytes", matching Meta.Dirs' doc
// comment. MeasuredBytes is the WHOLE cache_dir's size (dirSize), not just
// the entries this scan indexed, so it also catches any non-artifact
// remainder living alongside them.
func buildDirScans(cacheRoot string, res *scanResult) []DirScan {
	familyByDir := map[string]services.CacheFamily{}
	for _, ec := range services.EngineCacheDirs {
		familyByDir[ec.Dir] = ec.Family
	}
	dirs := make([]string, 0, len(familyByDir))
	for d := range familyByDir {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)

	var out []DirScan
	for _, d := range dirs {
		if !res.scannedDirs[d] {
			continue
		}
		out = append(out, DirScan{
			Dir:           d,
			Family:        string(familyByDir[d]),
			MeasuredBytes: dirSize(filepath.Join(cacheRoot, d)),
		})
	}
	return out
}

// probeLegacyHFCache sizes the caller-resolved legacy HF hub-cache dir (see
// ScanOptions.LegacyHFHubDir) for the durable "legacy duplicate" scan-metadata
// record (design doc §9.3). Returns nil when legacyHubDir is empty (the
// caller found nothing to report, or chose not to probe) or when it sizes to
// zero (nothing there is worth reporting as reclaimable).
func probeLegacyHFCache(legacyHubDir string) *LegacyHFCache {
	if legacyHubDir == "" {
		return nil
	}
	size := dirSize(legacyHubDir)
	if size <= 0 {
		return nil
	}
	return &LegacyHFCache{Path: filepath.Dir(legacyHubDir), SizeBytes: size}
}

func dirSize(dir string) int64 {
	var total int64
	filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		total += info.Size()
		return nil
	})
	return total
}

func dirModTime(dir string) time.Time {
	if fi, err := os.Stat(dir); err == nil {
		return fi.ModTime()
	}
	return time.Time{}
}
