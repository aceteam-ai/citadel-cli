// Package agentresources models inactive, hypothetical resource-policy plans.
// It performs no I/O, persistence, clock reads, cancellation or native admission.
package agentresources

const maxUint64 = ^uint64(0)

// Identity is an exact, operator-declared namespace, not discovered host proof.
type Identity struct {
	HostID   string
	PoolID   string
	ConfigID string
}

type Unit string

const (
	GB  Unit = "GB"
	GiB Unit = "GiB"
)

type Floor struct {
	Amount uint64
	Unit   Unit
}

type Filesystems struct {
	Repository string
	Worktree   string
	State      string
}

type Policy struct {
	Identity    Identity
	PoolCap     uint64
	Floor       Floor
	Filesystems Filesystems
}

type Phase string

const (
	Reserved       Phase = "reserved"
	Creating       Phase = "creating"
	CreateUnknown  Phase = "create_unknown"
	Idle           Phase = "idle"
	InUse          Phase = "in_use"
	CleanupPending Phase = "cleanup_pending"
	CleanupUnknown Phase = "cleanup_unknown"
)

type Custody string

const (
	Inactive       Custody = "inactive"
	Active         Custody = "active"
	CustodyUnknown Custody = "unknown"
)

// WorktreeRef is an opaque exact reference, never a path to open or remove.
type WorktreeRef struct {
	ID         string
	Filesystem string
}

type Allocation struct {
	ID             uint64
	Worktree       WorktreeRef
	Phase          Phase
	Custody        Custody
	CleanupAttempt uint64 // Revision at which this cleanup attempt was proposed.
}

type Snapshot struct {
	Identity         Identity
	Revision         uint64
	Complete         bool
	NextAllocationID uint64 // Never decremented, including after removal.
	Allocations      []Allocation
}

// Context binds supplied observations to one hypothetical decision. Freshness
// and canonical physical identity must eventually be proved by other adapters.
type Context struct {
	Identity Identity
	Revision uint64
	Round    string
}

type DiskObservation struct {
	Filesystem string
	Known      bool
	Fresh      bool
	FreeBytes  uint64
}

type DiskEvidence struct {
	Context      Context
	Observations []DiskObservation
}

type CleanupEvidence struct {
	Context      Context
	AllocationID uint64
	Worktree     WorktreeRef
	Fresh        bool
	Owned        bool
	Clean        bool
	Published    bool
	Inactive     bool
}

type RemovalReceipt struct {
	Context          Context
	AllocationID     uint64
	Worktree         WorktreeRef
	CleanupAttempt   uint64
	Fresh            bool
	ConfirmedRemoved bool
}

// Plan is data only. A matching ExpectedRevision is not a successful CAS or
// durable reservation, and cannot authorize creation, deletion or execution.
type Plan struct {
	ExpectedRevision uint64
	Next             Snapshot
}

type HoldReason string

const (
	InvalidPolicy     HoldReason = "invalid_policy"
	InvalidSnapshot   HoldReason = "invalid_snapshot"
	StaleEvidence     HoldReason = "stale_evidence"
	DiskUnknown       HoldReason = "disk_unknown"
	DiskLow           HoldReason = "disk_low"
	PoolFull          HoldReason = "pool_full"
	InvalidTransition HoldReason = "invalid_transition"
	Unconfirmed       HoldReason = "unconfirmed_cleanup"
	CounterExhausted  HoldReason = "counter_exhausted"
)

// Hold carries only static diagnostic text and copied accounting IDs. A
// cancellation requirement is not a cancellation operation or completion proof.
type Hold struct {
	reason       HoldReason
	cancellation []uint64
}

func (h *Hold) Error() string {
	return "agent resource policy holds: " + string(h.reason)
}

func (h *Hold) Reason() HoldReason {
	return h.reason
}

func (h *Hold) CancellationRequired() []uint64 {
	return append([]uint64(nil), h.cancellation...)
}

func hold(reason HoldReason) error { return &Hold{reason: reason} }

// IDs have one bounded, exact representation. This does not establish that two
// different supplied IDs cannot refer to the same real host or filesystem.
func validID(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for i := 0; i < len(value); i++ {
		b := value[i]
		alphanumeric := b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
		if !alphanumeric && (i == 0 || b != '.' && b != '_' && b != ':' && b != '-') {
			return false
		}
	}
	return true
}

func validIdentity(value Identity) bool {
	return validID(value.HostID) && validID(value.PoolID) && validID(value.ConfigID)
}

