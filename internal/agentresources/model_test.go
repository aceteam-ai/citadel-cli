package agentresources

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func fixture() (Policy, Snapshot, Context, DiskEvidence) {
	identity := Identity{HostID: "host-a", PoolID: "pool-a", ConfigID: "config-a"}
	policy := Policy{
		Identity: identity, PoolCap: 2, Floor: Floor{Amount: 40, Unit: GB},
		Filesystems: Filesystems{Repository: "repo-fs", Worktree: "worktree-fs", State: "state-fs"},
	}
	snapshot := Snapshot{
		Identity: identity, Revision: 5, Complete: true, NextAllocationID: 2,
		Allocations: []Allocation{{ID: 1, Worktree: WorktreeRef{ID: "tree-a", Filesystem: "worktree-fs"}, Phase: Idle, Custody: Inactive}},
	}
	context := Context{Identity: identity, Revision: snapshot.Revision, Round: "round-a"}
	evidence := DiskEvidence{Context: context, Observations: []DiskObservation{
		{Filesystem: "repo-fs", Known: true, Fresh: true, FreeBytes: 40_000_000_000},
		{Filesystem: "worktree-fs", Known: true, Fresh: true, FreeBytes: 40_000_000_000},
		{Filesystem: "state-fs", Known: true, Fresh: true, FreeBytes: 40_000_000_000},
	}}
	return policy, snapshot, context, evidence
}

func requireHold(t *testing.T, err error, reason HoldReason) *Hold {
	t.Helper()
	var failure *Hold
	if !errors.As(err, &failure) || failure.Reason() != reason {
		t.Fatalf("error=%v, want static hold %s", err, reason)
	}
	return failure
}

func requireNoPlan(t *testing.T, plan Plan, err error, reason HoldReason) *Hold {
	t.Helper()
	if !reflect.DeepEqual(plan, Plan{}) {
		t.Fatal("failed proposal leaked a next snapshot", plan)
	}
	return requireHold(t, err, reason)
}

func TestFloorRequiresExplicitUnitsAndCheckedFortyOrGreaterAmount(t *testing.T) {
	for _, test := range []struct {
		floor Floor
		want  uint64
	}{
		{Floor{Amount: 40, Unit: GB}, 40_000_000_000},
		{Floor{Amount: 40, Unit: GiB}, 42_949_672_960},
		{Floor{Amount: 41, Unit: GB}, 41_000_000_000},
		{Floor{}, 0}, {Floor{Amount: 40}, 0}, {Floor{Amount: 40, Unit: "Bytes"}, 0},
		{Floor{Amount: 39, Unit: GB}, 0}, {Floor{Amount: 39, Unit: GiB}, 0},
		{Floor{Amount: maxUint64, Unit: GB}, 0}, {Floor{Amount: maxUint64, Unit: GiB}, 0},
	} {
		got, err := FloorBytes(test.floor)
		if test.want == 0 {
			requireHold(t, err, InvalidPolicy)
			if got != 0 {
				t.Fatal("invalid floor returned an apparent threshold")
			}
		} else if err != nil || got != test.want {
			t.Fatalf("floor=%+v: got %d, %v; want %d", test.floor, got, err, test.want)
		}
	}
}

