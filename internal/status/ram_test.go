package status

import "testing"

func TestRAMBudgetBytes_GenerousWhenNothingPinned(t *testing.T) {
	// 64GB available, nothing pinned: ceiling = 64GB - 2GB headroom.
	got := RAMBudgetBytes(64*vgib, 0)
	want := 64*vgib - ramHeadroomBytes
	if got != want {
		t.Errorf("RAMBudgetBytes = %d, want %d", got, want)
	}
}

func TestRAMBudgetBytes_SubtractsPinnedFootprint(t *testing.T) {
	// 64GB available, 20GB held by pinned services: ceiling = 64 - 20 - 2 headroom.
	got := RAMBudgetBytes(64*vgib, 20*vgib)
	want := 64*vgib - 20*vgib - ramHeadroomBytes
	if got != want {
		t.Errorf("RAMBudgetBytes = %d, want %d", got, want)
	}
}

func TestRAMBudgetBytes_ReturnsZeroWhenPinnedExceedsAvailable(t *testing.T) {
	// Pinned footprint + headroom exceeds available RAM entirely: RAMBudgetBytes
	// must return 0 ("no safe ceiling"), NOT a clamped-up floor value -- a
	// fabricated ceiling with no relationship to what's actually free is exactly
	// the failure the design doc warns about for the Tier-2 2GB default, reached
	// a different way. The caller (applyRAMIsolation) treats 0 as "skip
	// isolation this start" (fail open).
	got := RAMBudgetBytes(10*vgib, 20*vgib)
	if got != 0 {
		t.Errorf("RAMBudgetBytes = %d, want 0 (no safe ceiling, not a clamped floor)", got)
	}
}

func TestRAMBudgetBytes_ReturnsZeroWhenCeilingWouldBeBelowViableMinimum(t *testing.T) {
	// available - pinned - headroom is positive but below minViableRAMCeilingBytes:
	// still returns 0, not the tiny positive value.
	got := RAMBudgetBytes(3*vgib, vgib) // 3 - 1 - 2(headroom) = 0, below the 2GiB minimum anyway
	if got != 0 {
		t.Errorf("RAMBudgetBytes = %d, want 0", got)
	}
}

func TestPlanRAMPreflight_NoRequirementIsNoOp(t *testing.T) {
	// requiredRAMBytes==0 means "unknown/undeclared": never refuse on an
	// absent signal, regardless of live RAM pressure -- mirrors
	// PlanPreemption's requiredVRAM==0 contract exactly.
	plan := PlanRAMPreflight(0, 1*vgib, 0)
	if !plan.Fits {
		t.Fatalf("requiredRAMBytes==0 must Fit, got %+v", plan)
	}
}

func TestPlanRAMPreflight_FitsWithinBudget(t *testing.T) {
	// 64GB available, nothing pinned => budget is huge; 8GB required easily fits.
	plan := PlanRAMPreflight(8*vgib, 64*vgib, 0)
	if !plan.Fits {
		t.Fatalf("expected fit, got %+v", plan)
	}
}

func TestPlanRAMPreflight_RefusesConfirmedShortfall(t *testing.T) {
	// 10GB available, 8GB pinned => reserved (pinned + 2GB headroom) exactly
	// consumes availability, so RAMBudgetBytes returns 0 (no safe ceiling). A
	// declared requirement of 20GB against a 0 budget is a CONFIRMED
	// shortfall: refuse.
	plan := PlanRAMPreflight(20*vgib, 10*vgib, 8*vgib)
	if plan.Fits {
		t.Fatalf("expected refusal on confirmed shortfall, got %+v", plan)
	}
	if plan.Reason == "" {
		t.Error("expected a non-empty reason on refusal")
	}
}

func TestPlanRAMPreflight_ExactFitAtBudgetBoundary(t *testing.T) {
	// requiredRAMBytes exactly equal to the budget must Fit (<=, not <).
	available := 64 * vgib
	pinned := uint64(0)
	budget := RAMBudgetBytes(available, pinned)
	plan := PlanRAMPreflight(budget, available, pinned)
	if !plan.Fits {
		t.Fatalf("expected fit at exact budget boundary, got %+v", plan)
	}
	// One byte over must refuse.
	plan = PlanRAMPreflight(budget+1, available, pinned)
	if plan.Fits {
		t.Fatalf("expected refusal one byte over budget, got %+v", plan)
	}
}
