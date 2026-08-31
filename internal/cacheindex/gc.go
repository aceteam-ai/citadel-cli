// internal/cacheindex/gc.go
//
// PlanGC is P5's pure planner (citadel #682 P5, docs/design-cache-ownership.md
// §10.4: "Structure mirrors #577's planner/executor split"
// (status.PlanPreemption + ServiceHandler.preemptForVRAM) -- a pure,
// unit-testable planner and a side-effectful executor). This file is the
// planner half: it decides WHICH entries are eligible for eviction and in
// WHAT ORDER, touching no filesystem/network/docker. The executor
// (internal/jobs' RunCacheGC) resolves every live signal (residency via
// status.DiscoverLocalEngines, pinning via citadel.yaml, disk pressure via
// gopsutil) into the plain data GCInputs carries, calls PlanGC, then walks
// the resulting Candidates one at a time, re-verifying and re-measuring
// between each eviction (design doc §10.1's "evict-one-then-remeasure, never
// plan-the-full-set-upfront" rule -- PlanGC does not decide HOW MANY entries
// to evict, only which ones are eligible and in what order).
package cacheindex

import (
	"sort"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/services"
)

// DirModel is the (cache_dir, model) pair GCInputs' residency-exemption maps
// key on. Exported as a plain struct (rather than reusing entryKey's opaque
// NUL-joined string encoding) so a caller building GCInputs from live engine
// data (internal/jobs) does not need to know the index's internal key
// encoding.
type DirModel struct {
	CacheDir string
	Model    string
}

// GCInputs carries P5 GC's plan-time inputs (design doc §10.4): everything
// PlanGC needs to decide which entries are eligible for eviction and in what
// order, all as plain data. Residency and pinning are LIVE, impure signals
// (status.DiscoverLocalEngines, citadel.yaml's pinned_models) that the
// CALLER resolves before calling PlanGC -- this keeps every exemption/
// ordering case here unit-testable without a live mesh, docker, or manifest.
type GCInputs struct {
	// Now is "the current time" for min-age and recency comparisons --
	// injected (not time.Now()) so tests are deterministic.
	Now time.Time
	// MinAge exempts anything younger than this (design doc §10.3.3, default
	// 24h): protects a just-pulled model whose SERVICE_START has not landed
	// yet (pull and deploy are separate jobs; GC between them would evict the
	// deploy's own weights). MinAge<=0 disables this exemption entirely.
	MinAge time.Duration
	// PinnedModels is the pinned_models manifest allowlist (design doc
	// §10.3.2), matched EXACTLY against Entry.Model (trimmed, case-sensitive
	// -- HuggingFace repo ids are case-sensitive, and this mirrors
	// serviceManifest.pinnedSet's identical exact-match convention for
	// pinned_services).
	PinnedModels map[string]bool
	// ExemptDirs are cache_dir names exempt IN FULL this cycle: a
	// single-engine gguf-dir (llamacpp, bonsai) whose owning engine is
	// running (design doc §10.3.1's whole-dir rule -- per-entry matching is
	// untrustworthy in the dangerous direction for a repo-keyed gguf pull
	// entry, so the whole dir is exempt instead), or an hf-hub/native dir
	// whose engine is running but its served-model list is empty/unknown (a
	// probe failure must never read as "not serving").
	ExemptDirs map[string]bool
	// ExemptModels are exact (cache_dir, model) pairs known to be actively
	// served right now -- hf-hub (matched against a running engine's served
	// list; the CALLER is responsible for any case-folding, see IsResident)
	// and native/ollama (ollama deletions always go through `ollama rm`,
	// never a raw file delete, so per-model exemption is safe for that
	// family alone).
	ExemptModels map[DirModel]bool
}

// GCPlan is PlanGC's pure output.
type GCPlan struct {
	// Candidates are eligible entries, ordered oldest-effective-recency
	// first (LeastRecentlyUsed's ordering rule) with size-DESCENDING as the
	// explicit tie-break (design doc §10.2: "idle-first collapses" once
	// residency is a categorical exemption -- every candidate here is
	// already non-resident, so this IS the whole ordering). The caller
	// evicts from the front, one at a time, remeasuring disk pressure after
	// each; PlanGC itself never decides how many to evict.
	Candidates []Entry
	// ExemptPinned / ExemptResident / ExemptYoung count entries excluded by
	// each rule -- observability for the heartbeat's gc.last_skip_reason
	// (design doc §10.4): a node stuck at high-water with everything
	// pinned/resident/young must be visible as such, not silent.
	ExemptPinned   int
	ExemptResident int
	ExemptYoung    int
}