func TestInvalidPolicyOrIncompleteStateNeverBecomesEmptyCapacity(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*Policy, *Snapshot)
		reason HoldReason
	}{
		{"zero policy", func(p *Policy, _ *Snapshot) { *p = Policy{} }, InvalidPolicy},
		{"missing host", func(p *Policy, _ *Snapshot) { p.Identity.HostID = "" }, InvalidPolicy},
		{"noncanonical pool", func(p *Policy, _ *Snapshot) { p.Identity.PoolID = " pool" }, InvalidPolicy},
		{"control config", func(p *Policy, _ *Snapshot) { p.Identity.ConfigID = "a\x1b" }, InvalidPolicy},
		{"oversized config", func(p *Policy, _ *Snapshot) { p.Identity.ConfigID = strings.Repeat("x", 129) }, InvalidPolicy},
		{"missing cap", func(p *Policy, _ *Snapshot) { p.PoolCap = 0 }, InvalidPolicy},
		{"missing floor unit", func(p *Policy, _ *Snapshot) { p.Floor.Unit = "" }, InvalidPolicy},
		{"low floor", func(p *Policy, _ *Snapshot) { p.Floor.Amount = 39 }, InvalidPolicy},
		{"overflowing floor", func(p *Policy, _ *Snapshot) { p.Floor.Amount = maxUint64 }, InvalidPolicy},
		{"missing filesystem", func(p *Policy, _ *Snapshot) { p.Filesystems.State = "" }, InvalidPolicy},
		{"zero snapshot", func(_ *Policy, s *Snapshot) { *s = Snapshot{} }, InvalidSnapshot},
		{"incomplete", func(_ *Policy, s *Snapshot) { s.Complete = false }, InvalidSnapshot},
		{"missing allocations", func(_ *Policy, s *Snapshot) { s.Allocations = nil }, InvalidSnapshot},
		{"other configuration", func(_ *Policy, s *Snapshot) { s.Identity.ConfigID = "config-b" }, InvalidSnapshot},
		{"missing revision", func(_ *Policy, s *Snapshot) { s.Revision = 0 }, InvalidSnapshot},
		{"missing counter", func(_ *Policy, s *Snapshot) { s.NextAllocationID = 0 }, InvalidSnapshot},
		{"counter reused", func(_ *Policy, s *Snapshot) { s.NextAllocationID = 1 }, InvalidSnapshot},
		{"unknown phase", func(_ *Policy, s *Snapshot) { s.Allocations[0].Phase = "missing" }, InvalidSnapshot},
		{"unknown custody", func(_ *Policy, s *Snapshot) { s.Allocations[0].Custody = "" }, InvalidSnapshot},
		{"idle but active", func(_ *Policy, s *Snapshot) { s.Allocations[0].Custody = Active }, InvalidSnapshot},
		{"in use but inactive", func(_ *Policy, s *Snapshot) { s.Allocations[0].Phase = InUse }, InvalidSnapshot},
		{"foreign filesystem", func(_ *Policy, s *Snapshot) { s.Allocations[0].Worktree.Filesystem = "other-fs" }, InvalidSnapshot},
		{"unexpected cleanup attempt", func(_ *Policy, s *Snapshot) { s.Allocations[0].CleanupAttempt = 1 }, InvalidSnapshot},
		{"missing cleanup attempt", func(_ *Policy, s *Snapshot) { s.Allocations[0].Phase = CleanupUnknown }, InvalidSnapshot},
		{"over capacity", func(p *Policy, s *Snapshot) {
			p.PoolCap = 1
			s.Allocations = append(s.Allocations, Allocation{
				ID: 2, Worktree: WorktreeRef{ID: "tree-b", Filesystem: "worktree-fs"}, Phase: Idle, Custody: Inactive,
			})
			s.NextAllocationID = 3
		}, InvalidSnapshot},
		{"duplicate ID with distinct tree", func(_ *Policy, s *Snapshot) {
			s.Allocations = append(s.Allocations, Allocation{
				ID: 1, Worktree: WorktreeRef{ID: "tree-b", Filesystem: "worktree-fs"}, Phase: Idle, Custody: Inactive,
			})
			s.NextAllocationID = 3
		}, InvalidSnapshot},
	} {
		t.Run(test.name, func(t *testing.T) {
			policy, snapshot, context, evidence := fixture()
			test.change(&policy, &snapshot)
			requireHold(t, Validate(policy, snapshot), test.reason)
			plan, err := PlanReserve(policy, snapshot, context, evidence, WorktreeRef{ID: "tree-b", Filesystem: "worktree-fs"})
			requireNoPlan(t, plan, err, test.reason)
		})
	}
}

func TestEveryOccupiedPhaseConsumesCapacityAndDuplicateTreesRefuse(t *testing.T) {
	for _, phase := range []Phase{Reserved, Creating, CreateUnknown, Idle, InUse, CleanupPending, CleanupUnknown} {
		policy, snapshot, context, evidence := fixture()
		policy.PoolCap = 1
		snapshot.Allocations[0].Phase = phase
		if phase == InUse {
			snapshot.Allocations[0].Custody = CustodyUnknown
		}
		if phase == CleanupPending || phase == CleanupUnknown {
			snapshot.Allocations[0].CleanupAttempt = snapshot.Revision
		}
		if err := Validate(policy, snapshot); err != nil {
			t.Fatalf("valid occupied phase %s was refused: %v", phase, err)
		}
		plan, err := PlanReserve(policy, snapshot, context, evidence, WorktreeRef{ID: "tree-b", Filesystem: "worktree-fs"})
		requireNoPlan(t, plan, err, PoolFull)
	}
	for _, phase := range []Phase{InUse, CreateUnknown} {
		policy, snapshot, context, _ := fixture()
		snapshot.NextAllocationID = 3
		snapshot.Allocations = append(snapshot.Allocations, Allocation{
			ID: 2, Worktree: snapshot.Allocations[0].Worktree, Phase: phase, Custody: CustodyUnknown,
		})
		requireHold(t, Validate(policy, snapshot), InvalidSnapshot)
		proof := cleanupProof(snapshot, context)
		plan, err := PlanBeginCleanup(policy, snapshot, context, proof)
		requireNoPlan(t, plan, err, InvalidSnapshot)
		snapshot.Allocations[1].Worktree.ID = "different-owned-tree"
		if err := Validate(policy, snapshot); err != nil {
			t.Fatal("distinct valid worktree control refused", err)
		}
		plan, err = PlanBeginCleanup(policy, snapshot, context, proof)
		if err != nil || plan.Next.Allocations[1].Phase != phase {
			t.Fatal("idle cleanup changed a distinct occupied allocation", err)
		}
	}
}

