package agentquota

import (
	"reflect"
	"testing"
)

func fixture() (Policy, Snapshot) {
	scope := Scope{HostID: "host-a", PoolID: "pool-a", ConfigID: "config-a"}
	policy := Policy{Scope: scope, Lanes: []LanePolicy{
		{LaneID: "a", Enabled: true, MaxTurns: 10, MaxTurnMillis: MaxTurnMillis},
		{LaneID: "b", Enabled: true, MaxTurns: 10, MaxTurnMillis: MaxTurnMillis},
	}}
	snapshot := Snapshot{
		Scope: scope, Complete: true, Revision: 1, NextID: 1,
		ClockEpoch: "epoch-a", StoreContinuityID: "store-a",
		Lanes: []Lane{{LaneID: "a", Charges: []Charge{}}, {LaneID: "b", Charges: []Charge{}}},
	}
	return policy, snapshot
}

func contextFor(snapshot Snapshot, now uint64) Context {
	binding := Binding{Scope: snapshot.Scope, Revision: snapshot.Revision, Round: snapshot.LastRound + 1}
	return Context{
		Binding: binding,
		Clock:   ClockObservation{Binding: binding, State: ClockKnown, Epoch: snapshot.ClockEpoch, ElapsedMillis: now},
		Store:   StoreObservation{Binding: binding, State: StoreContinuous, ContinuityID: snapshot.StoreContinuityID},
	}
}

func accept(t *testing.T, policy Policy, before Snapshot, plan Plan, err error) Snapshot {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected hold: %v", err)
	}
	if plan.ExpectedRevision != before.Revision || plan.Next.Revision != before.Revision+1 {
		t.Fatalf("bad revision proposal: %#v", plan)
	}
	if err := Validate(policy, plan.Next); err != nil {
		t.Fatalf("proposed invalid state: %v", err)
	}
	return plan.Next
}

func reject(t *testing.T, plan Plan, err error, reason HoldReason) *Hold {
	t.Helper()
	hold, ok := err.(*Hold)
	if !ok || hold.Reason != reason {
		t.Fatalf("hold = %#v, want %s", err, reason)
	}
	if !reflect.DeepEqual(plan, Plan{}) {
		t.Fatalf("hold exposed a proposal: %#v", plan)
	}
	return hold
}

func reserve(t *testing.T, policy Policy, snapshot Snapshot, now uint64, lane string) Snapshot {
	t.Helper()
	plan, err := PlanReserve(policy, snapshot, contextFor(snapshot, now), lane)
	return accept(t, policy, snapshot, plan, err)
}

func attempt(t *testing.T, policy Policy, snapshot Snapshot, now uint64, ref Reservation) Snapshot {
	t.Helper()
	plan, err := PlanAttempt(policy, snapshot, contextFor(snapshot, now), ref)
	return accept(t, policy, snapshot, plan, err)
}

func stop(t *testing.T, policy Policy, snapshot Snapshot, now uint64, ref Reservation) Snapshot {
	t.Helper()
	plan, err := PlanRequestStop(policy, snapshot, contextFor(snapshot, now), ref)
	return accept(t, policy, snapshot, plan, err)
}

func receiptFor(t *testing.T, snapshot Snapshot, ctx Context, ref Reservation) InactivityReceipt {
	t.Helper()
	for _, lane := range snapshot.Lanes {
		for _, charge := range lane.Charges {
			if lane.LaneID == ref.LaneID && charge.ReservationID == ref.ReservationID {
				return InactivityReceipt{
					Binding: ctx.Binding,
					Custody: Custody{Reservation: ref, AttemptID: charge.AttemptID, StopID: charge.StopID},
					State:   ActivityInactive,
				}
			}
		}
	}
	t.Fatal("missing fixture reservation")
	return InactivityReceipt{}
}

func inactive(t *testing.T, policy Policy, snapshot Snapshot, now uint64, ref Reservation) Snapshot {
	t.Helper()
	ctx := contextFor(snapshot, now)
	plan, err := PlanConfirmInactive(policy, snapshot, ctx, receiptFor(t, snapshot, ctx, ref))
	return accept(t, policy, snapshot, plan, err)
}

func tenInactive(t *testing.T) (Policy, Snapshot) {
	t.Helper()
	policy, snapshot := fixture()
	for i := 0; i < MaxTurnsPerWindow; i++ {
		id := snapshot.NextID
		snapshot = reserve(t, policy, snapshot, 0, "a")
		ref := Reservation{LaneID: "a", ReservationID: id}
		snapshot = stop(t, policy, snapshot, 0, ref)
		snapshot = inactive(t, policy, snapshot, 0, ref)
	}
	return policy, snapshot
}

func TestRoundMustAdvanceEvenWhenAllObservationBindingsAgree(t *testing.T) {
	for _, round := range []uint64{1, 2} {
		policy, snapshot := fixture()
		snapshot.LastRound = 2
		ctx := contextFor(snapshot, 0)
		if _, err := PlanReserve(policy, snapshot, ctx, "a"); err != nil {
			t.Fatal("fresh round control refused", err)
		}
		ctx.Binding.Round = round
		ctx.Clock.Binding, ctx.Store.Binding = ctx.Binding, ctx.Binding
		// Revision, scope, epoch and continuity remain current. Neither
		// observation equality nor the zero-round check can mask this guard.
		plan, err := PlanReserve(policy, snapshot, ctx, "a")
		reject(t, plan, err, HoldBinding)
	}
}

