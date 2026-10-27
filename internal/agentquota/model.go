// Package agentquota proposes inactive quota transitions over caller-supplied
// assertions. It performs no I/O and grants no permission to start native work.
package agentquota

const (
	WindowMillis      uint64 = 24 * 60 * 60 * 1000
	MaxTurnMillis     uint64 = 30 * 60 * 1000
	MaxTurnsPerWindow        = 10
	MaxLanes                 = 64
	MaxChargesPerLane        = MaxTurnsPerWindow + 1
	maxCounter        uint64 = ^uint64(0)
)

// Scope binds a record, not its storage namespace. The future canonical key is
// HostID+PoolID only; changing ConfigID must never select an empty quota ledger.
type Scope struct {
	HostID   string
	PoolID   string
	ConfigID string
}

type LanePolicy struct {
	LaneID        string
	Enabled       bool
	MaxTurns      uint32
	MaxTurnMillis uint64
}

// Policy has no activating defaults. A nonnil empty Lanes explicitly opts out
// every lane; removing a policy entry does not remove its stored lane or debt.
// Its content must be a trusted current input; ConfigID alone proves no policy
// freshness. Future outer CAS must bind policy edits and quota/resources alike.
type Policy struct {
	Scope Scope
	Lanes []LanePolicy
}

type Phase string

const (
	Reserved       Phase = "reserved"
	Attempting     Phase = "attempting"
	AttemptUnknown Phase = "attempt_unknown"
	Stopping       Phase = "stopping"
	StopUnknown    Phase = "stop_unknown"
	Inactive       Phase = "inactive"
)

type Charge struct {
	ReservationID   uint64
	AttemptID       uint64
	StopID          uint64
	ReservedAt      uint64
	TurnLimitMillis uint64
	Phase           Phase
}

type Lane struct {
	LaneID  string
	Charges []Charge
}

// Snapshot must already exist and be complete. Complete and all identity,
// clock and continuity fields are assertions, not proof furnished by this model.
// Inactive history may be pruned; NextID, LastRound and high water never reset.
type Snapshot struct {
	Scope             Scope
	Complete          bool
	Revision          uint64
	NextID            uint64
	ClockEpoch        string
	ElapsedHighWater  uint64
	LastRound         uint64
	StoreContinuityID string
	Lanes             []Lane
}

type Binding struct {
	Scope    Scope
	Revision uint64
	Round    uint64
}

type ClockState string

const (
	ClockKnown   ClockState = "known"
	ClockUnknown ClockState = "unknown"
)

// Known means a trusted logical elapsed sample, not merely a large wall-clock
// number. This package supplies no clock, freshness or epoch-continuity proof.
type ClockObservation struct {
	Binding       Binding
	State         ClockState
	Epoch         string
	ElapsedMillis uint64
}

type StoreState string

const (
	StoreContinuous    StoreState = "continuous"
	StoreUnknown       StoreState = "unknown"
	StoreRollback      StoreState = "rollback"
	StoreCommitUnknown StoreState = "commit_unknown"
)

// StoreObservation is independent of the clock. Only a future trusted adapter
// may establish continuity after commit ambiguity; no model operation repairs,
// upgrades or derives this assertion from an apparently empty old snapshot.
type StoreObservation struct {
	Binding      Binding
	State        StoreState
	ContinuityID string
}

type Context struct {
	Binding Binding
	Clock   ClockObservation
	Store   StoreObservation
}

type Reservation struct {
	LaneID        string
	ReservationID uint64
}

type Custody struct {
	Reservation
	AttemptID uint64
	StopID    uint64
}

type ActivityState string

const (
	ActivityUnknown  ActivityState = "unknown"
	ActivityActive   ActivityState = "active"
	ActivityInactive ActivityState = "confirmed_inactive"
)

type InactivityReceipt struct {
	Binding Binding
	Custody Custody
	State   ActivityState
}

// Plan is a hypothetical replacement, not CAS, a committed reservation, a
// commit acknowledgment, permission to launch, or proof of physical inactivity.
type Plan struct {
	ExpectedRevision uint64
	Next             Snapshot
}

type HoldReason string

