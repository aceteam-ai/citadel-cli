package cacheindex

import (
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/services"
)

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return ts
}

func TestPlanGC_ResidentEntryNeverEvicted(t *testing.T) {
	now := mustTime(t, "2026-08-30T12:00:00Z")
	// The resident entry is the OLDEST (best LRU candidate) and over any
	// disk-pressure threshold would be picked first if residency did not
	// exempt it -- this is the core safety guarantee under test.
	entries := []Entry{
		{CacheDir: "huggingface", Family: services.CacheFamilyHFHub, Model: "org/serving-model",
			LastUsed: mustTime(t, "2026-01-01T00:00:00Z"), SizeBytes: 100},
		{CacheDir: "huggingface", Family: services.CacheFamilyHFHub, Model: "org/idle-model",
			LastUsed: mustTime(t, "2026-08-01T00:00:00Z"), SizeBytes: 50},
	}
	in := GCInputs{
		Now:          now,
		PinnedModels: map[string]bool{},
		ExemptModels: map[DirModel]bool{
			{CacheDir: "huggingface", Model: "org/serving-model"}: true,
		},
	}
	plan := PlanGC(entries, in)
	if plan.ExemptResident != 1 {
		t.Fatalf("ExemptResident = %d, want 1", plan.ExemptResident)
	}
	for _, c := range plan.Candidates {
		if c.Model == "org/serving-model" {
			t.Fatalf("resident entry %q must never appear as a GC candidate", c.Model)
		}
	}
	if len(plan.Candidates) != 1 || plan.Candidates[0].Model != "org/idle-model" {
		t.Fatalf("Candidates = %+v, want exactly [org/idle-model]", plan.Candidates)
	}
}

func TestPlanGC_ResidentWholeDirExemption_GGUFDirAndUnknownServedList(t *testing.T) {
	now := mustTime(t, "2026-08-30T12:00:00Z")
	old := mustTime(t, "2026-01-01T00:00:00Z")
	entries := []Entry{
		// gguf-dir: whole-dir exemption even though this specific model is
		// not individually listed in ExemptModels.
		{CacheDir: "llamacpp", Family: services.CacheFamilyGGUFDir, Model: "some-repo/x.gguf", LastUsed: old, SizeBytes: 10},
		// hf-hub: engine running but served list unknown/empty -- whole dir
		// must be exempt (the "we couldn't ask" fail-safe), not just a
		// per-model miss.
		{CacheDir: "huggingface", Family: services.CacheFamilyHFHub, Model: "org/unknown-servedlist", LastUsed: old, SizeBytes: 10},
	}
	in := GCInputs{
		Now:          now,
		PinnedModels: map[string]bool{},
		ExemptDirs:   map[string]bool{"llamacpp": true, "huggingface": true},
		ExemptModels: map[DirModel]bool{},
	}
	plan := PlanGC(entries, in)
	if plan.ExemptResident != 2 {
		t.Fatalf("ExemptResident = %d, want 2", plan.ExemptResident)
	}
	if len(plan.Candidates) != 0 {
		t.Fatalf("Candidates = %+v, want none", plan.Candidates)
	}
}

func TestPlanGC_HFHubResidencyIsCaseInsensitive(t *testing.T) {
	now := mustTime(t, "2026-08-30T12:00:00Z")
	entries := []Entry{
		{CacheDir: "huggingface", Family: services.CacheFamilyHFHub, Model: "Org/Model-Name", LastUsed: mustTime(t, "2026-01-01T00:00:00Z"), SizeBytes: 10},
	}
	in := GCInputs{
		Now: now,
		ExemptModels: map[DirModel]bool{
			// Caller (buildGCResidencyExemptions) lower-cases hf-hub keys.
			{CacheDir: "huggingface", Model: "org/model-name"}: true,
		},
	}
	plan := PlanGC(entries, in)
	if plan.ExemptResident != 1 || len(plan.Candidates) != 0 {
		t.Fatalf("expected the differently-cased entry to be exempt via case-insensitive hf-hub match; plan=%+v", plan)
	}
}