func FloorBytes(floor Floor) (uint64, error) {
	var multiplier uint64
	switch floor.Unit {
	case GB:
		multiplier = 1_000_000_000
	case GiB:
		multiplier = 1_073_741_824
	default:
		return 0, hold(InvalidPolicy)
	}
	if floor.Amount < 40 || floor.Amount > maxUint64/multiplier {
		return 0, hold(InvalidPolicy)
	}
	return floor.Amount * multiplier, nil
}

func Validate(policy Policy, snapshot Snapshot) error {
	if !validIdentity(policy.Identity) || policy.PoolCap == 0 ||
		!validID(policy.Filesystems.Repository) || !validID(policy.Filesystems.Worktree) || !validID(policy.Filesystems.State) {
		return hold(InvalidPolicy)
	}
	if _, err := FloorBytes(policy.Floor); err != nil {
		return err
	}
	if snapshot.Identity != policy.Identity || !snapshot.Complete || snapshot.Revision == 0 ||
		snapshot.NextAllocationID == 0 || snapshot.Allocations == nil || uint64(len(snapshot.Allocations)) > policy.PoolCap {
		return hold(InvalidSnapshot)
	}
	previous := uint64(0)
	worktrees := make(map[WorktreeRef]bool, len(snapshot.Allocations))
	for _, allocation := range snapshot.Allocations {
		if allocation.ID <= previous || allocation.ID >= snapshot.NextAllocationID ||
			!validID(allocation.Worktree.ID) || allocation.Worktree.Filesystem != policy.Filesystems.Worktree || worktrees[allocation.Worktree] {
			return hold(InvalidSnapshot)
		}
		previous = allocation.ID
		worktrees[allocation.Worktree] = true
		switch allocation.Custody {
		case Inactive, Active, CustodyUnknown:
		default:
			return hold(InvalidSnapshot)
		}
		switch allocation.Phase {
		case Reserved, Creating, CreateUnknown, Idle, InUse:
			if allocation.CleanupAttempt != 0 || allocation.Phase == Idle && allocation.Custody != Inactive ||
				allocation.Phase == InUse && allocation.Custody == Inactive {
				return hold(InvalidSnapshot)
			}
		case CleanupPending, CleanupUnknown:
			if allocation.CleanupAttempt == 0 || allocation.CleanupAttempt > snapshot.Revision {
				return hold(InvalidSnapshot)
			}
		default:
			return hold(InvalidSnapshot)
		}
	}
	return nil
}

func validateContext(policy Policy, snapshot Snapshot, context Context) error {
	if err := Validate(policy, snapshot); err != nil {
		return err
	}
	if context.Identity != policy.Identity || context.Revision != snapshot.Revision || !validID(context.Round) {
		return hold(StaleEvidence)
	}
	return nil
}

// EvaluateDisk consumes already-classified evidence without reading a clock or
// filesystem. Unknown/stale evidence never becomes zero or an adequate default.
func EvaluateDisk(policy Policy, snapshot Snapshot, context Context, evidence DiskEvidence) error {
	if err := validateContext(policy, snapshot, context); err != nil {
		return err
	}
	if evidence.Context != context || len(evidence.Observations) > 3 {
		return hold(StaleEvidence)
	}
	required := map[string]bool{
		policy.Filesystems.Repository: true,
		policy.Filesystems.Worktree:   true,
		policy.Filesystems.State:      true,
	}
	seen := make(map[string]bool, 3)
	floor, _ := FloorBytes(policy.Floor) // validateContext already checked the policy.
	unknown, low := false, false
	for _, observation := range evidence.Observations {
		if !required[observation.Filesystem] || seen[observation.Filesystem] {
			return hold(StaleEvidence)
		}
		seen[observation.Filesystem] = true
		if !observation.Known || !observation.Fresh {
			unknown = true
		} else if observation.FreeBytes < floor {
			low = true
		}
	}
	if low {
		failure := &Hold{reason: DiskLow}
		for _, allocation := range snapshot.Allocations {
			if allocation.Custody == Active || allocation.Custody == CustodyUnknown {
				failure.cancellation = append(failure.cancellation, allocation.ID)
			}
		}
		return failure
	}
	if unknown || len(seen) != len(required) {
		return hold(DiskUnknown)
	}
	return nil
}