const (
	HoldPolicy      HoldReason = "invalid_policy"
	HoldState       HoldReason = "invalid_state"
	HoldBinding     HoldReason = "evidence_binding_mismatch"
	HoldClock       HoldReason = "clock_unavailable_or_discontinuous"
	HoldStore       HoldReason = "store_continuity_unconfirmed"
	HoldCounter     HoldReason = "counter_exhausted"
	HoldLane        HoldReason = "lane_not_enabled"
	HoldBusy        HoldReason = "lane_custody_occupied"
	HoldQuota       HoldReason = "rolling_quota_exhausted"
	HoldDeadline    HoldReason = "turn_deadline_stop_required"
	HoldReservation HoldReason = "reservation_mismatch"
	HoldPhase       HoldReason = "transition_not_available"
	HoldReceipt     HoldReason = "inactivity_unconfirmed"
)

// StopRequired names overdue custody without releasing or refunding it.
type Hold struct {
	Reason       HoldReason
	StopRequired []Custody
}

func (h *Hold) Error() string {
	return "agent quota held: " + string(h.Reason)
}

func held(reason HoldReason) error {
	return &Hold{Reason: reason}
}

func canonicalID(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return s != "." && s != ".."
}

func validScope(s Scope) bool {
	return canonicalID(s.HostID) && canonicalID(s.PoolID) && canonicalID(s.ConfigID)
}

// Validate bounds supplied typed state before cloning. It cannot establish
// durable completeness, canonical physical identity, freshness or no rollback.
func Validate(policy Policy, snapshot Snapshot) error {
	if !validScope(policy.Scope) || policy.Lanes == nil || len(policy.Lanes) > MaxLanes {
		return held(HoldPolicy)
	}
	for i, lane := range policy.Lanes {
		if !canonicalID(lane.LaneID) || lane.MaxTurns == 0 || lane.MaxTurns > MaxTurnsPerWindow || lane.MaxTurnMillis == 0 || lane.MaxTurnMillis > MaxTurnMillis {
			return held(HoldPolicy)
		}
		for _, prior := range policy.Lanes[:i] {
			if prior.LaneID == lane.LaneID {
				return held(HoldPolicy)
			}
		}
	}
	if snapshot.Scope != policy.Scope || !snapshot.Complete || snapshot.Revision == 0 || snapshot.NextID == 0 || !canonicalID(snapshot.ClockEpoch) || !canonicalID(snapshot.StoreContinuityID) || snapshot.Lanes == nil || len(snapshot.Lanes) == 0 || len(snapshot.Lanes) > MaxLanes {
		return held(HoldState)
	}
	ids := make(map[uint64]bool)
	for i, lane := range snapshot.Lanes {
		if !canonicalID(lane.LaneID) || lane.Charges == nil || len(lane.Charges) > MaxChargesPerLane {
			return held(HoldState)
		}
		for _, prior := range snapshot.Lanes[:i] {
			if prior.LaneID == lane.LaneID {
				return held(HoldState)
			}
		}
		occupied, recent := 0, 0
		for _, charge := range lane.Charges {
			if charge.ReservationID == 0 || charge.ReservedAt > snapshot.ElapsedHighWater || charge.TurnLimitMillis == 0 || charge.TurnLimitMillis > MaxTurnMillis {
				return held(HoldState)
			}
			switch charge.Phase {
			case Reserved:
				if charge.AttemptID != 0 || charge.StopID != 0 {
					return held(HoldState)
				}
			case Attempting, AttemptUnknown:
				if charge.AttemptID == 0 || charge.StopID != 0 {
					return held(HoldState)
				}
			case Stopping, StopUnknown, Inactive:
				if charge.StopID == 0 {
					return held(HoldState)
				}
			default:
				return held(HoldState)
			}
			if (charge.AttemptID != 0 && charge.AttemptID <= charge.ReservationID) || (charge.StopID != 0 && charge.StopID <= charge.ReservationID) || (charge.StopID != 0 && charge.AttemptID != 0 && charge.StopID <= charge.AttemptID) {
				return held(HoldState)
			}
			for _, id := range []uint64{charge.ReservationID, charge.AttemptID, charge.StopID} {
				if id != 0 {
					if id >= snapshot.NextID || ids[id] {
						return held(HoldState)
					}
					ids[id] = true
				}
			}
			if charge.Phase != Inactive {
				occupied++
			}
			if snapshot.ElapsedHighWater-charge.ReservedAt < WindowMillis {
				recent++
			}
		}
		if occupied > 1 || recent > MaxTurnsPerWindow {
			return held(HoldState)
		}
	}
	for _, lane := range policy.Lanes {
		if laneIndex(snapshot, lane.LaneID) < 0 {
			return held(HoldState)
		}
	}
	return nil
}

