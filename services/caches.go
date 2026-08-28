// services/caches.go
//
// Canonical per-engine cache-path table (citadel #906, #682 P1). Mirrors
// ports.go's shape: before this file, the engine->cache-directory mapping
// existed only as 12 independently-authored compose mounts plus a handful of
// unrelated Go download paths (internal/jobs/model_cache_pull.go), with
// nothing tying them together. That let `pullHuggingFace` be dispatched
// identically for vllm AND llamacpp: it writes the HuggingFace HUB-cache blob
// layout into ~/citadel-cache/huggingface, which is correct for vllm but
// wrong for llamacpp -- services/compose/llamacpp.yml mounts
// ~/citadel-cache/llamacpp:/models expecting flat, raw GGUF files, a
// directory and a layout `pullHuggingFace` never touches.
//
// This table is the fix: ONE map from engine name to its canonical cache
// subdirectory (under ~/citadel-cache) and on-disk layout family, asserted
// against the embedded compose files' actual volume mounts by
// TestEngineCacheDirsMatchComposeMounts (caches_test.go) -- string-matched,
// not hand-copied, so the table cannot silently drift the way the
// pre-existing 12-file-plus-Go-code arrangement already had.
//
// Scope (citadel #682's design doc, docs/design-cache-ownership.md, §5 P1):
// this is the path table + routing only. It does not build a durable cache
// index (P2, "which files on disk belong to which pull") or GC (P3/P5) --
// see the design doc for those phases.
package services

// CacheFamily identifies the on-disk layout at an engine's cache directory.
type CacheFamily string

const (
	// CacheFamilyHFHub is the HuggingFace hub-cache blob layout
	// (models--<org>--<name>/snapshots/...), consumed by every engine whose
	// compose mounts the shared ~/citadel-cache/huggingface directory at
	// /root/.cache/huggingface (or, for tei, at HUGGINGFACE_HUB_CACHE inside
	// its own mount -- see the "tei" entry below).
	CacheFamilyHFHub CacheFamily = "hf-hub"
	// CacheFamilyGGUFDir is a directory of raw, flat GGUF files -- NOT the HF
	// hub-cache blob layout. Both llamacpp (bring-your-own-GGUF, arbitrary
	// filenames) and bonsai (one fixed filename) use this family; they get
	// separate directories (see LlamaCppCacheDirName / BonsaiCacheDirName)
	// because their model sets do not overlap and mixing them would make an
	// eviction or disk-usage report unable to tell which engine a given file
	// belongs to.
	CacheFamilyGGUFDir CacheFamily = "gguf-dir"
	// CacheFamilyNative is an engine-owned store whose internal layout this
	// table does not model (ollama's content-addressed blob store,
	// lmstudio's own cache format). The directory is still worth recording
	// (disk-usage reporting, future GC), just not one MODEL_CACHE_PULL writes
	// into via the HF-hub or raw-GGUF code paths.
	CacheFamilyNative CacheFamily = "native"
)

// Canonical cache-directory names (relative to ~/citadel-cache). Exported as
// named constants -- not just inline map values below -- so a caller that
// needs to compute the actual on-disk path (internal/jobs, for
// MODEL_CACHE_PULL/MODEL_CACHE_EVICT) references the SAME literal this
// table's compose-mount test asserts against, rather than re-hardcoding
// "huggingface"/"llamacpp"/"bonsai" a second time in a different package.
const (
	HFHubCacheDirName    = "huggingface"
	LlamaCppCacheDirName = "llamacpp"
	BonsaiCacheDirName   = "bonsai"
)

// EngineCache names the canonical cache subdirectory and on-disk layout
// family for one engine.
type EngineCache struct {
	// Dir is the subdirectory of ~/citadel-cache this engine's weights (or,
	// for CacheFamilyNative, its native store) live in.
	Dir string
	// Family is the on-disk layout at Dir.
	Family CacheFamily
}

// EngineCacheDirs is the single source of truth for where each embedded
// services.ServiceMap engine's weights live on disk. Scoped to ServiceMap
// entries only (see TestEngineCacheDirsCoverServiceMap): catalog modules
// (claudecode, hermes, meeting, gotenberg, nvr, ...) author their own compose
// files outside this repo and are not covered here, mirroring how
// selfProvisioningEngines (internal/jobs/model_cache_pull.go) and
// idleCapableEngines (internal/status/engines.go) are also ServiceMap-scoped.
var EngineCacheDirs = map[string]EngineCache{
	// HF-hub-layout engines: every one of these mounts the SAME shared
	// ~/citadel-cache/huggingface directory (services/compose/*.yml), and
	// their weights are pulled/served via the standard HuggingFace hub cache
	// (models--org--name/...). kokoro additionally mounts its own
	// ~/citadel-cache/kokoro for voices/runtime data -- not modeled here,
	// since that data is not weights this table's compose-mount test needs
	// to track for MODEL_CACHE_PULL/EVICT purposes.
	"vllm":          {Dir: HFHubCacheDirName, Family: CacheFamilyHFHub},
	"sglang":        {Dir: HFHubCacheDirName, Family: CacheFamilyHFHub},
	"diffusers":     {Dir: HFHubCacheDirName, Family: CacheFamilyHFHub},
	"extraction":    {Dir: HFHubCacheDirName, Family: CacheFamilyHFHub},
	"transcribe":    {Dir: HFHubCacheDirName, Family: CacheFamilyHFHub},
	"unlimited-ocr": {Dir: HFHubCacheDirName, Family: CacheFamilyHFHub},
	"kokoro":        {Dir: HFHubCacheDirName, Family: CacheFamilyHFHub},

	// GGUF engines: raw files, NOT the HF hub-cache layout. Each gets its own
	// directory (see CacheFamilyGGUFDir's doc comment for why they are not
	// shared).
	"llamacpp": {Dir: LlamaCppCacheDirName, Family: CacheFamilyGGUFDir},
	"bonsai":   {Dir: BonsaiCacheDirName, Family: CacheFamilyGGUFDir},

	// Native/engine-owned stores -- location only, not a MODEL_CACHE_PULL
	// target this table's HF-hub or GGUF code paths route through.
	"ollama":   {Dir: "ollama", Family: CacheFamilyNative},
	"lmstudio": {Dir: "lmstudio", Family: CacheFamilyNative},
	// tei mounts ~/citadel-cache/tei at /data and points
	// HUGGINGFACE_HUB_CACHE=/data inside that mount (services/compose/tei.yml)
	// -- its internal layout is hub-shaped, but the DIRECTORY is tei's own,
	// separate from the shared ~/citadel-cache/huggingface bucket, so it is
	// tracked as native here rather than CacheFamilyHFHub (which would imply
	// the shared directory).
	"tei": {Dir: "tei", Family: CacheFamilyNative},
}
