package status

import (
	"reflect"
	"testing"
)

const vgib = uint64(1) << 30

func TestPlanPreemption_NoRequirementIsNoOp(t *testing.T) {
	// requiredVRAM==0 means "unknown/undeclared": never preempt on an absent
	// signal, regardless of what is running.
	plan := PlanPreemption([]PreemptCandidate{
		{Name: "vllm", VRAMBytes: 21 * vgib, Idle: true},
	}, 0, 0)
	if !plan.Fits {
		t.Fatalf("requiredVRAM==0 must Fit, got %+v", plan)
	}
	if len(plan.Stop) != 0 {
		t.Fatalf("requiredVRAM==0 must not preempt, got Stop=%v", plan.Stop)
	}
}

func TestPlanPreemption_AlreadyFitsNoPreemption(t *testing.T) {
	plan := PlanPreemption([]PreemptCandidate{
		{Name: "vllm", VRAMBytes: 21 * vgib, Idle: false},
	}, 8*vgib, 10*vgib) // 10 free >= 8 required
	if !plan.Fits || len(plan.Stop) != 0 {
		t.Fatalf("should fit without preemption, got %+v", plan)
	}
}

func TestPlanPreemption_StopsIdleFirst(t *testing.T) {
	// Need 8GB, 0 free. A busy 20GB and an idle 10GB both non-pinned. Idle-first
	// means the idle 10GB is chosen even though the busy one is larger.
	plan := PlanPreemption([]PreemptCandidate{
		{Name: "busy-big", VRAMBytes: 20 * vgib, Idle: false},
		{Name: "idle-mid", VRAMBytes: 10 * vgib, Idle: true},
	}, 8*vgib, 0)
	if !plan.Fits {
		t.Fatalf("should fit after one eviction, got %+v", plan)
	}
	if !reflect.DeepEqual(plan.Stop, []string{"idle-mid"}) {
		t.Fatalf("expected to stop idle-mid only, got %v", plan.Stop)
	}
}

func TestPlanPreemption_LargestFirstWithinSameIdleness(t *testing.T) {
	// Two idle services; need 12GB, 0 free. Largest-first frees enough with the
	// single 15GB stop rather than accumulating smaller ones.
	plan := PlanPreemption([]PreemptCandidate{
		{Name: "idle-small", VRAMBytes: 5 * vgib, Idle: true},
		{Name: "idle-large", VRAMBytes: 15 * vgib, Idle: true},
	}, 12*vgib, 0)
	if !reflect.DeepEqual(plan.Stop, []string{"idle-large"}) {
		t.Fatalf("expected largest-first single stop idle-large, got %v", plan.Stop)
	}
}

func TestPlanPreemption_BusyPreemptedWhenIdleInsufficient(t *testing.T) {
	// Idle-first is ordering, NOT a gate: when idle candidates can't free enough,
	// a busy non-pinned service is still stopped.
	plan := PlanPreemption([]PreemptCandidate{
		{Name: "idle-small", VRAMBytes: 2 * vgib, Idle: true},
		{Name: "busy-big", VRAMBytes: 20 * vgib, Idle: false},
	}, 10*vgib, 0)
	if !plan.Fits {
		t.Fatalf("should fit after stopping busy service, got %+v", plan)
	}
	// idle-small (2G) is stopped first but insufficient; then busy-big.
	if !reflect.DeepEqual(plan.Stop, []string{"idle-small", "busy-big"}) {
		t.Fatalf("expected [idle-small busy-big], got %v", plan.Stop)
	}
}

func TestPlanPreemption_NeverPreemptsPinned_FailsWithBlocked(t *testing.T) {
	// Need 20GB, 0 free. Only VRAM holder is pinned bonsai -> cannot fit.
	plan := PlanPreemption([]PreemptCandidate{
		{Name: "bonsai", VRAMBytes: 12 * vgib, Idle: true, Pinned: true},
		{Name: "tiny", VRAMBytes: 1 * vgib, Idle: true},
	}, 20*vgib, 0)
	if plan.Fits {
		t.Fatalf("must NOT fit without evicting pinned service, got %+v", plan)
	}
	if !reflect.DeepEqual(plan.Blocked, []string{"bonsai"}) {
		t.Fatalf("expected Blocked=[bonsai], got %v", plan.Blocked)
	}
	// A pinned service must never appear in the Stop list.
	for _, s := range plan.Stop {
		if s == "bonsai" {
			t.Fatalf("pinned service bonsai must never be in Stop, got %v", plan.Stop)
		}
	}
	if plan.Reason == "" {
		t.Fatalf("rejection must carry a reason")
	}
}

func TestPlanPreemption_FitsAfterEvictingNonPinnedAroundPinned(t *testing.T) {
	// Pinned bonsai holds 8GB and must stay; free 4GB; need 16GB. Stopping the
	// non-pinned vllm (20GB) frees enough (4 free + 20 = 24 >= 16) without
	// touching bonsai.
	plan := PlanPreemption([]PreemptCandidate{
		{Name: "bonsai", VRAMBytes: 8 * vgib, Idle: false, Pinned: true},
		{Name: "vllm", VRAMBytes: 20 * vgib, Idle: true},
	}, 16*vgib, 4*vgib)
	if !plan.Fits {
		t.Fatalf("should fit by evicting non-pinned vllm, got %+v", plan)
	}
	if !reflect.DeepEqual(plan.Stop, []string{"vllm"}) {
		t.Fatalf("expected to stop vllm only, got %v", plan.Stop)
	}
}

func TestPlanPreemption_MinimalPrefixOnly(t *testing.T) {
	// Need 6GB, 0 free; three idle services of 4GB each. Stopping two (8GB) is
	// enough; the third must NOT be stopped.
	plan := PlanPreemption([]PreemptCandidate{
		{Name: "a", VRAMBytes: 4 * vgib, Idle: true},
		{Name: "b", VRAMBytes: 4 * vgib, Idle: true},
		{Name: "c", VRAMBytes: 4 * vgib, Idle: true},
	}, 6*vgib, 0)
	if len(plan.Stop) != 2 {
		t.Fatalf("expected minimal prefix of 2 stops, got %v", plan.Stop)
	}
}

func TestPlanPreemption_SkipsZeroVRAMCandidates(t *testing.T) {
	// A running service holding no VRAM frees nothing when stopped, so it must
	// not be selected; the real VRAM holder is.
	plan := PlanPreemption([]PreemptCandidate{
		{Name: "sidecar", VRAMBytes: 0, Idle: true},
		{Name: "vllm", VRAMBytes: 20 * vgib, Idle: false},
	}, 10*vgib, 0)
	if !reflect.DeepEqual(plan.Stop, []string{"vllm"}) {
		t.Fatalf("expected to stop vllm only (skip zero-VRAM sidecar), got %v", plan.Stop)
	}
}

func TestPlanPreemption_DeterministicNameTieBreak(t *testing.T) {
	// Equal idleness and equal VRAM -> name-ascending order, so the plan is
	// deterministic across map/collection ordering.
	plan := PlanPreemption([]PreemptCandidate{
		{Name: "zeta", VRAMBytes: 8 * vgib, Idle: true},
		{Name: "alpha", VRAMBytes: 8 * vgib, Idle: true},
	}, 8*vgib, 0)
	if !reflect.DeepEqual(plan.Stop, []string{"alpha"}) {
		t.Fatalf("expected alpha (name tie-break) first, got %v", plan.Stop)
	}
}