func validateContext(policy Policy, snapshot Snapshot, ctx Context) error {
	if err := Validate(policy, snapshot); err != nil {
		return err
	}
	binding := ctx.Binding
	if binding.Scope != snapshot.Scope || binding.Revision != snapshot.Revision || binding.Round == 0 || binding.Round <= snapshot.LastRound || ctx.Clock.Binding != binding || ctx.Store.Binding != binding {
		return held(HoldBinding)
	}
	if ctx.Clock.State != ClockKnown || ctx.Clock.Epoch != snapshot.ClockEpoch || ctx.Clock.ElapsedMillis < snapshot.ElapsedHighWater {
		return held(HoldClock)
	}
	if ctx.Store.State != StoreContinuous || ctx.Store.ContinuityID != snapshot.StoreContinuityID {
		return held(HoldStore)
	}
	if snapshot.Revision == maxCounter || snapshot.LastRound == maxCounter {
		return held(HoldCounter)
	}
	return nil
}

func laneIndex(snapshot Snapshot, laneID string) int {
	for i, lane := range snapshot.Lanes {
		if lane.LaneID == laneID {
			return i
		}
	}
	return -1
}

func chargeIndex(snapshot Snapshot, ref Reservation) (int, int) {
	i := laneIndex(snapshot, ref.LaneID)
	if i >= 0 {
		for j, charge := range snapshot.Lanes[i].Charges {
			if charge.ReservationID == ref.ReservationID {
				return i, j
			}
		}
	}
	return -1, -1
}

func custody(laneID string, charge Charge) Custody {
	return Custody{Reservation: Reservation{LaneID: laneID, ReservationID: charge.ReservationID}, AttemptID: charge.AttemptID, StopID: charge.StopID}
}

func admissionDeadline(snapshot Snapshot, laneID string, now, currentLimit uint64) error {
	var overdue []Custody
	for _, lane := range snapshot.Lanes {
		if lane.LaneID != laneID {
			continue
		}
		for _, charge := range lane.Charges {
			limit := charge.TurnLimitMillis
			if currentLimit < limit {
				limit = currentLimit
			}
			if charge.Phase != Inactive && now-charge.ReservedAt >= limit {
				overdue = append(overdue, custody(lane.LaneID, charge))
			}
		}
	}
	if len(overdue) != 0 {
		return &Hold{Reason: HoldDeadline, StopRequired: overdue}
	}
	return nil
}

func proposed(snapshot Snapshot, ctx Context) Plan {
	next := snapshot
	next.Revision++
	next.LastRound = ctx.Binding.Round
	next.ElapsedHighWater = ctx.Clock.ElapsedMillis
	next.Lanes = make([]Lane, len(snapshot.Lanes))
	for i, lane := range snapshot.Lanes {
		next.Lanes[i] = Lane{LaneID: lane.LaneID, Charges: make([]Charge, 0, len(lane.Charges))}
		for _, charge := range lane.Charges {
			if charge.Phase == Inactive && ctx.Clock.ElapsedMillis-charge.ReservedAt >= WindowMillis {
				continue
			}
			next.Lanes[i].Charges = append(next.Lanes[i].Charges, charge)
		}
	}
	return Plan{ExpectedRevision: snapshot.Revision, Next: next}
}