func TestCanonicalIDsRejectDotAndDotDotWithoutPathNormalization(t *testing.T) {
	for _, id := range []string{".", ".."} {
		if canonicalID(id) {
			t.Fatalf("dot-only identifier accepted: %q", id)
		}
	}
	// These are opaque IDs, not safe path components or command arguments.
	// Their currently permitted spelling must not be silently normalized.
	for _, id := range []string{"...", "-lane", "host-a"} {
		if !canonicalID(id) {
			t.Fatalf("permitted opaque identifier refused: %q", id)
		}
	}
}

func TestUncertainAndStopTransitionsRejectMismatchedReservations(t *testing.T) {
	for _, transition := range []struct {
		name      string
		stopping  bool
		plan      func(Policy, Snapshot, Context, Reservation) (Plan, error)
		wantPhase Phase
	}{
		{"attempt uncertain", false, PlanAttemptUncertain, AttemptUnknown},
		{"stop uncertain", true, PlanStopUncertain, StopUnknown},
		{"request stop", false, PlanRequestStop, Stopping},
	} {
		t.Run(transition.name, func(t *testing.T) {
			policy, snapshot := fixture()
			ref := Reservation{LaneID: "a", ReservationID: 1}
			snapshot = attempt(t, policy, reserve(t, policy, snapshot, 0, "a"), 0, ref)
			if transition.stopping {
				snapshot = stop(t, policy, snapshot, 0, ref)
			}
			ctx := contextFor(snapshot, 0)
			before := snapshot.Lanes[0].Charges[0]
			for _, bad := range []Reservation{
				{LaneID: "missing", ReservationID: ref.ReservationID},
				{LaneID: "b", ReservationID: ref.ReservationID},
				{LaneID: "a", ReservationID: snapshot.NextID},
				{LaneID: "a", ReservationID: 0},
			} {
				plan, err := transition.plan(policy, snapshot, ctx, bad)
				reject(t, plan, err, HoldReservation)
				if snapshot.Lanes[0].Charges[0] != before {
					t.Fatal("reservation mismatch changed another turn's custody")
				}
			}
			plan, err := transition.plan(policy, snapshot, ctx, ref)
			next := accept(t, policy, snapshot, plan, err)
			if next.Lanes[0].Charges[0].Phase != transition.wantPhase {
				t.Fatal("exact reservation control did not make the intended transition")
			}
		})
	}
}

func TestChargePrecedesOneShotAttemptAndUncertaintyNeverRefunds(t *testing.T) {
	policy, snapshot := fixture()
	charged := reserve(t, policy, snapshot, 0, "a")
	ref := Reservation{LaneID: "a", ReservationID: 1}
	if got := charged.Lanes[0].Charges; len(got) != 1 || got[0].Phase != Reserved || got[0].AttemptID != 0 || charged.NextID != 2 {
		t.Fatalf("reservation did not charge before attempt: %#v", charged)
	}
	running := attempt(t, policy, charged, 0, ref)
	if got := running.Lanes[0].Charges[0]; got.AttemptID != 2 || got.Phase != Attempting || running.NextID != 3 {
		t.Fatalf("attempt identity = %#v", got)
	}
	plan, err := PlanAttempt(policy, running, contextFor(running, 0), ref)
	reject(t, plan, err, HoldPhase)
	plan, err = PlanAttemptUncertain(policy, running, contextFor(running, 0), ref)
	uncertain := accept(t, policy, running, plan, err)
	if got := uncertain.Lanes[0].Charges[0]; got.Phase != AttemptUnknown || got.AttemptID != 2 || uncertain.NextID != 3 {
		t.Fatalf("unknown launch erased charge/attempt: %#v", uncertain)
	}
	plan, err = PlanAttempt(policy, uncertain, contextFor(uncertain, 0), ref)
	reject(t, plan, err, HoldPhase)
	plan, err = PlanReserve(policy, uncertain, contextFor(uncertain, 0), "a")
	reject(t, plan, err, HoldBusy)
	stopping := stop(t, policy, uncertain, 0, ref)
	if stopping.Lanes[0].Charges[0].StopID != 3 {
		t.Fatal("stop did not mint the next global identity")
	}
	plan, err = PlanRequestStop(policy, stopping, contextFor(stopping, 0), ref)
	reject(t, plan, err, HoldPhase)
	plan, err = PlanStopUncertain(policy, stopping, contextFor(stopping, 0), ref)
	unknownStop := accept(t, policy, stopping, plan, err)
	if unknownStop.Lanes[0].Charges[0].Phase != StopUnknown {
		t.Fatal("stop uncertainty lost")
	}
	plan, err = PlanRequestStop(policy, unknownStop, contextFor(unknownStop, 0), ref)
	reject(t, plan, err, HoldPhase)
	released := inactive(t, policy, unknownStop, 0, ref)
	if len(released.Lanes[0].Charges) != 1 || released.Lanes[0].Charges[0].Phase != Inactive {
		t.Fatal("confirmed stop refunded the rolling charge")
	}
}

