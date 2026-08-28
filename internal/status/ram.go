// internal/status/ram.go
//
// RAM ceiling + preflight decisions for citadel#831 v1 (RAM isolation, the
// tractable half of docs/design-resource-isolation.md). Pure (no I/O), mirrors
// two existing patterns exactly rather than inventing new conventions:
//
//   - PlanPreemption (preempt.go, #577/VRAM): requiredX==0 (no declared
//     budget) always Fits -- never refuse on an absent signal.
//   - planDiskPreflight (internal/jobs/disk_space.go, #828): fail OPEN on an
//     estimation/signal failure, fail CLOSED only on a CONFIRMED shortfall.
//
// The design doc's §6 Q1 ("reserved-floor sizing policy") was left an
// explicit open question by the owner (see citadel#831's design-decision
// comment, which resolved preflight-fail behavior and VRAM/RAM enforcement
// posture but not this). ramHeadroomBytes/minViableRAMCeilingBytes below are
// the chosen, documented default for that open question -- generous enough
// that a well-behaved GPU/media service never hits its own limit under
// normal conditions, small enough that pinned production services keep real
// headroom. Reconsider if operational experience says otherwise; this is a
// starting point, not a value pinned by an external spec.
package status

import "fmt"

// ramHeadroomBytes is reserved for the OS and any other unpinned/untracked
// process, ON TOP OF pinned services' own measured RAM footprint, before a
// new GPU/media service's RAM ceiling is derived. See the package doc above
// for why this exact value is a chosen default, not a resolved spec value.
const ramHeadroomBytes uint64 = 2 << 30 // 2 GiB

// minViableRAMCeilingBytes is the smallest ceiling RAMBudgetBytes will ever
// return. Below this, RAMBudgetBytes returns 0 ("no safe ceiling can be
// derived") rather than clamping UP to this value. That direction matters: a
// clamped-up floor would be a FABRICATED number with no relationship to
// what's actually free, and applying it as a real inference engine's
// mem_limit reproduces the exact failure the design doc warns against for
// the Tier-2 2GB default ("breaks inference/embedding services") — just
// reached by a different path, with the added danger that a caller cannot
// tell "genuinely tight but real" apart from "clamped nonsense." Returning 0
// instead lets the caller (applyRAMIsolation) skip applying any ceiling this
// start — the same fail-open direction every other decision in this
// mechanism already takes on an unreliable signal.
const minViableRAMCeilingBytes uint64 = 2 << 30 // 2 GiB

// RAMBudgetBytes computes the mem_limit ceiling (bytes) for a new GPU/media
// service, or 0 when no ceiling can be safely derived (see
// minViableRAMCeilingBytes). Linux's OOM killer fires cgroup-scoped, at a
// container's own memory.max, before the host's global OOM killer ever needs
// to pick a victim across containers -- that is the whole mechanism
// citadel#831 relies on, and it is why a GENEROUS, dynamically-derived
// ceiling (rather than a fixed small default, which would break real
// inference/media-gen workloads that legitimately use 10-20GB+) is both safe
// and sufficient.
//
// ceiling = availableRAMBytes - pinnedRAMBytes - ramHeadroomBytes; returns 0
// when that would be below minViableRAMCeilingBytes (including negative,
// i.e. reserved >= available). Pure; callers supply availableRAMBytes (from
// SystemMetrics.MemoryAvailableGB, which is already "available for programs
// to allocate," not raw free -- see its doc comment) and pinnedRAMBytes (the
// sum of ServiceFootprint.RAMBytes across currently-running pinned_services,
// excluding the service being sized).
func RAMBudgetBytes(availableRAMBytes, pinnedRAMBytes uint64) uint64 {
	reserved := pinnedRAMBytes + ramHeadroomBytes
	if reserved >= availableRAMBytes {
		return 0
	}
	ceiling := availableRAMBytes - reserved
	if ceiling < minViableRAMCeilingBytes {
		return 0
	}
	return ceiling
}

// RAMPreflightResult is PlanRAMPreflight's decision.
type RAMPreflightResult struct {
	// Fits reports whether requiredRAMBytes can be satisfied without exceeding
	// the RAMBudgetBytes ceiling. When false, the caller MUST refuse the start
	// (job FAILURE) rather than proceed -- see PlanRAMPreflight's doc comment
	// for exactly when that happens.
	Fits bool
	// Reason is a human-readable explanation for logs and error messages.
	Reason string
}

// PlanRAMPreflight decides whether a job's declared RAM requirement fits
// alongside currently pinned services, mirroring PlanPreemption's (VRAM,
// #577) and planDiskPreflight's (#828) contract exactly:
//
//   - requiredRAMBytes == 0 (no declared budget -- there is no
//     backend-forwarded RAM field today, matching vram_mb/vram_gb's own
//     pre-#831 state; see internal/jobs.parseRequiredRAMBytes) => Fits,
//     UNCONDITIONALLY. Never refuse on an absent signal -- the identical
//     fail-safe direction #577's requiredVRAM==0 case already established
//     for VRAM, and the "fail open on an absent/failed estimate" half of
//     #828's disk-preflight policy.
//   - A declared requirement that exceeds RAMBudgetBytes is the ONLY refusing
//     case: a confirmed shortfall, never a guess. This matches the owner's
//     citadel#831 design decision ("refuse fast, clear error") for the
//     RAM preflight specifically.
//
// Pure (no I/O) so the decision is unit-testable without a live node.
func PlanRAMPreflight(requiredRAMBytes, availableRAMBytes, pinnedRAMBytes uint64) RAMPreflightResult {
	if requiredRAMBytes == 0 {
		return RAMPreflightResult{Fits: true, Reason: "no RAM requirement declared; preflight skipped"}
	}
	budget := RAMBudgetBytes(availableRAMBytes, pinnedRAMBytes)
	reserved := pinnedRAMBytes + ramHeadroomBytes
	if requiredRAMBytes <= budget {
		return RAMPreflightResult{
			Fits: true,
			Reason: fmt.Sprintf("fits: %s required <= %s budget (%s available, %s reserved for pinned services + headroom)",
				fmtGB(requiredRAMBytes), fmtGB(budget), fmtGB(availableRAMBytes), fmtGB(reserved)),
		}
	}
	return RAMPreflightResult{
		Fits: false,
		Reason: fmt.Sprintf("insufficient RAM: %s required > %s budget (%s available, %s reserved for pinned services + headroom)",
			fmtGB(requiredRAMBytes), fmtGB(budget), fmtGB(availableRAMBytes), fmtGB(reserved)),
	}
}
