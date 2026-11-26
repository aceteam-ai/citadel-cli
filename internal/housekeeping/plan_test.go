package housekeeping

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T) (time.Time, Input) {
	t.Helper()
	now := time.Date(2030, 1, 2, 12, 0, 0, 0, time.UTC)
	free := uint64(1)
	return now, Input{Version: 1, Targets: []Target{{ID: "cache-a", Kind: "go_build_cache", Observation: &Observation{
		ObservedAt: now, NewestEntryAt: now.Add(-8 * 24 * time.Hour), EstimatedBytes: 4096,
		AvailableBytes: &free, OpenHandles: No, InUse: No,
	}}}}
}

func TestPlanIsAdvisoryAndNeverReportsFreedBytes(t *testing.T) {
	now, input := fixture(t)
	receipt, err := Plan(now, input)
	if err != nil || receipt.Mode != "dry_run" || receipt.DeletionEnabled || receipt.FreedBytes != 0 || receipt.EstimatedCandidateBytes != 4096 || receipt.Targets[0].Decision != "candidate" {
		t.Fatalf("unexpected advisory receipt: %+v, %v", receipt, err)
	}
	if receipt.Targets[0].Reason != "native_revalidation_required" || receipt.Source != "supplied_metadata_only" {
		t.Fatal("input metadata was presented as native cleanup authority")
	}
}

func TestPlanRefusesUnsafeOrUnknownObservations(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		edit         func(*Target)
	}{
		{"missing", "observation_missing", func(x *Target) { x.Observation = nil }},
		{"free-unknown", "free_space_unknown", func(x *Target) { x.Observation.AvailableBytes = nil }},
		{"open", "open_handles", func(x *Target) { x.Observation.OpenHandles = Yes }},
		{"handles-unknown", "open_handles_unknown", func(x *Target) { x.Observation.OpenHandles = Unknown }},
		{"in-use", "in_use", func(x *Target) { x.Observation.InUse = Yes }},
		{"use-unknown", "in_use_unknown", func(x *Target) { x.Observation.InUse = Unknown }},
		{"young", "too_young", func(x *Target) { x.Observation.NewestEntryAt = x.Observation.ObservedAt }},
		{"age-unknown", "age_unknown", func(x *Target) { x.Observation.NewestEntryAt = time.Time{} }},
		{"healthy", "free_space_sufficient", func(x *Target) { n := DefaultKeepFreeBytes; x.Observation.AvailableBytes = &n }},
		{"stale", "observation_stale", func(x *Target) { x.Observation.ObservedAt = x.Observation.ObservedAt.Add(-6 * time.Minute) }},
		{"future", "observation_future", func(x *Target) { x.Observation.ObservedAt = x.Observation.ObservedAt.Add(time.Second) }},
		{"future-entry", "age_unknown", func(x *Target) { x.Observation.NewestEntryAt = x.Observation.ObservedAt.Add(time.Second) }},
		{"model", "protected_target", func(x *Target) { x.Kind = "model_cache" }},
		{"modules", "protected_target", func(x *Target) { x.Kind = "module_store" }},
		{"data", "protected_target", func(x *Target) { x.Kind = "data_directory" }},
		{"images", "protected_target", func(x *Target) { x.Kind = "container_images" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now, input := fixture(t)
			tc.edit(&input.Targets[0])
			got, err := Plan(now, input)
			if err != nil || got.Targets[0].Decision != "skip" || got.Targets[0].Reason != tc.reason || got.EstimatedCandidateBytes != 0 || got.FreedBytes != 0 {
				t.Fatalf("unsafe observation accepted: %+v, %v", got, err)
			}
		})
	}
}

func TestPlanWorktreesRequireBothCleanAndFullyPushed(t *testing.T) {
	for _, clean := range []State{Unknown, No, Yes} {
		for _, pushed := range []State{Unknown, No, Yes} {
			now, input := fixture(t)
			input.Targets[0].Kind = "worktree"
			input.Targets[0].Observation.WorktreeClean = clean
			input.Targets[0].Observation.FullyPushed = pushed
			got, err := Plan(now, input)
			if err != nil || (got.Targets[0].Decision == "candidate") != (clean == Yes && pushed == Yes) {
				t.Fatalf("clean=%s pushed=%s: %+v, %v", clean, pushed, got, err)
			}
		}
	}
}