func TestRollingTenTurnCapAndExactWindowBoundary(t *testing.T) {
	policy, full := tenInactive(t)
	if len(full.Lanes[0].Charges) != 10 {
		t.Fatal("fixture did not retain ten charged reservations")
	}
	for _, now := range []uint64{0, WindowMillis - 1} {
		plan, err := PlanReserve(policy, full, contextFor(full, now), "a")
		reject(t, plan, err, HoldQuota)
	}
	policy.Lanes[0].MaxTurns = 5
	plan, err := PlanReserve(policy, full, contextFor(full, 0), "a")
	reject(t, plan, err, HoldQuota)
	if len(full.Lanes[0].Charges) != 10 {
		t.Fatal("cap reduction removed debt")
	}
	aged := reserve(t, policy, full, WindowMillis, "a")
	if len(aged.Lanes[0].Charges) != 1 || aged.Lanes[0].Charges[0].ReservationID != full.NextID || aged.NextID != full.NextID+1 {
		t.Fatalf("boundary did not age only inactive charges: %#v", aged)
	}
	// A different lane has an independent quota, but the same aggregate revision.
	other := reserve(t, policy, full, 0, "b")
	if len(other.Lanes[0].Charges) != 10 || len(other.Lanes[1].Charges) != 1 {
		t.Fatal("cross-lane reservation changed another lane's quota")
	}
}

func TestTimeoutRequiresStopAndNeverReleasesCustody(t *testing.T) {
	policy, snapshot := fixture()
	ref := Reservation{LaneID: "a", ReservationID: 1}
	running := attempt(t, policy, reserve(t, policy, snapshot, 0, "a"), 0, ref)
	// A reserved turn may start one millisecond before its deadline.
	charged := reserve(t, policy, snapshot, 0, "a")
	before := attempt(t, policy, charged, MaxTurnMillis-1, ref)
	if before.Lanes[0].Charges[0].Phase != Attempting {
		t.Fatal("positive deadline control did not start")
	}
	for _, now := range []uint64{MaxTurnMillis, WindowMillis + 1} {
		plan, err := PlanReserve(policy, running, contextFor(running, now), "a")
		hold := reject(t, plan, err, HoldDeadline)
		want := []Custody{{Reservation: ref, AttemptID: 2}}
		if !reflect.DeepEqual(hold.StopRequired, want) {
			t.Fatalf("stop requests = %#v, want %#v", hold.StopRequired, want)
		}
		if len(running.Lanes[0].Charges) != 1 || running.Lanes[0].Charges[0].Phase != Attempting {
			t.Fatal("time passage freed custody")
		}
		other := reserve(t, policy, running, now, "b")
		if len(other.Lanes[0].Charges) != 1 || other.Lanes[0].Charges[0].Phase != Attempting {
			t.Fatal("unrelated lane admission pruned overdue active custody")
		}
	}
	stopping := stop(t, policy, running, WindowMillis+1, ref)
	if len(stopping.Lanes[0].Charges) != 1 {
		t.Fatal("stop request released an old active charge")
	}
	confirmed := inactive(t, policy, stopping, WindowMillis+1, ref)
	if len(confirmed.Lanes[0].Charges) != 0 || confirmed.NextID != 4 {
		t.Fatal("confirmed aged inactivity did not retain monotonic identity")
	}
}

func TestCurrentOptOutAndLowerLimitsPreserveCleanup(t *testing.T) {
	for _, change := range []string{"disabled", "removed", "empty"} {
		t.Run(change, func(t *testing.T) {
			policy, snapshot := fixture()
			charged := reserve(t, policy, snapshot, 0, "a")
			ref := Reservation{LaneID: "a", ReservationID: 1}
			switch change {
			case "disabled":
				policy.Lanes[0].Enabled = false
			case "removed":
				policy.Lanes = policy.Lanes[1:]
			case "empty":
				policy.Lanes = []LanePolicy{}
			}
			plan, err := PlanAttempt(policy, charged, contextFor(charged, 0), ref)
			reject(t, plan, err, HoldLane)
			plan, err = PlanReserve(policy, charged, contextFor(charged, 0), "a")
			reject(t, plan, err, HoldLane)
			stopping := stop(t, policy, charged, 0, ref)
			released := inactive(t, policy, stopping, 0, ref)
			if len(released.Lanes[0].Charges) != 1 || released.Lanes[0].Charges[0].AttemptID != 0 || released.Lanes[0].Charges[0].StopID == 0 {
				t.Fatal("never-attempted opt-out bypassed stop receipt or lost debt")
			}
		})
	}
	policy, snapshot := fixture()
	first := reserve(t, policy, snapshot, 0, "a")
	first = inactive(t, policy, stop(t, policy, first, 0, Reservation{LaneID: "a", ReservationID: 1}), 0, Reservation{LaneID: "a", ReservationID: 1})
	secondID := first.NextID
	second := reserve(t, policy, first, 0, "a")
	policy.Lanes[0].MaxTurns = 1
	ref := Reservation{LaneID: "a", ReservationID: secondID}
	plan, err := PlanAttempt(policy, second, contextFor(second, 0), ref)
	reject(t, plan, err, HoldQuota)
	cleaned := inactive(t, policy, stop(t, policy, second, 0, ref), 0, ref)
	if len(cleaned.Lanes[0].Charges) != 2 {
		t.Fatal("lowered quota erased prior charges")
	}
	policy, snapshot = fixture()
	charged := reserve(t, policy, snapshot, 0, "a")
	policy.Lanes[0].MaxTurnMillis = 1000
	plan, err = PlanAttempt(policy, charged, contextFor(charged, 1000), Reservation{LaneID: "a", ReservationID: 1})
	reject(t, plan, err, HoldDeadline)
	// Raising current policy cannot extend the stored admission limit either.
	policy.Lanes[0].MaxTurnMillis = 1000
	short := reserve(t, policy, snapshot, 0, "a")
	policy.Lanes[0].MaxTurnMillis = MaxTurnMillis
	plan, err = PlanAttempt(policy, short, contextFor(short, 1000), Reservation{LaneID: "a", ReservationID: 1})
	reject(t, plan, err, HoldDeadline)
	// An attempt admitted under a tightened limit keeps it after a later raise.
	policy.Lanes[0].MaxTurnMillis = 1000
	tightened := attempt(t, policy, charged, 999, Reservation{LaneID: "a", ReservationID: 1})
	if tightened.Lanes[0].Charges[0].TurnLimitMillis != 1000 {
		t.Fatal("attempt did not retain tightened limit")
	}
	policy.Lanes[0].MaxTurnMillis = MaxTurnMillis
	plan, err = PlanReserve(policy, tightened, contextFor(tightened, 1000), "a")
	reject(t, plan, err, HoldDeadline)
}