// PlanReserve charges the lane before any attempt. Reservations immediately
// occupy its custody slot, even when launch never occurs. There is no refund.
func PlanReserve(policy Policy, snapshot Snapshot, ctx Context, laneID string) (Plan, error) {
	if err := validateContext(policy, snapshot, ctx); err != nil {
		return Plan{}, err
	}
	var lanePolicy *LanePolicy
	for i := range policy.Lanes {
		if policy.Lanes[i].LaneID == laneID {
			lanePolicy = &policy.Lanes[i]
			break
		}
	}
	if lanePolicy == nil || !lanePolicy.Enabled {
		return Plan{}, held(HoldLane)
	}
	if err := admissionDeadline(snapshot, laneID, ctx.Clock.ElapsedMillis, lanePolicy.MaxTurnMillis); err != nil {
		return Plan{}, err
	}
	i := laneIndex(snapshot, laneID)
	recent := uint32(0)
	for _, charge := range snapshot.Lanes[i].Charges {
		if charge.Phase != Inactive {
			return Plan{}, held(HoldBusy)
		}
		if ctx.Clock.ElapsedMillis-charge.ReservedAt < WindowMillis {
			recent++
		}
	}
	if recent >= lanePolicy.MaxTurns {
		return Plan{}, held(HoldQuota)
	}
	if snapshot.NextID == maxCounter || ctx.Clock.ElapsedMillis > maxCounter-lanePolicy.MaxTurnMillis {
		return Plan{}, held(HoldCounter)
	}
	plan := proposed(snapshot, ctx)
	plan.Next.Lanes[i].Charges = append(plan.Next.Lanes[i].Charges, Charge{ReservationID: snapshot.NextID, ReservedAt: ctx.Clock.ElapsedMillis, TurnLimitMillis: lanePolicy.MaxTurnMillis, Phase: Reserved})
	plan.Next.NextID++
	return plan, nil
}

// PlanAttempt mints exactly one attempt ID. Neither replay nor uncertain launch
// can obtain a second attempt from the same charged reservation.
func PlanAttempt(policy Policy, snapshot Snapshot, ctx Context, ref Reservation) (Plan, error) {
	if err := validateContext(policy, snapshot, ctx); err != nil {
		return Plan{}, err
	}
	i, j := chargeIndex(snapshot, ref)
	if i < 0 {
		return Plan{}, held(HoldReservation)
	}
	if snapshot.Lanes[i].Charges[j].Phase != Reserved {
		return Plan{}, held(HoldPhase)
	}
	var lanePolicy *LanePolicy
	for k := range policy.Lanes {
		if policy.Lanes[k].LaneID == ref.LaneID {
			lanePolicy = &policy.Lanes[k]
			break
		}
	}
	if lanePolicy == nil || !lanePolicy.Enabled {
		return Plan{}, held(HoldLane)
	}
	if err := admissionDeadline(snapshot, ref.LaneID, ctx.Clock.ElapsedMillis, lanePolicy.MaxTurnMillis); err != nil {
		return Plan{}, err
	}
	effectiveLimit := snapshot.Lanes[i].Charges[j].TurnLimitMillis
	if lanePolicy.MaxTurnMillis < effectiveLimit {
		effectiveLimit = lanePolicy.MaxTurnMillis
	}
	if snapshot.Lanes[i].Charges[j].ReservedAt > maxCounter-effectiveLimit {
		return Plan{}, held(HoldCounter)
	}
	recent := uint32(0)
	for _, charge := range snapshot.Lanes[i].Charges {
		if ctx.Clock.ElapsedMillis-charge.ReservedAt < WindowMillis {
			recent++
		}
	}
	if recent > lanePolicy.MaxTurns {
		return Plan{}, held(HoldQuota)
	}
	if snapshot.NextID == maxCounter {
		return Plan{}, held(HoldCounter)
	}
	plan := proposed(snapshot, ctx)
	charge := &plan.Next.Lanes[i].Charges[retainedIndex(plan.Next.Lanes[i], ref.ReservationID)]
	charge.Phase = Attempting
	charge.AttemptID = snapshot.NextID
	charge.TurnLimitMillis = effectiveLimit
	plan.Next.NextID++
	return plan, nil
}