func TestDiskEvidenceRequiresEveryFreshExactFilesystemAndEqualityPasses(t *testing.T) {
	policy, snapshot, context, evidence := fixture()
	if err := EvaluateDisk(policy, snapshot, context, evidence); err != nil {
		t.Fatal("equality at decimal floor refused", err)
	}
	policy.Floor.Unit = GiB
	requireHold(t, EvaluateDisk(policy, snapshot, context, evidence), DiskLow)
	for i := range evidence.Observations {
		evidence.Observations[i].FreeBytes = 42_949_672_960
	}
	if err := EvaluateDisk(policy, snapshot, context, evidence); err != nil {
		t.Fatal("equality at binary floor refused", err)
	}
	for _, change := range []func(*DiskEvidence){
		func(e *DiskEvidence) { e.Observations = e.Observations[:2] },
		func(e *DiskEvidence) { e.Observations[0].Known = false },
		func(e *DiskEvidence) { e.Observations[1].Fresh = false },
	} {
		p, s, c, e := fixture()
		change(&e)
		requireHold(t, EvaluateDisk(p, s, c, e), DiskUnknown)
		plan, err := PlanReserve(p, s, c, e, WorktreeRef{ID: "tree-b", Filesystem: "worktree-fs"})
		requireNoPlan(t, plan, err, DiskUnknown)
	}
	for _, change := range []func(*DiskEvidence){
		func(e *DiskEvidence) { e.Context.Round = "old-round" },
		func(e *DiskEvidence) { e.Context.Revision-- },
		func(e *DiskEvidence) { e.Context.Identity.HostID = "host-b" },
		func(e *DiskEvidence) { e.Observations[1] = e.Observations[0] },
		func(e *DiskEvidence) { e.Observations[0].Filesystem = "unexpected-fs" },
	} {
		p, s, c, e := fixture()
		change(&e)
		requireHold(t, EvaluateDisk(p, s, c, e), StaleEvidence)
	}
	policy, snapshot, context, evidence = fixture()
	policy.Filesystems = Filesystems{Repository: "shared-fs", Worktree: "shared-fs", State: "shared-fs"}
	snapshot.Allocations[0].Worktree.Filesystem = "shared-fs"
	evidence.Observations = []DiskObservation{{Filesystem: "shared-fs", Known: true, Fresh: true, FreeBytes: 40_000_000_000}}
	if err := EvaluateDisk(policy, snapshot, context, evidence); err != nil {
		t.Fatal("one fresh observation did not cover shared filesystem identities", err)
	}
}

func TestLowDiskCarriesCancellationRequirementWithoutClaimingCompletion(t *testing.T) {
	policy, snapshot, context, evidence := fixture()
	policy.PoolCap, snapshot.NextAllocationID = 3, 4
	snapshot.Allocations = append(snapshot.Allocations,
		Allocation{ID: 2, Worktree: WorktreeRef{ID: "tree-b", Filesystem: "worktree-fs"}, Phase: InUse, Custody: Active},
		Allocation{ID: 3, Worktree: WorktreeRef{ID: "tree-c", Filesystem: "worktree-fs"}, Phase: CreateUnknown, Custody: CustodyUnknown},
	)
	evidence.Observations[0].FreeBytes--
	evidence.Observations[1].Known = false
	plan, err := PlanReserve(policy, snapshot, context, evidence, WorktreeRef{ID: "tree-d", Filesystem: "worktree-fs"})
	failure := requireNoPlan(t, plan, err, DiskLow)
	if !reflect.DeepEqual(failure.CancellationRequired(), []uint64{2, 3}) || len(snapshot.Allocations) != 3 || snapshot.Allocations[1].Custody != Active {
		t.Fatal("pressure lost active/uncertain custody or implied released capacity")
	}
	ids := failure.CancellationRequired()
	ids[0] = 99
	if failure.CancellationRequired()[0] != 2 {
		t.Fatal("caller mutated the hold's cancellation evidence")
	}
}