func TestClockAndStoreEvidenceFailIndependently(t *testing.T) {
	tests := []struct {
		name   string
		change func(*Context)
		reason HoldReason
	}{
		{"unknown clock", func(c *Context) { c.Clock.State = ClockUnknown }, HoldClock},
		{"missing clock", func(c *Context) { c.Clock.State = "" }, HoldClock},
		{"changed epoch", func(c *Context) { c.Clock.Epoch = "epoch-b" }, HoldClock},
		{"backwards", func(c *Context) { c.Clock.ElapsedMillis = 9 }, HoldClock},
		{"unknown store", func(c *Context) { c.Store.State = StoreUnknown }, HoldStore},
		{"missing store", func(c *Context) { c.Store.State = "" }, HoldStore},
		{"rollback", func(c *Context) { c.Store.State = StoreRollback }, HoldStore},
		{"commit unknown", func(c *Context) { c.Store.State = StoreCommitUnknown }, HoldStore},
		{"changed continuity", func(c *Context) { c.Store.ContinuityID = "store-b" }, HoldStore},
		{"foreign host", func(c *Context) { c.Binding.Scope.HostID = "host-b" }, HoldBinding},
		{"foreign pool", func(c *Context) { c.Binding.Scope.PoolID = "pool-b" }, HoldBinding},
		{"foreign config", func(c *Context) { c.Binding.Scope.ConfigID = "config-b" }, HoldBinding},
		{"stale revision", func(c *Context) { c.Binding.Revision-- }, HoldBinding},
		{"repeated round", func(c *Context) { c.Binding.Round-- }, HoldBinding},
		{"clock round", func(c *Context) { c.Clock.Binding.Round++ }, HoldBinding},
		{"store round", func(c *Context) { c.Store.Binding.Round++ }, HoldBinding},
		{"clock host", func(c *Context) { c.Clock.Binding.Scope.HostID = "host-b" }, HoldBinding},
		{"store pool", func(c *Context) { c.Store.Binding.Scope.PoolID = "pool-b" }, HoldBinding},
		{"clock revision", func(c *Context) { c.Clock.Binding.Revision++ }, HoldBinding},
		{"store revision", func(c *Context) { c.Store.Binding.Revision++ }, HoldBinding},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			policy, snapshot := fixture()
			snapshot.ElapsedHighWater = 10
			snapshot.LastRound = 1
			ctx := contextFor(snapshot, 10)
			test.change(&ctx)
			plan, err := PlanReserve(policy, snapshot, ctx, "a")
			reject(t, plan, err, test.reason)
			if snapshot.NextID != 1 || snapshot.Revision != 1 || len(snapshot.Lanes[0].Charges) != 0 {
				t.Fatal("uncertain evidence mutated the supplied snapshot")
			}
		})
	}
	policy, snapshot := fixture()
	snapshot.ElapsedHighWater = 10
	ctx := contextFor(snapshot, 10)
	plan, err := PlanReserve(policy, snapshot, ctx, "a")
	next := accept(t, policy, snapshot, plan, err)
	if next.ElapsedHighWater != 10 || next.LastRound != ctx.Binding.Round {
		t.Fatal("equal trusted clock sample rejected or round not retained")
	}
	plan, err = PlanReserve(policy, next, ctx, "b")
	reject(t, plan, err, HoldBinding)
	// Even a pre-reservation empty snapshot cannot bypass an unknown commit.
	ctx = contextFor(snapshot, 10)
	ctx.Store.State = StoreCommitUnknown
	plan, err = PlanReserve(policy, snapshot, ctx, "b")
	reject(t, plan, err, HoldStore)
}