func retainedIndex(lane Lane, reservationID uint64) int {
	for i, charge := range lane.Charges {
		if charge.ReservationID == reservationID {
			return i
		}
	}
	return -1
}

// PlanAttemptUncertain records uncertainty without retrying or freeing custody.
func PlanAttemptUncertain(policy Policy, snapshot Snapshot, ctx Context, ref Reservation) (Plan, error) {
	return planUncertain(policy, snapshot, ctx, ref, Attempting, AttemptUnknown)
}

// PlanStopUncertain retains the exact stop ID so only its subsequent confirmed
// inactivity receipt can release custody; no new stop attempt is issued here.
func PlanStopUncertain(policy Policy, snapshot Snapshot, ctx Context, ref Reservation) (Plan, error) {
	return planUncertain(policy, snapshot, ctx, ref, Stopping, StopUnknown)
}

func planUncertain(policy Policy, snapshot Snapshot, ctx Context, ref Reservation, from, to Phase) (Plan, error) {
	if err := validateContext(policy, snapshot, ctx); err != nil {
		return Plan{}, err
	}
	i, j := chargeIndex(snapshot, ref)
	if i < 0 {
		return Plan{}, held(HoldReservation)
	}
	if snapshot.Lanes[i].Charges[j].Phase != from {
		return Plan{}, held(HoldPhase)
	}
	plan := proposed(snapshot, ctx)
	plan.Next.Lanes[i].Charges[retainedIndex(plan.Next.Lanes[i], ref.ReservationID)].Phase = to
	return plan, nil
}

// PlanRequestStop is also available when policy disables/removes the lane or
// lowers its cap. It does not claim that a stop request was sent or took effect.
func PlanRequestStop(policy Policy, snapshot Snapshot, ctx Context, ref Reservation) (Plan, error) {
	if err := validateContext(policy, snapshot, ctx); err != nil {
		return Plan{}, err
	}
	i, j := chargeIndex(snapshot, ref)
	if i < 0 {
		return Plan{}, held(HoldReservation)
	}
	phase := snapshot.Lanes[i].Charges[j].Phase
	if phase != Reserved && phase != Attempting && phase != AttemptUnknown {
		return Plan{}, held(HoldPhase)
	}
	if snapshot.NextID == maxCounter {
		return Plan{}, held(HoldCounter)
	}
	plan := proposed(snapshot, ctx)
	charge := &plan.Next.Lanes[i].Charges[retainedIndex(plan.Next.Lanes[i], ref.ReservationID)]
	charge.Phase = Stopping
	charge.StopID = snapshot.NextID
	plan.Next.NextID++
	return plan, nil
}

// PlanConfirmInactive requires actual confirmed inactivity, not elapsed time,
// a cancel request, return status or an uncertain completion. Its charge remains
// for the full rolling window. A never-attempted reservation also needs a stop ID.
func PlanConfirmInactive(policy Policy, snapshot Snapshot, ctx Context, receipt InactivityReceipt) (Plan, error) {
	if err := validateContext(policy, snapshot, ctx); err != nil {
		return Plan{}, err
	}
	if receipt.Binding != ctx.Binding {
		return Plan{}, held(HoldBinding)
	}
	i, j := chargeIndex(snapshot, receipt.Custody.Reservation)
	if i < 0 {
		return Plan{}, held(HoldReservation)
	}
	charge := snapshot.Lanes[i].Charges[j]
	if charge.Phase != Stopping && charge.Phase != StopUnknown {
		return Plan{}, held(HoldPhase)
	}
	if receipt.State != ActivityInactive || receipt.Custody != custody(snapshot.Lanes[i].LaneID, charge) {
		return Plan{}, held(HoldReceipt)
	}
	plan := proposed(snapshot, ctx)
	k := retainedIndex(plan.Next.Lanes[i], charge.ReservationID)
	plan.Next.Lanes[i].Charges[k].Phase = Inactive
	if ctx.Clock.ElapsedMillis-charge.ReservedAt >= WindowMillis {
		plan.Next.Lanes[i].Charges = append(plan.Next.Lanes[i].Charges[:k], plan.Next.Lanes[i].Charges[k+1:]...)
	}
	return plan, nil
}