func TestPlanGC_PinnedNeverEvicted(t *testing.T) {
	now := mustTime(t, "2026-08-30T12:00:00Z")
	old := mustTime(t, "2026-01-01T00:00:00Z")
	entries := []Entry{
		{CacheDir: "huggingface", Family: services.CacheFamilyHFHub, Model: "org/pinned-model", LastUsed: old, SizeBytes: 100},
		{CacheDir: "huggingface", Family: services.CacheFamilyHFHub, Model: "org/other-model", LastUsed: old, SizeBytes: 100},
	}
	in := GCInputs{
		Now:          now,
		PinnedModels: map[string]bool{"org/pinned-model": true},
	}
	plan := PlanGC(entries, in)
	if plan.ExemptPinned != 1 {
		t.Fatalf("ExemptPinned = %d, want 1", plan.ExemptPinned)
	}
	for _, c := range plan.Candidates {
		if c.Model == "org/pinned-model" {
			t.Fatalf("pinned entry must never appear as a GC candidate")
		}
	}
}

func TestPlanGC_PinnedMatchIsExactCaseSensitive(t *testing.T) {
	now := mustTime(t, "2026-08-30T12:00:00Z")
	old := mustTime(t, "2026-01-01T00:00:00Z")
	entries := []Entry{
		{CacheDir: "huggingface", Family: services.CacheFamilyHFHub, Model: "Org/Model", LastUsed: old, SizeBytes: 10},
	}
	in := GCInputs{
		Now:          now,
		PinnedModels: map[string]bool{"org/model": true}, // different case -- must NOT match
	}
	plan := PlanGC(entries, in)
	if plan.ExemptPinned != 0 || len(plan.Candidates) != 1 {
		t.Fatalf("pinned match must be exact/case-sensitive; plan=%+v", plan)
	}
}

func TestPlanGC_BackfillEntriesAreEligible(t *testing.T) {
	now := mustTime(t, "2026-08-30T12:00:00Z")
	old := mustTime(t, "2026-01-01T00:00:00Z")
	entries := []Entry{
		{CacheDir: "huggingface", Family: services.CacheFamilyHFHub, Model: "org/backfilled",
			PulledAt: old, Source: SourceBackfill, SizeBytes: 40_000_000_000},
	}
	plan := PlanGC(entries, GCInputs{Now: now, PinnedModels: map[string]bool{}})
	if len(plan.Candidates) != 1 || plan.Candidates[0].Model != "org/backfilled" {
		t.Fatalf("a SourceBackfill entry must be GC-eligible (Jason's 2026-08-25 decision); plan=%+v", plan)
	}
}

func TestPlanGC_MinAgeExemption(t *testing.T) {
	now := mustTime(t, "2026-08-30T12:00:00Z")
	entries := []Entry{
		{CacheDir: "huggingface", Family: services.CacheFamilyHFHub, Model: "org/just-pulled",
			LastUsed: now.Add(-1 * time.Hour), SizeBytes: 10},
		{CacheDir: "huggingface", Family: services.CacheFamilyHFHub, Model: "org/old-enough",
			LastUsed: now.Add(-48 * time.Hour), SizeBytes: 10},
	}
	plan := PlanGC(entries, GCInputs{Now: now, MinAge: 24 * time.Hour, PinnedModels: map[string]bool{}})
	if plan.ExemptYoung != 1 {
		t.Fatalf("ExemptYoung = %d, want 1", plan.ExemptYoung)
	}
	if len(plan.Candidates) != 1 || plan.Candidates[0].Model != "org/old-enough" {
		t.Fatalf("Candidates = %+v, want exactly [org/old-enough]", plan.Candidates)
	}
}

func TestPlanGC_MinAgeExemption_UnknownRecencyTreatedAsTooYoung(t *testing.T) {
	now := mustTime(t, "2026-08-30T12:00:00Z")
	entries := []Entry{
		// Neither LastUsed nor PulledAt set -- a defensive/should-not-occur
		// case. Must NOT be guessed as "old enough".
		{CacheDir: "huggingface", Family: services.CacheFamilyHFHub, Model: "org/no-timestamps", SizeBytes: 10},
	}
	plan := PlanGC(entries, GCInputs{Now: now, MinAge: 24 * time.Hour, PinnedModels: map[string]bool{}})
	if plan.ExemptYoung != 1 || len(plan.Candidates) != 0 {
		t.Fatalf("an entry with no recency signal must be exempt under min-age, not guessed; plan=%+v", plan)
	}
}