func TestReleaseReceiptIsExactAndCanBeFreshlyReobserved(t *testing.T) {
	policy, snapshot := fixture()
	ref := Reservation{LaneID: "a", ReservationID: 1}
	running := attempt(t, policy, reserve(t, policy, snapshot, 0, "a"), 0, ref)
	stopping := stop(t, policy, running, 0, ref)
	ctx := contextFor(stopping, 0)
	good := receiptFor(t, stopping, ctx, ref)
	tests := []struct {
		name   string
		change func(*InactivityReceipt)
		reason HoldReason
	}{
		{"unknown activity", func(r *InactivityReceipt) { r.State = ActivityUnknown }, HoldReceipt},
		{"missing activity", func(r *InactivityReceipt) { r.State = "" }, HoldReceipt},
		{"still active", func(r *InactivityReceipt) { r.State = ActivityActive }, HoldReceipt},
		{"wrong attempt", func(r *InactivityReceipt) { r.Custody.AttemptID++ }, HoldReceipt},
		{"wrong stop", func(r *InactivityReceipt) { r.Custody.StopID++ }, HoldReceipt},
		{"missing stop", func(r *InactivityReceipt) { r.Custody.StopID = 0 }, HoldReceipt},
		{"wrong reservation", func(r *InactivityReceipt) { r.Custody.ReservationID++ }, HoldReservation},
		{"wrong lane", func(r *InactivityReceipt) { r.Custody.LaneID = "b" }, HoldReservation},
		{"stale revision", func(r *InactivityReceipt) { r.Binding.Revision-- }, HoldBinding},
		{"stale round", func(r *InactivityReceipt) { r.Binding.Round-- }, HoldBinding},
		{"foreign host", func(r *InactivityReceipt) { r.Binding.Scope.HostID = "host-b" }, HoldBinding},
		{"foreign pool", func(r *InactivityReceipt) { r.Binding.Scope.PoolID = "pool-b" }, HoldBinding},
		{"foreign config", func(r *InactivityReceipt) { r.Binding.Scope.ConfigID = "config-b" }, HoldBinding},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			receipt := good
			test.change(&receipt)
			plan, err := PlanConfirmInactive(policy, stopping, ctx, receipt)
			reject(t, plan, err, test.reason)
		})
	}
	// Another lane's commit changes the aggregate revision, not native identity.
	other := reserve(t, policy, stopping, 0, "b")
	fresh := contextFor(other, 0)
	plan, err := PlanConfirmInactive(policy, other, fresh, good)
	reject(t, plan, err, HoldBinding)
	reobserved := good
	reobserved.Binding = fresh.Binding
	plan, err = PlanConfirmInactive(policy, other, fresh, reobserved)
	released := accept(t, policy, other, plan, err)
	if released.Lanes[0].Charges[0].Phase != Inactive || released.Lanes[1].Charges[0].Phase != Reserved {
		t.Fatal("fresh observation did not release only the exact native custody")
	}
	plan, err = PlanConfirmInactive(policy, released, contextFor(released, 0), reobserved)
	reject(t, plan, err, HoldBinding)
	repeated := reobserved
	repeated.Binding = contextFor(released, 0).Binding
	plan, err = PlanConfirmInactive(policy, released, contextFor(released, 0), repeated)
	reject(t, plan, err, HoldPhase)
	newID := released.NextID
	newer := reserve(t, policy, released, 0, "a")
	newerRef := Reservation{LaneID: "a", ReservationID: newID}
	newer = stop(t, policy, newer, 0, newerRef)
	oldReceipt := reobserved
	oldReceipt.Binding = contextFor(newer, 0).Binding
	plan, err = PlanConfirmInactive(policy, newer, contextFor(newer, 0), oldReceipt)
	reject(t, plan, err, HoldPhase)
	if newer.Lanes[0].Charges[1].Phase != Stopping || newer.Lanes[0].Charges[1].ReservationID != newID {
		t.Fatal("old receipt released later same-lane custody")
	}
}