func TestPlanThresholdsAndDeterministicOrder(t *testing.T) {
	now, input := fixture(t)
	input.Targets[0].ID = "z"
	other := input.Targets[0]
	other.ID = "a"
	other.MinimumAgeHours = 24 * 9
	input.Targets = append(input.Targets, other)
	got, err := Plan(now, input)
	if err != nil || got.Targets[0].ID != "a" || got.Targets[0].Reason != "too_young" || got.Targets[1].ID != "z" || input.Targets[0].ID != "z" {
		t.Fatal("plan mutated inputs or ignored configured age/order")
	}
	input.Targets[0].KeepFreeBytes = 1
	got, err = Plan(now, input)
	if err != nil || got.EstimatedCandidateBytes != 0 {
		t.Fatal("per-target free-space threshold was ignored")
	}
}

func TestPlanRejectsMalformedAndAmbiguousPolicy(t *testing.T) {
	for _, edit := range []func(*Input){
		func(x *Input) { x.Version = 2 },
		func(x *Input) { x.Targets[0].ID = "../private-path" },
		func(x *Input) { x.Targets = append(x.Targets, x.Targets[0]) },
		func(x *Input) { x.Targets[0].Kind = "arbitrary" },
		func(x *Input) { x.Targets[0].Observation.OpenHandles = "maybe" },
		func(x *Input) { x.Targets[0].MinimumAgeHours = -1 },
		func(x *Input) { x.Targets[0].MinimumAgeHours = 24 * 366 },
		func(x *Input) { x.Targets = make([]Target, MaxTargets+1) },
	} {
		now, input := fixture(t)
		edit(&input)
		if _, err := Plan(now, input); err != ErrInput {
			t.Fatal("malformed policy accepted or reflected in error")
		}
	}
}

func TestReadInputIsBoundedStrictAndDoesNotReflectContent(t *testing.T) {
	_, input := fixture(t)
	good, _ := json.Marshal(input)
	if _, err := ReadInput(strings.NewReader(string(good))); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"", "null", string(good) + "{}", `{"version":1,"private-path":"fixture-secret"}`, strings.Repeat(" ", MaxInputBytes+1)} {
		if _, err := ReadInput(strings.NewReader(raw)); err != ErrInput || strings.Contains(err.Error(), "fixture-secret") {
			t.Fatal("unbounded/unknown/trailing input accepted or leaked")
		}
	}
}

func TestPlanBoundaryTimesZeroSpaceAndEstimateOverflow(t *testing.T) {
	now, input := fixture(t)
	zero := uint64(0)
	input.Targets[0].Observation.AvailableBytes = &zero
	input.Targets[0].Observation.ObservedAt = now.Add(-maxObservationAge)
	input.Targets[0].Observation.NewestEntryAt = input.Targets[0].Observation.ObservedAt.Add(-DefaultMinimumAgeHours * time.Hour)
	got, err := Plan(now, input)
	if err != nil || got.EstimatedCandidateBytes != 4096 {
		t.Fatal("exact age/freshness boundaries or known zero space misclassified")
	}
	input.Targets[0].Observation.EstimatedBytes = math.MaxUint64
	other := input.Targets[0]
	other.ID = "cache-b"
	input.Targets = append(input.Targets, other)
	if _, err := Plan(now, input); err != ErrInput {
		t.Fatal("estimate overflow accepted")
	}
	if _, err := Plan(time.Time{}, input); err != ErrInput {
		t.Fatal("unknown evaluation time accepted")
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("fixture-private-reader-error") }

func TestReadInputExactSizeAndReaderError(t *testing.T) {
	_, input := fixture(t)
	data, _ := json.Marshal(input)
	padded := string(data) + strings.Repeat(" ", MaxInputBytes-len(data))
	if _, err := ReadInput(strings.NewReader(padded)); err != nil {
		t.Fatal("exact bounded input refused")
	}
	if _, err := ReadInput(strings.NewReader(padded + " ")); err != ErrInput {
		t.Fatal("oversized document accepted")
	}
	if _, err := ReadInput(failingReader{}); err != ErrInput {
		t.Fatal("reader error reflected or lost")
	}
}
