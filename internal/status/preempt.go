package status

import (
	"fmt"
	"sort"
	"strings"
)

// PreemptCandidate is a running managed service considered as a source of VRAM
// to reclaim for an incoming deploy. It is the pure input to PlanPreemption:
// the executor derives these from the live node status (per-service footprint
// VRAM + an instantaneous idle signal + the manifest's pinned_services
// allowlist). See citadel-cli#577.
type PreemptCandidate struct {
	// Name is the logical service name (matches citadel.yaml services[].name).
	Name string
	// VRAMBytes is the VRAM currently attributed to this service's container.
	// Stopping a service reclaims exactly this much VRAM.
	VRAMBytes uint64
	// Idle is an instantaneous "not doing work" signal used ONLY to order
	// preemption (idle before busy). It is NOT a gate: a busy non-pinned service
	// is still preemptible when idle candidates cannot free enough VRAM.
	Idle bool
	// Pinned reports whether the service is in citadel.yaml pinned_services. A
	// pinned service is NEVER preempted; if a deploy cannot fit without evicting
	// one, the deploy is rejected.
	Pinned bool
}

// PreemptPlan is the decision returned by PlanPreemption.
type PreemptPlan struct {
	// Fits reports whether requiredVRAM can be satisfied. When true, stopping the
	// services named in Stop (in order) frees enough VRAM (Stop may be empty when
	// the deploy already fits). When false the deploy MUST be rejected: it cannot
	// fit without evicting a pinned service.
	Fits bool
	// Stop is the ordered list of non-pinned service names to durably stop,
	// idle-first then largest-VRAM-first. It is the minimal prefix that frees
	// enough VRAM (or, when !Fits, every non-pinned VRAM holder — still not
	// enough).
	Stop []string
	// Blocked lists pinned services still holding VRAM when the deploy does not
	// fit — the reason it was rejected. Nil when Fits.
	Blocked []string
	// Reason is a human-readable explanation for logs and error messages.
	Reason string
}

// PlanPreemption decides which running non-pinned services to stop so a deploy
// needing requiredVRAM bytes fits, given availableVRAM (currently-free) bytes.
//
// Contract:
//   - requiredVRAM == 0: unknown/undeclared requirement → Fits with no
//     preemption. Never evict on an absent signal (the fail-safe that keeps a
//     deploy carrying no VRAM budget from disrupting the node).
//   - availableVRAM >= requiredVRAM: already fits → Fits, no preemption.
//   - Pinned candidates are NEVER stopped. If the deploy cannot fit after
//     stopping every non-pinned candidate, Fits=false and Blocked names the
//     pinned VRAM holders.
//
// Ordering is idle-first (stop idle before busy) then largest-VRAM-first (free
// the most per eviction so the fewest services are disrupted), name-ascending as
// a deterministic tie-break. Busy non-pinned services ARE preemptible — idle is
// ordering, not a gate. Pure (no I/O) so the decision is unit-testable.
func PlanPreemption(candidates []PreemptCandidate, requiredVRAM, availableVRAM uint64) PreemptPlan {
	if requiredVRAM == 0 {
		return PreemptPlan{Fits: true, Reason: "no VRAM requirement declared; preemption skipped"}
	}
	if availableVRAM >= requiredVRAM {
		return PreemptPlan{
			Fits:   true,
			Reason: fmt.Sprintf("deploy fits without preemption: %s free ≥ %s required", fmtGB(availableVRAM), fmtGB(requiredVRAM)),
		}
	}

	// Partition pinned vs preemptible; remember pinned VRAM holders for the
	// rejection reason.
	preemptible := make([]PreemptCandidate, 0, len(candidates))
	var pinnedHolders []string
	for _, c := range candidates {
		if c.Pinned {
			if c.VRAMBytes > 0 {
				pinnedHolders = append(pinnedHolders, c.Name)
			}
			continue
		}
		preemptible = append(preemptible, c)
	}

	// Order: idle before busy, then larger VRAM first, then name ascending.
	sort.SliceStable(preemptible, func(i, j int) bool {
		a, b := preemptible[i], preemptible[j]
		if a.Idle != b.Idle {
			return a.Idle // idle first
		}
		if a.VRAMBytes != b.VRAMBytes {
			return a.VRAMBytes > b.VRAMBytes // larger reclaim first
		}
		return a.Name < b.Name
	})

	var (
		freed uint64
		stop  []string
	)
	for _, c := range preemptible {
		if availableVRAM+freed >= requiredVRAM {
			break
		}
		if c.VRAMBytes == 0 {
			continue // stopping a service that holds no VRAM frees nothing
		}
		stop = append(stop, c.Name)
		freed += c.VRAMBytes
	}

	if availableVRAM+freed >= requiredVRAM {
		return PreemptPlan{
			Fits: true,
			Stop: stop,
			Reason: fmt.Sprintf("preempting %d service(s) to free %s (%s free + %s reclaimed ≥ %s required)",
				len(stop), fmtGB(freed), fmtGB(availableVRAM), fmtGB(freed), fmtGB(requiredVRAM)),
		}
	}

	sort.Strings(pinnedHolders)
	reason := fmt.Sprintf("insufficient VRAM: %s free + %s reclaimable from preemptible services < %s required",
		fmtGB(availableVRAM), fmtGB(freed), fmtGB(requiredVRAM))
	if len(pinnedHolders) > 0 {
		reason += fmt.Sprintf("; pinned services hold VRAM and cannot be preempted: %s", strings.Join(pinnedHolders, ", "))
	}
	return PreemptPlan{Fits: false, Stop: stop, Blocked: pinnedHolders, Reason: reason}
}

// fmtGB renders a byte count as a compact "5.5GB" for human-readable reasons.
func fmtGB(b uint64) string {
	return fmt.Sprintf("%.1fGB", float64(b)/(1<<30))
}