// nativeAggregateModel is declared in cacheindex.go; reused here unchanged.

// PlanGC decides which cache-index entries are eligible for eviction and in
// what order (citadel #682 P5, design doc §10). Pure.
//
// Verify-before-delete (design doc §10.3.5, "the plan snapshot is advisory,
// the pre-delete check is the guarantee") is deliberately NOT this
// function's job: the executor re-verifies each candidate (Index.Verify,
// and a fresh residency check) immediately before deleting it, since a
// candidate can go stale between planning and execution.
//
// SourceBackfill entries are NOT exempt here (Jason's 2026-08-25 decision,
// design doc §11 "Backfill evictability" -- see SourceBackfill's doc
// comment in cacheindex.go): this function applies no source-based
// exemption at all. A "_store" native aggregate row (lmstudio/tei, no
// per-model tracking -- see scanNativeDir's doc comment) is never a
// candidate; it is silently skipped before the exemption counters below,
// since it was never a real "found and exempted" case, just structurally
// ineligible.
func PlanGC(entries []Entry, in GCInputs) GCPlan {
	plan := GCPlan{}
	var eligible []Entry
	for _, e := range entries {
		if e.Family == services.CacheFamilyNative && e.Model == nativeAggregateModel {
			continue
		}
		if isPinned(e, in.PinnedModels) {
			plan.ExemptPinned++
			continue
		}
		if IsResident(e, in.ExemptDirs, in.ExemptModels) {
			plan.ExemptResident++
			continue
		}
		if isTooYoung(e, in.Now, in.MinAge) {
			plan.ExemptYoung++
			continue
		}
		eligible = append(eligible, e)
	}
	sort.SliceStable(eligible, func(i, j int) bool {
		ti, tj := effectiveRecency(eligible[i]), effectiveRecency(eligible[j])
		iZero, jZero := ti.IsZero(), tj.IsZero()
		if iZero != jZero {
			return jZero // the entry with a real timestamp sorts first
		}
		if !iZero && !ti.Equal(tj) {
			return ti.Before(tj)
		}
		// Tie (equal timestamps, or both unknown): size-descending, the
		// explicit design doc §10.2 tie-break.
		return eligible[i].SizeBytes > eligible[j].SizeBytes
	})
	plan.Candidates = eligible
	return plan
}

func isPinned(e Entry, pinned map[string]bool) bool {
	return pinned[e.Model]
}

// IsResident reports whether entry e is currently protected by the design
// doc §10.3.1 residency exemption, given exemptDirs/exemptModels the caller
// has already resolved from a live signal (status.DiscoverLocalEngines plus,
// for the executor, an independent container-running check -- see
// internal/jobs' buildGCResidencyExemptions doc comment for why the two are
// not the same call). Exported so the executor can re-check a single
// candidate's residency immediately before deleting it (design doc §10.3:
// "re-checked immediately before each individual deletion") without
// re-running the whole planner.
//
// hf-hub matching is case-insensitive (design doc §10.3.1: "exact
// case-insensitive model-id match") -- the CALLER is responsible for
// lower-casing the Model half of any hf-hub key it puts in exemptModels;
// this function lower-cases the entry's own Model before lookup only when
// e.Family is hf-hub, so a caller that already agrees on that convention
// (buildGCResidencyExemptions does) gets a correct match. Every other
// family matches exactly, unfolded.
func IsResident(e Entry, exemptDirs map[string]bool, exemptModels map[DirModel]bool) bool {
	if exemptDirs[e.CacheDir] {
		return true
	}
	model := e.Model
	if e.Family == services.CacheFamilyHFHub {
		model = strings.ToLower(model)
	}
	return exemptModels[DirModel{CacheDir: e.CacheDir, Model: model}]
}

// isTooYoung reports whether e is exempt under the min-age rule (design doc
// §10.3.3). An entry with NO recency signal at all (both LastUsed and
// PulledAt zero -- should not occur for a well-formed entry, a defensive
// case) is treated as too-young-to-know rather than guessed at, mirroring
// LeastRecentlyUsed's "unknown is least evictable" rule: an unconfirmed age
// must never be read as "old enough".
func isTooYoung(e Entry, now time.Time, minAge time.Duration) bool {
	if minAge <= 0 {
		return false
	}
	rec := effectiveRecency(e)
	if rec.IsZero() {
		return true
	}
	return now.Sub(rec) < minAge
}