func TestPlanGC_OrderingLRUFirstThenSizeDescendingTiebreak(t *testing.T) {
	now := mustTime(t, "2026-08-30T12:00:00Z")
	entries := []Entry{
		{CacheDir: "huggingface", Family: services.CacheFamilyHFHub, Model: "org/newest",
			LastUsed: mustTime(t, "2026-08-29T00:00:00Z"), SizeBytes: 5},
		{CacheDir: "huggingface", Family: services.CacheFamilyHFHub, Model: "org/oldest",
			LastUsed: mustTime(t, "2026-01-01T00:00:00Z"), SizeBytes: 5},
		{CacheDir: "huggingface", Family: services.CacheFamilyHFHub, Model: "org/middle",
			LastUsed: mustTime(t, "2026-06-01T00:00:00Z"), SizeBytes: 5},
		// Tie with "oldest" on recency: SourceBackfill entry with a matching
		// PulledAt-derived effective recency and a LARGER size -- must sort
		// BEFORE "oldest" (size-descending tie-break).
		{CacheDir: "huggingface", Family: services.CacheFamilyHFHub, Model: "org/oldest-but-bigger",
			PulledAt: mustTime(t, "2026-01-01T00:00:00Z"), SizeBytes: 50, Source: SourceBackfill},
	}
	plan := PlanGC(entries, GCInputs{Now: now, PinnedModels: map[string]bool{}})
	got := make([]string, len(plan.Candidates))
	for i, c := range plan.Candidates {
		got[i] = c.Model
	}
	want := []string{"org/oldest-but-bigger", "org/oldest", "org/middle", "org/newest"}
	if len(got) != len(want) {
		t.Fatalf("Candidates order = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Candidates order = %v, want %v", got, want)
		}
	}
}

func TestPlanGC_UnknownRecencyEntrySortsLastNotFirst(t *testing.T) {
	// Mirrors LeastRecentlyUsed's own "unknown is least evictable" rule
	// (design doc §8.5, the inverse of citadel #632): a zero-recency entry
	// must not look like the coldest, best eviction candidate just because
	// it predates the index.
	now := mustTime(t, "2026-08-30T12:00:00Z")
	entries := []Entry{
		{CacheDir: "huggingface", Family: services.CacheFamilyHFHub, Model: "org/has-recency",
			LastUsed: mustTime(t, "2026-01-01T00:00:00Z"), SizeBytes: 10},
		{CacheDir: "huggingface", Family: services.CacheFamilyHFHub, Model: "org/unknown-recency", SizeBytes: 10},
	}
	// MinAge 0 disables the min-age exemption so both entries are candidates
	// and only ORDERING is under test here.
	plan := PlanGC(entries, GCInputs{Now: now, PinnedModels: map[string]bool{}})
	if len(plan.Candidates) != 2 {
		t.Fatalf("Candidates = %+v, want 2", plan.Candidates)
	}
	if plan.Candidates[0].Model != "org/has-recency" {
		t.Fatalf("Candidates[0] = %q, want org/has-recency (a real timestamp sorts before unknown)", plan.Candidates[0].Model)
	}
}

func TestPlanGC_NativeStoreAggregateRowNeverACandidate(t *testing.T) {
	now := mustTime(t, "2026-08-30T12:00:00Z")
	old := mustTime(t, "2026-01-01T00:00:00Z")
	entries := []Entry{
		{CacheDir: "lmstudio", Family: services.CacheFamilyNative, Model: nativeAggregateModel, PulledAt: old, SizeBytes: 1000},
	}
	plan := PlanGC(entries, GCInputs{Now: now, PinnedModels: map[string]bool{}})
	if len(plan.Candidates) != 0 {
		t.Fatalf("a native '_store' aggregate row must never be a GC candidate; plan=%+v", plan)
	}
	if plan.ExemptPinned != 0 || plan.ExemptResident != 0 || plan.ExemptYoung != 0 {
		t.Fatalf("the '_store' row is structurally ineligible, not 'found and exempted'; plan=%+v", plan)
	}
}

func TestPlanGC_EmptyEntriesEmptyPlan(t *testing.T) {
	plan := PlanGC(nil, GCInputs{Now: mustTime(t, "2026-08-30T12:00:00Z"), PinnedModels: map[string]bool{}})
	if len(plan.Candidates) != 0 {
		t.Fatalf("Candidates = %+v, want none", plan.Candidates)
	}
}