func TestNoSilentGenesisAndBoundedCorruptStateRefusals(t *testing.T) {
	tests := []struct {
		name   string
		change func(*Snapshot)
	}{
		{"incomplete", func(s *Snapshot) { s.Complete = false }},
		{"zero revision", func(s *Snapshot) { s.Revision = 0 }},
		{"zero next id", func(s *Snapshot) { s.NextID = 0 }},
		{"missing clock epoch", func(s *Snapshot) { s.ClockEpoch = "" }},
		{"missing store continuity", func(s *Snapshot) { s.StoreContinuityID = "" }},
		{"missing lanes", func(s *Snapshot) { s.Lanes = nil }},
		{"empty lanes", func(s *Snapshot) { s.Lanes = []Lane{} }},
		{"missing charge collection", func(s *Snapshot) { s.Lanes[0].Charges = nil }},
		{"duplicate lane", func(s *Snapshot) { s.Lanes[1].LaneID = "a" }},
		{"unknown phase", func(s *Snapshot) { s.Lanes[0].Charges[0].Phase = "done" }},
		{"unissued id", func(s *Snapshot) { s.Lanes[0].Charges[0].ReservationID = s.NextID }},
		{"future charge", func(s *Snapshot) { s.Lanes[0].Charges[0].ReservedAt = 1 }},
		{"reserved with attempt", func(s *Snapshot) {
			s.Lanes[0].Charges[0].AttemptID = 2
			s.NextID = 3
		}},
		{"attempt without id", func(s *Snapshot) { s.Lanes[0].Charges[0].Phase = Attempting }},
		{"stopping without stop", func(s *Snapshot) { s.Lanes[0].Charges[0].Phase = Stopping }},
		{"inactive without stop", func(s *Snapshot) { s.Lanes[0].Charges[0].Phase = Inactive }},
		{"invalid stored turn limit", func(s *Snapshot) { s.Lanes[0].Charges[0].TurnLimitMillis = MaxTurnMillis + 1 }},
		{"duplicate global id", func(s *Snapshot) {
			s.Lanes[1].Charges = []Charge{s.Lanes[0].Charges[0]}
		}},
		{"two occupied slots", func(s *Snapshot) {
			s.Lanes[0].Charges = append(s.Lanes[0].Charges, Charge{ReservationID: 2, TurnLimitMillis: MaxTurnMillis, Phase: Reserved})
			s.NextID = 3
		}},
		{"oversized lane collection", func(s *Snapshot) {
			for i := 0; i < MaxLanes-1; i++ {
				id := string([]byte{'l', '-', '0' + byte(i/10), '0' + byte(i%10)})
				s.Lanes = append(s.Lanes, Lane{LaneID: id, Charges: []Charge{}})
			}
		}},
		{"oversized charge collection", func(s *Snapshot) {
			s.Lanes[0].Charges = []Charge{}
			s.ElapsedHighWater = WindowMillis
			for i := uint64(0); i < MaxChargesPerLane+1; i++ {
				s.Lanes[0].Charges = append(s.Lanes[0].Charges, Charge{ReservationID: i*2 + 1, StopID: i*2 + 2, TurnLimitMillis: MaxTurnMillis, Phase: Inactive})
			}
			s.NextID = 2*(MaxChargesPerLane+1) + 1
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			policy, snapshot := fixture()
			snapshot = reserve(t, policy, snapshot, 0, "a")
			test.change(&snapshot)
			plan, err := PlanReserve(policy, snapshot, contextFor(snapshot, snapshot.ElapsedHighWater), "b")
			reject(t, plan, err, HoldState)
		})
	}
	policy, _ := fixture()
	plan, err := PlanReserve(policy, Snapshot{}, Context{}, "a")
	reject(t, plan, err, HoldState)
	// Eleven recent, nonactive, otherwise valid charges exceed the hard maximum.
	policy, snapshot := fixture()
	for i := uint64(0); i < 11; i++ {
		snapshot.Lanes[0].Charges = append(snapshot.Lanes[0].Charges, Charge{ReservationID: i*2 + 1, StopID: i*2 + 2, TurnLimitMillis: MaxTurnMillis, Phase: Inactive})
	}
	snapshot.NextID = 23
	plan, err = PlanReserve(policy, snapshot, contextFor(snapshot, 0), "b")
	reject(t, plan, err, HoldState)
}

func TestInvalidPolicyIsNotDefaultedOrMigrated(t *testing.T) {
	tests := []struct {
		name   string
		change func(*Policy)
		reason HoldReason
	}{
		{"empty host", func(p *Policy) { p.Scope.HostID = "" }, HoldPolicy},
		{"noncanonical pool", func(p *Policy) { p.Scope.PoolID = "pool/../other" }, HoldPolicy},
		{"missing config", func(p *Policy) { p.Scope.ConfigID = "" }, HoldPolicy},
		{"changed config", func(p *Policy) { p.Scope.ConfigID = "config-b" }, HoldState},
		{"missing lane policy", func(p *Policy) { p.Lanes = nil }, HoldPolicy},
		{"zero cap", func(p *Policy) { p.Lanes[0].MaxTurns = 0 }, HoldPolicy},
		{"raised cap", func(p *Policy) { p.Lanes[0].MaxTurns = 11 }, HoldPolicy},
		{"zero duration", func(p *Policy) { p.Lanes[0].MaxTurnMillis = 0 }, HoldPolicy},
		{"raised duration", func(p *Policy) { p.Lanes[0].MaxTurnMillis = MaxTurnMillis + 1 }, HoldPolicy},
		{"duplicate lane", func(p *Policy) { p.Lanes[1].LaneID = "a" }, HoldPolicy},
		{"new uninitialized lane", func(p *Policy) { p.Lanes[1].LaneID = "c" }, HoldState},
		{"too many lanes", func(p *Policy) {
			for i := 0; i < MaxLanes-1; i++ {
				id := string([]byte{'l', '-', '0' + byte(i/10), '0' + byte(i%10)})
				p.Lanes = append(p.Lanes, LanePolicy{LaneID: id, Enabled: true, MaxTurns: 10, MaxTurnMillis: MaxTurnMillis})
			}
		}, HoldPolicy},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			policy, snapshot := fixture()
			test.change(&policy)
			plan, err := PlanReserve(policy, snapshot, contextFor(snapshot, 0), "a")
			reject(t, plan, err, test.reason)
		})
	}
}