func cleanupProof(snapshot Snapshot, context Context) CleanupEvidence {
	allocation := snapshot.Allocations[0]
	return CleanupEvidence{
		Context: context, AllocationID: allocation.ID, Worktree: allocation.Worktree,
		Fresh: true, Owned: true, Clean: true, Published: true, Inactive: true,
	}
}

func TestCleanupRetainsCapacityUntilExactRemovalAndDoesNotReuseIDs(t *testing.T) {
	policy, snapshot, context, evidence := fixture()
	policy.PoolCap = 1
	before := snapshot.Allocations[0]
	pending, err := PlanBeginCleanup(policy, snapshot, context, cleanupProof(snapshot, context))
	if err != nil || pending.ExpectedRevision != 5 || pending.Next.Revision != 6 || pending.Next.Allocations[0].CleanupAttempt != 6 || pending.Next.Allocations[0].Phase != CleanupPending || snapshot.Allocations[0] != before {
		t.Fatal("cleanup did not produce an isolated hypothetical pending record", err)
	}
	context.Revision, evidence.Context.Revision = pending.Next.Revision, pending.Next.Revision
	plan, err := PlanReserve(policy, pending.Next, context, evidence, WorktreeRef{ID: "tree-b", Filesystem: "worktree-fs"})
	requireNoPlan(t, plan, err, PoolFull)
	unknown, err := PlanCleanupUncertain(policy, pending.Next, context, 1, 6)
	if err != nil || len(unknown.Next.Allocations) != 1 || unknown.Next.Allocations[0].Phase != CleanupUnknown || unknown.Next.Allocations[0].CleanupAttempt != 6 {
		t.Fatal("cleanup uncertainty released a slot or replaced the attempt", err)
	}
	context.Revision, evidence.Context.Revision = unknown.Next.Revision, unknown.Next.Revision
	plan, err = PlanReserve(policy, unknown.Next, context, evidence, WorktreeRef{ID: "tree-b", Filesystem: "worktree-fs"})
	requireNoPlan(t, plan, err, PoolFull)
	receipt := RemovalReceipt{Context: context, AllocationID: 1, Worktree: before.Worktree, CleanupAttempt: 6, Fresh: true, ConfirmedRemoved: true}
	removed, err := PlanConfirmRemoval(policy, unknown.Next, context, receipt)
	if err != nil || removed.Next.Allocations == nil || len(removed.Next.Allocations) != 0 || removed.Next.NextAllocationID != 2 {
		t.Fatal("exact removal did not retain the monotonic ID history", err)
	}
	context.Revision, evidence.Context.Revision = removed.Next.Revision, removed.Next.Revision
	reserved, err := PlanReserve(policy, removed.Next, context, evidence, before.Worktree)
	if err != nil || reserved.Next.Allocations[0].ID != 2 || reserved.Next.NextAllocationID != 3 {
		t.Fatal("new reservation reused a removed allocation identity", err)
	}
	receipt.Context = context
	plan, err = PlanConfirmRemoval(policy, removed.Next, context, receipt)
	requireNoPlan(t, plan, err, InvalidTransition)
	context.Revision = reserved.Next.Revision
	receipt.Context = context
	plan, err = PlanConfirmRemoval(policy, reserved.Next, context, receipt)
	requireNoPlan(t, plan, err, InvalidTransition)
	if len(reserved.Next.Allocations) != 1 || reserved.Next.Allocations[0].ID != 2 || reserved.Next.Allocations[0].Worktree != before.Worktree {
		t.Fatal("old receipt released a later reservation of the same worktree reference")
	}
}