func nextPlan(snapshot Snapshot) (Plan, error) {
	if snapshot.Revision == maxUint64 {
		return Plan{}, hold(CounterExhausted)
	}
	next := snapshot
	next.Revision++
	next.Allocations = make([]Allocation, len(snapshot.Allocations))
	copy(next.Allocations, snapshot.Allocations)
	return Plan{ExpectedRevision: snapshot.Revision, Next: next}, nil
}

func PlanReserve(policy Policy, snapshot Snapshot, context Context, evidence DiskEvidence, worktree WorktreeRef) (Plan, error) {
	if err := EvaluateDisk(policy, snapshot, context, evidence); err != nil {
		return Plan{}, err
	}
	if uint64(len(snapshot.Allocations)) >= policy.PoolCap {
		return Plan{}, hold(PoolFull)
	}
	if !validID(worktree.ID) || worktree.Filesystem != policy.Filesystems.Worktree {
		return Plan{}, hold(InvalidTransition)
	}
	for _, allocation := range snapshot.Allocations {
		if allocation.Worktree == worktree {
			return Plan{}, hold(InvalidTransition)
		}
	}
	if snapshot.NextAllocationID == maxUint64 {
		return Plan{}, hold(CounterExhausted)
	}
	plan, err := nextPlan(snapshot)
	if err != nil {
		return Plan{}, err
	}
	plan.Next.Allocations = append(plan.Next.Allocations, Allocation{
		ID: snapshot.NextAllocationID, Worktree: worktree, Phase: Reserved, Custody: Inactive,
	})
	plan.Next.NextAllocationID++
	return plan, nil
}

func allocationIndex(snapshot Snapshot, id uint64) int {
	for i, allocation := range snapshot.Allocations {
		if allocation.ID == id {
			return i
		}
	}
	return -1
}

func PlanBeginCleanup(policy Policy, snapshot Snapshot, context Context, evidence CleanupEvidence) (Plan, error) {
	if err := validateContext(policy, snapshot, context); err != nil {
		return Plan{}, err
	}
	if evidence.Context != context || !evidence.Fresh || !evidence.Owned || !evidence.Clean || !evidence.Published || !evidence.Inactive {
		return Plan{}, hold(Unconfirmed)
	}
	index := allocationIndex(snapshot, evidence.AllocationID)
	if index < 0 || snapshot.Allocations[index].Phase != Idle || snapshot.Allocations[index].Custody != Inactive ||
		snapshot.Allocations[index].Worktree != evidence.Worktree {
		return Plan{}, hold(InvalidTransition)
	}
	plan, err := nextPlan(snapshot)
	if err != nil {
		return Plan{}, err
	}
	plan.Next.Allocations[index].Phase = CleanupPending
	plan.Next.Allocations[index].CleanupAttempt = plan.Next.Revision
	return plan, nil
}

func PlanCleanupUncertain(policy Policy, snapshot Snapshot, context Context, id uint64, attempt uint64) (Plan, error) {
	if err := validateContext(policy, snapshot, context); err != nil {
		return Plan{}, err
	}
	index := allocationIndex(snapshot, id)
	if index < 0 || snapshot.Allocations[index].Phase != CleanupPending || snapshot.Allocations[index].CleanupAttempt != attempt {
		return Plan{}, hold(InvalidTransition)
	}
	plan, err := nextPlan(snapshot)
	if err != nil {
		return Plan{}, err
	}
	plan.Next.Allocations[index].Phase = CleanupUnknown
	return plan, nil
}

func PlanConfirmRemoval(policy Policy, snapshot Snapshot, context Context, receipt RemovalReceipt) (Plan, error) {
	if err := validateContext(policy, snapshot, context); err != nil {
		return Plan{}, err
	}
	if receipt.Context != context || !receipt.Fresh || !receipt.ConfirmedRemoved {
		return Plan{}, hold(Unconfirmed)
	}
	index := allocationIndex(snapshot, receipt.AllocationID)
	if index < 0 {
		return Plan{}, hold(InvalidTransition)
	}
	allocation := snapshot.Allocations[index]
	if (allocation.Phase != CleanupPending && allocation.Phase != CleanupUnknown) || allocation.Custody != Inactive ||
		allocation.Worktree != receipt.Worktree || allocation.CleanupAttempt != receipt.CleanupAttempt {
		return Plan{}, hold(InvalidTransition)
	}
	plan, err := nextPlan(snapshot)
	if err != nil {
		return Plan{}, err
	}
	plan.Next.Allocations = append(plan.Next.Allocations[:index], plan.Next.Allocations[index+1:]...)
	return plan, nil
}