func TestCheckedCountersAndSubtractionNearUint64Limit(t *testing.T) {
	policy, snapshot := fixture()
	snapshot.Revision = maxCounter
	plan, err := PlanReserve(policy, snapshot, contextFor(snapshot, 0), "a")
	reject(t, plan, err, HoldCounter)
	policy, snapshot = fixture()
	snapshot.NextID = maxCounter
	plan, err = PlanReserve(policy, snapshot, contextFor(snapshot, 0), "a")
	reject(t, plan, err, HoldCounter)
	policy, snapshot = fixture()
	charged := reserve(t, policy, snapshot, 0, "a")
	charged.NextID = maxCounter
	ref := Reservation{LaneID: "a", ReservationID: 1}
	plan, err = PlanAttempt(policy, charged, contextFor(charged, 0), ref)
	reject(t, plan, err, HoldCounter)
	plan, err = PlanRequestStop(policy, charged, contextFor(charged, 0), ref)
	reject(t, plan, err, HoldCounter)
	policy, snapshot = fixture()
	snapshot.LastRound = maxCounter
	plan, err = PlanReserve(policy, snapshot, contextFor(snapshot, 0), "a")
	reject(t, plan, err, HoldBinding)
	policy, snapshot = fixture()
	policy.Lanes[0].MaxTurns = 1
	snapshot.ElapsedHighWater = maxCounter - 1
	snapshot.NextID = 3
	snapshot.Lanes[0].Charges = []Charge{{ReservationID: 1, StopID: 2, ReservedAt: maxCounter - WindowMillis, TurnLimitMillis: MaxTurnMillis, Phase: Inactive}}
	plan, err = PlanReserve(policy, snapshot, contextFor(snapshot, maxCounter-1), "a")
	reject(t, plan, err, HoldQuota)
	// At exactly 24 hours debt ages out, but no representable new turn remains.
	plan, err = PlanReserve(policy, snapshot, contextFor(snapshot, maxCounter), "a")
	reject(t, plan, err, HoldCounter)
	snapshot.Lanes[0].Charges[0].Phase = Stopping
	boundary := inactive(t, policy, snapshot, maxCounter, Reservation{LaneID: "a", ReservationID: 1})
	if len(boundary.Lanes[0].Charges) != 0 {
		t.Fatal("window arithmetic wrapped or prevented exact cleanup")
	}
	policy, snapshot = fixture()
	plan, err = PlanReserve(policy, snapshot, contextFor(snapshot, maxCounter-1), "a")
	reject(t, plan, err, HoldCounter)
	charged = reserve(t, policy, snapshot, maxCounter-MaxTurnMillis, "a")
	ref = Reservation{LaneID: "a", ReservationID: 1}
	before := attempt(t, policy, charged, maxCounter-1, ref)
	if before.Lanes[0].Charges[0].Phase != Attempting {
		t.Fatal("exact representable endpoint refused too early")
	}
	plan, err = PlanAttempt(policy, charged, contextFor(charged, maxCounter), ref)
	reject(t, plan, err, HoldDeadline)
	// Imported unsafe reservation remains cleanupable but cannot start.
	unsafe := charged
	unsafe.Lanes = []Lane{{LaneID: "a", Charges: []Charge{charged.Lanes[0].Charges[0]}}, {LaneID: "b", Charges: []Charge{}}}
	unsafe.ElapsedHighWater = maxCounter - 1
	unsafe.Lanes[0].Charges[0].ReservedAt = maxCounter - 1
	plan, err = PlanAttempt(policy, unsafe, contextFor(unsafe, maxCounter-1), ref)
	reject(t, plan, err, HoldCounter)
	cleaned := inactive(t, policy, stop(t, policy, unsafe, maxCounter-1, ref), maxCounter-1, ref)
	if cleaned.Lanes[0].Charges[0].Phase != Inactive {
		t.Fatal("unrepresentable deadline prevented exact stop cleanup")
	}
}

func TestAllLaneProposalsShareOneRevisionAndCopiesAreIndependent(t *testing.T) {
	policy, snapshot := fixture()
	ctx := contextFor(snapshot, 0)
	a, errA := PlanReserve(policy, snapshot, ctx, "a")
	b, errB := PlanReserve(policy, snapshot, ctx, "b")
	if errA != nil || errB != nil || a.ExpectedRevision != b.ExpectedRevision || a.Next.NextID != b.Next.NextID {
		t.Fatalf("not competing at one aggregate: %v %v", errA, errB)
	}
	// An in-memory stand-in models only revision comparison, not real durability.
	current := snapshot
	apply := func(plan Plan) bool {
		if current.Revision != plan.ExpectedRevision {
			return false
		}
		current = plan.Next
		return true
	}
	if !apply(a) || apply(b) {
		t.Fatal("two same-revision plans both applied")
	}
	if len(current.Lanes[0].Charges) != 1 || len(current.Lanes[1].Charges) != 0 {
		t.Fatal("fake CAS lost the winning reservation")
	}
	current = reserve(t, policy, current, 0, "b")
	if current.Lanes[1].Charges[0].ReservationID != 2 {
		t.Fatal("retry did not use fresh aggregate identity")
	}
	// Nested slices must not alias the input or a sibling proposal.
	a.Next.Lanes[0].Charges[0].Phase = "mutated"
	a.Next.Lanes[1].LaneID = "changed"
	if len(snapshot.Lanes[0].Charges) != 0 || snapshot.Lanes[1].LaneID != "b" || b.Next.Lanes[1].LaneID != "b" {
		t.Fatal("reservation plan aliases input or sibling plan")
	}
	ref := Reservation{LaneID: "a", ReservationID: 1}
	charged := reserve(t, policy, snapshot, 0, "a")
	running := attempt(t, policy, charged, 0, ref)
	running.Lanes[0].Charges[0].AttemptID = 99
	if charged.Lanes[0].Charges[0].AttemptID != 0 {
		t.Fatal("attempt plan aliases existing charged history")
	}
}