func TestCleanupProofsAndReceiptsCannotReleaseOtherOrUncertainCustody(t *testing.T) {
	for _, change := range []func(*CleanupEvidence){
		func(e *CleanupEvidence) { e.Context.Revision-- },
		func(e *CleanupEvidence) { e.Context.Round = "old-round" },
		func(e *CleanupEvidence) { e.AllocationID++ },
		func(e *CleanupEvidence) { e.Worktree.ID = "other-tree" },
		func(e *CleanupEvidence) { e.Worktree.Filesystem = "other-fs" },
		func(e *CleanupEvidence) { e.Fresh = false },
		func(e *CleanupEvidence) { e.Owned = false },
		func(e *CleanupEvidence) { e.Clean = false },
		func(e *CleanupEvidence) { e.Published = false },
		func(e *CleanupEvidence) { e.Inactive = false },
	} {
		policy, snapshot, context, _ := fixture()
		proof := cleanupProof(snapshot, context)
		change(&proof)
		plan, err := PlanBeginCleanup(policy, snapshot, context, proof)
		if err == nil || !reflect.DeepEqual(plan, Plan{}) || snapshot.Allocations[0].Phase != Idle {
			t.Fatal("unproved cleanup produced a plan or changed its source")
		}
	}
	for _, phase := range []Phase{Reserved, Creating, CreateUnknown, InUse} {
		policy, snapshot, context, _ := fixture()
		snapshot.Allocations[0].Phase = phase
		if phase == InUse {
			snapshot.Allocations[0].Custody = CustodyUnknown
		}
		plan, err := PlanBeginCleanup(policy, snapshot, context, cleanupProof(snapshot, context))
		requireNoPlan(t, plan, err, InvalidTransition)
	}
	for _, change := range []func(*Snapshot, *RemovalReceipt){
		func(_ *Snapshot, r *RemovalReceipt) { r.Context.Identity.PoolID = "other-pool" },
		func(_ *Snapshot, r *RemovalReceipt) { r.Context.Revision-- },
		func(_ *Snapshot, r *RemovalReceipt) { r.AllocationID++ },
		func(_ *Snapshot, r *RemovalReceipt) { r.Worktree.ID = "other-tree" },
		func(_ *Snapshot, r *RemovalReceipt) { r.Worktree.Filesystem = "other-fs" },
		func(_ *Snapshot, r *RemovalReceipt) { r.CleanupAttempt-- },
		func(_ *Snapshot, r *RemovalReceipt) { r.Fresh = false },
		func(_ *Snapshot, r *RemovalReceipt) { r.ConfirmedRemoved = false },
		func(s *Snapshot, _ *RemovalReceipt) { s.Allocations[0].Custody = CustodyUnknown },
	} {
		policy, snapshot, context, _ := fixture()
		snapshot.Allocations[0].Phase, snapshot.Allocations[0].CleanupAttempt = CleanupUnknown, snapshot.Revision
		receipt := RemovalReceipt{Context: context, AllocationID: 1, Worktree: snapshot.Allocations[0].Worktree, CleanupAttempt: snapshot.Revision, Fresh: true, ConfirmedRemoved: true}
		change(&snapshot, &receipt)
		plan, err := PlanConfirmRemoval(policy, snapshot, context, receipt)
		if err == nil || !reflect.DeepEqual(plan, Plan{}) || len(snapshot.Allocations) != 1 {
			t.Fatal("stale removal or uncertain custody released capacity")
		}
	}
}

func TestPlansCloneSnapshotsCheckOverflowAndDoNotImplementCAS(t *testing.T) {
	policy, snapshot, context, evidence := fixture()
	first, err := PlanReserve(policy, snapshot, context, evidence, WorktreeRef{ID: "tree-b", Filesystem: "worktree-fs"})
	if err != nil || first.ExpectedRevision != snapshot.Revision || first.Next.Revision != snapshot.Revision+1 {
		t.Fatal("reserve did not bind its proposal to the observed revision", err)
	}
	second, err := PlanReserve(policy, snapshot, context, evidence, WorktreeRef{ID: "tree-c", Filesystem: "worktree-fs"})
	if err != nil || second.ExpectedRevision != first.ExpectedRevision || len(snapshot.Allocations) != 1 || snapshot.NextAllocationID != 2 {
		t.Fatal("hypothetical concurrent plans changed their shared input or claimed CAS", err)
	}
	first.Next.Allocations[0].Worktree.ID = "mutated-plan"
	if snapshot.Allocations[0].Worktree.ID != "tree-a" || second.Next.Allocations[0].Worktree.ID != "tree-a" {
		t.Fatal("plans alias an input or another plan's allocation storage")
	}
	snapshot.Allocations[0].Worktree.ID = "mutated-source"
	if second.Next.Allocations[0].Worktree.ID != "tree-a" {
		t.Fatal("source mutation changed an already prepared plan")
	}
	for _, field := range []string{"revision", "allocation"} {
		policy, snapshot, context, evidence = fixture()
		if field == "revision" {
			snapshot.Revision = maxUint64
			context.Revision, evidence.Context.Revision = maxUint64, maxUint64
		} else {
			snapshot.NextAllocationID = maxUint64
		}
		plan, err := PlanReserve(policy, snapshot, context, evidence, WorktreeRef{ID: "tree-b", Filesystem: "worktree-fs"})
		requireNoPlan(t, plan, err, CounterExhausted)
	}
	policy, snapshot, context, evidence = fixture()
	context.Revision--
	plan, err := PlanReserve(policy, snapshot, context, evidence, WorktreeRef{ID: "tree-b", Filesystem: "worktree-fs"})
	requireNoPlan(t, plan, err, StaleEvidence)
}