func TestExhaustionRetainsCustodyWithoutCounterReset(t *testing.T) {
	policy, snapshot := fixture()
	ref := Reservation{LaneID: "a", ReservationID: 1}
	charged := reserve(t, policy, snapshot, 0, "a")
	charged.NextID = maxCounter - 1
	running := attempt(t, policy, charged, 0, ref)
	if running.NextID != maxCounter || running.Lanes[0].Charges[0].AttemptID != maxCounter-1 {
		t.Fatal("final attempt identity wrapped")
	}
	plan, err := PlanRequestStop(policy, running, contextFor(running, 0), ref)
	reject(t, plan, err, HoldCounter)
	if len(running.Lanes[0].Charges) != 1 || running.Lanes[0].Charges[0].Phase != Attempting {
		t.Fatal("ID exhaustion released custody")
	}
	for _, which := range []string{"revision", "round"} {
		t.Run(which, func(t *testing.T) {
			policy, snapshot := fixture()
			stopping := stop(t, policy, reserve(t, policy, snapshot, 0, "a"), 0, ref)
			if which == "revision" {
				stopping.Revision = maxCounter
			} else {
				stopping.LastRound = maxCounter
			}
			ctx := contextFor(stopping, 0)
			plan, err := PlanConfirmInactive(policy, stopping, ctx, receiptFor(t, stopping, ctx, ref))
			reason := HoldCounter
			if which == "round" {
				reason = HoldBinding
			}
			reject(t, plan, err, reason)
			if stopping.Lanes[0].Charges[0].Phase != Stopping {
				t.Fatal("exhaustion released stopping custody")
			}
		})
	}
	// No ID is needed for confirming an already issued stop.
	policy, snapshot = fixture()
	stopping := stop(t, policy, reserve(t, policy, snapshot, 0, "a"), 0, ref)
	stopping.NextID = maxCounter
	confirmed := inactive(t, policy, stopping, 0, ref)
	if confirmed.NextID != maxCounter || confirmed.Lanes[0].Charges[0].Phase != Inactive {
		t.Fatal("ID-only exhaustion incorrectly reset or prevented cleanup")
	}
}

func TestPrunedHistoryDoesNotAuthorizeOldReceiptAgainstNewCustody(t *testing.T) {
	policy, snapshot := fixture()
	oldRef := Reservation{LaneID: "a", ReservationID: 1}
	stopping := stop(t, policy, reserve(t, policy, snapshot, 0, "a"), 0, oldRef)
	oldReceipt := receiptFor(t, stopping, contextFor(stopping, 0), oldRef)
	released := inactive(t, policy, stopping, 0, oldRef)
	newID := released.NextID
	newer := reserve(t, policy, released, WindowMillis, "a")
	newRef := Reservation{LaneID: "a", ReservationID: newID}
	newer = attempt(t, policy, newer, WindowMillis, newRef)
	newer = stop(t, policy, newer, WindowMillis, newRef)
	if len(newer.Lanes[0].Charges) != 1 || newer.Lanes[0].Charges[0].ReservationID == oldRef.ReservationID {
		t.Fatal("fixture did not prune old history and allocate distinct custody")
	}
	ctx := contextFor(newer, WindowMillis)
	oldReceipt.Binding = ctx.Binding
	plan, err := PlanConfirmInactive(policy, newer, ctx, oldReceipt)
	reject(t, plan, err, HoldReservation)
	if newer.Lanes[0].Charges[0].Phase != Stopping {
		t.Fatal("pruned reservation identity was reused")
	}
	good := receiptFor(t, newer, ctx, newRef)
	wrongAttempt := good
	wrongAttempt.Custody.AttemptID = oldReceipt.Custody.AttemptID
	plan, err = PlanConfirmInactive(policy, newer, ctx, wrongAttempt)
	reject(t, plan, err, HoldReceipt)
	wrongStop := good
	wrongStop.Custody.StopID = oldReceipt.Custody.StopID
	plan, err = PlanConfirmInactive(policy, newer, ctx, wrongStop)
	reject(t, plan, err, HoldReceipt)
	plan, err = PlanConfirmInactive(policy, newer, ctx, good)
	confirmed := accept(t, policy, newer, plan, err)
	if len(confirmed.Lanes[0].Charges) != 1 || confirmed.Lanes[0].Charges[0].Phase != Inactive || confirmed.Lanes[0].Charges[0].ReservationID != newID {
		t.Fatal("fresh exact newer receipt failed to release only newer custody")
	}
}

func TestFakeOuterPolicyEditInvalidatesEarlierAdmissionPlan(t *testing.T) {
	policy, snapshot := fixture()
	snapshot = reserve(t, policy, snapshot, 0, "a")
	prior, err := PlanReserve(policy, snapshot, contextFor(snapshot, 0), "b")
	if err != nil {
		t.Fatalf("positive enabled admission: %v", err)
	}
	// This is a typed fake policy commit, not a production edit/CAS adapter.
	current := snapshot
	current.Revision++
	policy.Lanes[1].Enabled = false
	if current.Revision == prior.ExpectedRevision {
		t.Fatal("stale enabled plan could win after shared policy revision advance")
	}
	if current.Lanes[0].Charges[0].Phase != Reserved || len(current.Lanes[0].Charges) != 1 {
		t.Fatal("fake policy edit erased preexisting debt/custody")
	}
	plan, err := PlanReserve(policy, current, contextFor(current, 0), "b")
	reject(t, plan, err, HoldLane)
}
